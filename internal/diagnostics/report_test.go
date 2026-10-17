package diagnostics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/debt-flow/sip-gateway/internal/config"
)

func TestReportIsAllowlistedAndExcludesSecretMaterial(t *testing.T) {
	cfg := config.Default()
	cfg.NodeID = "gateway-01"
	cfg.Capability.ProtectedKeys = map[string]string{"key-b": `C:\secret\key.dpapi`, "key-a": "/secret/key"}
	cfg.Capability.PlaintextKeys = map[string]string{"dev": "SUPER-SECRET-PLAINTEXT-KEY"}
	cfg.Capability.AllowPlaintextKeys = true
	cfg.Storage.Path = `C:\sensitive\gateway.db`

	encoded, err := json.Marshal(Build(cfg, "1.2.3", time.Unix(1, 0)))
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{
		"SUPER-SECRET-PLAINTEXT-KEY", `C:\\secret\\key.dpapi`, "/secret/key",
		`C:\\sensitive\\gateway.db`, "plaintextKeys", "protectedKeys",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("diagnostics report leaked prohibited value %q", forbidden)
		}
	}
	if !strings.Contains(text, `"protectedKeyIds":["key-a","key-b"]`) {
		t.Fatalf("expected stable protected key metadata: %s", text)
	}
}
