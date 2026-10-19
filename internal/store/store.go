package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/debt-flow/sip-gateway/internal/call"
	_ "modernc.org/sqlite"
)

type Limits struct {
	PerSession int
	Global     int
	MaxBytes   int64
}

type Store struct {
	db      *sql.DB
	path    string
	limits  Limits
	writeMu sync.Mutex
}

type CommandStatus string

const (
	CommandReceived  CommandStatus = "received"
	CommandExecuting CommandStatus = "executing"
	CommandCompleted CommandStatus = "completed"
	CommandFailed    CommandStatus = "failed"
)

type CommandRecord struct {
	CommandID   string
	PayloadHash string
	Status      CommandStatus
	Result      []byte
	ErrorCode   string
}

type BeginCommandResult struct {
	Record    CommandRecord
	Duplicate bool
	Conflict  bool
}

type Event struct {
	SessionID           string
	AgentInstanceID     string
	UserSessionID       string
	CollectionID        string
	CollectionBindingID string
	Sequence            uint64
	Type                string
	Payload             []byte
	CreatedAt           time.Time
}

// EventWrite describes one durable outbox event that must be committed with a
// call snapshot. Payload ownership remains with the caller; the store copies it
// into SQLite before this method returns.
type EventWrite struct {
	Key     string
	Type    string
	Payload []byte
}

type Stats struct {
	PendingEvents           int64
	MaxSessionPendingEvents int64
	DatabaseBytes           int64
	WALBytes                int64
}

type MaintenanceResult struct {
	PurgedRows         int64
	CheckpointBusy     int
	WALFrames          int
	CheckpointedFrames int
}

var ErrCapacity = errors.New("persistent outbox capacity reached")

func Open(ctx context.Context, path string, limits Limits) (*Store, error) {
	if limits.PerSession <= 0 || limits.Global < limits.PerSession || limits.MaxBytes <= 0 {
		return nil, errors.New("invalid store limits")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	store := &Store{db: db, path: path, limits: limits}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) BindSession(ctx context.Context, sessionID, agentInstanceID, userSessionID,
	collectionID, collectionBindingID string, now time.Time) error {
	if sessionID == "" || agentInstanceID == "" || userSessionID == "" ||
		collectionID == "" || collectionBindingID == "" {
		return errors.New("complete session binding is required")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO session_bindings(
  session_id, agent_instance_id, user_session_id, collection_id, collection_binding_id, updated_at_utc)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET updated_at_utc=excluded.updated_at_utc
WHERE session_bindings.agent_instance_id=excluded.agent_instance_id
  AND session_bindings.user_session_id=excluded.user_session_id
  AND session_bindings.collection_id=excluded.collection_id
  AND session_bindings.collection_binding_id=excluded.collection_binding_id`,
		sessionID, agentInstanceID, userSessionID, collectionID, collectionBindingID, utc(now))
	if err != nil {
		return fmt.Errorf("bind gateway session: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect gateway session binding: %w", err)
	}
	if rows != 1 {
		return errors.New("gateway session binding conflicts with persisted identity")
	}
	return nil
}

func (s *Store) CheckIntegrity(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return fmt.Errorf("sqlite quick_check: %w", err)
	}
	defer rows.Close()
	var failures []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("scan sqlite quick_check: %w", err)
		}
		if !strings.EqualFold(strings.TrimSpace(result), "ok") {
			failures = append(failures, result)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite quick_check: %w", err)
	}
	if len(failures) > 0 {
		return fmt.Errorf("sqlite integrity check failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS schema_info (
    version INTEGER NOT NULL
);
INSERT INTO schema_info(version)
SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM schema_info);

CREATE TABLE IF NOT EXISTS commands (
    command_id TEXT PRIMARY KEY,
    payload_hash TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('received','executing','completed','failed')),
    result BLOB,
    error_code TEXT NOT NULL DEFAULT '',
    created_at_utc TEXT NOT NULL,
    updated_at_utc TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS outbox (
    session_id TEXT NOT NULL,
    agent_instance_id TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    event_key TEXT,
    event_type TEXT NOT NULL,
    payload BLOB NOT NULL,
    created_at_utc TEXT NOT NULL,
    acked_at_utc TEXT,
    PRIMARY KEY(session_id, sequence),
    UNIQUE(event_key)
);
CREATE INDEX IF NOT EXISTS ix_outbox_pending ON outbox(session_id, acked_at_utc, sequence);
CREATE INDEX IF NOT EXISTS ix_outbox_unacked_session ON outbox(session_id, sequence) WHERE acked_at_utc IS NULL;
CREATE INDEX IF NOT EXISTS ix_outbox_unacked_global ON outbox(sequence) WHERE acked_at_utc IS NULL;

CREATE TABLE IF NOT EXISTS agent_sequences (
    agent_instance_id TEXT PRIMARY KEY,
    next_sequence INTEGER NOT NULL CHECK(next_sequence > 0)
);

CREATE TABLE IF NOT EXISTS session_bindings (
    session_id TEXT PRIMARY KEY,
    agent_instance_id TEXT NOT NULL,
    user_session_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    collection_binding_id TEXT NOT NULL,
    updated_at_utc TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_session_bindings_agent_user
ON session_bindings(agent_instance_id, user_session_id, session_id);

CREATE TABLE IF NOT EXISTS calls (
    call_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    agent_instance_id TEXT NOT NULL,
    snapshot_json BLOB NOT NULL,
    active INTEGER NOT NULL,
    restart_terminalized INTEGER NOT NULL DEFAULT 0,
    updated_at_utc TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_calls_active ON calls(active, restart_terminalized);

CREATE TABLE IF NOT EXISTS consumed_capabilities (
    capability_id TEXT PRIMARY KEY,
    expires_at_utc TEXT NOT NULL,
    consumed_at_utc TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_consumed_capabilities_expiry ON consumed_capabilities(expires_at_utc);

`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	// Versions 1 and 2 did not persist the agent instance beside outbox rows or
	// active calls. The legacy fallback is the session identity because the old
	// schema has no data from which to reconstruct the installation identity.
	// New writes always persist the real agent identity, making the cursor fully
	// reconstructable after retention, restart, or a later migration.
	hasOutboxAgentInstance, err := s.tableHasColumn(ctx, "outbox", "agent_instance_id")
	if err != nil {
		return fmt.Errorf("inspect outbox schema: %w", err)
	}
	if !hasOutboxAgentInstance {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE outbox ADD COLUMN agent_instance_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add outbox agent instance: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE outbox SET agent_instance_id=session_id WHERE agent_instance_id=''`); err != nil {
		return fmt.Errorf("backfill outbox agent instance: %w", err)
	}
	hasAgentInstance, err := s.tableHasColumn(ctx, "calls", "agent_instance_id")
	if err != nil {
		return fmt.Errorf("inspect calls schema: %w", err)
	}
	if !hasAgentInstance {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE calls ADD COLUMN agent_instance_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add calls agent instance: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE calls SET agent_instance_id=session_id WHERE agent_instance_id=''`); err != nil {
		return fmt.Errorf("backfill calls agent instance: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO agent_sequences(agent_instance_id, next_sequence)
SELECT agent_instance_id, MAX(sequence) + 1 FROM outbox GROUP BY agent_instance_id
ON CONFLICT(agent_instance_id) DO UPDATE SET
    next_sequence = MAX(agent_sequences.next_sequence, excluded.next_sequence)`); err != nil {
		return fmt.Errorf("backfill agent sequence cursor: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE schema_info SET version = 6 WHERE version < 6`); err != nil {
		return fmt.Errorf("update sqlite schema version: %w", err)
	}
	return nil
}

func (s *Store) tableHasColumn(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) ConsumeCapability(ctx context.Context, capabilityID string, expiresAt, now time.Time) (bool, error) {
	if capabilityID == "" || !expiresAt.After(now) {
		return false, errors.New("invalid capability consumption")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM consumed_capabilities WHERE expires_at_utc < ?`, utc(now)); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO consumed_capabilities(capability_id, expires_at_utc, consumed_at_utc)
VALUES(?, ?, ?)`, capabilityID, utc(expiresAt), utc(now)); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "constraint") {
			return false, nil
		}
		return false, fmt.Errorf("consume capability: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func HashPayload(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (s *Store) BeginCommand(ctx context.Context, commandID string, payload []byte, now time.Time) (BeginCommandResult, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	hash := HashPayload(payload)
	record, err := s.Command(ctx, commandID)
	if err == nil {
		return BeginCommandResult{Record: record, Duplicate: true, Conflict: record.PayloadHash != hash}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return BeginCommandResult{}, err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO commands(command_id, payload_hash, status, created_at_utc, updated_at_utc)
VALUES(?, ?, ?, ?, ?)`, commandID, hash, CommandReceived, utc(now), utc(now))
	if err != nil {
		// A concurrent insert is resolved through the authoritative stored record.
		record, readErr := s.Command(ctx, commandID)
		if readErr != nil {
			return BeginCommandResult{}, fmt.Errorf("begin command: %w", err)
		}
		return BeginCommandResult{Record: record, Duplicate: true, Conflict: record.PayloadHash != hash}, nil
	}
	return BeginCommandResult{Record: CommandRecord{
		CommandID: commandID, PayloadHash: hash, Status: CommandReceived,
	}}, nil
}

func (s *Store) Command(ctx context.Context, commandID string) (CommandRecord, error) {
	var record CommandRecord
	err := s.db.QueryRowContext(ctx, `
SELECT command_id, payload_hash, status, COALESCE(result, X''), error_code
FROM commands WHERE command_id = ?`, commandID).Scan(
		&record.CommandID, &record.PayloadHash, &record.Status, &record.Result, &record.ErrorCode,
	)
	return record, err
}

func (s *Store) FinishCommand(ctx context.Context, commandID string, status CommandStatus, result []byte, errorCode string, now time.Time) error {
	if status != CommandCompleted && status != CommandFailed {
		return errors.New("command final status must be completed or failed")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx, `
UPDATE commands SET status = ?, result = ?, error_code = ?, updated_at_utc = ?
WHERE command_id = ? AND status IN ('received','executing')`, status, result, errorCode, utc(now), commandID)
	if err != nil {
		return fmt.Errorf("finish command: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows != 1 {
		return errors.New("command is missing or already terminal")
	}
	return nil
}

func (s *Store) RecoverInterruptedCommands(ctx context.Context, now time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `
UPDATE commands
SET status = 'failed', error_code = 'gateway_restarted', updated_at_utc = ?
WHERE status IN ('received','executing')`, utc(now))
	if err != nil {
		return 0, fmt.Errorf("recover interrupted commands: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count recovered interrupted commands: %w", err)
	}
	return count, nil
}

func (s *Store) AppendEvent(ctx context.Context, sessionID, agentInstanceID, eventKey, eventType string, payload []byte, now time.Time) (Event, error) {
	if sessionID == "" || agentInstanceID == "" {
		return Event{}, errors.New("event session and agent instance are required")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, err
	}
	defer tx.Rollback()
	if err := s.ensureCapacity(ctx, tx, sessionID, eventType == "call.ended"); err != nil {
		return Event{}, err
	}
	sequence, err := nextSequence(ctx, tx, agentInstanceID)
	if err != nil {
		return Event{}, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO outbox(session_id, agent_instance_id, sequence, event_key, event_type, payload, created_at_utc)
VALUES(?, ?, ?, NULLIF(?, ''), ?, ?, ?)`, sessionID, agentInstanceID, sequence, eventKey, eventType, payload, utc(now))
	if err != nil {
		return Event{}, fmt.Errorf("append outbox event: %w", err)
	}
	stored := Event{
		SessionID: sessionID, AgentInstanceID: agentInstanceID, Sequence: sequence,
		Type: eventType, Payload: payload, CreatedAt: now.UTC(),
	}
	bindingErr := tx.QueryRowContext(ctx, `
SELECT user_session_id, collection_id, collection_binding_id
FROM session_bindings WHERE session_id=? AND agent_instance_id=?`,
		sessionID, agentInstanceID).Scan(
		&stored.UserSessionID, &stored.CollectionID, &stored.CollectionBindingID)
	if bindingErr != nil && !errors.Is(bindingErr, sql.ErrNoRows) {
		return Event{}, fmt.Errorf("read event session binding: %w", bindingErr)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, err
	}
	return stored, nil
}

func (s *Store) PendingEvents(ctx context.Context, sessionID string, after uint64, limit int) ([]Event, error) {
	if limit <= 0 || limit > s.limits.PerSession {
		limit = s.limits.PerSession
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT o.session_id, o.agent_instance_id,
       COALESCE(b.user_session_id, ''), COALESCE(b.collection_id, ''),
       COALESCE(b.collection_binding_id, ''),
       o.sequence, o.event_type, o.payload, o.created_at_utc
FROM outbox o
LEFT JOIN session_bindings b ON b.session_id=o.session_id
WHERE o.session_id = ? AND o.acked_at_utc IS NULL AND o.sequence > ?
ORDER BY o.sequence LIMIT ?`, sessionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		var created string
		if err := rows.Scan(&event.SessionID, &event.AgentInstanceID, &event.UserSessionID,
			&event.CollectionID, &event.CollectionBindingID, &event.Sequence,
			&event.Type, &event.Payload, &created); err != nil {
			return nil, err
		}
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("parse outbox timestamp: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) PendingAgentEvents(ctx context.Context, agentInstanceID, userSessionID string,
	after uint64, limit int) ([]Event, error) {
	if agentInstanceID == "" || userSessionID == "" {
		return nil, errors.New("agent and user session are required")
	}
	if limit <= 0 || limit > s.limits.Global {
		limit = s.limits.Global
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT o.session_id, o.agent_instance_id, b.user_session_id, b.collection_id,
       b.collection_binding_id, o.sequence, o.event_type, o.payload, o.created_at_utc
FROM outbox o
JOIN session_bindings b ON b.session_id=o.session_id
WHERE o.agent_instance_id=? AND b.user_session_id=?
  AND o.acked_at_utc IS NULL AND o.sequence > ?
ORDER BY o.sequence LIMIT ?`, agentInstanceID, userSessionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		var created string
		if err := rows.Scan(&event.SessionID, &event.AgentInstanceID, &event.UserSessionID,
			&event.CollectionID, &event.CollectionBindingID, &event.Sequence,
			&event.Type, &event.Payload, &created); err != nil {
			return nil, err
		}
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("parse outbox timestamp: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) Acknowledge(ctx context.Context, sessionID string, through uint64, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
UPDATE outbox SET acked_at_utc = COALESCE(acked_at_utc, ?)
WHERE session_id = ? AND sequence <= ? AND acked_at_utc IS NULL`, utc(now), sessionID, through)
	return err
}

// AcknowledgeAgent advances the cumulative cursor across logical gateway
// sessions owned by the same persistent browser/agent identity. A browser
// refresh or Collection switch creates a new session ID, while event sequences
// remain global to agent_instance_id; limiting ACK to the newest session would
// leave already-committed rows orphaned forever.
func (s *Store) AcknowledgeAgent(ctx context.Context, agentInstanceID, userSessionID string,
	through uint64, now time.Time) (uint64, error) {
	if agentInstanceID == "" || userSessionID == "" {
		return 0, errors.New("agent instance and user session are required")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var nextSequence uint64
	err = tx.QueryRowContext(ctx,
		`SELECT next_sequence FROM agent_sequences WHERE agent_instance_id=?`, agentInstanceID,
	).Scan(&nextSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errors.New("event acknowledgement has no produced sequence")
	}
	if err != nil {
		return 0, err
	}
	if nextSequence == 0 || through >= nextSequence {
		return 0, errors.New("event acknowledgement exceeds produced sequence")
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE outbox SET acked_at_utc = COALESCE(acked_at_utc, ?)
WHERE rowid IN (
  SELECT o.rowid FROM outbox o
  JOIN session_bindings b ON b.session_id=o.session_id
  WHERE o.agent_instance_id=? AND b.user_session_id=?
    AND o.sequence <= ? AND o.acked_at_utc IS NULL
)`, utc(now), agentInstanceID, userSessionID, through); err != nil {
		return 0, err
	}
	confirmed, err := confirmedCursor(ctx, tx, agentInstanceID, userSessionID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return confirmed, nil
}

// ConfirmedCursor returns the highest sequence for which this authenticated
// user has no uncommitted event. Sequence numbers are agent-wide, so gaps that
// belong to a different user session are deliberately skipped without ACKing
// that other user's rows.
func (s *Store) ConfirmedCursor(ctx context.Context, agentInstanceID, userSessionID string) (uint64, error) {
	if agentInstanceID == "" || userSessionID == "" {
		return 0, errors.New("agent instance and user session are required")
	}
	return confirmedCursor(ctx, s.db, agentInstanceID, userSessionID)
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func confirmedCursor(ctx context.Context, query rowQuerier, agentInstanceID, userSessionID string) (uint64, error) {
	var cursor uint64
	err := query.QueryRowContext(ctx, `
WITH produced AS (
  SELECT COALESCE((SELECT next_sequence - 1 FROM agent_sequences WHERE agent_instance_id=?), 0) AS last_sequence
), pending AS (
  SELECT MIN(o.sequence) AS first_sequence
  FROM outbox o
  JOIN session_bindings b ON b.session_id=o.session_id
  WHERE o.agent_instance_id=? AND b.user_session_id=? AND o.acked_at_utc IS NULL
)
SELECT CASE
  WHEN pending.first_sequence IS NULL THEN produced.last_sequence
  ELSE pending.first_sequence - 1
END
FROM produced, pending`, agentInstanceID, agentInstanceID, userSessionID).Scan(&cursor)
	if err != nil {
		return 0, err
	}
	return cursor, nil
}

func (s *Store) SaveCall(ctx context.Context, sessionID, agentInstanceID string, snapshot call.Snapshot, now time.Time) error {
	if sessionID == "" || agentInstanceID == "" {
		return errors.New("call session and agent instance are required")
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	active := snapshot.State != call.StateEnded
	_, err = s.db.ExecContext(ctx, `
INSERT INTO calls(call_id, session_id, agent_instance_id, snapshot_json, active, updated_at_utc)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(call_id) DO UPDATE SET
  session_id=excluded.session_id,
  agent_instance_id=excluded.agent_instance_id,
  snapshot_json=excluded.snapshot_json,
  active=excluded.active,
  updated_at_utc=excluded.updated_at_utc`,
		snapshot.CallID, sessionID, agentInstanceID, raw, active, utc(now))
	return err
}

// SaveCallWithEvents commits the authoritative call snapshot and its externally
// observable lifecycle events in one SQLite transaction. This prevents the
// in-memory lifecycle from producing an outbox gap when either write fails.
func (s *Store) SaveCallWithEvents(
	ctx context.Context,
	sessionID, agentInstanceID string,
	snapshot call.Snapshot,
	writes []EventWrite,
	now time.Time,
) ([]Event, error) {
	if sessionID == "" || agentInstanceID == "" {
		return nil, errors.New("call session and agent instance are required")
	}
	if err := call.ValidateSnapshot(snapshot); err != nil {
		return nil, err
	}
	rawSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	for _, write := range writes {
		if write.Type == "" || len(write.Payload) == 0 {
			return nil, errors.New("event type and payload are required")
		}
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	active := snapshot.State != call.StateEnded
	if _, err := tx.ExecContext(ctx, `
INSERT INTO calls(call_id, session_id, agent_instance_id, snapshot_json, active, updated_at_utc)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(call_id) DO UPDATE SET
  session_id=excluded.session_id,
  agent_instance_id=excluded.agent_instance_id,
  snapshot_json=excluded.snapshot_json,
  active=excluded.active,
  updated_at_utc=excluded.updated_at_utc`,
		snapshot.CallID, sessionID, agentInstanceID, rawSnapshot, active, utc(now)); err != nil {
		return nil, fmt.Errorf("save call with events: %w", err)
	}

	terminalGroup := false
	for _, write := range writes {
		terminalGroup = terminalGroup || write.Type == "call.ended"
	}
	stored := make([]Event, 0, len(writes))
	for _, write := range writes {
		if err := s.ensureCapacity(ctx, tx, sessionID, terminalGroup); err != nil {
			return nil, err
		}
		sequence, err := nextSequence(ctx, tx, agentInstanceID)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO outbox(session_id, agent_instance_id, sequence, event_key, event_type, payload, created_at_utc)
VALUES(?, ?, ?, NULLIF(?, ''), ?, ?, ?)`, sessionID, agentInstanceID, sequence,
			write.Key, write.Type, write.Payload, utc(now)); err != nil {
			return nil, fmt.Errorf("append call outbox event: %w", err)
		}
		stored = append(stored, Event{
			SessionID: sessionID, AgentInstanceID: agentInstanceID, Sequence: sequence,
			Type: write.Type, Payload: write.Payload, CreatedAt: now.UTC(),
		})
	}

	var userSessionID, collectionID, collectionBindingID string
	bindingErr := tx.QueryRowContext(ctx, `
SELECT user_session_id, collection_id, collection_binding_id
FROM session_bindings WHERE session_id=? AND agent_instance_id=?`,
		sessionID, agentInstanceID).Scan(&userSessionID, &collectionID, &collectionBindingID)
	if bindingErr != nil && !errors.Is(bindingErr, sql.ErrNoRows) {
		return nil, fmt.Errorf("read call event session binding: %w", bindingErr)
	}
	for index := range stored {
		stored[index].UserSessionID = userSessionID
		stored[index].CollectionID = collectionID
		stored[index].CollectionBindingID = collectionBindingID
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return stored, nil
}

func (s *Store) RecoverRestartedCalls(ctx context.Context, now time.Time) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
SELECT call_id, session_id, agent_instance_id, snapshot_json
FROM calls WHERE active = 1 AND restart_terminalized = 0`)
	if err != nil {
		return 0, err
	}
	type activeCall struct {
		callID, sessionID, agentInstanceID string
		raw                                []byte
	}
	var active []activeCall
	for rows.Next() {
		var item activeCall
		if err := rows.Scan(&item.callID, &item.sessionID, &item.agentInstanceID, &item.raw); err != nil {
			rows.Close()
			return 0, err
		}
		active = append(active, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, item := range active {
		machine, err := restoreStoredCall(item.raw, item.callID)
		if err != nil {
			return 0, fmt.Errorf("decode call %s: %w", item.callID, err)
		}
		machine.RemoteHangup(call.EndGatewayRestarted, now)
		snapshot := machine.Snapshot()
		payload, err := json.Marshal(snapshot)
		if err != nil {
			return 0, err
		}
		for _, terminal := range []struct {
			key, eventType string
		}{
			{key: "restart-ended-state:" + item.callID, eventType: "call.state_changed"},
			{key: "restart-ended:" + item.callID, eventType: "call.ended"},
		} {
			if err := s.ensureCapacity(ctx, tx, item.sessionID, true); err != nil {
				return 0, err
			}
			sequence, err := nextSequence(ctx, tx, item.agentInstanceID)
			if err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO outbox(session_id, agent_instance_id, sequence, event_key, event_type, payload, created_at_utc)
VALUES(?, ?, ?, ?, ?, ?, ?)`, item.sessionID, item.agentInstanceID, sequence,
				terminal.key, terminal.eventType, payload, utc(now)); err != nil {
				return 0, err
			}
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE calls SET snapshot_json=?, active=0, restart_terminalized=1, updated_at_utc=? WHERE call_id=?`,
			payload, utc(now), item.callID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(active), nil
}

func restoreStoredCall(raw []byte, expectedCallID string) (*call.Machine, error) {
	var snapshot call.Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, err
	}
	if snapshot.CallID != expectedCallID {
		return nil, errors.New("stored call identity mismatch")
	}
	return call.Restore(snapshot)
}

func (s *Store) Purge(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.Maintain(ctx, before, time.Now().UTC())
	return result.PurgedRows, err
}

func (s *Store) CanStartCall(ctx context.Context, sessionID string) (bool, error) {
	var sessionCount, globalCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE session_id=? AND acked_at_utc IS NULL`, sessionID).Scan(&sessionCount); err != nil {
		return false, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE acked_at_utc IS NULL`).Scan(&globalCount); err != nil {
		return false, err
	}
	bytes, _, err := s.storageBytes()
	if err != nil {
		return false, err
	}
	return !atOrAboveReserve(sessionCount, s.limits.PerSession) &&
		!atOrAboveReserve(globalCount, s.limits.Global) &&
		!atOrAboveReserve64(bytes, s.limits.MaxBytes), nil
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var value Stats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE acked_at_utc IS NULL`).Scan(&value.PendingEvents); err != nil {
		return Stats{}, err
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(pending), 0)
		FROM (
			SELECT COUNT(*) AS pending
			FROM outbox
			WHERE acked_at_utc IS NULL
			GROUP BY session_id
		)
	`).Scan(&value.MaxSessionPendingEvents); err != nil {
		return Stats{}, err
	}
	totalBytes, walBytes, err := s.storageBytes()
	if err != nil {
		return Stats{}, err
	}
	value.DatabaseBytes = totalBytes - walBytes
	value.WALBytes = walBytes
	return value, nil
}

func (s *Store) Maintain(ctx context.Context, before, now time.Time) (MaintenanceResult, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MaintenanceResult{}, err
	}
	defer tx.Rollback()
	statements := []struct {
		query string
		arg   string
	}{
		{`DELETE FROM outbox WHERE acked_at_utc IS NOT NULL AND acked_at_utc < ?`, utc(before)},
		{`DELETE FROM commands WHERE status IN ('completed','failed') AND updated_at_utc < ?`, utc(before)},
		{`DELETE FROM calls WHERE active = 0 AND updated_at_utc < ?`, utc(before)},
		{`DELETE FROM consumed_capabilities WHERE expires_at_utc < ?`, utc(now)},
		{`DELETE FROM session_bindings
WHERE updated_at_utc < ?
  AND NOT EXISTS (SELECT 1 FROM outbox WHERE outbox.session_id=session_bindings.session_id)
  AND NOT EXISTS (SELECT 1 FROM calls WHERE calls.session_id=session_bindings.session_id)`, utc(before)},
	}
	var result MaintenanceResult
	for _, statement := range statements {
		res, err := tx.ExecContext(ctx, statement.query, statement.arg)
		if err != nil {
			return MaintenanceResult{}, err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return MaintenanceResult{}, err
		}
		result.PurgedRows += rows
	}
	if err := tx.Commit(); err != nil {
		return MaintenanceResult{}, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(
		&result.CheckpointBusy, &result.WALFrames, &result.CheckpointedFrames,
	); err != nil {
		return MaintenanceResult{}, fmt.Errorf("sqlite WAL checkpoint: %w", err)
	}
	return result, nil
}

func (s *Store) ensureCapacity(ctx context.Context, tx *sql.Tx, sessionID string, terminal bool) error {
	var sessionCount, globalCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE session_id=? AND acked_at_utc IS NULL`, sessionID).Scan(&sessionCount); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE acked_at_utc IS NULL`).Scan(&globalCount); err != nil {
		return err
	}
	if sessionCount >= s.limits.PerSession || globalCount >= s.limits.Global {
		return ErrCapacity
	}
	bytes, _, err := s.storageBytes()
	if err != nil {
		return err
	}
	if bytes >= s.limits.MaxBytes {
		return ErrCapacity
	}
	if !terminal && (atOrAboveReserve(sessionCount, s.limits.PerSession) ||
		atOrAboveReserve(globalCount, s.limits.Global) || atOrAboveReserve64(bytes, s.limits.MaxBytes)) {
		return ErrCapacity
	}
	return nil
}

func (s *Store) storageBytes() (total, wal int64, err error) {
	for _, item := range []struct {
		path  string
		isWAL bool
	}{{s.path, false}, {s.path + "-wal", true}, {s.path + "-shm", false}} {
		info, statErr := os.Stat(item.path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return 0, 0, statErr
		}
		total += info.Size()
		if item.isWAL {
			wal = info.Size()
		}
	}
	return total, wal, nil
}

func atOrAboveReserve(value, limit int) bool {
	if limit <= 0 {
		return value >= limit
	}
	return value >= limit-limit/10
}

func atOrAboveReserve64(value, limit int64) bool {
	if limit <= 0 {
		return value >= limit
	}
	return value >= limit-limit/10
}

func nextSequence(ctx context.Context, tx *sql.Tx, agentInstanceID string) (uint64, error) {
	var sequence int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO agent_sequences(agent_instance_id, next_sequence) VALUES(?, 2)
ON CONFLICT(agent_instance_id) DO UPDATE SET next_sequence = agent_sequences.next_sequence + 1
RETURNING next_sequence - 1`, agentInstanceID).Scan(&sequence)
	if err != nil {
		return 0, err
	}
	if sequence <= 0 {
		return 0, errors.New("invalid persisted agent sequence")
	}
	return uint64(sequence), nil
}

func utc(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
