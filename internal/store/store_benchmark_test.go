package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkOutboxAppendAndAcknowledge(b *testing.B) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(b.TempDir(), "benchmark.db"), Limits{
		PerSession: max(1000, b.N+1), Global: max(1000, b.N+1), MaxBytes: 1 << 30,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer database.Close()
	payload := []byte(`{"callId":"benchmark","state":"ringing"}`)
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		event, err := database.AppendEvent(ctx, "session-1", "agent-1", "", "call.state_changed", payload, now)
		if err != nil {
			b.Fatal(err)
		}
		if err := database.Acknowledge(ctx, "session-1", event.Sequence, now); err != nil {
			b.Fatal(err)
		}
	}
}
