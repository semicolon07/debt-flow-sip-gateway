package gateway

import (
	"fmt"
	"testing"
	"time"

	"github.com/debt-flow/sip-gateway/internal/call"
)

func BenchmarkRuntimeStats250Sessions200Calls(b *testing.B) {
	coordinator := &Coordinator{sessions: make(map[string]*runtimeSession, 250)}
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	for index := range 250 {
		session := &runtimeSession{
			claims:    Claims{SessionID: fmt.Sprintf("session-%03d", index)},
			connected: true, registration: "registered",
		}
		if index < 200 {
			machine, _ := call.NewOutbound(fmt.Sprintf("call-%03d", index), now)
			_ = machine.BeginAuthorization()
			_, _ = machine.AuthorizationSucceeded()
			session.activeCall = machine
		}
		coordinator.sessions[session.claims.SessionID] = session
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		stats := coordinator.Stats()
		if stats.AuthenticatedSessions != 250 || stats.ConnectedSessions != 250 || len(stats.Calls) != 1 {
			b.Fatalf("unexpected runtime stats: %+v", stats)
		}
	}
}
