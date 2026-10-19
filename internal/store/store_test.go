package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/debt-flow/sip-gateway/internal/call"
)

func openTestStore(t *testing.T, perSession, global int) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "gateway.db"), Limits{
		PerSession: perSession, Global: global, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestCommandDeduplicationAndConflict(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 10, 100)
	now := time.Now().UTC()
	first, err := store.BeginCommand(ctx, "command-1", []byte(`{"value":1}`), now)
	if err != nil || first.Duplicate || first.Conflict {
		t.Fatalf("unexpected first result: %+v %v", first, err)
	}
	duplicate, err := store.BeginCommand(ctx, "command-1", []byte(`{"value":1}`), now)
	if err != nil || !duplicate.Duplicate || duplicate.Conflict {
		t.Fatalf("unexpected duplicate: %+v %v", duplicate, err)
	}
	conflict, err := store.BeginCommand(ctx, "command-1", []byte(`{"value":2}`), now)
	if err != nil || !conflict.Duplicate || !conflict.Conflict {
		t.Fatalf("unexpected conflict: %+v %v", conflict, err)
	}
	if err := store.FinishCommand(ctx, "command-1", CommandCompleted, []byte(`{"ok":true}`), "", now); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedCommandRecoveryIsTerminalAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 10, 100)
	now := time.Now().UTC()
	if _, err := store.BeginCommand(ctx, "command-1", []byte(`{"value":1}`), now); err != nil {
		t.Fatal(err)
	}
	count, err := store.RecoverInterruptedCommands(ctx, now.Add(time.Second))
	if err != nil || count != 1 {
		t.Fatalf("first command recovery: count=%d err=%v", count, err)
	}
	record, err := store.Command(ctx, "command-1")
	if err != nil || record.Status != CommandFailed || record.ErrorCode != "gateway_restarted" {
		t.Fatalf("unexpected recovered command: %+v err=%v", record, err)
	}
	count, err = store.RecoverInterruptedCommands(ctx, now.Add(2*time.Second))
	if err != nil || count != 0 {
		t.Fatalf("second command recovery: count=%d err=%v", count, err)
	}
}

func TestOutboxSequenceAckAndLimit(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 2, 3)
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		event, err := store.AppendEvent(ctx, "session-1", "agent-1", "", "call.state_changed", []byte(`{}`), now)
		if err != nil || event.Sequence != uint64(i+1) {
			t.Fatalf("append %d: %+v %v", i, event, err)
		}
	}
	if _, err := store.AppendEvent(ctx, "session-1", "agent-1", "", "call.ended", []byte(`{}`), now); !errors.Is(err, ErrCapacity) {
		t.Fatalf("expected capacity error, got %v", err)
	}
	if err := store.Acknowledge(ctx, "session-1", 1, now); err != nil {
		t.Fatal(err)
	}
	events, err := store.PendingEvents(ctx, "session-1", 0, 10)
	if err != nil || len(events) != 1 || events[0].Sequence != 2 {
		t.Fatalf("unexpected pending events: %+v %v", events, err)
	}
}

func TestOutboxSequenceDoesNotResetAfterAcknowledgedRowsArePurged(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 10, 100)
	now := time.Now().UTC()
	first, err := store.AppendEvent(
		ctx, "session-1", "agent-1", "first", "call.state_changed", []byte(`{}`), now.Add(-2*time.Hour),
	)
	if err != nil || first.Sequence != 1 {
		t.Fatalf("first append: event=%+v err=%v", first, err)
	}
	if err := store.Acknowledge(ctx, "session-1", first.Sequence, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if result, err := store.Maintain(ctx, now.Add(-time.Hour), now); err != nil || result.PurgedRows == 0 {
		t.Fatalf("purge acknowledged event: result=%+v err=%v", result, err)
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE session_id=?`, "session-1").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("purged outbox still has %d rows", rows)
	}
	second, err := store.AppendEvent(ctx, "session-1", "agent-1", "second", "call.state_changed", []byte(`{}`), now)
	if err != nil || second.Sequence != 2 {
		t.Fatalf("sequence reset after purge: event=%+v err=%v", second, err)
	}
}

func TestOutboxSequenceIsMonotonicAcrossSessionsForTheSameAgent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 10, 100)
	now := time.Now().UTC()
	first, err := store.AppendEvent(ctx, "session-1", "agent-1", "first", "call.state_changed", []byte(`{}`), now)
	if err != nil || first.Sequence != 1 {
		t.Fatalf("first append: event=%+v err=%v", first, err)
	}
	second, err := store.AppendEvent(ctx, "session-2", "agent-1", "second", "call.state_changed", []byte(`{}`), now)
	if err != nil || second.Sequence != 2 {
		t.Fatalf("second session reused agent sequence: event=%+v err=%v", second, err)
	}
	otherAgent, err := store.AppendEvent(ctx, "session-3", "agent-2", "third", "call.state_changed", []byte(`{}`), now)
	if err != nil || otherAgent.Sequence != 1 {
		t.Fatalf("independent agent sequence: event=%+v err=%v", otherAgent, err)
	}
}

func TestAgentACKClearsCommittedRowsAcrossGatewaySessions(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 10, 100)
	now := time.Now().UTC()
	for _, binding := range []struct{ session, agent, user, collection string }{
		{"session-1", "agent-1", "user-session-1", "col-1"},
		{"session-2", "agent-1", "user-session-1", "col-2"},
		{"session-3", "agent-1", "user-session-2", "col-3"},
		{"session-4", "agent-1", "user-session-1", "col-4"},
		{"session-5", "agent-2", "user-session-2", "col-5"},
	} {
		if err := store.BindSession(ctx, binding.session, binding.agent, binding.user,
			binding.collection, "binding-"+binding.collection, now); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.AppendEvent(ctx, "session-1", "agent-1", "first", "call.state_changed", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendEvent(ctx, "session-2", "agent-1", "second", "call.state_changed", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	otherUser, err := store.AppendEvent(ctx, "session-3", "agent-1", "other-user", "call.state_changed", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := store.AppendEvent(ctx, "session-4", "agent-1", "fourth", "call.state_changed", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	otherAgent, err := store.AppendEvent(ctx, "session-5", "agent-2", "other-agent", "call.state_changed", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.PendingAgentEvents(ctx, "agent-1", "user-session-1", 0, 10)
	if err != nil || len(replay) != 3 || replay[0].CollectionID != "col-1" ||
		replay[0].CollectionBindingID != "binding-col-1" || replay[1].CollectionID != "col-2" ||
		replay[2].CollectionID != "col-4" {
		t.Fatalf("unexpected cross-session replay: events=%+v err=%v", replay, err)
	}
	isolated, err := store.PendingAgentEvents(ctx, "agent-1", "user-session-2", 0, 10)
	if err != nil || len(isolated) != 1 || isolated[0].Sequence != otherUser.Sequence {
		t.Fatalf("event replay crossed authenticated user session: events=%+v err=%v", isolated, err)
	}
	confirmed, err := store.AcknowledgeAgent(ctx, "agent-1", "user-session-1", second.Sequence, now)
	if err != nil || confirmed != otherUser.Sequence {
		t.Fatalf("ACK did not skip the other user's sequence: confirmed=%d err=%v", confirmed, err)
	}
	confirmed, err = store.AcknowledgeAgent(ctx, "agent-1", "user-session-1", fourth.Sequence, now)
	if err != nil || confirmed != fourth.Sequence {
		t.Fatalf("final ACK: confirmed=%d err=%v", confirmed, err)
	}
	if _, err := store.AcknowledgeAgent(ctx, "agent-1", "user-session-1", 999, now); err == nil {
		t.Fatal("expected an ACK beyond the produced sequence to fail")
	}
	for _, sessionID := range []string{"session-1", "session-2", "session-4"} {
		pending, err := store.PendingEvents(ctx, sessionID, 0, 10)
		if err != nil || len(pending) != 0 {
			t.Fatalf("agent ACK left session %s pending: events=%+v err=%v", sessionID, pending, err)
		}
	}
	pending, err := store.PendingEvents(ctx, "session-3", 0, 10)
	if err != nil || len(pending) != 1 || pending[0].Sequence != otherUser.Sequence {
		t.Fatalf("agent ACK crossed identity boundary: events=%+v err=%v", pending, err)
	}
	pending, err = store.PendingEvents(ctx, "session-5", 0, 10)
	if err != nil || len(pending) != 1 || pending[0].Sequence != otherAgent.Sequence {
		t.Fatalf("agent ACK crossed agent boundary: events=%+v err=%v", pending, err)
	}
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("unexpected agent sequences: first=%d second=%d", first.Sequence, second.Sequence)
	}
}

func TestSessionBindingIsImmutableAndMaintenanceRemovesOnlyUnreferencedBindings(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 10, 100)
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	if err := store.BindSession(ctx, "session-empty", "agent-1", "user-session-1",
		"col-1", "binding-col-1", old); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(ctx, "session-referenced", "agent-1", "user-session-1",
		"col-1", "binding-col-1", old); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(ctx, "session-touched", "agent-1", "user-session-1",
		"col-1", "binding-col-1", old); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(ctx, "session-touched", "agent-1", "user-session-1",
		"col-1", "binding-col-1", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, "session-referenced", "agent-1", "event-1",
		"call.state_changed", []byte(`{}`), old); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(ctx, "session-referenced", "agent-1", "user-session-1",
		"col-2", "binding-col-2", now); err == nil {
		t.Fatal("expected immutable session binding conflict")
	}
	if _, err := store.Maintain(ctx, now.Add(-time.Hour), now); err != nil {
		t.Fatal(err)
	}
	var empty, referenced, touched int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_bindings WHERE session_id='session-empty'`).Scan(&empty); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_bindings WHERE session_id='session-referenced'`).Scan(&referenced); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_bindings WHERE session_id='session-touched'`).Scan(&touched); err != nil {
		t.Fatal(err)
	}
	if empty != 0 || referenced != 1 || touched != 1 {
		t.Fatalf("unexpected binding maintenance: empty=%d referenced=%d touched=%d",
			empty, referenced, touched)
	}
}

func TestConcurrentWritersAreSerializedWithoutSQLiteBusy(t *testing.T) {
	const workers = 200
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "concurrent.db"), Limits{
		PerSession: 10, Global: workers * 10, MaxBytes: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	now := time.Now().UTC()
	start := make(chan struct{})
	errorsFound := make(chan error, workers+1)
	sequences := make(chan uint64, workers)
	var wait sync.WaitGroup

	for index := range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			commandID := fmt.Sprintf("command-%d", index)
			sessionID := fmt.Sprintf("session-%d", index)
			if _, err := database.BeginCommand(ctx, commandID, []byte(`{"type":"call.start"}`), now); err != nil {
				errorsFound <- fmt.Errorf("begin %s: %w", commandID, err)
				return
			}
			event, err := database.AppendEvent(ctx, sessionID, "agent-1", commandID,
				"registration.state_changed", []byte(`{"state":"registered"}`), now)
			if err != nil {
				errorsFound <- fmt.Errorf("append %s: %w", commandID, err)
				return
			}
			sequences <- event.Sequence
			if err := database.FinishCommand(ctx, commandID, CommandCompleted, nil, "", now); err != nil {
				errorsFound <- fmt.Errorf("finish %s: %w", commandID, err)
				return
			}
			if err := database.Acknowledge(ctx, sessionID, event.Sequence, now); err != nil {
				errorsFound <- fmt.Errorf("acknowledge %s: %w", commandID, err)
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		for range 10 {
			if _, err := database.Maintain(ctx, now.Add(-time.Hour), now); err != nil {
				errorsFound <- fmt.Errorf("maintain: %w", err)
				return
			}
		}
	}()

	close(start)
	wait.Wait()
	close(errorsFound)
	close(sequences)
	for err := range errorsFound {
		t.Error(err)
	}
	seen := make(map[uint64]struct{}, workers)
	for sequence := range sequences {
		seen[sequence] = struct{}{}
	}
	if len(seen) != workers {
		t.Fatalf("unique sequences=%d, want %d", len(seen), workers)
	}
}

func TestAgentSequenceCursorCanBeRebuiltFromTheOutbox(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := Open(ctx, path, Limits{PerSession: 10, Global: 100, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.AppendEvent(ctx, "session-1", "agent-1", "first", "call.state_changed", []byte(`{}`), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM agent_sequences WHERE agent_instance_id='agent-1'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path, Limits{PerSession: 10, Global: 100, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	second, err := store.AppendEvent(ctx, "session-2", "agent-1", "second", "call.state_changed", []byte(`{}`), now)
	if err != nil || second.Sequence != 2 {
		t.Fatalf("rebuilt cursor: event=%+v err=%v", second, err)
	}
}

func TestMigrationBackfillsSequenceCursorFromAVersionOneOutbox(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.ExecContext(ctx, `
CREATE TABLE schema_info (version INTEGER NOT NULL);
INSERT INTO schema_info(version) VALUES(1);
CREATE TABLE outbox (
    session_id TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    event_key TEXT,
    event_type TEXT NOT NULL,
    payload BLOB NOT NULL,
    created_at_utc TEXT NOT NULL,
    acked_at_utc TEXT,
    PRIMARY KEY(session_id, sequence),
    UNIQUE(event_key)
);
INSERT INTO outbox(session_id, sequence, event_key, event_type, payload, created_at_utc)
VALUES('session-1', 7, 'legacy-event', 'call.state_changed', X'7B7D', '2026-10-17T00:00:00Z');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, path, Limits{PerSession: 10, Global: 100, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	event, err := store.AppendEvent(ctx, "session-1", "session-1", "new-event", "call.state_changed", []byte(`{}`), time.Now())
	if err != nil || event.Sequence != 8 {
		t.Fatalf("migration did not preserve sequence: event=%+v err=%v", event, err)
	}
	var version int
	if err := store.db.QueryRowContext(ctx, `SELECT version FROM schema_info LIMIT 1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 6 {
		t.Fatalf("schema version=%d, want 6", version)
	}
	var optimizedIndexes int
	if err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master
WHERE type='index' AND name IN ('ix_outbox_unacked_session', 'ix_outbox_unacked_global')`).Scan(&optimizedIndexes); err != nil {
		t.Fatal(err)
	}
	if optimizedIndexes != 2 {
		t.Fatalf("optimized outbox indexes=%d, want 2", optimizedIndexes)
	}
}

func TestMigrationAddsAgentScopeToVersionTwoCalls(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.ExecContext(ctx, `
CREATE TABLE schema_info (version INTEGER NOT NULL);
INSERT INTO schema_info(version) VALUES(2);
CREATE TABLE calls (
    call_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    snapshot_json BLOB NOT NULL,
    active INTEGER NOT NULL,
    restart_terminalized INTEGER NOT NULL DEFAULT 0,
    updated_at_utc TEXT NOT NULL
);
INSERT INTO calls(call_id, session_id, snapshot_json, active, updated_at_utc)
VALUES('call-legacy', 'session-legacy', X'7B7D', 0, '2026-10-17T00:00:00Z');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, path, Limits{PerSession: 10, Global: 100, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var agentInstanceID string
	if err := store.db.QueryRowContext(ctx,
		`SELECT agent_instance_id FROM calls WHERE call_id='call-legacy'`).Scan(&agentInstanceID); err != nil {
		t.Fatal(err)
	}
	if agentInstanceID != "session-legacy" {
		t.Fatalf("legacy agent scope=%q, want session fallback", agentInstanceID)
	}
	var version int
	if err := store.db.QueryRowContext(ctx, `SELECT version FROM schema_info LIMIT 1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 6 {
		t.Fatalf("schema version=%d, want 6", version)
	}
}

func TestRestartTerminalLifecycleIsCreatedOnce(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 10, 100)
	now := time.Now().UTC()
	machine, _ := call.NewInbound("call-1", now)
	if err := store.SaveCall(ctx, "session-1", "agent-1", machine.Snapshot(), now); err != nil {
		t.Fatal(err)
	}
	count, err := store.RecoverRestartedCalls(ctx, now.Add(time.Second))
	if err != nil || count != 1 {
		t.Fatalf("first recovery: count=%d err=%v", count, err)
	}
	count, err = store.RecoverRestartedCalls(ctx, now.Add(2*time.Second))
	if err != nil || count != 0 {
		t.Fatalf("second recovery: count=%d err=%v", count, err)
	}
	events, err := store.PendingEvents(ctx, "session-1", 0, 10)
	if err != nil || len(events) != 2 || events[0].Type != "call.state_changed" ||
		events[1].Type != "call.ended" || events[1].Sequence != events[0].Sequence+1 {
		t.Fatalf("unexpected restart events: %+v %v", events, err)
	}
}

func TestRestartTerminalEventContinuesThePersistedAgentSequence(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 10, 100)
	now := time.Now().UTC()
	if _, err := store.AppendEvent(ctx, "session-before", "agent-1", "before", "call.state_changed", []byte(`{}`), now); err != nil {
		t.Fatal(err)
	}
	machine, _ := call.NewInbound("call-2", now)
	if err := store.SaveCall(ctx, "session-active", "agent-1", machine.Snapshot(), now); err != nil {
		t.Fatal(err)
	}
	if count, err := store.RecoverRestartedCalls(ctx, now.Add(time.Second)); err != nil || count != 1 {
		t.Fatalf("recovery: count=%d err=%v", count, err)
	}
	events, err := store.PendingEvents(ctx, "session-active", 0, 10)
	if err != nil || len(events) != 2 || events[0].Sequence != 2 || events[1].Sequence != 3 ||
		events[0].Type != "call.state_changed" || events[1].Type != "call.ended" {
		t.Fatalf("restart event did not continue agent sequence: events=%+v err=%v", events, err)
	}
}

func TestRestartTerminalLifecycleRollsBackWhenPairCannotFit(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, 2, 10)
	now := time.Now().UTC()
	if _, err := database.AppendEvent(ctx, "session-1", "agent-1", "existing",
		"registration.state_changed", []byte(`{"state":"registered"}`), now); err != nil {
		t.Fatal(err)
	}
	machine, _ := call.NewInbound("call-restart-capacity", now)
	if err := database.SaveCall(ctx, "session-1", "agent-1", machine.Snapshot(), now); err != nil {
		t.Fatal(err)
	}
	if count, err := database.RecoverRestartedCalls(ctx, now.Add(time.Second)); !errors.Is(err, ErrCapacity) || count != 0 {
		t.Fatalf("recovery count=%d err=%v, want atomic capacity failure", count, err)
	}
	events, err := database.PendingEvents(ctx, "session-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "registration.state_changed" {
		t.Fatalf("partial restart lifecycle escaped rolled-back transaction: %+v", events)
	}
}

func TestSaveCallWithEventsCommitsSnapshotAndOrderedOutboxAtomically(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, 10, 100)
	now := time.Now().UTC()
	if err := database.BindSession(ctx, "session-1", "agent-1", "user-session-1",
		"collection-1", "binding-1", now); err != nil {
		t.Fatal(err)
	}
	machine, _ := call.NewOutbound("call-atomic", now)
	if err := machine.BeginAuthorization(); err != nil {
		t.Fatal(err)
	}
	stored, err := database.SaveCallWithEvents(ctx, "session-1", "agent-1", machine.Snapshot(), []EventWrite{
		{Key: "created-atomic", Type: "call.created", Payload: []byte(`{"state":"created"}`)},
		{Key: "authorizing-atomic", Type: "call.state_changed", Payload: []byte(`{"state":"authorizing"}`)},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].Sequence != 1 || stored[1].Sequence != 2 ||
		stored[0].CollectionID != "collection-1" || stored[1].CollectionBindingID != "binding-1" {
		t.Fatalf("unexpected atomic events: %+v", stored)
	}
	var raw []byte
	if err := database.db.QueryRowContext(ctx,
		`SELECT snapshot_json FROM calls WHERE call_id='call-atomic'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var snapshot call.Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.State != call.StateAuthorizing || snapshot.Revision != 2 {
		t.Fatalf("unexpected stored snapshot: %+v", snapshot)
	}
}

func TestSaveCallWithEventsRollsBackSnapshotAndAllEventsOnFailure(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, 10, 100)
	now := time.Now().UTC()
	machine, _ := call.NewOutbound("call-rollback", now)
	if err := database.SaveCall(ctx, "session-1", "agent-1", machine.Snapshot(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AppendEvent(ctx, "session-1", "agent-1", "duplicate-key",
		"call.created", []byte(`{"state":"created"}`), now); err != nil {
		t.Fatal(err)
	}
	if err := machine.BeginAuthorization(); err != nil {
		t.Fatal(err)
	}
	_, err := database.SaveCallWithEvents(ctx, "session-1", "agent-1", machine.Snapshot(), []EventWrite{
		{Key: "would-be-inserted", Type: "call.state_changed", Payload: []byte(`{"state":"authorizing"}`)},
		{Key: "duplicate-key", Type: "call.state_changed", Payload: []byte(`{"state":"authorizing"}`)},
	}, now.Add(time.Second))
	if err == nil {
		t.Fatal("expected duplicate event key to roll back the transaction")
	}
	var raw []byte
	if err := database.db.QueryRowContext(ctx,
		`SELECT snapshot_json FROM calls WHERE call_id='call-rollback'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var snapshot call.Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.State != call.StateCreated || snapshot.Revision != 1 {
		t.Fatalf("call snapshot escaped rolled-back transaction: %+v", snapshot)
	}
	var count int
	if err := database.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outbox WHERE event_key='would-be-inserted'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("partial event escaped rolled-back transaction: count=%d", count)
	}
}

func TestStoredCallRecoveryRejectsIdentityMismatch(t *testing.T) {
	now := time.Now().UTC()
	machine, _ := call.NewInbound("call-other", now)
	raw, err := json.Marshal(machine.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restoreStoredCall(raw, "call-expected"); err == nil {
		t.Fatal("expected stored call identity mismatch")
	}
}

func TestCapabilityCanOnlyBeConsumedOnce(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 10, 100)
	now := time.Now().UTC()
	consumed, err := store.ConsumeCapability(ctx, "capability-1", now.Add(time.Minute), now)
	if err != nil || !consumed {
		t.Fatalf("first consume: consumed=%v err=%v", consumed, err)
	}
	consumed, err = store.ConsumeCapability(ctx, "capability-1", now.Add(time.Minute), now)
	if err != nil || consumed {
		t.Fatalf("replay consume: consumed=%v err=%v", consumed, err)
	}
}

func TestTerminalEventUsesReservedOutboxCapacity(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 10, 10)
	now := time.Now().UTC()
	for index := 0; index < 9; index++ {
		if _, err := store.AppendEvent(ctx, "session-1", "agent-1", "", "call.state_changed", []byte(`{}`), now); err != nil {
			t.Fatalf("append event %d: %v", index, err)
		}
	}
	if allowed, err := store.CanStartCall(ctx, "session-1"); err != nil || allowed {
		t.Fatalf("expected new calls to be blocked at reserve threshold: allowed=%v err=%v", allowed, err)
	}
	if _, err := store.AppendEvent(ctx, "session-1", "agent-1", "", "call.state_changed", []byte(`{}`), now); !errors.Is(err, ErrCapacity) {
		t.Fatalf("expected non-terminal event to preserve reserve, got %v", err)
	}
	if _, err := store.AppendEvent(ctx, "session-1", "agent-1", "terminal", "call.ended", []byte(`{}`), now); err != nil {
		t.Fatalf("terminal event did not use reserve: %v", err)
	}
}

func TestByteReserveThresholdRoundsUpToNinetyPercent(t *testing.T) {
	for _, test := range []struct {
		value int64
		limit int64
		want  bool
	}{
		{value: 17, limit: 19, want: false},
		{value: 18, limit: 19, want: true},
		{value: 8, limit: 9, want: false},
		{value: 9, limit: 9, want: true},
	} {
		if got := atOrAboveReserve64(test.value, test.limit); got != test.want {
			t.Errorf("atOrAboveReserve64(%d, %d)=%v, want %v", test.value, test.limit, got, test.want)
		}
	}
}

func TestEventReserveThresholdAvoidsIntegerOverflow(t *testing.T) {
	limit := int(^uint(0) >> 1)
	cutoff := limit - limit/10
	if atOrAboveReserve(cutoff-1, limit) {
		t.Fatal("event reserve started below the ninety-percent cutoff")
	}
	if !atOrAboveReserve(cutoff, limit) {
		t.Fatal("event reserve did not start at the ninety-percent cutoff")
	}
}

func TestStatsTrackHighestPerSessionOutboxPressure(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 10, 100)
	now := time.Now().UTC()
	for _, sessionID := range []string{"session-1", "session-1", "session-1", "session-2", "session-2"} {
		if _, err := store.AppendEvent(ctx, sessionID, "agent-1", "", "call.state_changed", []byte(`{}`), now); err != nil {
			t.Fatalf("append event for %s: %v", sessionID, err)
		}
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PendingEvents != 5 || stats.MaxSessionPendingEvents != 3 {
		t.Fatalf("unexpected outbox stats: %+v", stats)
	}
}

func TestIntegrityStatsAndMaintenance(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 10, 100)
	now := time.Now().UTC()
	if err := store.CheckIntegrity(ctx); err != nil {
		t.Fatal(err)
	}
	event, err := store.AppendEvent(ctx, "session-1", "agent-1", "", "call.state_changed", []byte(`{}`), now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Acknowledge(ctx, "session-1", event.Sequence, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if consumed, err := store.ConsumeCapability(ctx, "expired-capability", now.Add(time.Minute), now); err != nil || !consumed {
		t.Fatalf("consume capability: consumed=%v err=%v", consumed, err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.PendingEvents != 0 || stats.DatabaseBytes <= 0 {
		t.Fatalf("unexpected store stats: %+v err=%v", stats, err)
	}
	result, err := store.Maintain(ctx, now, now.Add(2*time.Minute))
	if err != nil || result.PurgedRows < 2 {
		t.Fatalf("unexpected maintenance result: %+v err=%v", result, err)
	}
}
