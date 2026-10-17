package call

import (
	"testing"
	"time"
)

func TestOutboundHappyPath(t *testing.T) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	machine, err := NewOutbound("call-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.BeginAuthorization(); err != nil {
		t.Fatal(err)
	}
	action, err := machine.AuthorizationSucceeded()
	if err != nil || action != ActionInvite {
		t.Fatalf("expected invite, got %q %v", action, err)
	}
	if err := machine.MarkRinging(); err != nil {
		t.Fatal(err)
	}
	if err := machine.Connected(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	action, emitted := machine.LocalHangup(now.Add(2 * time.Second))
	if !emitted || action != ActionBYE {
		t.Fatalf("expected one BYE, got %q emitted=%v", action, emitted)
	}
	if _, emitted := machine.LocalHangup(now.Add(3 * time.Second)); emitted {
		t.Fatal("terminal event must be emitted once")
	}
	if snapshot := machine.Snapshot(); snapshot.State != StateEnded || snapshot.EndReason != EndLocalHangup {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestOutboundTryingAndExplicitEndingLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)
	machine, _ := NewOutbound("call-1", now)
	_ = machine.BeginAuthorization()
	_, _ = machine.AuthorizationSucceeded()
	if err := machine.MarkTrying(); err != nil {
		t.Fatal(err)
	}
	if snapshot := machine.Snapshot(); snapshot.State != StateTrying {
		t.Fatalf("expected trying, got %+v", snapshot)
	}
	if err := machine.MarkRinging(); err != nil {
		t.Fatal(err)
	}
	if _, emitted := machine.BeginEnding(); !emitted {
		t.Fatal("expected ending transition")
	}
	if snapshot := machine.Snapshot(); snapshot.State != StateEnding {
		t.Fatalf("expected ending, got %+v", snapshot)
	}
	if !machine.CompleteEnding(EndLocalHangup, now.Add(time.Second)) {
		t.Fatal("expected terminal transition")
	}
	if snapshot := machine.Snapshot(); snapshot.State != StateEnded || snapshot.EndReason != EndLocalHangup {
		t.Fatalf("unexpected terminal snapshot: %+v", snapshot)
	}
}

func TestLateAnswerAfterCancelRequiresACKThenBYE(t *testing.T) {
	now := time.Now().UTC()
	machine, _ := NewOutbound("call-1", now)
	_ = machine.BeginAuthorization()
	_, _ = machine.AuthorizationSucceeded()
	action, emitted := machine.LocalHangup(now.Add(time.Second))
	if action != ActionCancel || !emitted {
		t.Fatalf("expected CANCEL, got %q", action)
	}
	if action := machine.LateAnswerAfterEnd(); action != ActionACKBYE {
		t.Fatalf("expected ACK+BYE, got %q", action)
	}
	if machine.Snapshot().State != StateEnded {
		t.Fatal("late answer must not revive the call")
	}
}

func TestConcurrentTerminalizationEmitsOnce(t *testing.T) {
	now := time.Now().UTC()
	machine, _ := NewInbound("call-1", now)
	if err := machine.MarkIncoming(); err != nil {
		t.Fatal(err)
	}
	results := make(chan bool, 2)
	go func() { results <- machine.RemoteHangup(EndRemoteHangup, now) }()
	go func() {
		_, emitted := machine.LocalHangup(now)
		results <- emitted
	}()
	count := 0
	for range 2 {
		if <-results {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one terminalization, got %d", count)
	}
}

func TestInboundRejectUsesRejectedTerminalReason(t *testing.T) {
	now := time.Now().UTC()
	machine, _ := NewInbound("call-1", now)
	if err := machine.MarkIncoming(); err != nil {
		t.Fatal(err)
	}
	action, emitted := machine.RejectInbound(now.Add(time.Second))
	if !emitted || action != ActionReject {
		t.Fatalf("expected reject action, got action=%q emitted=%v", action, emitted)
	}
	if snapshot := machine.Snapshot(); snapshot.State != StateEnded || snapshot.EndReason != EndRejected {
		t.Fatalf("unexpected rejected snapshot: %+v", snapshot)
	}
	if _, emitted := machine.RejectInbound(now.Add(2 * time.Second)); emitted {
		t.Fatal("reject terminal event must be emitted once")
	}
}

func TestRestoreRejectsSemanticallyInvalidSnapshot(t *testing.T) {
	now := time.Now().UTC()
	tests := []Snapshot{
		{CallID: "call-1", Direction: "invalid", State: StateCreated, CreatedAt: now, Revision: 1},
		{CallID: "call-1", Direction: Outbound, State: "invalid", CreatedAt: now, Revision: 1},
		{CallID: "call-1", Direction: Outbound, State: StateConnected, CreatedAt: now, Revision: 1},
		{CallID: "call-1", Direction: Outbound, State: StateEnded, CreatedAt: now, Revision: 1},
		{CallID: "call-1", Direction: Outbound, State: StateCreated, EndReason: EndBusy,
			CreatedAt: now, Revision: 1},
	}
	for index, snapshot := range tests {
		if _, err := Restore(snapshot); err == nil {
			t.Fatalf("case %d: expected invalid snapshot to be rejected", index)
		}
	}
}
