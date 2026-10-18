package diagnostics

import (
	"embed"
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"time"
)

//go:embed web/*
var webAssets embed.FS

type WebLimits struct {
	Sessions int `json:"sessions"`
	Calls    int `json:"calls"`
}

type WebRuntime struct {
	AuthenticatedSessions int            `json:"authenticatedSessions"`
	ConnectedSessions     int            `json:"connectedSessions"`
	Registrations         map[string]int `json:"registrations"`
	Calls                 []Call         `json:"calls"`
}

type Call struct {
	State     string `json:"state"`
	Direction string `json:"direction"`
	Count     int    `json:"count"`
}

type Infrastructure struct {
	PeerConnections int            `json:"peerConnections"`
	WSSConnections  int            `json:"wssConnections"`
	WSSWriterQueue  int            `json:"wssWriterQueue"`
	ICERoutes       map[string]int `json:"iceRoutes"`
}

type WebTURN struct {
	Allocations     int     `json:"allocations"`
	AllocationLimit int     `json:"allocationLimit"`
	QuotaRatio      float64 `json:"quotaRatio"`
}

type WebStorage struct {
	Healthy                 bool  `json:"healthy"`
	PendingEvents           int64 `json:"pendingEvents"`
	MaxSessionPendingEvents int64 `json:"maxSessionPendingEvents"`
	DatabaseBytes           int64 `json:"databaseBytes"`
	WALBytes                int64 `json:"walBytes"`
}

type Process struct {
	CPUSeconds          float64 `json:"cpuSeconds"`
	CPUCores            int     `json:"cpuCores"`
	ResidentMemoryBytes float64 `json:"residentMemoryBytes"`
	VirtualMemoryBytes  float64 `json:"virtualMemoryBytes"`
	Goroutines          uint32  `json:"goroutines"`
	HeapBytes           uint64  `json:"heapBytes"`
	HeapInUse           uint64  `json:"heapInUseBytes"`
	SystemBytes         uint64  `json:"systemBytes"`
	GCCount             uint32  `json:"gcCount"`
}

type Snapshot struct {
	GeneratedAtUTC time.Time      `json:"generatedAtUtc"`
	NodeID         string         `json:"nodeId"`
	Version        string         `json:"version"`
	Ready          bool           `json:"ready"`
	Limits         WebLimits      `json:"limits"`
	Runtime        WebRuntime     `json:"runtime"`
	Infrastructure Infrastructure `json:"infrastructure"`
	TURN           WebTURN        `json:"turn"`
	Storage        WebStorage     `json:"storage"`
	Process        Process        `json:"process"`
}

type SnapshotProvider func() Snapshot

type WebHandler struct {
	provider SnapshotProvider
}

func NewWebHandler(provider SnapshotProvider) *WebHandler {
	return &WebHandler{provider: provider}
}

func (h *WebHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	setSecurityHeaders(writer.Header())
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch request.URL.Path {
	case "/diagnostics":
		http.Redirect(writer, request, "/diagnostics/", http.StatusPermanentRedirect)
	case "/diagnostics/":
		h.serveAsset(writer, request, "web/index.html", "text/html; charset=utf-8")
	case "/diagnostics/app.js":
		h.serveAsset(writer, request, "web/app.js", "text/javascript; charset=utf-8")
	case "/diagnostics/styles.css":
		h.serveAsset(writer, request, "web/styles.css", "text/css; charset=utf-8")
	case "/diagnostics/api/v1/snapshot":
		h.serveSnapshot(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

func (h *WebHandler) serveAsset(writer http.ResponseWriter, request *http.Request, name, contentType string) {
	raw, err := webAssets.ReadFile(name)
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(raw)
	}
}

func (h *WebHandler) serveSnapshot(writer http.ResponseWriter, request *http.Request) {
	if h.provider == nil {
		http.Error(writer, "diagnostics unavailable", http.StatusServiceUnavailable)
		return
	}
	value := h.provider()
	if value.GeneratedAtUTC.IsZero() {
		value.GeneratedAtUTC = time.Now().UTC()
	}
	value.Process = currentProcess(value.Process)
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method == http.MethodHead {
		writer.WriteHeader(http.StatusOK)
		return
	}
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(true)
	_ = encoder.Encode(value)
}

func currentProcess(value Process) Process {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	value.Goroutines = uint32(runtime.NumGoroutine())
	value.HeapBytes = memory.HeapAlloc
	value.HeapInUse = memory.HeapInuse
	value.SystemBytes = memory.Sys
	value.GCCount = memory.NumGC
	if value.CPUCores == 0 {
		value.CPUCores = runtime.NumCPU()
	}
	return value
}

func setSecurityHeaders(header http.Header) {
	header.Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'", "script-src 'self'", "style-src 'self'",
		"connect-src 'self'", "img-src 'self' data:", "base-uri 'none'",
		"form-action 'none'", "frame-ancestors 'none'",
	}, "; "))
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
}
