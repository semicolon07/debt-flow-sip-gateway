package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultRequiresTURNAdvertisedIP(t *testing.T) {
	cfg := Default()
	makeCapabilityValid(&cfg)
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "turn.advertisedIp") {
		t.Fatalf("expected TURN advertised IP error, got %v", err)
	}
	cfg.TURN.AdvertisedIP = "10.0.0.10"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid defaults: %v", err)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("unknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "field unknown") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestValidateRejectsPublicHTTPListener(t *testing.T) {
	cfg := Default()
	makeCapabilityValid(&cfg)
	cfg.TURN.AdvertisedIP = "10.0.0.10"
	cfg.HTTP.Listen = "0.0.0.0:8080"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "127.0.0.1:8080") {
		t.Fatalf("expected loopback error, got %v", err)
	}
}

func TestValidateAllowsExplicitDevelopmentLoopbackOrigin(t *testing.T) {
	cfg := Default()
	makeCapabilityValid(&cfg)
	cfg.TURN.AdvertisedIP = "10.0.0.10"
	cfg.HTTP.AllowedOrigin = "http://127.0.0.1:8765"
	cfg.HTTP.AllowInsecureLoopbackOrigin = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit loopback development origin should be valid: %v", err)
	}
}

func TestValidateRejectsInsecureOriginWithoutOptInOrOutsideLoopback(t *testing.T) {
	for name, test := range map[string]struct {
		origin  string
		enabled bool
	}{
		"not opted in": {origin: "http://127.0.0.1:8765", enabled: false},
		"non-loopback": {origin: "http://10.0.0.20:8765", enabled: true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			makeCapabilityValid(&cfg)
			cfg.TURN.AdvertisedIP = "10.0.0.10"
			cfg.HTTP.AllowedOrigin = test.origin
			cfg.HTTP.AllowInsecureLoopbackOrigin = test.enabled
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected insecure origin to be rejected")
			}
		})
	}
}

func TestValidateRejectsPlaintextCapabilityKeyWithoutExplicitDevelopmentOptIn(t *testing.T) {
	cfg := Default()
	cfg.NodeID = "gateway-1"
	cfg.TURN.AdvertisedIP = "10.0.0.10"
	cfg.Capability.PlaintextKeys = map[string]string{"key-1": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "allowPlaintextKeys") {
		t.Fatalf("expected plaintext key opt-in error, got %v", err)
	}
}

func TestValidateRejectsDuplicateCapabilityKeyAcrossStores(t *testing.T) {
	cfg := Default()
	cfg.NodeID = "gateway-1"
	cfg.TURN.AdvertisedIP = "10.0.0.10"
	cfg.Capability.AllowPlaintextKeys = true
	cfg.Capability.ProtectedKeys = map[string]string{"key-1": "capability-key.dpapi"}
	cfg.Capability.PlaintextKeys = map[string]string{"key-1": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "both protected and plaintext") {
		t.Fatalf("expected duplicate key ID error, got %v", err)
	}
}

func TestValidateRequiresEmbeddedTURN(t *testing.T) {
	cfg := Default()
	makeCapabilityValid(&cfg)
	cfg.TURN.Enabled = false
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "embedded TURN") {
		t.Fatalf("expected embedded TURN error, got %v", err)
	}
}

func TestValidateRequiresBoundedTURNBandwidth(t *testing.T) {
	cfg := Default()
	makeCapabilityValid(&cfg)
	cfg.TURN.AdvertisedIP = "10.0.0.10"
	cfg.TURN.BytesPerSecond = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "bytesPerSecond") {
		t.Fatalf("expected TURN bandwidth error, got %v", err)
	}
}

func TestValidateRequiresPortCapacityForCallsAndTURNAllocations(t *testing.T) {
	cfg := Default()
	makeCapabilityValid(&cfg)
	cfg.TURN.AdvertisedIP = "10.0.0.10"
	cfg.SIP.RTPMinPort = 20000
	cfg.SIP.RTPMaxPort = 20001
	cfg.TURN.RelayMinPort = 30000
	cfg.TURN.RelayMaxPort = 30000
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "RTP/RTCP pair") ||
		!strings.Contains(err.Error(), "one port per maximum allocation") {
		t.Fatalf("expected RTP and TURN capacity errors, got %v", err)
	}
}

func TestValidateRejectsMoreCallsThanSessions(t *testing.T) {
	cfg := Default()
	makeCapabilityValid(&cfg)
	cfg.TURN.AdvertisedIP = "10.0.0.10"
	cfg.Limits.MaxCalls = cfg.Limits.MaxSessions + 1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "maximum calls") {
		t.Fatalf("expected call/session capacity error, got %v", err)
	}
}

func TestValidateRequiresAdvertisedICEIP(t *testing.T) {
	cfg := Default()
	makeCapabilityValid(&cfg)
	cfg.TURN.AdvertisedIP = "10.0.0.10"
	cfg.ICE.AdvertisedIP = "not-an-ip"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ice.advertisedIp") {
		t.Fatalf("expected ICE advertised IP error, got %v", err)
	}
}

func TestValidateRejectsListenerAndMediaPortConflicts(t *testing.T) {
	cfg := Default()
	makeCapabilityValid(&cfg)
	cfg.TURN.AdvertisedIP = "10.0.0.10"
	cfg.ICE.UDPMuxListen = "0.0.0.0:5060"
	cfg.SIP.RTPMinPort = 20000
	cfg.SIP.RTPMaxPort = 30000
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "share UDP port 5060") ||
		!strings.Contains(err.Error(), "port ranges must not overlap") {
		t.Fatalf("expected listener and media range conflicts, got %v", err)
	}
}

func TestValidateRejectsListenerInsideAllocationRange(t *testing.T) {
	cfg := Default()
	makeCapabilityValid(&cfg)
	cfg.TURN.AdvertisedIP = "10.0.0.10"
	cfg.ICE.UDPMuxListen = "0.0.0.0:30001"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "ice.udpMuxListen UDP port conflicts with turn.relay range") {
		t.Fatalf("expected ICE and TURN relay conflict, got %v", err)
	}
}

func TestValidateRejectsNonnumericOrZeroListenPorts(t *testing.T) {
	cfg := Default()
	makeCapabilityValid(&cfg)
	cfg.TURN.AdvertisedIP = "10.0.0.10"
	cfg.SIP.ListenUDP = "0.0.0.0:sip"
	cfg.SIP.ListenTCP = "0.0.0.0:0"
	err := cfg.Validate()
	if err == nil || strings.Count(err.Error(), "numeric port from 1 to 65535") != 2 {
		t.Fatalf("expected both invalid listen ports to be rejected, got %v", err)
	}
}

func makeCapabilityValid(cfg *Config) {
	cfg.NodeID = "gateway-1"
	cfg.ICE.AdvertisedIP = "10.0.0.10"
	cfg.Capability.ProtectedKeys = map[string]string{"key-1": "capability-key.dpapi"}
}
