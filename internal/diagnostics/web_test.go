package diagnostics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebHandlerServesReadOnlyDashboardAndSanitizedSnapshot(t *testing.T) {
	handler := NewWebHandler(func() Snapshot {
		return Snapshot{NodeID: "gateway-01", Version: "1.0.0", Ready: true}
	})

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/diagnostics/", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "SIP Gateway Diagnostics") {
		t.Fatalf("unexpected dashboard response: status=%d body=%s", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("dashboard must set a restrictive content security policy")
	}

	snapshot := httptest.NewRecorder()
	handler.ServeHTTP(snapshot, httptest.NewRequest(http.MethodGet, "/diagnostics/api/v1/snapshot", nil))
	if snapshot.Code != http.StatusOK {
		t.Fatalf("unexpected snapshot status %d", snapshot.Code)
	}
	var value Snapshot
	if err := json.Unmarshal(snapshot.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value.NodeID != "gateway-01" || !value.Ready || value.Process.Goroutines == 0 || value.Process.CPUCores == 0 {
		t.Fatalf("unexpected snapshot: %+v", value)
	}
	if snapshot.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("snapshot must not be cached")
	}
}

func TestWebHandlerRejectsMutationMethods(t *testing.T) {
	handler := NewWebHandler(func() Snapshot { return Snapshot{} })
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/diagnostics/api/v1/snapshot", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", response.Code)
	}
}
