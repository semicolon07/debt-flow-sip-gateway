package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/debt-flow/sip-gateway/internal/protocol"
)

type Claims struct {
	CapabilityID        string
	SessionID           string
	UserSessionID       string
	CollectionID        string
	CollectionBindingID string
	AgentInstanceID     string
	ExpiresAtUTC        time.Time
}

type Redeemer interface {
	Redeem(context.Context, protocol.Authenticate) (Claims, error)
}

type Runtime interface {
	Connect(context.Context, Claims) ([]protocol.Envelope, <-chan protocol.Envelope, error)
	Handle(context.Context, Claims, protocol.Envelope) ([]protocol.Envelope, error)
	HasActiveCall(userSessionID, collectionID string) bool
	Disconnect(Claims)
}

type ServerConfig struct {
	AllowedOrigins              []string
	AllowInsecureLoopbackOrigin bool
	AuthTimeout                 time.Duration
	Heartbeat                   time.Duration
	IdleTimeout                 time.Duration
	ReconnectGrace              time.Duration
	MaxMessageBytes             int64
	MessagesPerWindow           int
	MessageWindow               time.Duration
	WriterQueue                 int
}

type Server struct {
	cfg      ServerConfig
	redeemer Redeemer
	runtime  Runtime
	logger   *slog.Logger
	registry *registry
	now      func() time.Time
	observer WSSObserver
}

type WSSObserver interface {
	ObserveWSSDisconnect(reason string)
}

type client struct {
	claims Claims
	conn   *websocket.Conn
	send   chan protocol.Envelope
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (s *Server) SetObserver(observer WSSObserver) {
	s.observer = observer
}

type registry struct {
	mu          sync.Mutex
	clients     map[sessionOwner]*client
	runtime     Runtime
	grace       time.Duration
	admissionMu sync.Mutex
	admissions  map[sessionOwner]*admission
}

type admission struct {
	mu   sync.Mutex
	refs int
}

type sessionOwner struct {
	userSessionID string
	collectionID  string
}

type ServerStats struct {
	Connections      int
	WriterQueueDepth int
}

func NewServer(cfg ServerConfig, redeemer Redeemer, runtime Runtime, logger *slog.Logger) (*Server, error) {
	if len(cfg.AllowedOrigins) == 0 || cfg.AuthTimeout <= 0 || cfg.Heartbeat <= 0 || cfg.IdleTimeout <= 0 ||
		cfg.ReconnectGrace <= 0 || cfg.MaxMessageBytes <= 0 || cfg.MessagesPerWindow <= 0 ||
		cfg.MessageWindow <= 0 || cfg.WriterQueue <= 0 {
		return nil, errors.New("invalid gateway server configuration")
	}
	for _, origin := range cfg.AllowedOrigins {
		if origin != "*" && !validBrowserOrigin(origin, cfg.AllowInsecureLoopbackOrigin) {
			return nil, errors.New("invalid gateway server origin configuration")
		}
	}
	if redeemer == nil || runtime == nil {
		return nil, errors.New("gateway redeemer and runtime are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	cfg.AllowedOrigins = append([]string(nil), cfg.AllowedOrigins...)
	return &Server{
		cfg: cfg, redeemer: redeemer, runtime: runtime, logger: logger,
		registry: &registry{
			clients: make(map[sessionOwner]*client), runtime: runtime, grace: cfg.ReconnectGrace,
			admissions: make(map[sessionOwner]*admission),
		},
		now: time.Now,
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/gateway/v1", s.handleGateway)
	return mux
}

func (s *Server) Stats() ServerStats {
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	result := ServerStats{Connections: len(s.registry.clients)}
	for _, current := range s.registry.clients {
		result.WriterQueueDepth += len(current.send)
	}
	return result
}

func (s *Server) handleGateway(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !originAllowed(request.Header.Get("Origin"), s.cfg.AllowedOrigins, s.cfg.AllowInsecureLoopbackOrigin) {
		http.Error(writer, "origin is not allowed", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		// Origin is checked against the configured allowlist above. Keep the
		// library check disabled so multiple exact origins and explicit wildcard
		// semantics are handled consistently in one place.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		// Keep handshake logs bounded: request headers and capability material must
		// never be copied into operational logs.
		s.logger.Warn("websocket accept failed", "reason", "handshake_rejected")
		return
	}
	conn.SetReadLimit(s.cfg.MaxMessageBytes)
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	defer conn.CloseNow()
	disconnectReason := "transport_closed"
	defer func() {
		if s.observer != nil {
			s.observer.ObserveWSSDisconnect(disconnectReason)
		}
	}()

	claims, err := s.authenticate(ctx, conn)
	if err != nil {
		disconnectReason = "authentication_failed"
		_ = conn.Close(websocket.StatusPolicyViolation, safeCloseReason(err))
		return
	}
	current := &client{
		claims: claims, conn: conn, send: make(chan protocol.Envelope, s.cfg.WriterQueue),
		cancel: cancel, done: make(chan struct{}),
	}
	releaseAdmission := s.registry.beginAdmission(ownerOf(claims))
	initial, live, err := s.runtime.Connect(ctx, claims)
	if err != nil {
		releaseAdmission()
		disconnectReason = "runtime_unavailable"
		s.runtime.Disconnect(claims)
		_ = conn.Close(websocket.StatusInternalError, "runtime unavailable")
		return
	}
	if err := s.registry.acquire(current); err != nil {
		releaseAdmission()
		disconnectReason = "takeover_rejected"
		s.runtime.Disconnect(claims)
		_ = conn.Close(websocket.StatusPolicyViolation, safeCloseReason(err))
		return
	}
	releaseAdmission()
	defer s.registry.release(current)
	writerDone := make(chan error, 1)
	go func() { writerDone <- s.writeLoop(ctx, current) }()
	liveDone, err := s.enqueueInitialAndStream(ctx, current, initial, live, writerDone)
	if err != nil {
		disconnectReason = classifyWSSDisconnect(err)
		cancel()
		conn.CloseNow()
		return
	}
	readDone := make(chan error, 1)
	go func() { readDone <- s.readLoop(ctx, current) }()
	var loopErr error
	var otherDone <-chan error
	select {
	case loopErr = <-readDone:
		otherDone = writerDone
	case loopErr = <-writerDone:
		otherDone = readDone
	case loopErr = <-liveDone:
		otherDone = readDone
	}
	disconnectReason = classifyWSSDisconnect(loopErr)
	cancel()
	conn.CloseNow()
	select {
	case <-otherDone:
	case <-time.After(time.Second):
	}
	if loopErr != nil && !errors.Is(loopErr, context.Canceled) && websocket.CloseStatus(loopErr) == -1 {
		s.logger.Info("gateway session disconnected", "sessionId", claims.SessionID, "reason", disconnectReason)
	}
}

func originAllowed(candidate string, allowed []string, allowInsecureLoopback bool) bool {
	if !validBrowserOrigin(candidate, allowInsecureLoopback) {
		return false
	}
	for _, configured := range allowed {
		if configured == "*" || configured == candidate {
			return true
		}
	}
	return false
}

func validBrowserOrigin(value string, allowInsecureLoopback bool) bool {
	origin, err := url.Parse(value)
	if err != nil || origin.Host == "" || origin.Path != "" || origin.RawQuery != "" ||
		origin.Fragment != "" || origin.User != nil {
		return false
	}
	if origin.Scheme == "https" {
		return true
	}
	if origin.Scheme != "http" || !allowInsecureLoopback {
		return false
	}
	host := origin.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) authenticate(parent context.Context, conn *websocket.Conn) (Claims, error) {
	ctx, cancel := context.WithTimeout(parent, s.cfg.AuthTimeout)
	defer cancel()
	var raw []byte
	messageType, raw, err := conn.Read(ctx)
	if err != nil {
		return Claims{}, errors.New("authentication message required")
	}
	if messageType != websocket.MessageText {
		return Claims{}, errors.New("authentication must be JSON text")
	}
	envelope, err := protocol.Decode(raw)
	if err != nil || envelope.Type != "session.authenticate" {
		return Claims{}, errors.New("session.authenticate must be the first command")
	}
	authentication, err := protocol.DecodePayload[protocol.Authenticate](envelope)
	if err != nil {
		return Claims{}, errors.New("invalid authentication payload")
	}
	if err := protocol.ValidateAuthenticate(authentication); err != nil {
		return Claims{}, err
	}
	claims, err := s.redeemer.Redeem(ctx, authentication)
	if err != nil {
		return Claims{}, errors.New("authentication rejected")
	}
	if claims.SessionID == "" || claims.UserSessionID == "" || claims.CollectionID != authentication.CollectionID {
		return Claims{}, errors.New("authentication claims are invalid")
	}
	return claims, nil
}

func (s *Server) readLoop(ctx context.Context, current *client) error {
	limiter := newWindowLimiter(s.cfg.MessagesPerWindow, s.cfg.MessageWindow, s.now)
	for {
		// Connection liveness is governed by the WebSocket ping/pong exchange in
		// writeLoop. Timing out an application-data read would disconnect a healthy
		// browser merely because it had no commands to send.
		messageType, raw, err := current.conn.Read(ctx)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageText {
			return errors.New("binary messages are not allowed")
		}
		if !limiter.Allow() {
			return errors.New("message rate limit exceeded")
		}
		envelope, err := protocol.Decode(raw)
		if err != nil {
			return errors.New("invalid command envelope")
		}
		if envelope.Type == "session.authenticate" {
			return errors.New("session is already authenticated")
		}
		events, err := s.runtime.Handle(ctx, current.claims, envelope)
		if err != nil {
			payload := map[string]string{"code": safeRuntimeCode(err)}
			if commandID := commandIDFromEnvelope(envelope); commandID != "" {
				payload["commandId"] = commandID
			}
			errorEvent, eventErr := protocol.NewEvent("error", payload, s.now())
			if eventErr != nil || !s.enqueue(current, errorEvent) {
				return errors.New("writer queue full")
			}
			continue
		}
		for _, event := range events {
			if !s.enqueue(current, event) {
				return errors.New("writer queue full")
			}
		}
	}
}

func (s *Server) writeLoop(ctx context.Context, current *client) error {
	ticker := time.NewTicker(s.cfg.Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-current.send:
			writeCtx, cancel := context.WithTimeout(ctx, s.cfg.Heartbeat)
			err := wsjson.Write(writeCtx, current.conn, event)
			cancel()
			if err != nil {
				return err
			}
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, s.cfg.IdleTimeout)
			err := current.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return err
			}
		}
	}
}

func (s *Server) enqueue(current *client, event protocol.Envelope) bool {
	select {
	case current.send <- event:
		return true
	default:
		return false
	}
}

func (s *Server) enqueueInitialAndStream(
	ctx context.Context,
	current *client,
	initial []protocol.Envelope,
	live <-chan protocol.Envelope,
	writerDone <-chan error,
) (<-chan error, error) {
	// Queue the welcome, snapshot and durable replay synchronously before the
	// live pump starts. Initial replay applies backpressure through the bounded
	// writer queue instead of rejecting a healthy reconnect merely because its
	// replay is longer than that queue.
	for _, event := range initial {
		select {
		case current.send <- event:
		case err := <-writerDone:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var liveDone chan error
	if live != nil {
		liveDone = make(chan error, 1)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case event, ok := <-live:
					if !ok {
						liveDone <- errors.New("runtime session closed")
						return
					}
					select {
					case current.send <- event:
					case err := <-writerDone:
						liveDone <- err
						return
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	return liveDone, nil
}

func (r *registry) acquire(next *client) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner := ownerOf(next.claims)
	existing := r.clients[owner]
	if existing != nil {
		if existing.claims.SessionID != next.claims.SessionID &&
			r.runtime.HasActiveCall(next.claims.UserSessionID, next.claims.CollectionID) {
			return errors.New("active call prevents session takeover")
		}
		existing.cancel()
	}
	r.clients[owner] = next
	return nil
}

func (r *registry) beginAdmission(owner sessionOwner) func() {
	r.admissionMu.Lock()
	if r.admissions == nil {
		r.admissions = make(map[sessionOwner]*admission)
	}
	value := r.admissions[owner]
	if value == nil {
		value = &admission{}
		r.admissions[owner] = value
	}
	value.refs++
	r.admissionMu.Unlock()
	value.mu.Lock()
	return func() {
		value.mu.Unlock()
		r.admissionMu.Lock()
		value.refs--
		if value.refs == 0 {
			delete(r.admissions, owner)
		}
		r.admissionMu.Unlock()
	}
}

func (r *registry) release(current *client) {
	current.once.Do(func() { close(current.done) })
	r.runtime.Disconnect(current.claims)
	timer := time.NewTimer(r.grace)
	go func() {
		defer timer.Stop()
		<-timer.C
		r.mu.Lock()
		owner := ownerOf(current.claims)
		if r.clients[owner] == current {
			delete(r.clients, owner)
		}
		r.mu.Unlock()
	}()
}

func ownerOf(claims Claims) sessionOwner {
	return sessionOwner{userSessionID: claims.UserSessionID, collectionID: claims.CollectionID}
}

type windowLimiter struct {
	mu         sync.Mutex
	limit      int
	window     time.Duration
	now        func() time.Time
	timestamps []time.Time
}

func newWindowLimiter(limit int, window time.Duration, now func() time.Time) *windowLimiter {
	return &windowLimiter{limit: limit, window: window, now: now, timestamps: make([]time.Time, 0, limit)}
}

func (l *windowLimiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)
	firstLive := 0
	for firstLive < len(l.timestamps) && !l.timestamps[firstLive].After(cutoff) {
		firstLive++
	}
	if firstLive > 0 {
		copy(l.timestamps, l.timestamps[firstLive:])
		l.timestamps = l.timestamps[:len(l.timestamps)-firstLive]
	}
	if len(l.timestamps) >= l.limit {
		return false
	}
	l.timestamps = append(l.timestamps, now)
	return true
}

func safeCloseReason(err error) string {
	if err == nil {
		return "closed"
	}
	message := err.Error()
	if len(message) > 100 {
		message = message[:100]
	}
	return message
}

func safeRuntimeCode(err error) string {
	type coder interface{ Code() string }
	var value coder
	if errors.As(err, &value) {
		return value.Code()
	}
	return "gateway_command_failed"
}

func commandIDFromEnvelope(envelope protocol.Envelope) string {
	var identity protocol.CommandIdentity
	if json.Unmarshal(envelope.Payload, &identity) != nil {
		return ""
	}
	return identity.CommandID
}

func classifyWSSDisconnect(err error) string {
	if err == nil || errors.Is(err, context.Canceled) || websocket.CloseStatus(err) >= 0 {
		return "client_closed"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "idle_timeout"
	}
	switch err.Error() {
	case "writer queue full":
		return "writer_queue_full"
	case "runtime session closed":
		return "runtime_closed"
	case "message rate limit exceeded":
		return "rate_limited"
	case "invalid command envelope", "binary messages are not allowed", "session is already authenticated":
		return "protocol_violation"
	default:
		return "transport_error"
	}
}

func (c Claims) String() string {
	return fmt.Sprintf("session=%s collection=%s", c.SessionID, c.CollectionID)
}
