package diagnostics

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"slices"
	"time"

	"github.com/debt-flow/sip-gateway/internal/config"
)

type Report struct {
	SchemaVersion  int        `json:"schemaVersion"`
	GeneratedAtUTC time.Time  `json:"generatedAtUtc"`
	Version        string     `json:"version"`
	Runtime        Runtime    `json:"runtime"`
	NodeID         string     `json:"nodeId"`
	HTTP           HTTP       `json:"http"`
	Logging        Logging    `json:"logging"`
	Capability     Capability `json:"capability"`
	Storage        Storage    `json:"storage"`
	ICE            ICE        `json:"ice"`
	TURN           TURN       `json:"turn"`
	SIP            SIP        `json:"sip"`
	Limits         Limits     `json:"limits"`
	Lease          Lease      `json:"lease"`
	ShutdownGrace  string     `json:"shutdownGrace"`
}

type Runtime struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	NumCPU int    `json:"numCpu"`
	Go     string `json:"goVersion"`
}

type HTTP struct {
	Listen        string `json:"listen"`
	MetricsListen string `json:"metricsListen"`
	AllowedOrigin string `json:"allowedOrigin"`
	AuthTimeout   string `json:"authTimeout"`
	Heartbeat     string `json:"heartbeat"`
	IdleTimeout   string `json:"idleTimeout"`
}

type Logging struct {
	MaxBytes   int64 `json:"maxBytes"`
	MaxBackups int   `json:"maxBackups"`
}

type Capability struct {
	Issuer          string   `json:"issuer"`
	Audience        string   `json:"audience"`
	ProtectedKeyIDs []string `json:"protectedKeyIds"`
	PlainKeyCount   int      `json:"plainKeyCount"`
}

type Storage struct {
	MaxBytes          int64 `json:"maxBytes"`
	RetentionDays     int   `json:"retentionDays"`
	MaxPendingGlobal  int   `json:"maxPendingGlobal"`
	MaxPendingSession int   `json:"maxPendingSession"`
}

type ICE struct {
	UDPMuxListen string `json:"udpMuxListen"`
	AdvertisedIP string `json:"advertisedIp"`
}

type TURN struct {
	Enabled        bool   `json:"enabled"`
	ListenUDP      string `json:"listenUdp"`
	ListenTCP      string `json:"listenTcp"`
	Realm          string `json:"realm"`
	AdvertisedIP   string `json:"advertisedIp"`
	RelayMinPort   uint16 `json:"relayMinPort"`
	RelayMaxPort   uint16 `json:"relayMaxPort"`
	CredentialTTL  string `json:"credentialTtl"`
	MaxAllocations int    `json:"maxAllocations"`
	BytesPerSecond int    `json:"bytesPerSecond"`
}

type SIP struct {
	SpikeApproved   bool   `json:"spikeApproved"`
	ListenUDP       string `json:"listenUdp"`
	ListenTCP       string `json:"listenTcp"`
	RTPAdvertisedIP string `json:"rtpAdvertisedIp"`
	RTPMinPort      uint16 `json:"rtpMinPort"`
	RTPMaxPort      uint16 `json:"rtpMaxPort"`
}

type Limits struct {
	MaxMessageBytes   int64  `json:"maxMessageBytes"`
	MessagesPerWindow int    `json:"messagesPerWindow"`
	MessageWindow     string `json:"messageWindow"`
	WriterQueue       int    `json:"writerQueue"`
	MaxSessions       int    `json:"maxSessions"`
	MaxCalls          int    `json:"maxCalls"`
}

type Lease struct {
	RenewEvery     string `json:"renewEvery"`
	ExpiresAfter   string `json:"expiresAfter"`
	ReconnectGrace string `json:"reconnectGrace"`
}

func Build(cfg config.Config, version string, now time.Time) Report {
	keyIDs := make([]string, 0, len(cfg.Capability.ProtectedKeys))
	for keyID := range cfg.Capability.ProtectedKeys {
		keyIDs = append(keyIDs, keyID)
	}
	slices.Sort(keyIDs)
	return Report{
		SchemaVersion: 1, GeneratedAtUTC: now.UTC(), Version: version,
		Runtime: Runtime{OS: runtime.GOOS, Arch: runtime.GOARCH, NumCPU: runtime.NumCPU(), Go: runtime.Version()},
		NodeID:  cfg.NodeID,
		HTTP: HTTP{
			Listen: cfg.HTTP.Listen, MetricsListen: cfg.HTTP.MetricsListen, AllowedOrigin: cfg.HTTP.AllowedOrigin,
			AuthTimeout: cfg.HTTP.AuthTimeout.String(), Heartbeat: cfg.HTTP.Heartbeat.String(),
			IdleTimeout: cfg.HTTP.IdleTimeout.String(),
		},
		Logging: Logging{MaxBytes: cfg.Logging.MaxBytes, MaxBackups: cfg.Logging.MaxBackups},
		Capability: Capability{
			Issuer: cfg.Capability.Issuer, Audience: cfg.Capability.Audience,
			ProtectedKeyIDs: keyIDs, PlainKeyCount: len(cfg.Capability.PlaintextKeys),
		},
		Storage: Storage{
			MaxBytes: cfg.Storage.MaxBytes, RetentionDays: cfg.Storage.RetentionDays,
			MaxPendingGlobal: cfg.Storage.MaxPendingGlobal, MaxPendingSession: cfg.Storage.MaxPendingSession,
		},
		ICE: ICE{UDPMuxListen: cfg.ICE.UDPMuxListen, AdvertisedIP: cfg.ICE.AdvertisedIP},
		TURN: TURN{
			Enabled: cfg.TURN.Enabled, ListenUDP: cfg.TURN.ListenUDP, ListenTCP: cfg.TURN.ListenTCP,
			Realm: cfg.TURN.Realm, AdvertisedIP: cfg.TURN.AdvertisedIP,
			RelayMinPort: cfg.TURN.RelayMinPort, RelayMaxPort: cfg.TURN.RelayMaxPort,
			CredentialTTL: cfg.TURN.CredentialTTL.String(), MaxAllocations: cfg.TURN.MaxAllocations,
			BytesPerSecond: cfg.TURN.BytesPerSecond,
		},
		SIP: SIP{
			SpikeApproved: cfg.SIP.SpikeApproved, ListenUDP: cfg.SIP.ListenUDP,
			ListenTCP: cfg.SIP.ListenTCP, RTPAdvertisedIP: cfg.SIP.RTPAdvertisedIP,
			RTPMinPort: cfg.SIP.RTPMinPort, RTPMaxPort: cfg.SIP.RTPMaxPort,
		},
		Limits: Limits{
			MaxMessageBytes: cfg.Limits.MaxMessageBytes, MessagesPerWindow: cfg.Limits.MessagesPerWindow,
			MessageWindow: cfg.Limits.MessageWindow.String(), WriterQueue: cfg.Limits.WriterQueue,
			MaxSessions: cfg.Limits.MaxSessions, MaxCalls: cfg.Limits.MaxCalls,
		},
		Lease: Lease{
			RenewEvery: cfg.Lease.RenewEvery.String(), ExpiresAfter: cfg.Lease.ExpiresAfter.String(),
			ReconnectGrace: cfg.Lease.ReconnectGrace.String(),
		},
		ShutdownGrace: cfg.ShutdownGrace.String(),
	}
}

func Write(path string, cfg config.Config, version string, now time.Time) error {
	if path == "" {
		return errors.New("diagnostics output path is required")
	}
	encoded, err := json.MarshalIndent(Build(cfg, version, now), "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return os.WriteFile(path, encoded, 0o600)
}
