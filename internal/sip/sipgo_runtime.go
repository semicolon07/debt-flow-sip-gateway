package sip

import (
	"context"
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
	CanAcceptIncoming func(sessionID string) bool
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
	calls     map[string]*sipgoCall
	sipCallID map[string]string
	udpConn   net.PacketConn
	tcpListen net.Listener
	ready     atomic.Bool
	closeOnce sync.Once
}

type sipgoSession struct {
	configuration  Configuration
	contact        sipmessage.ContactHeader
	client         *sipgo.Client
	clientDialogs  *sipgo.DialogClientCache
	serverDialogs  *sipgo.DialogServerCache
	registered     bool
	registerCancel context.CancelFunc
}

type sipgoCall struct {
	mu        sync.RWMutex
	sessionID string
	callID    string
	sipCallID string
	outbound  *sipgo.DialogClientSession
	inbound   *sipgo.DialogServerSession
	cancel    context.CancelCauseFunc
	connected bool
}

const sipOperationTimeout = 10 * time.Second

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
	sipmessage.SIPDebug = false
	sipmessage.SetDefaultLogger(silent)
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
		calls: make(map[string]*sipgoCall), sipCallID: make(map[string]string),
		udpConn: udpConn, tcpListen: tcpListener,
	}
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
	contact, err := buildContact(r.cfg, configuration)
	if err != nil {
		return err
	}
	client := r.clients[configuration.Transport]
	if client == nil {
		return errors.New("SIP transport client is unavailable")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for ownerID, owner := range r.sessions {
		if ownerID != sessionID && owner.configuration.Username == configuration.Username {
			return errors.New("SIP username is already owned by another session")
		}
	}
	if existing := r.sessions[sessionID]; existing != nil && existing.registerCancel != nil {
		existing.registerCancel()
	}
	r.sessions[sessionID] = &sipgoSession{
		configuration: configuration, contact: contact, client: client,
		clientDialogs: sipgo.NewDialogClientCache(client, contact),
		serverDialogs: sipgo.NewDialogServerCache(client, contact),
	}
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
		r.emit(Event{Type: EventRegistrationFailed, SessionID: sessionID, Reason: "register_failed"})
		r.setRegistered(sessionID, false)
		r.startRegistrationLoop(sessionID, time.Second, true)
		return err
	}
	r.setRegistered(sessionID, true)
	r.emit(Event{Type: EventRegistered, SessionID: sessionID, StatusCode: 200})
	r.startRegistrationLoop(sessionID, 90*time.Second, false)
	return nil
}

func (r *SIPGoRuntime) Unregister(ctx context.Context, sessionID string) error {
	ctx, cancel := boundedSIPContext(ctx)
	defer cancel()
	session, err := r.session(sessionID)
	if err != nil {
		return nil
	}
	r.mu.Lock()
	if session.registerCancel != nil {
		session.registerCancel()
		session.registerCancel = nil
	}
	r.mu.Unlock()
	err = r.registerOnce(ctx, session, 0)
	r.mu.Lock()
	session.registered = false
	r.mu.Unlock()
	r.emit(Event{Type: EventUnregistered, SessionID: sessionID})
	return err
}

func (r *SIPGoRuntime) StartCall(ctx context.Context, sessionID, callID, destination string, localSDP []byte) error {
	ctx, operationCancel := boundedSIPContext(ctx)
	defer operationCancel()
	if len(localSDP) == 0 {
		return errors.New("local SIP SDP is required")
	}
	session, err := r.session(sessionID)
	if err != nil {
		return err
	}
	recipient, err := outboundRecipient(destination, session.configuration)
	if err != nil {
		return err
	}
	setURITransport(&recipient, session.configuration.Transport)
	callCtx, cancel := context.WithCancelCause(context.Background())
	dialog, err := session.clientDialogs.Invite(ctx, recipient, localSDP,
		sipmessage.NewHeader("Content-Type", "application/sdp"),
	)
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
		outbound: dialog, cancel: cancel,
	}
	r.mu.Lock()
	r.calls[callID] = callValue
	if sipCallID != "" {
		r.sipCallID[sipCallID] = callID
	}
	r.mu.Unlock()
	go r.waitOutbound(callCtx, callValue, session.configuration)
	return nil
}

func (r *SIPGoRuntime) Answer(_ context.Context, sessionID, callID string, localSDP []byte) error {
	callValue, err := r.call(sessionID, callID)
	if err != nil || callValue.inbound == nil {
		return errors.New("inbound call not found")
	}
	if err := callValue.inbound.RespondSDP(localSDP); err != nil {
		return err
	}
	callValue.setConnected()
	r.emit(Event{Type: EventConnected, SessionID: sessionID, CallID: callID, StatusCode: 200})
	return nil
}

func (r *SIPGoRuntime) Reject(_ context.Context, sessionID, callID string, status int) error {
	callValue, err := r.call(sessionID, callID)
	if err != nil || callValue.inbound == nil {
		return errors.New("inbound call not found")
	}
	if status == 0 {
		status = sipmessage.StatusBusyHere
	}
	err = callValue.inbound.Respond(status, statusReason(status), nil)
	r.removeCall(callID)
	return err
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
		} else {
			callValue.cancel(errors.New("local hangup"))
		}
	} else if callValue.inbound != nil {
		if callValue.isConnected() {
			err = callValue.inbound.Bye(ctx)
		} else {
			err = callValue.inbound.Respond(sipmessage.StatusRequestTerminated, "Request Terminated", nil)
		}
	}
	r.removeCall(callID)
	return err
}

func (*SIPGoRuntime) SendRTP(context.Context, string, string, *rtp.Packet) error {
	return errors.New("PBX RTP transport is owned by the media bridge")
}

func (*SIPGoRuntime) SendDTMF(context.Context, string, string, byte) error {
	return errors.New("RFC 4733 transport is owned by the media bridge")
}

func (r *SIPGoRuntime) CloseSession(ctx context.Context, sessionID string) error {
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
	_ = r.Unregister(ctx, sessionID)
	r.mu.Lock()
	delete(r.sessions, sessionID)
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
	r.server.OnOptions(func(request *sipmessage.Request, tx sipmessage.ServerTransaction) {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request, sipmessage.StatusOK, "OK", nil))
	})
	r.server.OnNoRoute(func(request *sipmessage.Request, tx sipmessage.ServerTransaction) {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request, sipmessage.StatusMethodNotAllowed, "Method Not Allowed", nil))
	})
}

func (r *SIPGoRuntime) onInvite(request *sipmessage.Request, tx sipmessage.ServerTransaction) {
	username := request.Recipient.User
	sessionID, session, registered := r.sessionByUsername(username)
	if session == nil || !registered {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request, sipmessage.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil))
		return
	}
	if r.cfg.CanAcceptIncoming != nil && !r.cfg.CanAcceptIncoming(sessionID) {
		_ = tx.Respond(sipmessage.NewResponseFromRequest(request, sipmessage.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil))
		return
	}
	dialog, err := session.serverDialogs.ReadInvite(request, tx)
	if err != nil {
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
	}
	r.mu.Lock()
	r.calls[callID] = callValue
	if sipCallID != "" {
		r.sipCallID[sipCallID] = callID
	}
	r.mu.Unlock()
	_ = dialog.Respond(sipmessage.StatusTrying, "Trying", nil)
	_ = dialog.Respond(sipmessage.StatusRinging, "Ringing", nil)
	remoteParty := "unknown"
	if from := request.From(); from != nil && from.Address.User != "" {
		remoteParty = normalizeRemoteParty(from.Address.User)
	}
	r.emit(Event{
		Type: EventIncoming, SessionID: sessionID, CallID: callID,
		StatusCode: sipmessage.StatusRinging, RemoteSDP: append([]byte(nil), request.Body()...),
		RemoteParty: remoteParty,
	})
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
	if callValue == nil || callValue.inbound == nil {
		return
	}
	_ = callValue.inbound.ReadAck(request, tx)
}

func (r *SIPGoRuntime) onCancel(request *sipmessage.Request, tx sipmessage.ServerTransaction) {
	callValue := r.callBySIPRequest(request)
	_ = tx.Respond(sipmessage.NewResponseFromRequest(request, sipmessage.StatusOK, "OK", nil))
	if callValue == nil || callValue.inbound == nil {
		return
	}
	_ = callValue.inbound.Respond(sipmessage.StatusRequestTerminated, "Request Terminated", nil)
	r.emit(Event{Type: EventEnded, SessionID: callValue.sessionID, CallID: callValue.callID, Reason: "cancelled"})
	r.removeCall(callValue.callID)
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
	r.removeCall(callValue.callID)
}

func (r *SIPGoRuntime) waitOutbound(ctx context.Context, callValue *sipgoCall, configuration Configuration) {
	err := callValue.outbound.WaitAnswer(ctx, sipgo.AnswerOptions{
		Username: configuration.Username, Password: configuration.Password,
		OnResponse: func(response *sipmessage.Response) error {
			switch response.StatusCode {
			case sipmessage.StatusTrying:
				r.emit(Event{Type: EventTrying, SessionID: callValue.sessionID, CallID: callValue.callID, StatusCode: response.StatusCode})
			case sipmessage.StatusRinging, sipmessage.StatusSessionInProgress:
				r.emit(Event{Type: EventRinging, SessionID: callValue.sessionID, CallID: callValue.callID, StatusCode: response.StatusCode})
			}
			return nil
		},
	})
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
			r.emit(Event{Type: EventEnded, SessionID: callValue.sessionID, CallID: callValue.callID, Reason: "cancelled"})
			r.removeCall(callValue.callID)
			return
		}
		status := 0
		var responseError *sipgo.ErrDialogResponse
		if errors.As(err, &responseError) && responseError.Res != nil {
			status = responseError.Res.StatusCode
		}
		r.emit(Event{Type: EventFailed, SessionID: callValue.sessionID, CallID: callValue.callID, StatusCode: status, Reason: (AsteriskProfile{}).NormalizeFailure(status)})
		r.removeCall(callValue.callID)
		return
	}
	ackCtx, ackCancel := context.WithTimeout(context.Background(), sipOperationTimeout)
	err = callValue.outbound.Ack(ackCtx)
	ackCancel()
	if err != nil {
		r.emit(Event{Type: EventFailed, SessionID: callValue.sessionID, CallID: callValue.callID, Reason: "ack_failed"})
		r.removeCall(callValue.callID)
		return
	}
	if context.Cause(ctx) != nil {
		byeCtx, byeCancel := context.WithTimeout(context.Background(), sipOperationTimeout)
		_ = callValue.outbound.Bye(byeCtx)
		byeCancel()
		r.emit(Event{Type: EventEnded, SessionID: callValue.sessionID, CallID: callValue.callID, Reason: "cancelled"})
		r.removeCall(callValue.callID)
		return
	}
	callValue.setConnected()
	r.emit(Event{
		Type: EventConnected, SessionID: callValue.sessionID, CallID: callValue.callID,
		StatusCode: callValue.outbound.InviteResponse.StatusCode,
		RemoteSDP:  append([]byte(nil), callValue.outbound.InviteResponse.Body()...),
	})
}

func (r *SIPGoRuntime) registerOnce(ctx context.Context, session *sipgoSession, expires uint32) error {
	cfg := session.configuration
	recipient := sipmessage.Uri{User: cfg.Username, Host: cfg.Host, Port: int(cfg.Port)}
	request := sipmessage.NewRequest(sipmessage.REGISTER, recipient)
	request.AppendHeader(&session.contact)
	expiresHeader := sipmessage.ExpiresHeader(expires)
	request.AppendHeader(&expiresHeader)
	request.SetTransport(strings.ToUpper(string(cfg.Transport)))
	request.SetDestination(net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))))
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
		return fmt.Errorf("SIP REGISTER rejected with status %d", response.StatusCode)
	}
	return nil
}

func buildContact(cfg SIPGoConfig, configuration Configuration) (sipmessage.ContactHeader, error) {
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
	address := sipmessage.Uri{User: configuration.Username, Host: cfg.AdvertisedIP, Port: port}
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

func (r *SIPGoRuntime) startRegistrationLoop(sessionID string, firstDelay time.Duration, retrying bool) {
	r.mu.Lock()
	session := r.sessions[sessionID]
	if session == nil {
		r.mu.Unlock()
		return
	}
	if session.registerCancel != nil {
		session.registerCancel()
	}
	refreshCtx, cancel := context.WithCancel(r.ctx)
	session.registerCancel = cancel
	r.mu.Unlock()
	go r.refreshRegistration(refreshCtx, sessionID, firstDelay, retrying)
}

func (r *SIPGoRuntime) refreshRegistration(
	ctx context.Context, sessionID string, delay time.Duration, retrying bool,
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
		session, err := r.session(sessionID)
		if err != nil {
			return
		}
		if retrying {
			r.emit(Event{Type: EventRegistrationRetrying, SessionID: sessionID})
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = r.registerOnce(refreshCtx, session, 120)
		cancel()
		if err != nil {
			failures++
			retrying = true
			r.setRegistered(sessionID, false)
			if failures%5 == 0 {
				r.emit(Event{Type: EventRegistrationFailed, SessionID: sessionID, Reason: "refresh_failed"})
			}
			delay = registrationRetryDelay(failures)
		} else {
			failures = 0
			retrying = false
			delay = 90 * time.Second
			r.setRegistered(sessionID, true)
			r.emit(Event{Type: EventRegistered, SessionID: sessionID, StatusCode: 200})
		}
	}
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
	}
	r.mu.Unlock()
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

func (r *SIPGoRuntime) sessionByUsername(username string) (string, *sipgoSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for sessionID, session := range r.sessions {
		if session.configuration.Username == username {
			return sessionID, session, session.registered
		}
	}
	return "", nil, false
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
	header := request.CallID()
	if header == nil {
		return nil
	}
	r.mu.RLock()
	callID := r.sipCallID[header.Value()]
	callValue := r.calls[callID]
	r.mu.RUnlock()
	return callValue
}

func (r *SIPGoRuntime) removeCall(callID string) {
	r.mu.Lock()
	callValue := r.calls[callID]
	delete(r.calls, callID)
	if callValue != nil && callValue.sipCallID != "" {
		delete(r.sipCallID, callValue.sipCallID)
	}
	r.mu.Unlock()
	if callValue != nil && callValue.cancel != nil {
		callValue.cancel(errors.New("call ended"))
	}
}

func (c *sipgoCall) setConnected() {
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()
}

func (c *sipgoCall) isConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
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
