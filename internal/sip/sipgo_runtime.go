package sip

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	sipmessage "github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/icholy/digest"
	"github.com/pion/rtp"
)

type SIPGoConfig struct {
	ListenUDP         string
	ListenTCP         string
	AdvertisedIP      string
	UserAgent         string
	EventBuffer       int
	OutboundTimeout   time.Duration
	CanAcceptIncoming func(sessionID string) bool
	ResolveIPv4       func(context.Context, string) ([]net.IP, error)
	Logger            *slog.Logger
	Debug             bool
}

type SIPGoRuntime struct {
	cfg       SIPGoConfig
	ua        *sipgo.UserAgent
	clients   map[Transport]*sipgo.Client
	server    *sipgo.Server
	events    chan Event
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.RWMutex
	sessions  map[string]*sipgoSession
	sipOwners map[Identity]string
	contacts  map[string]string
	calls     map[string]*sipgoCall
	sipCallID map[string]string
	udpConn   net.PacketConn
	tcpListen net.Listener
	ready     atomic.Bool
	closeOnce sync.Once
	logger    *slog.Logger
}

var configureSIPGoLoggingOnce sync.Once

type sipgoSession struct {
	configuration  Configuration
	identity       Identity
	contact        sipmessage.ContactHeader
	contactUser    string
	registeredPeer string
	resolvedTarget string
	client         *sipgo.Client
	clientDialogs  *sipgo.DialogClientCache
	serverDialogs  *sipgo.DialogServerCache
	registered     bool
	registerCancel context.CancelFunc
}

type sipgoCall struct {
	mu                   sync.RWMutex
	inviteDone           chan struct{}
	inviteDoneOnce       sync.Once
	outboundDone         chan struct{}
	outboundDoneOnce     sync.Once
	outboundResolveOnce  sync.Once
	outboundTerminalOnce sync.Once
	sessionID            string
	callID               string
	sipCallID            string
	outboundContext      context.Context
	outbound             *sipgo.DialogClientSession
	inbound              *sipgo.DialogServerSession
	cancel               context.CancelCauseFunc
	answering            bool
	rejecting            bool
	connected            bool
	terminateAfterAnswer bool
	localSDP             []byte
	codec                string
	lastReliableRSeq     uint32
	reinviteCSeq         uint32
	reinviteAck          chan struct{}
	reinviteNeedsAnswer  bool
}

type sipRegisterResponseError struct {
	status int
}

func (e *sipRegisterResponseError) Error() string {
	return fmt.Sprintf("SIP REGISTER rejected with status %d", e.status)
}

const (
	sipOperationTimeout   = 10 * time.Second
	outboundAnswerTimeout = 60 * time.Second
	inboundInviteTimeout  = 2 * time.Minute
)

var errOutboundAnswerTimeout = errors.New("outbound answer timeout")

var (
	// ErrRegistrationNotReady lets the coordinator distinguish a stale
	// registration view from an adapter or transport failure.
	ErrRegistrationNotReady   = errors.New("SIP registration is not ready")
	errSIPSessionChanged      = errors.New("SIP session changed during operation")
	errSIPHostIPv4Unavailable = errors.New("SIP host has no usable IPv4 A record")
)

func NewSIPGoRuntime(cfg SIPGoConfig) (*SIPGoRuntime, error) {
	if cfg.ListenUDP == "" || cfg.ListenTCP == "" || net.ParseIP(cfg.AdvertisedIP) == nil {
		return nil, errors.New("invalid SIPGo listener configuration")
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "DebtFlowSipGateway"
	}
	if cfg.EventBuffer <= 0 {
		cfg.EventBuffer = 1024
	}
	if cfg.OutboundTimeout <= 0 {
		cfg.OutboundTimeout = outboundAnswerTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ResolveIPv4 == nil {
		cfg.ResolveIPv4 = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip4", host)
		}
	}
	udpConn, err := net.ListenPacket("udp", cfg.ListenUDP)
	if err != nil {
		return nil, fmt.Errorf("listen SIP UDP: %w", err)
	}
	tcpListener, err := net.Listen("tcp", cfg.ListenTCP)
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("listen SIP TCP: %w", err)
	}
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	configureSIPGoLoggingOnce.Do(func() {
		sipmessage.SIPDebug = cfg.Debug
		if cfg.Debug {
			sipmessage.SIPDebugTracer(&sanitizedSIPTracer{logger: cfg.Logger})
		}
		sipmessage.SetDefaultLogger(silent)
	})
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(cfg.UserAgent), sipgo.WithUserAgentHostname(cfg.AdvertisedIP),
	)
	if err != nil {
		_ = udpConn.Close()
		_ = tcpListener.Close()
		return nil, err
	}
	clients := make(map[Transport]*sipgo.Client, 2)
	for transport, listenAddress := range map[Transport]string{
		TransportUDP: cfg.ListenUDP, TransportTCP: cfg.ListenTCP,
	} {
		_, portText, splitErr := net.SplitHostPort(listenAddress)
		if splitErr != nil {
			_ = udpConn.Close()
			_ = tcpListener.Close()
			_ = ua.Close()
			return nil, splitErr
		}
		port, conversionErr := strconv.Atoi(portText)
		if conversionErr != nil {
			_ = udpConn.Close()
			_ = tcpListener.Close()
			_ = ua.Close()
			return nil, conversionErr
		}
		client, clientErr := sipgo.NewClient(ua,
			sipgo.WithClientHostname(cfg.AdvertisedIP), sipgo.WithClientPort(port),
			sipgo.WithClientLogger(silent),
		)
		if clientErr != nil {
			_ = udpConn.Close()
			_ = tcpListener.Close()
			for _, existing := range clients {
				_ = existing.Close()
			}
			_ = ua.Close()
			return nil, clientErr
		}
		clients[transport] = client
	}
	server, err := sipgo.NewServer(ua, sipgo.WithServerLogger(silent))
	if err != nil {
		_ = udpConn.Close()
		_ = tcpListener.Close()
		for _, client := range clients {
			_ = client.Close()
		}
		_ = ua.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &SIPGoRuntime{
		cfg: cfg, ua: ua, clients: clients, server: server, events: make(chan Event, cfg.EventBuffer),
		ctx: ctx, cancel: cancel, sessions: make(map[string]*sipgoSession),
		sipOwners: make(map[Identity]string), contacts: make(map[string]string),
		calls: make(map[string]*sipgoCall), sipCallID: make(map[string]string),
		udpConn: udpConn, tcpListen: tcpListener,
		logger: cfg.Logger,
	}
	// Observe parsed transport messages after the transaction layer. This is a
	// safety net for a final INVITE response that reaches the socket but is not
	// delivered to DialogClientSession.WaitAnswer (for example, an upstream Via
	// rewrite that prevents transaction matching). The desktop Agent has the
	// same transport-level lifecycle fallback.
	ua.TransportLayer().OnMessage(runtime.observeTransportMessage)
	runtime.installHandlers()
	runtime.ready.Store(true)
	go runtime.serveUDP()
	go runtime.serveTCP()
	return runtime, nil
}

func (r *SIPGoRuntime) Events() <-chan Event { return r.events }

func (r *SIPGoRuntime) Ready() bool { return r.ready.Load() }

func (r *SIPGoRuntime) Configure(_ context.Context, sessionID string, configuration Configuration) error {
	if err := (AsteriskProfile{}).ValidateConfiguration(configuration); err != nil {
		return err
	}
	identity, err := IdentityOf(configuration)
	if err != nil {
		return err
	}
	configuration.Host = identity.Host
	configuration.Transport = identity.Transport
	configuration.Realm = identity.Realm
	contactUser := contactUserFor(sessionID, identity)
	contact, err := buildContact(r.cfg, configuration, contactUser)
	if err != nil {
		return err
	}
	client := r.clients[configuration.Transport]
	if client == nil {
		return errors.New("SIP transport client is unavailable")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ownerID := r.sipOwners[identity]; ownerID != "" && ownerID != sessionID {
		return errors.New("SIP identity is already owned by another session")
	}
	if existing := r.sessions[sessionID]; existing != nil {
		if existing.registerCancel != nil {
			existing.registerCancel()
		}
		if r.sipOwners[existing.identity] == sessionID {
			delete(r.sipOwners, existing.identity)
		}
		if r.contacts[existing.contactUser] == sessionID {
			delete(r.contacts, existing.contactUser)
		}
	}
	r.sessions[sessionID] = &sipgoSession{
		configuration: configuration, identity: identity, contact: contact, contactUser: contactUser, client: client,
		clientDialogs: sipgo.NewDialogClientCache(client, contact),
		serverDialogs: sipgo.NewDialogServerCache(client, contact),
	}
	r.sipOwners[identity] = sessionID
	r.contacts[contactUser] = sessionID
	return nil
}

func (r *SIPGoRuntime) Register(ctx context.Context, sessionID string) error {
	ctx, cancel := boundedSIPContext(ctx)
	defer cancel()
	session, err := r.session(sessionID)
	if err != nil {
		return err
	}
	r.emit(Event{Type: EventRegistering, SessionID: sessionID})
	if err := r.registerOnce(ctx, session, 120); err != nil {
		if _, current := r.setRegisteredIfCurrent(sessionID, session, false); !current {
			return errSIPSessionChanged
		}
		r.logRegistrationFailure("SIP registration failed", sessionID, err, 1)
		r.emit(Event{Type: EventRegistrationFailed, SessionID: sessionID, Reason: "register_failed"})
		r.startRegistrationLoop(sessionID, session, time.Second, true)
		return err
	}
	if _, current := r.setRegisteredIfCurrent(sessionID, session, true); !current {
		return errSIPSessionChanged
	}
	r.logger.Info("SIP registration succeeded", "sessionId", sessionID,
		"transport", session.configuration.Transport)
	r.emit(Event{Type: EventRegistered, SessionID: sessionID, StatusCode: 200})
	r.startRegistrationLoop(sessionID, session, 90*time.Second, false)
	return nil
}

func (r *SIPGoRuntime) Unregister(ctx context.Context, sessionID string) error {
	ctx, cancel := boundedSIPContext(ctx)
	defer cancel()
	session, err := r.session(sessionID)
	if err != nil {
		return nil
	}
	return r.unregisterSession(ctx, sessionID, session)
}

func (r *SIPGoRuntime) unregisterSession(
	ctx context.Context, sessionID string, session *sipgoSession,
) error {
	r.mu.Lock()
	if r.sessions[sessionID] != session {
		r.mu.Unlock()
		return errSIPSessionChanged
	}
	if session.registerCancel != nil {
		session.registerCancel()
		session.registerCancel = nil
	}
	r.mu.Unlock()
	err := r.registerOnce(ctx, session, 0)
	if _, current := r.setRegisteredIfCurrent(sessionID, session, false); !current {
		return errSIPSessionChanged
	}
	r.emit(Event{Type: EventUnregistered, SessionID: sessionID})
	return err
}

func (r *SIPGoRuntime) StartCall(ctx context.Context, sessionID, callID, destination string, localSDP []byte) error {
	ctx, operationCancel := boundedSIPContext(ctx)
	defer operationCancel()
	if len(localSDP) == 0 {
		return errors.New("local SIP SDP is required")
	}
	localMedia, err := ParseRemoteSDP(localSDP)
	if err != nil {
		return fmt.Errorf("invalid local SIP SDP: %w", err)
	}
	session, resolvedTarget, err := r.registeredSession(sessionID)
	if err != nil {
		return err
	}
	recipient, err := outboundRecipient(destination, session.configuration)
	if err != nil {
		return err
	}
	setURITransport(&recipient, session.configuration.Transport)
	fromParams := sipmessage.NewParams()
	fromParams.Add("tag", sipmessage.GenerateTagN(16))
	from := &sipmessage.FromHeader{
		Address: sipmessage.Uri{
			Scheme: "sip",
			User:   session.configuration.Username,
			Host:   session.configuration.Host,
		},
		Params: fromParams,
	}
	// Configure/Unregister may have replaced or invalidated the session while
	// the request was being prepared. Never originate an INVITE from a stale or
	// unregistered SIP account even when a coordinator has stale state.
	if !r.isCurrentRegisteredSession(sessionID, session) {
		return ErrRegistrationNotReady
	}
	callCtx, cancel := context.WithCancelCause(context.Background())
	invite := sipmessage.NewRequest(sipmessage.INVITE, recipient)
	invite.SetBody(localSDP)
	invite.SetTransport(strings.ToUpper(string(session.configuration.Transport)))
	invite.SetDestination(resolvedTarget)
	invite.AppendHeader(sipmessage.NewHeader("Content-Type", "application/sdp"))
	invite.AppendHeader(from)
	dialog, err := session.clientDialogs.WriteInvite(ctx, invite)
	if err != nil {
		cancel(err)
		return err
	}
	sipCallID := ""
	if header := dialog.InviteRequest.CallID(); header != nil {
		sipCallID = header.Value()
	}
	callValue := &sipgoCall{
		sessionID: sessionID, callID: callID, sipCallID: sipCallID,
		outboundContext: callCtx, outbound: dialog, cancel: cancel,
		outboundDone: make(chan struct{}), localSDP: append([]byte(nil), localSDP...),
		codec: localMedia.Codec,
	}
	r.mu.Lock()
	r.calls[callID] = callValue
	if sipCallID != "" {
		r.sipCallID[sipCallID] = callID
	}
	r.mu.Unlock()
	r.logger.Info("outbound SIP call started",
		"sessionId", sessionID, "callId", callID,
		"callRef", shortSIPCallRef(sipCallID))
	go r.runOutbound(callCtx, callValue, session.configuration)
	return nil
}

func (r *SIPGoRuntime) runOutbound(
	ctx context.Context,
	callValue *sipgoCall,
	configuration Configuration,
) {
	callRef := shortSIPCallRef(callValue.sipCallID)
	answerTimer := time.AfterFunc(r.cfg.OutboundTimeout, func() {
		// A SIP INVITE transaction stops its built-in Timer B after the first
		// provisional response. Without an application deadline, a lost final
		// response after 180 Ringing can therefore retain the call forever.
		if context.Cause(ctx) != nil || !callValue.claimOutboundResolution() {
			return
		}
		callValue.cancel(errOutboundAnswerTimeout)
		r.logger.Warn("outbound SIP call timed out while ringing",
			"sessionId", callValue.sessionID, "callId", callValue.callID,
			"callRef", callRef, "timeout", r.cfg.OutboundTimeout,
			"reason", "no_answer")
		// Publish and release the application call immediately. WaitAnswer may
		// remain alive briefly to complete the RFC 3261 CANCEL/487 handshake;
		// it performs the physical dialog cleanup when that wait finishes.
		r.terminalizeOutbound(callValue, Event{Type: EventFailed,
			SessionID: callValue.sessionID, CallID: callValue.callID,
			Reason: "no_answer"})
	})
	defer answerTimer.Stop()
	defer func() {
		if recover() == nil {
			return
		}
		// A malformed or unexpectedly closed transaction must fail only this call,
		// never the entire Windows service. Do not log the recovered value because
		// third-party parser panics can contain raw SIP headers or credentials.
		r.logger.Error("outbound SIP transaction recovered from panic",
			"sessionId", callValue.sessionID, "callId", callValue.callID,
			"callRef", callRef, "reason", "sip_transaction_panic")
		callValue.claimOutboundResolution()
		r.finishOutbound(callValue, Event{Type: EventFailed, SessionID: callValue.sessionID,
			CallID: callValue.callID, Reason: "sip_failed"})
	}()
	r.waitOutbound(ctx, callValue, configuration)
}

func (r *SIPGoRuntime) Answer(_ context.Context, sessionID, callID string, localSDP []byte) error {
	callValue, err := r.call(sessionID, callID)
	if err != nil || callValue.inbound == nil {
		return errors.New("inbound call not found")
	}
	if len(localSDP) == 0 {
		return errors.New("local SIP SDP is required")
	}
	localMedia, err := ParseRemoteSDP(localSDP)
	if err != nil {
		return fmt.Errorf("invalid local SIP SDP: %w", err)
	}
	if !callValue.beginAnswer() {
		return errors.New("inbound call answer is already in progress")
	}
	callValue.setLocalMedia(localSDP, localMedia.Codec)
	// sipgo waits for the ACK while RespondSDP retransmits the 200 response. Do
	// not run that wait on the WebSocket command reader: a slow/missing ACK must
	// not block session renewal, hangup, or the browser command result.
	go r.completeInboundAnswer(callValue, append([]byte(nil), localSDP...))
	return nil
}

func (r *SIPGoRuntime) completeInboundAnswer(callValue *sipgoCall, localSDP []byte) {
	err := callValue.inbound.RespondSDP(localSDP)
	callValue.completeInvite()
	terminate := callValue.finishAnswer(err == nil)
	if err != nil {
		kind := sipTransactionFailureKind(err)
		r.logger.Warn("inbound SIP call answer failed", "callId", callValue.callID,
			"reason", "sip_response_failed", "failureKind", kind)
		r.logger.Debug("inbound SIP call answer failure detail", "callId", callValue.callID,
			"failureKind", kind, "error", err)
		r.removeCallIfCurrent(callValue)
		if !terminate {
			r.emit(Event{Type: EventFailed, SessionID: callValue.sessionID,
				CallID: callValue.callID, Reason: "sip_answer_failed"})
		}
		return
	}
	if terminate {
		ctx, cancel := context.WithTimeout(context.Background(), sipOperationTimeout)
		err = callValue.inbound.Bye(ctx)
		cancel()
		if err != nil {
			r.logger.Warn("inbound SIP post-answer cleanup failed", "callId", callValue.callID,
				"reason", "sip_bye_failed", "failureKind", sipTransactionFailureKind(err))
		}
		r.removeCallIfCurrent(callValue)
		return
	}
	if !r.isCurrentCall(callValue) {
		return
	}
	r.logger.Info("inbound SIP call answered", "callId", callValue.callID,
		"sipStatusCode", sipmessage.StatusOK)
	r.emit(Event{Type: EventConnected, SessionID: callValue.sessionID,
		CallID: callValue.callID, StatusCode: sipmessage.StatusOK})
}

func (r *SIPGoRuntime) Reject(_ context.Context, sessionID, callID string, status int) error {
	callValue, err := r.call(sessionID, callID)
	if err != nil || callValue.inbound == nil {
		return errors.New("inbound call not found")
	}
	if status == 0 {
		status = sipmessage.StatusBusyHere
	}
	if !callValue.beginReject() {
		return errors.New("inbound call already has a final response in progress")
	}
	go r.completeInboundReject(callValue, status)
	return nil
}

func (r *SIPGoRuntime) completeInboundReject(callValue *sipgoCall, status int) {
	err := callValue.inbound.Respond(status, statusReason(status), nil)
	callValue.completeInvite()
	if errors.Is(err, sipmessage.ErrTransactionCanceled) {
		r.logger.Info("inbound SIP rejection superseded by remote cancel", "callId", callValue.callID,
			"sipStatusCode", status, "reason", "remote_cancel")
		err = nil
	} else if err != nil {
		r.logger.Warn("inbound SIP call rejection response failed", "callId", callValue.callID,
			"sipStatusCode", status, "reason", "sip_response_failed",
			"failureKind", sipTransactionFailureKind(err))
		r.logger.Debug("inbound SIP rejection failure detail", "callId", callValue.callID,
			"error", err)
	} else {
		r.logger.Info("inbound SIP rejection response sent", "callId", callValue.callID,
			"sipStatusCode", status)
	}
	r.removeCallIfCurrent(callValue)
}

func (r *SIPGoRuntime) Hangup(ctx context.Context, sessionID, callID string) error {
	ctx, operationCancel := boundedSIPContext(ctx)
	defer operationCancel()
	callValue, err := r.call(sessionID, callID)
	if err != nil {
		return nil
	}
	if callValue.outbound != nil {
		if callValue.isConnected() {
			err = callValue.outbound.Bye(ctx)
			r.removeCallIfCurrent(callValue)
			callValue.completeOutbound()
		} else {
			callValue.cancel(errors.New("local hangup"))
			select {
			case <-callValue.outboundDone:
				return nil
			case <-ctx.Done():
				return fmt.Errorf("wait for outbound SIP cancellation: %w", ctx.Err())
			}
		}
	} else if callValue.inbound != nil {
		if callValue.isConnected() {
			err = callValue.inbound.Bye(ctx)
		} else if answering, _ := callValue.markTerminationWhileAnswering(); answering {
			// RespondSDP owns the INVITE transaction until ACK. It will send BYE
			// immediately after that ACK instead of racing a 487 against the 200.
			return nil
		} else if callValue.beginReject() {
			go r.completeInboundReject(callValue, sipmessage.StatusRequestTerminated)
			return nil
		}
	}
	if callValue.outbound == nil {
		r.removeCall(callID)
	}
	return err
}

func (*SIPGoRuntime) SendRTP(context.Context, string, string, *rtp.Packet) error {
	return errors.New("PBX RTP transport is owned by the media bridge")
}

func (*SIPGoRuntime) SendDTMF(context.Context, string, string, byte) error {
	return errors.New("RFC 4733 transport is owned by the media bridge")
}

func (r *SIPGoRuntime) CloseSession(ctx context.Context, sessionID string) error {
	session, err := r.session(sessionID)
	if err != nil {
		return nil
	}
	r.mu.RLock()
	var calls []string
	for callID, callValue := range r.calls {
		if callValue.sessionID == sessionID {
			calls = append(calls, callID)
		}
	}
	r.mu.RUnlock()
	for _, callID := range calls {
		_ = r.Hangup(ctx, sessionID, callID)
	}
	_ = r.unregisterSession(ctx, sessionID, session)
	r.mu.Lock()
	if r.sessions[sessionID] == session {
		delete(r.sessions, sessionID)
		if r.sipOwners[session.identity] == sessionID {
			delete(r.sipOwners, session.identity)
		}
		if r.contacts[session.contactUser] == sessionID {
			delete(r.contacts, session.contactUser)
		}
	}
	r.mu.Unlock()
	return nil
}

func (r *SIPGoRuntime) Close() error {
	var result error
	r.closeOnce.Do(func() {
		r.ready.Store(false)
		r.cancel()
		_ = r.udpConn.Close()
		_ = r.tcpListen.Close()
		result = r.server.Close()
		for _, client := range r.clients {
			result = errors.Join(result, client.Close())
		}
		result = errors.Join(result, r.ua.Close())
	})
	return result
}

func (r *SIPGoRuntime) installHandlers() {
	r.server.OnInvite(r.onInvite)
	r.server.OnAck(r.onAck)
	r.server.OnCancel(r.onCancel)
	r.server.OnBye(r.onBye)
	r.server.OnPrack(r.onPrack)
	r.server.OnUpdate(r.onUpdate)
	r.server.OnOptions(func(request *sipmessage.Request, tx sipmessage.ServerTransaction) {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request, sipmessage.StatusOK, "OK", nil))
	})
	r.server.OnNoRoute(func(request *sipmessage.Request, tx sipmessage.ServerTransaction) {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request, sipmessage.StatusMethodNotAllowed, "Method Not Allowed", nil))
	})
}

func (r *SIPGoRuntime) observeTransportMessage(message sipmessage.Message) {
	response, ok := message.(*sipmessage.Response)
	if !ok || response == nil || response.StatusCode < 400 || response.StatusCode > 699 ||
		response.StatusCode == sipmessage.StatusUnauthorized ||
		response.StatusCode == sipmessage.StatusProxyAuthRequired {
		return
	}
	cseq := response.CSeq()
	if cseq == nil || cseq.MethodName != sipmessage.INVITE {
		return
	}
	callValue := r.callBySIPMessage(response)
	if callValue == nil || callValue.outbound == nil {
		return
	}
	callRef := sipMessageRef(response)
	// TransportLayer invokes observers synchronously. Never persist/publish from
	// its read loop or one slow consumer could stall SIP traffic for all sessions.
	go r.handleTransportFinalResponse(callValue, response.StatusCode, callRef)
}

func (r *SIPGoRuntime) handleTransportFinalResponse(callValue *sipgoCall, statusCode int, callRef string) {
	if callValue.outboundContext != nil && context.Cause(callValue.outboundContext) != nil ||
		!callValue.claimOutboundResolution() {
		return
	}
	reason := outboundFailureReason(statusCode, nil)
	r.logger.Info("transport-level outbound SIP final response received",
		"sessionId", callValue.sessionID, "callId", callValue.callID,
		"callRef", callRef, "sipStatusCode", statusCode,
		"reason", reason)
	// terminalizeOutbound removes the Call-ID mapping before cancelling the
	// waiter, so retransmissions cannot affect a subsequent call/session.
	r.terminalizeOutbound(callValue, Event{Type: EventFailed,
		SessionID: callValue.sessionID, CallID: callValue.callID,
		StatusCode: statusCode, Reason: reason})
}

func (r *SIPGoRuntime) onReinvite(
	callValue *sipgoCall,
	request *sipmessage.Request,
	tx sipmessage.ServerTransaction,
) {
	if unsupported := unsupportedRequiredExtensions(request); len(unsupported) > 0 {
		r.respondUnsupportedExtension(request, tx, unsupported)
		return
	}
	if !callValue.isConnected() {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request,
			sipmessage.StatusRequestPending, "Request Pending", nil))
		return
	}
	if err := r.readInDialogRequest(callValue, request, tx); err != nil {
		status := sipmessage.StatusCallTransactionDoesNotExists
		reason := "Call/Transaction Does Not Exist"
		if errors.Is(err, sipgo.ErrDialogInvalidCseq) {
			status = sipmessage.StatusInternalServerError
			reason = "Server Internal Error"
		}
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request, status, reason, nil))
		return
	}
	localSDP, codec := callValue.localMedia()
	if len(localSDP) == 0 || codec == "" {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request,
			sipmessage.StatusNotAcceptableHere, "Not Acceptable Here", nil))
		return
	}
	remoteSDP := append([]byte(nil), request.Body()...)
	if len(remoteSDP) > 0 {
		if _, err := ParseRemoteSDPForCodec(remoteSDP, codec); err != nil {
			_ = tx.Respond(sipmessage.NewResponseFromRequest(request,
				sipmessage.StatusNotAcceptableHere, "Not Acceptable Here", nil))
			return
		}
	}
	cseq := request.CSeq()
	if cseq == nil {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request,
			sipmessage.StatusBadRequest, "Bad Request", nil))
		return
	}
	ack, accepted := callValue.beginReinvite(cseq.SeqNo, len(remoteSDP) == 0)
	if !accepted {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request,
			sipmessage.StatusRequestPending, "Request Pending", nil))
		return
	}
	response := sipmessage.NewSDPResponseFromRequest(request, localSDP)
	if session, err := r.session(callValue.sessionID); err == nil && response.Contact() == nil {
		response.AppendHeader(sipmessage.HeaderClone(&session.contact))
	}
	if err := tx.Respond(response); err != nil {
		callValue.finishReinvite(cseq.SeqNo)
		r.logger.Warn("in-dialog SIP re-INVITE response failed", "callId", callValue.callID,
			"reason", "sip_response_failed", "failureKind", sipTransactionFailureKind(err))
		return
	}
	if len(remoteSDP) > 0 {
		r.emit(Event{Type: EventMediaUpdated, SessionID: callValue.sessionID,
			CallID: callValue.callID, StatusCode: sipmessage.StatusOK, RemoteSDP: remoteSDP})
	}
	r.waitReinviteAck(callValue, cseq.SeqNo, ack, tx, response)
}

func (r *SIPGoRuntime) waitReinviteAck(
	callValue *sipgoCall,
	cseq uint32,
	ack <-chan struct{},
	tx sipmessage.ServerTransaction,
	response *sipmessage.Response,
) {
	interval := sipmessage.T1
	retransmit := time.NewTimer(interval)
	deadline := time.NewTimer(64 * sipmessage.T1)
	defer retransmit.Stop()
	defer deadline.Stop()
	defer callValue.finishReinvite(cseq)
	for {
		select {
		case <-ack:
			return
		case <-retransmit.C:
			if err := tx.Respond(response); err != nil {
				r.logger.Warn("in-dialog SIP re-INVITE retransmission failed",
					"callId", callValue.callID, "reason", "sip_response_failed",
					"failureKind", sipTransactionFailureKind(err))
				return
			}
			interval *= 2
			if interval > sipmessage.T2 {
				interval = sipmessage.T2
			}
			retransmit.Reset(interval)
		case <-deadline.C:
			r.logger.Warn("in-dialog SIP re-INVITE ACK timed out", "callId", callValue.callID,
				"reason", "ack_missing")
			r.emit(Event{Type: EventFailed, SessionID: callValue.sessionID,
				CallID: callValue.callID, Reason: "media_failed"})
			return
		case <-r.ctx.Done():
			return
		}
	}
}

func (r *SIPGoRuntime) onUpdate(request *sipmessage.Request, tx sipmessage.ServerTransaction) {
	callValue := r.callBySIPRequest(request)
	if callValue == nil || !requestHasDialogTag(request) {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request,
			sipmessage.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
		return
	}
	if unsupported := unsupportedRequiredExtensions(request); len(unsupported) > 0 {
		r.respondUnsupportedExtension(request, tx, unsupported)
		return
	}
	if !callValue.isConnected() {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request,
			sipmessage.StatusRequestPending, "Request Pending", nil))
		return
	}
	if err := r.readInDialogRequest(callValue, request, tx); err != nil {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request,
			sipmessage.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
		return
	}
	remoteSDP := append([]byte(nil), request.Body()...)
	localSDP, codec := callValue.localMedia()
	if len(remoteSDP) > 0 {
		if len(localSDP) == 0 {
			_ = tx.Respond(sipmessage.NewResponseFromRequest(request,
				sipmessage.StatusNotAcceptableHere, "Not Acceptable Here", nil))
			return
		}
		if _, err := ParseRemoteSDPForCodec(remoteSDP, codec); err != nil {
			_ = tx.Respond(sipmessage.NewResponseFromRequest(request,
				sipmessage.StatusNotAcceptableHere, "Not Acceptable Here", nil))
			return
		}
	}
	var response *sipmessage.Response
	if len(remoteSDP) > 0 {
		response = sipmessage.NewSDPResponseFromRequest(request, localSDP)
	} else {
		response = sipmessage.NewResponseFromRequest(request, sipmessage.StatusOK, "OK", nil)
	}
	if session, err := r.session(callValue.sessionID); err == nil && response.Contact() == nil {
		response.AppendHeader(sipmessage.HeaderClone(&session.contact))
	}
	if err := tx.Respond(response); err != nil {
		return
	}
	if len(remoteSDP) > 0 {
		r.emit(Event{Type: EventMediaUpdated, SessionID: callValue.sessionID,
			CallID: callValue.callID, StatusCode: sipmessage.StatusOK, RemoteSDP: remoteSDP})
	}
}

func (r *SIPGoRuntime) onPrack(request *sipmessage.Request, tx sipmessage.ServerTransaction) {
	// The gateway never emits reliable provisional responses as a UAS. A PRACK
	// therefore cannot match an outstanding reliable response.
	_ = tx.Respond(sipmessage.NewResponseFromRequest(request,
		sipmessage.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
}

func (r *SIPGoRuntime) readInDialogRequest(
	callValue *sipgoCall,
	request *sipmessage.Request,
	tx sipmessage.ServerTransaction,
) error {
	session, err := r.session(callValue.sessionID)
	if err != nil {
		return err
	}
	if callValue.inbound != nil {
		dialog, err := session.serverDialogs.MatchDialogRequest(request)
		if err != nil || dialog != callValue.inbound {
			return errors.Join(sipgo.ErrDialogDoesNotExists, err)
		}
		return dialog.ReadRequest(request, tx)
	}
	if callValue.outbound != nil {
		dialog, err := session.clientDialogs.MatchRequestDialog(request)
		if err != nil || dialog != callValue.outbound {
			return errors.Join(sipgo.ErrDialogDoesNotExists, err)
		}
		return dialog.ReadRequest(request, tx)
	}
	return sipgo.ErrDialogDoesNotExists
}

func (r *SIPGoRuntime) respondUnsupportedExtension(
	request *sipmessage.Request,
	tx sipmessage.ServerTransaction,
	unsupported []string,
) {
	response := sipmessage.NewResponseFromRequest(request,
		sipmessage.StatusBadExtension, "Bad Extension", nil)
	response.AppendHeader(sipmessage.NewHeader("Unsupported", strings.Join(unsupported, ", ")))
	_ = tx.Respond(response)
}

func unsupportedRequiredExtensions(message sipmessage.Message) []string {
	var unsupported []string
	seen := make(map[string]struct{})
	for _, header := range message.GetHeaders("Require") {
		for _, value := range strings.Split(header.Value(), ",") {
			token := strings.ToLower(strings.TrimSpace(value))
			if token == "" {
				continue
			}
			if _, duplicate := seen[token]; duplicate {
				continue
			}
			seen[token] = struct{}{}
			unsupported = append(unsupported, token)
		}
	}
	return unsupported
}

func requestHasDialogTag(request *sipmessage.Request) bool {
	to := request.To()
	return to != nil && to.Params.Has("tag")
}

func (r *SIPGoRuntime) onInvite(request *sipmessage.Request, tx sipmessage.ServerTransaction) {
	callRef := sipRequestRef(request)
	r.logger.Debug("inbound SIP INVITE received", "callRef", callRef)
	if callValue := r.callBySIPRequest(request); callValue != nil && requestHasDialogTag(request) {
		r.onReinvite(callValue, request, tx)
		return
	}
	if requestHasDialogTag(request) {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request,
			sipmessage.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
		return
	}
	if unsupported := unsupportedRequiredExtensions(request); len(unsupported) > 0 {
		r.respondUnsupportedExtension(request, tx, unsupported)
		return
	}
	sessionID, session, registered := r.sessionForInboundRequest(request)
	if session == nil || !registered {
		r.logInboundRejection(callRef, "registration_not_ready", sipmessage.StatusTemporarilyUnavailable)
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request, sipmessage.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil))
		return
	}
	if r.cfg.CanAcceptIncoming != nil && !r.cfg.CanAcceptIncoming(sessionID) {
		r.logInboundRejection(callRef, "browser_session_or_webrtc_not_ready", sipmessage.StatusTemporarilyUnavailable)
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request, sipmessage.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil))
		return
	}
	dialog, err := session.serverDialogs.ReadInvite(request, tx)
	if err != nil {
		r.logInboundRejection(callRef, "invalid_sip_dialog", sipmessage.StatusBadRequest)
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request, sipmessage.StatusBadRequest, "Bad Request", nil))
		return
	}
	callID := uuid.NewString()
	sipCallID := ""
	if header := request.CallID(); header != nil {
		sipCallID = header.Value()
	}
	callValue := &sipgoCall{
		sessionID: sessionID, callID: callID, sipCallID: sipCallID, inbound: dialog,
		inviteDone: make(chan struct{}),
	}
	r.mu.Lock()
	for _, active := range r.calls {
		if active.sessionID == sessionID {
			r.mu.Unlock()
			r.logInboundRejection(callRef, "active_call_exists", sipmessage.StatusBusyHere)
			_ = dialog.Respond(sipmessage.StatusBusyHere, "Busy Here", nil)
			return
		}
	}
	r.calls[callID] = callValue
	if sipCallID != "" {
		r.sipCallID[sipCallID] = callID
	}
	r.mu.Unlock()
	if err := dialog.Respond(sipmessage.StatusTrying, "Trying", nil); err != nil {
		r.logInboundResponseFailure(callValue, "trying_response_failed", err)
		r.removeCall(callID)
		return
	}
	if err := dialog.Respond(sipmessage.StatusRinging, "Ringing", nil); err != nil {
		r.logInboundResponseFailure(callValue, "ringing_response_failed", err)
		r.removeCall(callID)
		return
	}
	remoteParty := "unknown"
	if from := request.From(); from != nil && from.Address.User != "" {
		remoteParty = normalizeRemoteParty(from.Address.User)
	}
	r.emit(Event{
		Type: EventIncoming, SessionID: sessionID, CallID: callID,
		StatusCode: sipmessage.StatusRinging, RemoteSDP: append([]byte(nil), request.Body()...),
		RemoteParty: remoteParty,
	})
	r.logger.Info("inbound SIP call ringing", "callId", callID, "callRef", callRef,
		"sessionId", sessionID, "sipStatusCode", sipmessage.StatusRinging)
	// sipgo consumes a matching CANCEL inside the INVITE server transaction, so
	// Server.OnCancel is not invoked for the normal RFC 3261 cancellation path.
	// Register on the INVITE transaction after publishing EventIncoming to keep
	// the gateway lifecycle ordered even when CANCEL races the 180 response.
	if registered := tx.OnCancel(func(*sipmessage.Request) {
		r.handleInviteCancellation(callValue)
	}); !registered {
		// A CANCEL may have reached the transaction immediately before callback
		// registration. The state guards make this fallback idempotent with a
		// concurrently executing callback.
		r.handleInviteCancellation(callValue)
	}
	// sipgo terminates the server transaction when this callback returns. Keep
	// the INVITE callback alive until answer/reject/CANCEL has sent its final
	// response; otherwise a later browser call.answer always targets a closed
	// transaction and fails before it can send 200 OK.
	ringTimer := time.NewTimer(inboundInviteTimeout)
	defer ringTimer.Stop()
	select {
	case <-callValue.inviteDone:
	case <-ringTimer.C:
		if callValue.beginReject() {
			r.logger.Info("inbound SIP call timed out while ringing", "callId", callID,
				"reason", "no_answer")
			r.emit(Event{Type: EventEnded, SessionID: sessionID, CallID: callID,
				Reason: "no_answer"})
			r.completeInboundReject(callValue, sipmessage.StatusTemporarilyUnavailable)
		}
	case <-r.ctx.Done():
	}
}

func normalizeRemoteParty(value string) string {
	if value == "" || len(value) > 32 {
		return "unknown"
	}
	for _, char := range value {
		if (char < '0' || char > '9') && char != '+' && char != '*' && char != '#' {
			return "unknown"
		}
	}
	return value
}

func (r *SIPGoRuntime) onAck(request *sipmessage.Request, tx sipmessage.ServerTransaction) {
	callValue := r.callBySIPRequest(request)
	if callValue == nil {
		return
	}
	if cseq := request.CSeq(); cseq != nil {
		matched, needsAnswer, codec := callValue.ackReinvite(cseq.SeqNo)
		if matched {
			remoteSDP := append([]byte(nil), request.Body()...)
			if needsAnswer && len(remoteSDP) == 0 {
				r.logger.Warn("offerless SIP re-INVITE ACK omitted SDP answer",
					"callId", callValue.callID, "reason", "invalid_sdp")
				r.emit(Event{Type: EventFailed, SessionID: callValue.sessionID,
					CallID: callValue.callID, Reason: "media_failed"})
			} else if len(remoteSDP) > 0 {
				if _, err := ParseRemoteSDPForCodec(remoteSDP, codec); err != nil {
					r.logger.Warn("SIP re-INVITE ACK SDP rejected", "callId", callValue.callID,
						"reason", "invalid_sdp")
					r.emit(Event{Type: EventFailed, SessionID: callValue.sessionID,
						CallID: callValue.callID, Reason: "media_failed"})
				} else {
					r.emit(Event{Type: EventMediaUpdated, SessionID: callValue.sessionID,
						CallID: callValue.callID, RemoteSDP: remoteSDP})
				}
			}
		}
	}
	if callValue.inbound == nil {
		return
	}
	if err := callValue.inbound.ReadAck(request, tx); err != nil {
		r.logger.Warn("inbound SIP ACK rejected", "callId", callValue.callID,
			"reason", "invalid_ack", "failureKind", sipTransactionFailureKind(err))
		r.logger.Debug("inbound SIP ACK failure detail", "callId", callValue.callID,
			"error", err)
	}
}

func (r *SIPGoRuntime) onCancel(request *sipmessage.Request, tx sipmessage.ServerTransaction) {
	// Matching CANCEL requests are handled by the INVITE transaction callback
	// registered in onInvite. Reaching this handler means no pending INVITE
	// transaction matched the CANCEL branch, which RFC 3261 answers with 481.
	_ = tx.Respond(sipmessage.NewResponseFromRequest(request,
		sipmessage.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
}

func (r *SIPGoRuntime) handleInviteCancellation(callValue *sipgoCall) {
	if callValue.isConnected() {
		// CANCEL has no effect after a final 2xx response; the remote endpoint
		// must ACK and use BYE to terminate the established dialog.
		r.logger.Debug("late inbound SIP CANCEL ignored", "callId", callValue.callID)
		return
	}
	if answering, first := callValue.markTerminationWhileAnswering(); answering {
		if first {
			r.logger.Info("inbound SIP call cancelled while answer was in progress",
				"callId", callValue.callID, "reason", "remote_cancel")
			r.emit(Event{Type: EventEnded, SessionID: callValue.sessionID,
				CallID: callValue.callID, Reason: "cancelled"})
		}
		return
	}
	if callValue.beginReject() {
		r.logger.Info("inbound SIP call cancelled by remote", "callId", callValue.callID,
			"reason", "remote_cancel")
		r.emit(Event{Type: EventEnded, SessionID: callValue.sessionID,
			CallID: callValue.callID, Reason: "cancelled"})
		// sipgo has already sent 200 for CANCEL and 487 for the INVITE. Do not
		// issue a second final response from the application layer.
		r.removeCallIfCurrent(callValue)
	}
}

func (r *SIPGoRuntime) onBye(request *sipmessage.Request, tx sipmessage.ServerTransaction) {
	callValue := r.callBySIPRequest(request)
	if callValue == nil {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request, sipmessage.StatusCallTransactionDoesNotExists, "Call Does Not Exist", nil))
		return
	}
	var err error
	if callValue.inbound != nil {
		err = callValue.inbound.ReadBye(request, tx)
	} else {
		session, sessionErr := r.session(callValue.sessionID)
		if sessionErr == nil {
			err = session.clientDialogs.ReadBye(request, tx)
		}
	}
	if err != nil {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request, sipmessage.StatusBadRequest, "Bad Request", nil))
		return
	}
	r.emit(Event{Type: EventEnded, SessionID: callValue.sessionID, CallID: callValue.callID, Reason: "remote_hangup"})
	if callValue.inbound != nil {
		r.logger.Info("inbound SIP call ended", "callId", callValue.callID,
			"reason", "remote_hangup")
	}
	r.removeCall(callValue.callID)
	callValue.completeOutbound()
}

func (r *SIPGoRuntime) logInboundRejection(callRef, reason string, status int) {
	r.logger.Warn("inbound SIP call rejected", "callRef", callRef,
		"reason", reason, "sipStatusCode", status)
}

func sipRequestRef(request *sipmessage.Request) string {
	header := request.CallID()
	if header == nil || header.Value() == "" {
		return "unavailable"
	}
	return shortSIPCallRef(header.Value())
}

func sipTransactionFailureKind(err error) string {
	switch {
	case errors.Is(err, sipmessage.ErrTransactionCanceled):
		return "cancelled"
	case errors.Is(err, sipmessage.ErrTransactionTimeout):
		return "timeout"
	case errors.Is(err, sipmessage.ErrTransactionTransport):
		return "transport"
	case errors.Is(err, sipmessage.ErrTransactionTerminated):
		return "terminated"
	case strings.Contains(err.Error(), "ID do not match"):
		return "dialog_id_mismatch"
	case strings.Contains(err.Error(), "No ACK received"):
		return "ack_missing"
	case strings.Contains(err.Error(), "missing tag param"):
		return "dialog_tag_missing"
	default:
		return "other"
	}
}

func (r *SIPGoRuntime) waitOutbound(ctx context.Context, callValue *sipgoCall, configuration Configuration) {
	callRef := shortSIPCallRef(callValue.sipCallID)
	err := callValue.outbound.WaitAnswer(ctx, sipgo.AnswerOptions{
		Username: configuration.Username, Password: configuration.Password,
		OnResponse: func(response *sipmessage.Response) error {
			if response == nil {
				return errors.New("SIP transaction returned an empty response")
			}
			if response.StatusCode > sipmessage.StatusTrying && response.IsProvisional() &&
				messageHasHeaderToken(response, "Require", "100rel") {
				if err := r.sendReliableProvisionalACK(ctx, callValue, response); err != nil {
					return err
				}
			}
			if response.StatusCode == sipmessage.StatusTrying {
				r.logger.Debug("outbound SIP provisional response",
					"sessionId", callValue.sessionID, "callId", callValue.callID,
					"callRef", callRef,
					"sipStatusCode", response.StatusCode)
				r.emit(Event{Type: EventTrying, SessionID: callValue.sessionID, CallID: callValue.callID, StatusCode: response.StatusCode})
			} else if response.IsProvisional() {
				hasEarlyMedia := len(response.Body()) > 0
				r.logger.Info("outbound SIP provisional response received",
					"sessionId", callValue.sessionID, "callId", callValue.callID,
					"callRef", callRef,
					"sipStatusCode", response.StatusCode,
					"earlyMedia", hasEarlyMedia)
				if !hasEarlyMedia {
					r.emit(Event{Type: EventRinging, SessionID: callValue.sessionID,
						CallID: callValue.callID, StatusCode: response.StatusCode})
					return nil
				}
				r.emit(Event{Type: EventEarlyMedia, SessionID: callValue.sessionID,
					CallID: callValue.callID, StatusCode: response.StatusCode,
					RemoteSDP: append([]byte(nil), response.Body()...)})
			} else {
				r.logger.Info("outbound SIP final response received",
					"sessionId", callValue.sessionID, "callId", callValue.callID,
					"callRef", callRef,
					"sipStatusCode", response.StatusCode)
			}
			return nil
		},
	})
	if !callValue.claimOutboundResolution() {
		// The application timeout already released this call. A success response
		// racing that deadline still needs ACK + BYE so the PBX cannot retain a
		// half-open dialog, but it must not resurrect the browser call state.
		if callValue.outbound.InviteResponse != nil && callValue.outbound.InviteResponse.IsSuccess() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), sipOperationTimeout)
			_ = callValue.outbound.Ack(cleanupCtx)
			_ = callValue.outbound.Bye(cleanupCtx)
			cleanupCancel()
		}
		r.cleanupOutbound(callValue)
		return
	}
	if err != nil {
		if context.Cause(ctx) != nil {
			// RFC 3261 requires a late 2xx response after local CANCEL to be
			// acknowledged and then terminated with BYE.
			if response := callValue.outbound.InviteResponse; response != nil && response.IsSuccess() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), sipOperationTimeout)
				_ = callValue.outbound.Ack(cleanupCtx)
				_ = callValue.outbound.Bye(cleanupCtx)
				cleanupCancel()
			}
			r.finishOutbound(callValue, Event{Type: EventEnded, SessionID: callValue.sessionID,
				CallID: callValue.callID, Reason: "cancelled"})
			return
		}
		status := 0
		var responseError *sipgo.ErrDialogResponse
		if errors.As(err, &responseError) && responseError.Res != nil {
			status = responseError.Res.StatusCode
		}
		reason := outboundFailureReason(status, err)
		r.logger.Info("outbound SIP call ended before answer",
			"sessionId", callValue.sessionID, "callId", callValue.callID,
			"callRef", callRef,
			"sipStatusCode", status, "reason", reason,
			"failureKind", sipTransactionFailureKind(err))
		r.finishOutbound(callValue, Event{Type: EventFailed, SessionID: callValue.sessionID,
			CallID: callValue.callID, StatusCode: status,
			Reason: reason})
		return
	}
	ackCtx, ackCancel := context.WithTimeout(context.Background(), sipOperationTimeout)
	err = callValue.outbound.Ack(ackCtx)
	ackCancel()
	if err != nil {
		r.finishOutbound(callValue, Event{Type: EventFailed, SessionID: callValue.sessionID,
			CallID: callValue.callID, Reason: "ack_failed"})
		return
	}
	if context.Cause(ctx) != nil {
		byeCtx, byeCancel := context.WithTimeout(context.Background(), sipOperationTimeout)
		_ = callValue.outbound.Bye(byeCtx)
		byeCancel()
		r.finishOutbound(callValue, Event{Type: EventEnded, SessionID: callValue.sessionID,
			CallID: callValue.callID, Reason: "cancelled"})
		return
	}
	callValue.setConnected()
	r.logger.Info("outbound SIP call answered",
		"sessionId", callValue.sessionID, "callId", callValue.callID,
		"callRef", callRef,
		"sipStatusCode", callValue.outbound.InviteResponse.StatusCode)
	r.emit(Event{
		Type: EventConnected, SessionID: callValue.sessionID, CallID: callValue.callID,
		StatusCode: callValue.outbound.InviteResponse.StatusCode,
		RemoteSDP:  append([]byte(nil), callValue.outbound.InviteResponse.Body()...),
	})
}

func (r *SIPGoRuntime) sendReliableProvisionalACK(
	parent context.Context,
	callValue *sipgoCall,
	response *sipmessage.Response,
) error {
	rseqHeader := response.GetHeader("RSeq")
	inviteCSeq := callValue.outbound.InviteRequest.CSeq()
	if rseqHeader == nil || inviteCSeq == nil {
		return errors.New("reliable provisional response omitted RSeq or INVITE CSeq")
	}
	rseqValue, err := strconv.ParseUint(strings.TrimSpace(rseqHeader.Value()), 10, 32)
	if err != nil || rseqValue == 0 {
		return errors.New("reliable provisional response has invalid RSeq")
	}
	if !callValue.claimReliableProvisional(uint32(rseqValue)) {
		return nil
	}
	recipient := callValue.outbound.InviteRequest.Recipient
	if contact := response.Contact(); contact != nil {
		recipient = contact.Address
	}
	prack := sipmessage.NewRequest(sipmessage.PRACK, recipient)
	prack.AppendHeader(sipmessage.NewHeader("RAck", fmt.Sprintf("%d %d INVITE",
		rseqValue, inviteCSeq.SeqNo)))
	ctx, cancel := boundedSIPContext(parent)
	defer cancel()
	result, err := callValue.outbound.Do(ctx, prack)
	if err != nil {
		return fmt.Errorf("send PRACK: %w", err)
	}
	if result == nil || !result.IsSuccess() {
		status := 0
		if result != nil {
			status = result.StatusCode
		}
		return fmt.Errorf("PRACK rejected with status %d", status)
	}
	r.logger.Debug("outbound reliable provisional response acknowledged",
		"sessionId", callValue.sessionID, "callId", callValue.callID,
		"sipStatusCode", response.StatusCode)
	return nil
}

func messageHasHeaderToken(message sipmessage.Message, name, expected string) bool {
	expected = strings.ToLower(strings.TrimSpace(expected))
	for _, header := range message.GetHeaders(name) {
		for _, value := range strings.Split(header.Value(), ",") {
			if strings.ToLower(strings.TrimSpace(value)) == expected {
				return true
			}
		}
	}
	return false
}

func outboundFailureReason(status int, err error) string {
	if status != 0 {
		return (AsteriskProfile{}).NormalizeFailure(status)
	}
	switch sipTransactionFailureKind(err) {
	case "timeout":
		return "no_answer"
	case "transport":
		return "provider_unavailable"
	case "cancelled":
		return "cancelled"
	default:
		return "sip_failed"
	}
}

func (r *SIPGoRuntime) finishOutbound(callValue *sipgoCall, event Event) {
	r.terminalizeOutbound(callValue, event)
	r.cleanupOutbound(callValue)
}

func (r *SIPGoRuntime) terminalizeOutbound(callValue *sipgoCall, event Event) {
	callValue.outboundTerminalOnce.Do(func() {
		r.removeCallIfCurrent(callValue)
		// Releasing the application call must not depend on the event consumer
		// keeping up. The coordinator persists the terminal event durably.
		r.emit(event)
	})
}

func (r *SIPGoRuntime) cleanupOutbound(callValue *sipgoCall) {
	if callValue.outbound != nil {
		_ = callValue.outbound.Close()
	}
	callValue.completeOutbound()
}

func (r *SIPGoRuntime) registerOnce(ctx context.Context, session *sipgoSession, expires uint32) error {
	cfg := session.configuration
	resolvedTarget, err := r.resolveSessionTarget(ctx, session, expires != 0)
	if err != nil {
		return err
	}
	recipient := sipmessage.Uri{User: cfg.Username, Host: cfg.Host, Port: int(cfg.Port)}
	request := sipmessage.NewRequest(sipmessage.REGISTER, recipient)
	request.AppendHeader(&session.contact)
	expiresHeader := sipmessage.ExpiresHeader(expires)
	request.AppendHeader(&expiresHeader)
	request.SetTransport(strings.ToUpper(string(cfg.Transport)))
	request.SetDestination(resolvedTarget)
	transaction, err := session.client.TransactionRequest(ctx, request, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return err
	}
	defer transaction.Terminate()
	response, err := responseFromTransaction(ctx, transaction)
	if err != nil {
		return err
	}
	if response.StatusCode == sipmessage.StatusUnauthorized || response.StatusCode == sipmessage.StatusProxyAuthRequired {
		headerName := "WWW-Authenticate"
		authorizationName := "Authorization"
		if response.StatusCode == sipmessage.StatusProxyAuthRequired {
			headerName = "Proxy-Authenticate"
			authorizationName = "Proxy-Authorization"
		}
		challengeHeader := response.GetHeader(headerName)
		if challengeHeader == nil {
			return errors.New("SIP authentication challenge is missing")
		}
		challenge, err := digest.ParseChallenge(challengeHeader.Value())
		if err != nil {
			return errors.New("SIP authentication challenge is malformed")
		}
		credential, err := digest.Digest(challenge, digest.Options{
			Method: sipmessage.REGISTER.String(), URI: recipient.Addr(),
			Username: cfg.Username, Password: cfg.Password,
		})
		if err != nil {
			return err
		}
		authenticated := request.Clone()
		authenticated.RemoveHeader("Via")
		authenticated.AppendHeader(sipmessage.NewHeader(authorizationName, credential.String()))
		transaction, err = session.client.TransactionRequest(ctx, authenticated,
			sipgo.ClientRequestIncreaseCSEQ, sipgo.ClientRequestAddVia,
		)
		if err != nil {
			return err
		}
		defer transaction.Terminate()
		response, err = responseFromTransaction(ctx, transaction)
		if err != nil {
			return err
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &sipRegisterResponseError{status: response.StatusCode}
	}
	r.mu.Lock()
	session.registeredPeer = canonicalPeerAddress(response.Source())
	r.mu.Unlock()
	return nil
}

func buildContact(cfg SIPGoConfig, configuration Configuration, contactUsers ...string) (sipmessage.ContactHeader, error) {
	listenAddress := cfg.ListenUDP
	if configuration.Transport == TransportTCP {
		listenAddress = cfg.ListenTCP
	}
	_, portText, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return sipmessage.ContactHeader{}, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return sipmessage.ContactHeader{}, err
	}
	contactUser := configuration.Username
	if len(contactUsers) > 0 && contactUsers[0] != "" {
		contactUser = contactUsers[0]
	}
	address := sipmessage.Uri{User: contactUser, Host: cfg.AdvertisedIP, Port: port}
	setURITransport(&address, configuration.Transport)
	return sipmessage.ContactHeader{Address: address}, nil
}

func outboundRecipient(destination string, configuration Configuration) (sipmessage.Uri, error) {
	value := destination
	lower := strings.ToLower(value)
	if !strings.HasPrefix(lower, "sip:") && !strings.HasPrefix(lower, "sips:") {
		if !isCanonicalDialDestination(value) {
			return sipmessage.Uri{}, errors.New("canonical destination is invalid")
		}
		value = "sip:" + value + "@" + net.JoinHostPort(
			configuration.Host, strconv.Itoa(int(configuration.Port)),
		)
	}
	var recipient sipmessage.Uri
	if err := sipmessage.ParseUri(value, &recipient); err != nil {
		return sipmessage.Uri{}, errors.New("canonical destination is not a valid SIP URI")
	}
	return recipient, nil
}

func isCanonicalDialDestination(value string) bool {
	digits := 0
	for index, character := range value {
		switch {
		case character >= '0' && character <= '9':
			digits++
		case character == '*' || character == '#':
			digits++
		case character == '+' && index == 0:
		default:
			return false
		}
	}
	return digits >= 1 && digits <= 32
}

func setURITransport(uri *sipmessage.Uri, transport Transport) {
	if uri.UriParams == nil {
		uri.UriParams = sipmessage.NewParams()
	}
	uri.UriParams.Add("transport", string(transport))
}

func (r *SIPGoRuntime) startRegistrationLoop(
	sessionID string, session *sipgoSession, firstDelay time.Duration, retrying bool,
) {
	r.mu.Lock()
	if r.sessions[sessionID] != session {
		r.mu.Unlock()
		return
	}
	if session.registerCancel != nil {
		session.registerCancel()
	}
	refreshCtx, cancel := context.WithCancel(r.ctx)
	session.registerCancel = cancel
	r.mu.Unlock()
	go r.refreshRegistration(refreshCtx, sessionID, session, firstDelay, retrying)
}

func (r *SIPGoRuntime) refreshRegistration(
	ctx context.Context, sessionID string, session *sipgoSession,
	delay time.Duration, retrying bool,
) {
	failures := 0
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
		if !r.isCurrentSession(sessionID, session) {
			return
		}
		if retrying {
			r.emit(Event{Type: EventRegistrationRetrying, SessionID: sessionID})
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := r.registerOnce(refreshCtx, session, 120)
		cancel()
		if err != nil {
			failures++
			wasRetrying := retrying
			retrying = true
			if _, current := r.setRegisteredIfCurrent(sessionID, session, false); !current {
				return
			}
			// The first failed refresh must be authoritative immediately. The
			// coordinator must stop calls instead of believing the account is
			// still registered until the fifth retry fails.
			if !wasRetrying || failures%5 == 0 {
				r.logRegistrationFailure("SIP registration refresh failed", sessionID, err, failures)
				r.emit(Event{Type: EventRegistrationFailed, SessionID: sessionID, Reason: "refresh_failed"})
			}
			delay = registrationRetryDelay(failures)
		} else {
			wasRetrying := retrying
			failures = 0
			retrying = false
			delay = 90 * time.Second
			if _, current := r.setRegisteredIfCurrent(sessionID, session, true); !current {
				return
			}
			if wasRetrying {
				r.logger.Info("SIP registration restored", "sessionId", sessionID,
					"transport", session.configuration.Transport)
			}
			r.emit(Event{Type: EventRegistered, SessionID: sessionID, StatusCode: 200})
		}
	}
}

func (r *SIPGoRuntime) logRegistrationFailure(
	message, sessionID string, err error, failures int,
) {
	status := 0
	failureKind := sipTransactionFailureKind(err)
	var responseError *sipRegisterResponseError
	if errors.As(err, &responseError) {
		status = responseError.status
		failureKind = "rejected"
	}
	r.logger.Warn(message, "sessionId", sessionID,
		"sipStatusCode", status, "failureKind", failureKind,
		"consecutiveFailures", failures)
}

func registrationRetryDelay(failures int) time.Duration {
	if failures < 1 {
		return time.Second
	}
	shift := min(failures-1, 5)
	return min(time.Second*time.Duration(1<<shift), 30*time.Second)
}

func (r *SIPGoRuntime) setRegistered(sessionID string, registered bool) {
	r.mu.Lock()
	if session := r.sessions[sessionID]; session != nil {
		session.registered = registered
		if registered && session.registeredPeer == "" {
			session.registeredPeer = canonicalPeerAddress(net.JoinHostPort(
				session.identity.Host, strconv.Itoa(int(session.identity.Port)),
			))
		}
		if registered && session.resolvedTarget == "" {
			if address := net.ParseIP(session.identity.Host); address != nil && address.To4() != nil {
				session.resolvedTarget = net.JoinHostPort(
					address.To4().String(), strconv.Itoa(int(session.identity.Port)),
				)
			}
		}
	}
	r.mu.Unlock()
}

// setRegisteredIfCurrent prevents a completed network operation from an old
// registration generation from mutating a replacement session with the same ID.
func (r *SIPGoRuntime) setRegisteredIfCurrent(
	sessionID string, expected *sipgoSession, registered bool,
) (previous bool, current bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session := r.sessions[sessionID]
	if session != expected {
		return false, false
	}
	previous = session.registered
	session.registered = registered
	return previous, true
}

func (r *SIPGoRuntime) isCurrentSession(sessionID string, expected *sipgoSession) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[sessionID] == expected
}

func (r *SIPGoRuntime) isCurrentRegisteredSession(sessionID string, expected *sipgoSession) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session := r.sessions[sessionID]
	return session == expected && session.registered
}

func (r *SIPGoRuntime) serveUDP() {
	if err := r.server.ServeUDP(r.udpConn); r.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
		r.ready.Store(false)
		r.cancel()
	}
}

func (r *SIPGoRuntime) serveTCP() {
	if err := r.server.ServeTCP(r.tcpListen); r.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
		r.ready.Store(false)
		r.cancel()
	}
}

func (r *SIPGoRuntime) emit(event Event) {
	select {
	case r.events <- event:
	case <-r.ctx.Done():
	}
}

func (r *SIPGoRuntime) session(sessionID string) (*sipgoSession, error) {
	r.mu.RLock()
	session := r.sessions[sessionID]
	r.mu.RUnlock()
	if session == nil {
		return nil, errors.New("SIP session is not configured")
	}
	return session, nil
}

func (r *SIPGoRuntime) registeredSession(sessionID string) (*sipgoSession, string, error) {
	r.mu.RLock()
	session := r.sessions[sessionID]
	registered := session != nil && session.registered && session.resolvedTarget != ""
	resolvedTarget := ""
	if session != nil {
		resolvedTarget = session.resolvedTarget
	}
	r.mu.RUnlock()
	if session == nil {
		return nil, "", errors.New("SIP session is not configured")
	}
	if !registered {
		return nil, "", ErrRegistrationNotReady
	}
	return session, resolvedTarget, nil
}

func (r *SIPGoRuntime) resolveSessionTarget(
	ctx context.Context,
	session *sipgoSession,
	refresh bool,
) (string, error) {
	r.mu.RLock()
	current := session.resolvedTarget
	r.mu.RUnlock()
	if !refresh && current != "" {
		return current, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	host := session.identity.Host
	var resolved net.IP
	if literal := net.ParseIP(host); literal != nil {
		resolved = literal.To4()
	} else {
		addresses, err := r.cfg.ResolveIPv4(ctx, host)
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		for _, address := range addresses {
			if ipv4 := address.To4(); ipv4 != nil {
				resolved = ipv4
				break
			}
		}
	}
	if resolved == nil {
		return "", errSIPHostIPv4Unavailable
	}
	target := net.JoinHostPort(resolved.String(), strconv.Itoa(int(session.identity.Port)))
	r.mu.Lock()
	session.resolvedTarget = target
	r.mu.Unlock()
	return target, nil
}

func (r *SIPGoRuntime) sessionForInboundRequest(request *sipmessage.Request) (string, *sipgoSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	transport := Transport(strings.ToLower(request.Transport()))
	if sessionID := r.contacts[request.Recipient.User]; sessionID != "" {
		session := r.sessions[sessionID]
		if session != nil && session.identity.Transport == transport {
			return sessionID, session, session.registered
		}
	}
	peer := canonicalPeerAddress(request.Source())
	var matchedID string
	var matched *sipgoSession
	var soleID string
	var sole *sipgoSession
	candidates := 0
	for sessionID, session := range r.sessions {
		if !session.registered || session.identity.Username != request.Recipient.User ||
			session.identity.Transport != transport {
			continue
		}
		candidates++
		soleID, sole = sessionID, session
		if session.registeredPeer == "" || session.registeredPeer != peer {
			continue
		}
		if matched != nil {
			return "", nil, false
		}
		matchedID, matched = sessionID, session
	}
	if matched != nil {
		return matchedID, matched, true
	}
	if candidates == 1 {
		return soleID, sole, true
	}
	return "", nil, false
}

func contactUserFor(sessionID string, identity Identity) string {
	digest := sha256.Sum256([]byte(sessionID + "\x00" + string(identity.Transport) + "\x00" + identity.Host +
		"\x00" + strconv.Itoa(int(identity.Port)) + "\x00" + identity.Realm + "\x00" + identity.Username))
	return "df-" + base64.RawURLEncoding.EncodeToString(digest[:12])
}

func canonicalPeerAddress(value string) string {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return ""
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil {
		return ""
	}
	return net.JoinHostPort(ip.To4().String(), port)
}

func (r *SIPGoRuntime) call(sessionID, callID string) (*sipgoCall, error) {
	r.mu.RLock()
	callValue := r.calls[callID]
	r.mu.RUnlock()
	if callValue == nil || callValue.sessionID != sessionID {
		return nil, errors.New("SIP call not found")
	}
	return callValue, nil
}

func (r *SIPGoRuntime) callBySIPRequest(request *sipmessage.Request) *sipgoCall {
	return r.callBySIPMessage(request)
}

func (r *SIPGoRuntime) callBySIPMessage(message sipmessage.Message) *sipgoCall {
	if message == nil {
		return nil
	}
	header := message.CallID()
	if header == nil {
		return nil
	}
	r.mu.RLock()
	callID := r.sipCallID[header.Value()]
	callValue := r.calls[callID]
	r.mu.RUnlock()
	return callValue
}

func (r *SIPGoRuntime) isCurrentCall(callValue *sipgoCall) bool {
	r.mu.RLock()
	current := r.calls[callValue.callID]
	r.mu.RUnlock()
	return current == callValue
}

func (r *SIPGoRuntime) removeCallIfCurrent(callValue *sipgoCall) {
	callValue.completeInvite()
	r.mu.Lock()
	if r.calls[callValue.callID] != callValue {
		r.mu.Unlock()
		return
	}
	delete(r.calls, callValue.callID)
	if callValue.sipCallID != "" {
		delete(r.sipCallID, callValue.sipCallID)
	}
	r.mu.Unlock()
	if callValue.cancel != nil {
		callValue.cancel(errors.New("call ended"))
	}
}

func (r *SIPGoRuntime) removeCall(callID string) {
	r.mu.Lock()
	callValue := r.calls[callID]
	delete(r.calls, callID)
	if callValue != nil && callValue.sipCallID != "" {
		delete(r.sipCallID, callValue.sipCallID)
	}
	r.mu.Unlock()
	if callValue != nil {
		callValue.completeInvite()
	}
	if callValue != nil && callValue.cancel != nil {
		callValue.cancel(errors.New("call ended"))
	}
}

func (c *sipgoCall) setConnected() {
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()
}

func (c *sipgoCall) completeOutbound() {
	if c.outboundDone == nil {
		return
	}
	c.outboundDoneOnce.Do(func() { close(c.outboundDone) })
}

func (c *sipgoCall) claimOutboundResolution() bool {
	claimed := false
	c.outboundResolveOnce.Do(func() { claimed = true })
	return claimed
}

func (c *sipgoCall) completeInvite() {
	if c.inviteDone == nil {
		return
	}
	c.inviteDoneOnce.Do(func() { close(c.inviteDone) })
}

func (c *sipgoCall) beginAnswer() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.answering || c.rejecting || c.connected || c.terminateAfterAnswer {
		return false
	}
	c.answering = true
	return true
}

func (c *sipgoCall) beginReject() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.answering || c.rejecting || c.connected {
		return false
	}
	c.rejecting = true
	return true
}

func (c *sipgoCall) finishAnswer(connected bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answering = false
	c.connected = connected
	return c.terminateAfterAnswer
}

func (c *sipgoCall) markTerminationWhileAnswering() (answering, first bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.answering {
		return false, false
	}
	first = !c.terminateAfterAnswer
	c.terminateAfterAnswer = true
	return true, first
}

func (c *sipgoCall) isConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

func (c *sipgoCall) setLocalMedia(localSDP []byte, codec string) {
	c.mu.Lock()
	c.localSDP = append(c.localSDP[:0], localSDP...)
	c.codec = codec
	c.mu.Unlock()
}

func (c *sipgoCall) localMedia() ([]byte, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]byte(nil), c.localSDP...), c.codec
}

func (c *sipgoCall) claimReliableProvisional(rseq uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rseq <= c.lastReliableRSeq {
		return false
	}
	c.lastReliableRSeq = rseq
	return true
}

func (c *sipgoCall) beginReinvite(cseq uint32, needsAnswer bool) (<-chan struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected || c.reinviteAck != nil {
		return nil, false
	}
	c.reinviteCSeq = cseq
	c.reinviteNeedsAnswer = needsAnswer
	c.reinviteAck = make(chan struct{})
	return c.reinviteAck, true
}

func (c *sipgoCall) ackReinvite(cseq uint32) (matched, needsAnswer bool, codec string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reinviteAck == nil || c.reinviteCSeq != cseq {
		return false, false, ""
	}
	close(c.reinviteAck)
	c.reinviteAck = nil
	needsAnswer = c.reinviteNeedsAnswer
	c.reinviteNeedsAnswer = false
	c.reinviteCSeq = 0
	return true, needsAnswer, c.codec
}

func (c *sipgoCall) finishReinvite(cseq uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reinviteAck == nil || c.reinviteCSeq != cseq {
		return
	}
	c.reinviteAck = nil
	c.reinviteNeedsAnswer = false
	c.reinviteCSeq = 0
}

func (r *SIPGoRuntime) logInboundResponseFailure(callValue *sipgoCall, reason string, err error) {
	kind := sipTransactionFailureKind(err)
	r.logger.Warn("inbound SIP provisional response failed", "callId", callValue.callID,
		"reason", reason, "failureKind", kind)
	r.logger.Debug("inbound SIP provisional response failure detail", "callId", callValue.callID,
		"reason", reason, "failureKind", kind, "error", err)
}

func responseFromTransaction(ctx context.Context, transaction sipmessage.ClientTransaction) (*sipmessage.Response, error) {
	for {
		select {
		case response := <-transaction.Responses():
			if response != nil && !response.IsProvisional() {
				return response, nil
			}
		case <-transaction.Done():
			return nil, errors.Join(errors.New("SIP transaction terminated"), transaction.Err())
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func statusReason(status int) string {
	switch status {
	case sipmessage.StatusBusyHere:
		return "Busy Here"
	case sipmessage.StatusTemporarilyUnavailable:
		return "Temporarily Unavailable"
	case sipmessage.StatusRequestTerminated:
		return "Request Terminated"
	default:
		return "Declined"
	}
}

func boundedSIPContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= sipOperationTimeout {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, sipOperationTimeout)
}
