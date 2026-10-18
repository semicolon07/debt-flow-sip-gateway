package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/debt-flow/sip-gateway/internal/protocol"
	"github.com/google/uuid"
)

type fakeRedeemer struct{}

func (fakeRedeemer) Redeem(_ context.Context, auth protocol.Authenticate) (Claims, error) {
	return Claims{
		CapabilityID: "capability", SessionID: "gateway-session", UserSessionID: "portal-session",
		CollectionID: auth.CollectionID, ExpiresAtUTC: time.Now().Add(2 * time.Minute),
	}, nil
}

type fakeRuntime struct {
	mu           sync.Mutex
	disconnected int
	connectErr   error
	handleErr    error
}

func (f *fakeRuntime) Connect(_ context.Context, claims Claims) ([]protocol.Envelope, <-chan protocol.Envelope, error) {
	f.mu.Lock()
	connectErr := f.connectErr
	f.mu.Unlock()
	if connectErr != nil {
		return nil, nil, connectErr
	}
	welcome, _ := protocol.NewEvent("session.welcome", map[string]string{"sessionId": claims.SessionID}, time.Now())
	snapshot, _ := protocol.NewEvent("gateway.snapshot", map[string]string{"state": "idle"}, time.Now())
	return []protocol.Envelope{welcome, snapshot}, nil, nil
}

func (f *fakeRuntime) Handle(_ context.Context, _ Claims, command protocol.Envelope) ([]protocol.Envelope, error) {
	f.mu.Lock()
	handleErr := f.handleErr
	f.mu.Unlock()
	if handleErr != nil {
		return nil, handleErr
	}
	event, _ := protocol.NewEvent("command_result", map[string]string{"type": command.Type}, time.Now())
	return []protocol.Envelope{event}, nil
}

func (*fakeRuntime) HasActiveCall(string, string) bool { return false }

func (f *fakeRuntime) Disconnect(Claims) {
	f.mu.Lock()
	f.disconnected++
	f.mu.Unlock()
}

func testServer(t *testing.T) (*httptest.Server, *fakeRuntime) {
	return testServerWithHeartbeat(t, time.Second, 2*time.Second)
}

func testServerWithHeartbeat(
	t *testing.T,
	heartbeat time.Duration,
	idleTimeout time.Duration,
) (*httptest.Server, *fakeRuntime) {
	t.Helper()
	runtime := &fakeRuntime{}
	server, err := NewServer(ServerConfig{
		AllowedOrigins: []string{"https://debt-flow.uniga.app"}, AuthTimeout: time.Second,
		Heartbeat: heartbeat, IdleTimeout: idleTimeout, ReconnectGrace: 10 * time.Millisecond,
		MaxMessageBytes: 64 << 10, MessagesPerWindow: 60, MessageWindow: 10 * time.Second,
		WriterQueue: 8,
	}, fakeRedeemer{}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer, runtime
}

func TestGatewayKeepsAnIdleConnectionAliveWhilePongsSucceed(t *testing.T) {
	server, _ := testServerWithHeartbeat(t, 20*time.Millisecond, 60*time.Millisecond)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/gateway/v1"
	conn, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://debt-flow.uniga.app"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	authPayload, _ := json.Marshal(protocol.Authenticate{
		CollectionID: "collection", SessionCapability: "capability", PortalVersion: "1.0.0",
		SupportedProtocolVersions: []int{1},
	})
	if err := wsjson.Write(context.Background(), conn, protocol.Envelope{
		ProtocolVersion: 1, Kind: protocol.KindCommand, MessageID: uuid.NewString(),
		SentAtUTC: time.Now().UTC(), Type: "session.authenticate", Payload: authPayload,
	}); err != nil {
		t.Fatal(err)
	}
	events := make(chan protocol.Envelope, 3)
	errors := make(chan error, 1)
	readCtx, cancelRead := context.WithCancel(context.Background())
	defer cancelRead()
	go func() {
		for {
			var event protocol.Envelope
			if err := wsjson.Read(readCtx, conn, &event); err != nil {
				errors <- err
				return
			}
			events <- event
		}
	}()
	for range 2 {
		select {
		case <-events:
		case err := <-errors:
			t.Fatalf("startup read failed: %v", err)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for startup event")
		}
	}
	// The old application-read deadline closed this connection after 60 ms even
	// though transport pongs succeeded. Stay idle beyond that deadline twice.
	time.Sleep(140 * time.Millisecond)
	if err := wsjson.Write(context.Background(), conn, protocol.Envelope{
		ProtocolVersion: 1, Kind: protocol.KindCommand, MessageID: uuid.NewString(),
		SentAtUTC: time.Now().UTC(), Type: "state.get", Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("idle connection was closed: %v", err)
	}
	select {
	case event := <-events:
		if event.Type != "command_result" {
			t.Fatalf("unexpected event after idle period: %s", event.Type)
		}
	case err := <-errors:
		t.Fatalf("idle connection stopped reading: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for command result after idle period")
	}
}

func TestGatewayRequiresExactOrigin(t *testing.T) {
	server, _ := testServer(t)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/gateway/v1"
	_, response, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}},
	})
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, response=%v err=%v", response, err)
	}
}

func TestOriginAllowedSupportsMultipleExactOriginsAndWildcard(t *testing.T) {
	for name, test := range map[string]struct {
		candidate     string
		allowed       []string
		allowLoopback bool
		want          bool
	}{
		"first exact origin": {
			candidate: "https://app.example", allowed: []string{"https://app.example", "https://admin.example"}, want: true,
		},
		"second exact origin": {
			candidate: "https://admin.example", allowed: []string{"https://app.example", "https://admin.example"}, want: true,
		},
		"unlisted origin": {
			candidate: "https://evil.example", allowed: []string{"https://app.example", "https://admin.example"}, want: false,
		},
		"wildcard HTTPS": {
			candidate: "https://tenant.example", allowed: []string{"*"}, want: true,
		},
		"wildcard rejects missing origin": {candidate: "", allowed: []string{"*"}, want: false},
		"wildcard rejects opaque origin":  {candidate: "null", allowed: []string{"*"}, want: false},
		"wildcard rejects public HTTP": {
			candidate: "http://tenant.example", allowed: []string{"*"}, want: false,
		},
		"wildcard allows opted-in HTTP loopback": {
			candidate: "http://127.0.0.1:8765", allowed: []string{"*"}, allowLoopback: true, want: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := originAllowed(test.candidate, test.allowed, test.allowLoopback); got != test.want {
				t.Fatalf("originAllowed(%q)=%v, want %v", test.candidate, got, test.want)
			}
		})
	}
}

func TestGatewayAuthenticatesThenSendsWelcomeSnapshotAndResults(t *testing.T) {
	server, _ := testServer(t)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/gateway/v1"
	conn, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://debt-flow.uniga.app"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	authPayload, _ := json.Marshal(protocol.Authenticate{
		CollectionID: "collection", SessionCapability: "capability", PortalVersion: "1.0.0",
		SupportedProtocolVersions: []int{1},
	})
	auth := protocol.Envelope{
		ProtocolVersion: 1, Kind: protocol.KindCommand, MessageID: uuid.NewString(),
		SentAtUTC: time.Now().UTC(), Type: "session.authenticate", Payload: authPayload,
	}
	if err := wsjson.Write(context.Background(), conn, auth); err != nil {
		t.Fatal(err)
	}
	var first, second protocol.Envelope
	if err := wsjson.Read(context.Background(), conn, &first); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(context.Background(), conn, &second); err != nil {
		t.Fatal(err)
	}
	if first.Type != "session.welcome" || second.Type != "gateway.snapshot" {
		t.Fatalf("unexpected startup order: %s then %s", first.Type, second.Type)
	}
	command := protocol.Envelope{
		ProtocolVersion: 1, Kind: protocol.KindCommand, MessageID: uuid.NewString(),
		SentAtUTC: time.Now().UTC(), Type: "state.get", Payload: json.RawMessage(`{}`),
	}
	if err := wsjson.Write(context.Background(), conn, command); err != nil {
		t.Fatal(err)
	}
	var result protocol.Envelope
	if err := wsjson.Read(context.Background(), conn, &result); err != nil {
		t.Fatal(err)
	}
	if result.Type != "command_result" {
		t.Fatalf("unexpected command result %s", result.Type)
	}
}

func TestGatewayCorrelatesRuntimeErrorsToTheCommand(t *testing.T) {
	server, runtime := testServer(t)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/gateway/v1"
	conn, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://debt-flow.uniga.app"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	authPayload, _ := json.Marshal(protocol.Authenticate{
		CollectionID: "collection", SessionCapability: "capability", PortalVersion: "1.0.0",
		SupportedProtocolVersions: []int{1},
	})
	if err := wsjson.Write(context.Background(), conn, protocol.Envelope{
		ProtocolVersion: 1, Kind: protocol.KindCommand, MessageID: uuid.NewString(),
		SentAtUTC: time.Now().UTC(), Type: "session.authenticate", Payload: authPayload,
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		var startup protocol.Envelope
		if err := wsjson.Read(context.Background(), conn, &startup); err != nil {
			t.Fatal(err)
		}
	}
	runtime.mu.Lock()
	runtime.handleErr = commandError{"registration_required"}
	runtime.mu.Unlock()
	commandID := uuid.NewString()
	payload, _ := json.Marshal(protocol.CommandIdentity{CommandID: commandID})
	if err := wsjson.Write(context.Background(), conn, protocol.Envelope{
		ProtocolVersion: 1, Kind: protocol.KindCommand, MessageID: uuid.NewString(),
		SentAtUTC: time.Now().UTC(), Type: "call.start", Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	var response protocol.Envelope
	if err := wsjson.Read(context.Background(), conn, &response); err != nil {
		t.Fatal(err)
	}
	var responsePayload struct {
		Code      string `json:"code"`
		CommandID string `json:"commandId"`
	}
	if err := json.Unmarshal(response.Payload, &responsePayload); err != nil {
		t.Fatal(err)
	}
	if response.Type != "error" || responsePayload.Code != "registration_required" || responsePayload.CommandID != commandID {
		t.Fatalf("unexpected correlated error: type=%q payload=%+v", response.Type, responsePayload)
	}
}

func TestFailedReplacementDoesNotEvictHealthyConnection(t *testing.T) {
	server, runtime := testServer(t)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/gateway/v1"
	dial := func(t *testing.T) *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
			HTTPHeader: http.Header{"Origin": []string{"https://debt-flow.uniga.app"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		authPayload, _ := json.Marshal(protocol.Authenticate{
			CollectionID: "collection", SessionCapability: "capability", PortalVersion: "1.0.0",
			SupportedProtocolVersions: []int{1},
		})
		auth := protocol.Envelope{
			ProtocolVersion: 1, Kind: protocol.KindCommand, MessageID: uuid.NewString(),
			SentAtUTC: time.Now().UTC(), Type: "session.authenticate", Payload: authPayload,
		}
		if err := wsjson.Write(context.Background(), conn, auth); err != nil {
			t.Fatal(err)
		}
		return conn
	}

	healthy := dial(t)
	defer healthy.CloseNow()
	for range 2 {
		var startup protocol.Envelope
		if err := wsjson.Read(context.Background(), healthy, &startup); err != nil {
			t.Fatal(err)
		}
	}

	runtime.mu.Lock()
	runtime.connectErr = errors.New("runtime unavailable")
	runtime.mu.Unlock()
	failed := dial(t)
	defer failed.CloseNow()
	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var ignored protocol.Envelope
	if err := wsjson.Read(readCtx, failed, &ignored); websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("replacement close status=%v err=%v", websocket.CloseStatus(err), err)
	}

	runtime.mu.Lock()
	runtime.connectErr = nil
	runtime.mu.Unlock()
	command := protocol.Envelope{
		ProtocolVersion: 1, Kind: protocol.KindCommand, MessageID: uuid.NewString(),
		SentAtUTC: time.Now().UTC(), Type: "state.get", Payload: json.RawMessage(`{}`),
	}
	if err := wsjson.Write(context.Background(), healthy, command); err != nil {
		t.Fatalf("healthy connection was evicted: %v", err)
	}
	var result protocol.Envelope
	if err := wsjson.Read(context.Background(), healthy, &result); err != nil {
		t.Fatalf("healthy connection stopped responding: %v", err)
	}
	if result.Type != "command_result" {
		t.Fatalf("unexpected command result %s", result.Type)
	}
}

func TestWindowLimiter(t *testing.T) {
	now := time.Now()
	limiter := newWindowLimiter(2, time.Second, func() time.Time { return now })
	if !limiter.Allow() {
		t.Fatal("first message should be allowed")
	}
	now = now.Add(900 * time.Millisecond)
	if !limiter.Allow() {
		t.Fatal("second message should be allowed")
	}
	now = now.Add(100 * time.Millisecond)
	if !limiter.Allow() {
		t.Fatal("message exactly one window after the first should be allowed")
	}
	if limiter.Allow() {
		t.Fatal("rolling window must reject a boundary burst still containing two live messages")
	}
	now = now.Add(901 * time.Millisecond)
	if !limiter.Allow() {
		t.Fatal("expired timestamps should release rolling-window capacity")
	}
}

func TestClassifyWSSDisconnectUsesBoundedReasons(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{nil, "client_closed"},
		{context.DeadlineExceeded, "idle_timeout"},
		{errors.New("writer queue full"), "writer_queue_full"},
		{errors.New("message rate limit exceeded"), "rate_limited"},
		{errors.New("invalid command envelope"), "protocol_violation"},
		{errors.New("socket details that must not become a label"), "transport_error"},
	} {
		if got := classifyWSSDisconnect(test.err); got != test.want {
			t.Errorf("classifyWSSDisconnect(%v)=%q, want %q", test.err, got, test.want)
		}
	}
}

func TestCommandIDFromEnvelopeCorrelatesErrors(t *testing.T) {
	envelope := protocol.Envelope{Payload: json.RawMessage(`{"commandId":"command-123"}`)}
	if got := commandIDFromEnvelope(envelope); got != "command-123" {
		t.Fatalf("commandIDFromEnvelope()=%q", got)
	}
	if got := commandIDFromEnvelope(protocol.Envelope{Payload: json.RawMessage(`{"commandId":`)}); got != "" {
		t.Fatalf("invalid payload returned command ID %q", got)
	}
}

func TestInitialReplayIsQueuedBeforeLiveEvents(t *testing.T) {
	server := &Server{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	current := &client{send: make(chan protocol.Envelope, 3), cancel: cancel}
	initial := []protocol.Envelope{{Type: "session.welcome"}, {Type: "gateway.snapshot"}}
	live := make(chan protocol.Envelope, 1)
	live <- protocol.Envelope{Type: "call.state_changed"}
	writerDone := make(chan error, 1)
	if _, err := server.enqueueInitialAndStream(ctx, current, initial, live, writerDone); err != nil {
		t.Fatalf("initial events were rejected: %v", err)
	}
	want := []string{"session.welcome", "gateway.snapshot", "call.state_changed"}
	for index, eventType := range want {
		select {
		case event := <-current.send:
			if event.Type != eventType {
				t.Fatalf("event[%d]=%q, want %q", index, event.Type, eventType)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", index)
		}
	}
}

func TestInitialReplayCanExceedTheWriterQueueWithoutReordering(t *testing.T) {
	server := &Server{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	current := &client{send: make(chan protocol.Envelope, 1), cancel: cancel}
	initial := []protocol.Envelope{
		{Type: "session.welcome"},
		{Type: "gateway.snapshot"},
		{Type: "call.created"},
		{Type: "call.state_changed"},
	}
	live := make(chan protocol.Envelope, 1)
	live <- protocol.Envelope{Type: "call.ended"}
	writerDone := make(chan error, 1)
	drained := make(chan string, len(initial)+1)
	go func() {
		for range len(initial) + 1 {
			event := <-current.send
			drained <- event.Type
		}
	}()
	if _, err := server.enqueueInitialAndStream(ctx, current, initial, live, writerDone); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"session.welcome", "gateway.snapshot", "call.created", "call.state_changed", "call.ended",
	}
	for index, eventType := range want {
		select {
		case got := <-drained:
			if got != eventType {
				t.Fatalf("event[%d]=%q, want %q", index, got, eventType)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", index)
		}
	}
}

func TestRegistrySeparatesTheSameUserAcrossCollections(t *testing.T) {
	runtime := &fakeRuntime{}
	value := &registry{
		clients: make(map[sessionOwner]*client),
		runtime: runtime,
		grace:   time.Millisecond,
	}
	firstCanceled := false
	first := &client{
		claims: Claims{UserSessionID: "user-session", CollectionID: "collection-a", SessionID: "session-a"},
		cancel: func() { firstCanceled = true },
	}
	second := &client{
		claims: Claims{UserSessionID: "user-session", CollectionID: "collection-b", SessionID: "session-b"},
		cancel: func() {},
	}
	if err := value.acquire(first); err != nil {
		t.Fatal(err)
	}
	if err := value.acquire(second); err != nil {
		t.Fatal(err)
	}
	if firstCanceled {
		t.Fatal("connection in another collection was canceled")
	}
	if len(value.clients) != 2 {
		t.Fatalf("registry clients=%d, want 2", len(value.clients))
	}
}

func TestRegistrySerializesAdmissionOnlyForTheSameOwner(t *testing.T) {
	value := &registry{admissions: make(map[sessionOwner]*admission)}
	owner := sessionOwner{userSessionID: "user-session", collectionID: "collection-a"}
	releaseFirst := value.beginAdmission(owner)
	sameOwnerAcquired := make(chan func(), 1)
	go func() { sameOwnerAcquired <- value.beginAdmission(owner) }()

	select {
	case release := <-sameOwnerAcquired:
		release()
		t.Fatal("same owner admission was not serialized")
	case <-time.After(30 * time.Millisecond):
	}

	otherOwner := sessionOwner{userSessionID: "user-session", collectionID: "collection-b"}
	otherOwnerAcquired := make(chan func(), 1)
	go func() { otherOwnerAcquired <- value.beginAdmission(otherOwner) }()
	select {
	case release := <-otherOwnerAcquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("admission for another owner was unnecessarily serialized")
	}

	releaseFirst()
	select {
	case release := <-sameOwnerAcquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("same owner admission did not resume")
	}
	value.admissionMu.Lock()
	defer value.admissionMu.Unlock()
	if len(value.admissions) != 0 {
		t.Fatalf("admission locks leaked: %d", len(value.admissions))
	}
}
