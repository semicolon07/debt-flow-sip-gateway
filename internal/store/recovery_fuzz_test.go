package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/debt-flow/sip-gateway/internal/call"
)

func FuzzStoredCallRecovery(f *testing.F) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	machine, _ := call.NewOutbound("call-seed", now)
	seed, _ := json.Marshal(machine.Snapshot())
	f.Add(seed, "call-seed")
	f.Add([]byte(`{"callId":"call-seed"}`), "call-seed")
	f.Add([]byte("not-json"), "call-seed")

	f.Fuzz(func(t *testing.T, raw []byte, expectedCallID string) {
		if len(raw) > 64*1024 || len(expectedCallID) > 256 {
			t.Skip()
		}
		_, _ = restoreStoredCall(raw, expectedCallID)
	})
}
