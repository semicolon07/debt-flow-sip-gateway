package gateway

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/debt-flow/sip-gateway/internal/call"
	"github.com/debt-flow/sip-gateway/internal/capability"
	gatewaymedia "github.com/debt-flow/sip-gateway/internal/media"
	"github.com/debt-flow/sip-gateway/internal/protocol"
	"github.com/debt-flow/sip-gateway/internal/sip"
	"github.com/debt-flow/sip-gateway/internal/store"
	gatewayturn "github.com/debt-flow/sip-gateway/internal/turn"
	gatewaywebrtc "github.com/debt-flow/sip-gateway/internal/webrtc"
	"github.com/google/uuid"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

type CapabilityVerifier interface {
	VerifySession(string, string) (capability.SessionClaims, error)
	VerifyCall(string, capability.CallBinding) (capability.CallClaims, error)
}

type RuntimeObserver interface {
	ObserveCommand(commandType, outcome string)
	ObserveCapability(kind, outcome string)
	ObserveCallConnected(direction string, latency time.Duration)
	ObserveCallEnded(direction, outcome string)
	ObserveRTPPacket(direction string)
	ObserveRTPQuality(direction string, lost, jitter uint32)
	ObserveRTPBridgeError(direction string)
	ObserveLease(outcome string)
}

type RTC interface {
	Open(string, gatewaywebrtc.TURNCredential) error
	AcceptOffer(string, string, string) (string, error)
	AddCandidate(string, pion.ICECandidateInit) error
	WriteRTP(string, *rtp.Packet) error
	WriteRTCP(string, []rtcp.Packet) error
	Codec(string) (gatewaywebrtc.CodecInfo, error)
	State(string) (pion.PeerConnectionState, error)
	CloseSession(string) error
}

type RTPMedia interface {
	Allocate(callID string) (*net.UDPAddr, error)
	SetRemote(callID string, remoteRTP, remoteRTCP *net.UDPAddr) error
	Write(callID string, packet *rtp.Packet) error
	WriteRTCP(callID string, packets []rtcp.Packet) error
	CloseCall(callID string) error
}

type CoordinatorConfig struct {
	NodeID            string
	MaxSessions       int
	MaxCalls          int
	TURNURLs          []string
	LeaseRenewEvery   time.Duration
	LeaseExpiresAfter time.Duration
	ReconnectGrace    time.Duration
}

type Coordinator struct {
	cfg        CoordinatorConfig
	capability CapabilityVerifier
	store      *store.Store
	turn       *gatewayturn.Credentials
	rtc        RTC
	media      RTPMedia
	sip        sip.Runtime
	logger     *slog.Logger
	now        func() time.Time
	mu         sync.RWMutex
	redeemMu   sync.Mutex
	sessions   map[string]*runtimeSession
	sipOwners  map[string]string
	callOwners map[string]string
	closed     chan struct{}
	closeOnce  sync.Once
	observer   RuntimeObserver
}

func (c *Coordinator) SetObserver(observer RuntimeObserver) {
	c.observer = observer
}

type CallStat struct {
	State     string
	Direction string
	Count     int
}

type RuntimeStats struct {
	AuthenticatedSessions int
	ConnectedSessions     int
	Registrations         map[string]int
	Calls                 []CallStat
}

type runtimeSession struct {
	mu                      sync.Mutex
	callMu                  sync.Mutex
	connectMu               sync.Mutex
	eventMu                 sync.Mutex
	claims                  Claims
	sipConfig               sip.Configuration
	live                    chan protocol.Envelope
	connected               bool
	generation              uint64
	registration            string
	activeCall              *call.Machine
	remoteParty             string
	callContext             string
	callCommand             string
	earlyMediaCallID        string
	earlyMediaAudibleCallID string
	earlyMediaStatusCode    int
	activeMedia             *callMedia
	leaseExpires            time.Time
}

type callMedia struct {
	callID           string
	codec            gatewaywebrtc.CodecInfo
	localSDP         []byte
	toSIP            *gatewaymedia.Translator
	toBrowser        *gatewaymedia.Translator
	sipAudioPayload  uint8
	sipDTMFPayload   uint8
	audibleRTPStreak uint8
	dtmfSSRC         uint32
	dtmfSequence     uint16
	dtmfTimestamp    uint32
	qualityMu        sync.Mutex
	browserLoss      map[uint32]uint32
	pbxLoss          map[uint32]uint32
	toSIPMu          sync.Mutex
	toBrowserMu      sync.Mutex
	toSIPPacket      rtp.Packet
	toBrowserPacket  rtp.Packet
}

type commandError struct{ code string }

const (
	liveEventQueueCapacity           = 256
	earlyMediaAudiblePacketThreshold = 5
)

func (e commandError) Error() string { return e.code }
func (e commandError) Code() string  { return e.code }

func NewCoordinator(
	cfg CoordinatorConfig,
	capabilityVerifier CapabilityVerifier,
	storeValue *store.Store,
	turnCredentials *gatewayturn.Credentials,
	rtc RTC,
	media RTPMedia,
	sipRuntime sip.Runtime,
	logger *slog.Logger,
) (*Coordinator, error) {
	if cfg.NodeID == "" || cfg.MaxSessions <= 0 || cfg.MaxCalls <= 0 || len(cfg.TURNURLs) == 0 || cfg.LeaseRenewEvery <= 0 || cfg.LeaseExpiresAfter <= 0 || cfg.ReconnectGrace <= 0 {
		return nil, errors.New("invalid coordinator configuration")
	}
	if capabilityVerifier == nil || storeValue == nil || turnCredentials == nil || rtc == nil || media == nil || sipRuntime == nil {
		return nil, errors.New("coordinator dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	coordinator := &Coordinator{
		cfg: cfg, capability: capabilityVerifier, store: storeValue, turn: turnCredentials, rtc: rtc, media: media,
		sip: sipRuntime, logger: logger, now: time.Now,
		sessions: make(map[string]*runtimeSession), sipOwners: make(map[string]string),
		callOwners: make(map[string]string),
		closed:     make(chan struct{}),
	}
	go coordinator.pumpSIPEvents()
	return coordinator, nil
}

func (c *Coordinator) Redeem(ctx context.Context, auth protocol.Authenticate) (Claims, error) {
	response, err := c.capability.VerifySession(auth.SessionCapability, auth.CollectionID)
	if err != nil {
		c.observeCapability("session", "rejected")
		return Claims{}, err
	}
	now := c.now().UTC()
	consumed, err := c.store.ConsumeCapability(ctx, response.CapabilityID, time.Unix(response.ExpiresAt, 0).UTC(), now)
	if err != nil || !consumed {
		c.observeCapability("session", "replayed")
		return Claims{}, errors.New("session capability was already used")
	}
	claims := Claims{
		CapabilityID: response.CapabilityID, SessionID: response.SessionID,
		UserSessionID: response.UserSessionID, CollectionID: response.CollectionID,
		CollectionBindingID: response.CollectionBindingID, AgentInstanceID: response.AgentInstanceID,
		ExpiresAtUTC: time.Unix(response.LeaseExpiresAt, 0).UTC(),
	}
	if claims.SessionID == "" || claims.UserSessionID == "" || claims.CollectionID != auth.CollectionID || !claims.ExpiresAtUTC.After(now) {
		c.observeCapability("session", "rejected")
		return Claims{}, errors.New("redeemed gateway session is invalid")
	}
	transport := sip.Transport(response.SIP.Transport)
	configuration := sip.Configuration{
		Host: response.SIP.Host, Port: uint16(response.SIP.Port), Transport: transport,
		Username: response.SIP.Username, Password: response.SIP.Password, Realm: response.SIP.Realm,
	}
	if err := c.store.BindSession(ctx, claims.SessionID, claims.AgentInstanceID,
		claims.UserSessionID, claims.CollectionID, claims.CollectionBindingID, now); err != nil {
		c.observeCapability("session", "rejected")
		return Claims{}, fmt.Errorf("persist gateway session binding: %w", err)
	}
	session := &runtimeSession{
		claims: claims, registration: "idle",
		leaseExpires: claims.ExpiresAtUTC,
		sipConfig:    configuration,
	}
	c.redeemMu.Lock()
	defer c.redeemMu.Unlock()
	c.mu.Lock()
	if existing := c.sessions[claims.SessionID]; existing != nil {
		existing.mu.Lock()
		if existing.claims.UserSessionID != claims.UserSessionID ||
			existing.claims.CollectionID != claims.CollectionID ||
			existing.claims.CollectionBindingID != claims.CollectionBindingID ||
			existing.claims.AgentInstanceID != claims.AgentInstanceID ||
			!sameSIPConfiguration(existing.sipConfig, configuration) {
			existing.mu.Unlock()
			c.mu.Unlock()
			c.observeCapability("session", "rejected")
			return Claims{}, errors.New("gateway session identity binding changed")
		}
		existing.mu.Unlock()
		c.mu.Unlock()
		c.observeCapability("session", "accepted")
		return claims, nil
	}
	// A SIP address-of-record must have exactly one owner in this process. Without
	// this takeover, a browser refresh/new identity can leave the old registration
	// retrying during reconnect grace and inbound INVITEs become nondeterministic.
	if ownerID := c.sipOwners[configuration.Username]; ownerID != "" && ownerID != claims.SessionID {
		owner := c.sessions[ownerID]
		c.mu.Unlock()
		if owner != nil {
			owner.mu.Lock()
			active := owner.activeCall != nil && owner.activeCall.Snapshot().State != call.StateEnded
			owner.mu.Unlock()
			if active {
				c.observeCapability("session", "rejected")
				return Claims{}, errors.New("SIP identity has an active call")
			}
			c.removeSession(ownerID)
		}
		c.mu.Lock()
	}
	if len(c.sessions) >= c.cfg.MaxSessions {
		c.mu.Unlock()
		c.observeCapability("session", "capacity_rejected")
		return Claims{}, errors.New("gateway session capacity reached")
	}
	c.sessions[claims.SessionID] = session
	c.sipOwners[configuration.Username] = claims.SessionID
	c.mu.Unlock()
	c.observeCapability("session", "accepted")
	go c.monitorLease(claims.SessionID, claims.CapabilityID)
	return claims, nil
}

func (c *Coordinator) Connect(_ context.Context, claims Claims) ([]protocol.Envelope, <-chan protocol.Envelope, error) {
	session, err := c.session(claims.SessionID)
	if err != nil {
		return nil, nil, err
	}
	session.connectMu.Lock()
	defer session.connectMu.Unlock()
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !c.isCurrentSession(session) {
		return nil, nil, commandError{"session_connection_stale"}
	}
	session.eventMu.Lock()
	defer session.eventMu.Unlock()
	session.mu.Lock()
	if !sameSessionIdentity(session.claims, claims) || !c.now().Before(session.leaseExpires) {
		session.mu.Unlock()
		return nil, nil, commandError{"session_connection_stale"}
	}
	session.mu.Unlock()
	credential, err := c.turn.Issue(claims.SessionID, claims.ExpiresAtUTC, c.now())
	if err != nil {
		return nil, nil, err
	}
	// A resumed browser session negotiates a fresh PeerConnection while the SIP call
	// and RTP bridge stay attached to the same logical gateway session.
	if err := c.rtc.Open(claims.SessionID, gatewaywebrtc.TURNCredential{
		Username: credential.Username, Password: credential.Password,
	}); err != nil {
		c.turn.Revoke(credential.Username)
		return nil, nil, err
	}
	c.turn.RetainSessionCredential(claims.SessionID, credential.Username)
	session.mu.Lock()
	if !sameSessionIdentity(session.claims, claims) || !c.now().Before(session.leaseExpires) {
		session.mu.Unlock()
		_ = c.rtc.CloseSession(claims.SessionID)
		c.turn.Revoke(credential.Username)
		return nil, nil, commandError{"session_connection_stale"}
	}
	monitorNeeded := session.claims.CapabilityID != claims.CapabilityID
	session.claims = claims
	session.leaseExpires = claims.ExpiresAtUTC
	session.connected = true
	session.generation++
	previousLive := session.live
	nextLive := make(chan protocol.Envelope, liveEventQueueCapacity)
	session.live = nextLive
	snapshot := c.snapshotLocked(session)
	session.mu.Unlock()
	if previousLive != nil {
		close(previousLive)
	}
	pending, err := c.store.PendingAgentEvents(context.Background(), claims.AgentInstanceID,
		claims.UserSessionID, 0, 10_000)
	if err != nil {
		c.closeLiveStream(session, nextLive)
		return nil, nil, err
	}
	replay := make([]protocol.Envelope, 0, len(pending))
	for _, event := range pending {
		envelope, envelopeErr := replayEnvelope(event)
		if envelopeErr != nil {
			c.closeLiveStream(session, nextLive)
			return nil, nil, envelopeErr
		}
		replay = append(replay, envelope)
	}
	acknowledgedThrough, err := c.store.ConfirmedCursor(context.Background(), claims.AgentInstanceID,
		claims.UserSessionID)
	if err != nil {
		c.closeLiveStream(session, nextLive)
		return nil, nil, err
	}
	if monitorNeeded {
		go c.monitorLease(claims.SessionID, claims.CapabilityID)
	}
	welcome, _ := protocol.NewEvent("session.welcome", map[string]any{
		"sessionId": claims.SessionID, "protocolVersion": protocol.Version,
		"leaseExpiresAtUtc": claims.ExpiresAtUTC, "acknowledgedThrough": acknowledgedThrough,
		"turn": map[string]any{
			"urls":     append([]string(nil), c.cfg.TURNURLs...),
			"username": credential.Username, "password": credential.Password,
			"expiresAtUtc": credential.ExpiresAt,
		},
	}, c.now())
	snapshotEvent, _ := protocol.NewEvent("gateway.snapshot", snapshot, c.now())
	// Rebuild state from durable events first, then finish with the authoritative
	// in-memory snapshot so stale replay cannot regress the browser UI.
	initial := append([]protocol.Envelope{welcome}, replay...)
	initial = append(initial, snapshotEvent)
	return initial, nextLive, nil
}

func (c *Coordinator) Handle(ctx context.Context, claims Claims, envelope protocol.Envelope) (events []protocol.Envelope, resultErr error) {
	defer func() {
		outcome := "completed"
		if resultErr != nil {
			outcome = "failed"
		}
		c.observeCommand(envelope.Type, outcome)
	}()
	session, err := c.session(claims.SessionID)
	if err != nil {
		return nil, commandError{"session_not_found"}
	}
	session.mu.Lock()
	currentConnection := sameConnectionClaims(session.claims, claims) && c.now().Before(session.leaseExpires)
	session.mu.Unlock()
	if !currentConnection {
		return nil, commandError{"session_connection_stale"}
	}
	switch envelope.Type {
	case "state.get":
		session.mu.Lock()
		snapshot := c.snapshotLocked(session)
		session.mu.Unlock()
		event, _ := protocol.NewEvent("gateway.snapshot", snapshot, c.now())
		return []protocol.Envelope{event}, nil
	case "session.renew":
		return c.renewSession(ctx, session, claims, envelope)
	case "events.ack":
		ack, err := protocol.DecodePayload[protocol.EventsACK](envelope)
		if err != nil || ack.Sequence == 0 ||
			(ack.CommandID != "" && protocol.ValidateCommandID(ack.CommandID) != nil) {
			return nil, commandError{"invalid_events_ack"}
		}
		acknowledgedThrough, err := c.store.AcknowledgeAgent(ctx, claims.AgentInstanceID,
			claims.UserSessionID, ack.Sequence, c.now())
		if err != nil {
			return nil, commandError{"event_ack_failed"}
		}
		// Legacy POC clients did not send a command ID. Keep that shape accepted,
		// while production clients receive a correlated result and only advance
		// their local cumulative ACK after SQLite has committed it.
		if ack.CommandID != "" {
			result, _ := protocol.NewEvent("command_result", map[string]any{
				"commandId": ack.CommandID, "commandType": "events.ack",
				"status": "completed", "sequence": acknowledgedThrough,
			}, c.now())
			return []protocol.Envelope{result}, nil
		}
		return nil, nil
	case "webrtc.offer":
		offer, err := protocol.DecodePayload[protocol.WebRTCOffer](envelope)
		if err != nil || offer.SDP == "" {
			return nil, commandError{"invalid_webrtc_offer"}
		}
		session.mu.Lock()
		preferredCodec := ""
		if session.activeMedia != nil {
			preferredCodec = session.activeMedia.codec.Name
		}
		session.mu.Unlock()
		answer, err := c.rtc.AcceptOffer(claims.SessionID, offer.SDP, preferredCodec)
		if err != nil {
			c.logger.Error("accept WebRTC offer failed", "sessionId", claims.SessionID, "error", err)
			return nil, commandError{"webrtc_offer_rejected"}
		}
		event, _ := protocol.NewEvent("webrtc.answer", map[string]string{"sdp": answer}, c.now())
		return []protocol.Envelope{event}, nil
	case "webrtc.ice_candidate":
		candidate, err := protocol.DecodePayload[protocol.ICECandidate](envelope)
		if err != nil || candidate.Candidate == "" {
			return nil, commandError{"invalid_ice_candidate"}
		}
		if err := c.rtc.AddCandidate(claims.SessionID, pion.ICECandidateInit{
			Candidate: candidate.Candidate, SDPMid: candidate.SDPMid,
			SDPMLineIndex: candidate.SDPMLineIndex,
		}); err != nil {
			return nil, commandError{"ice_candidate_rejected"}
		}
		return nil, nil
	case "registration.start":
		return c.startRegistration(ctx, session, envelope)
	case "registration.stop":
		return c.stopRegistration(ctx, session, envelope)
	case "call.start":
		return c.startCall(ctx, session, envelope)
	case "call.hangup":
		return c.hangup(ctx, session, envelope)
	case "call.answer":
		return c.answer(ctx, session, envelope)
	case "call.reject":
		return c.reject(ctx, session, envelope)
	case "call.dtmf":
		return c.sendDTMF(ctx, session, envelope)
	default:
		return nil, commandError{"unsupported_command"}
	}
}

func (c *Coordinator) HasActiveCall(userSessionID, collectionID string) bool {
	c.mu.RLock()
	sessions := make([]*runtimeSession, 0, len(c.sessions))
	for _, session := range c.sessions {
		sessions = append(sessions, session)
	}
	c.mu.RUnlock()
	for _, session := range sessions {
		if session.claims.UserSessionID != userSessionID || session.claims.CollectionID != collectionID {
			continue
		}
		session.mu.Lock()
		active := session.activeCall != nil && session.activeCall.Snapshot().State != call.StateEnded
		session.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}

func (c *Coordinator) Stats() RuntimeStats {
	c.mu.RLock()
	sessions := make([]*runtimeSession, 0, len(c.sessions))
	for _, session := range c.sessions {
		sessions = append(sessions, session)
	}
	c.mu.RUnlock()
	result := RuntimeStats{
		AuthenticatedSessions: len(sessions), Registrations: make(map[string]int),
	}
	type callKey struct{ state, direction string }
	callCounts := make(map[callKey]int)
	for _, session := range sessions {
		session.mu.Lock()
		if session.connected {
			result.ConnectedSessions++
		}
		result.Registrations[session.registration]++
		if session.activeCall != nil {
			snapshot := session.activeCall.Snapshot()
			if snapshot.State != call.StateEnded {
				callCounts[callKey{string(snapshot.State), string(snapshot.Direction)}]++
			}
		}
		session.mu.Unlock()
	}
	for key, count := range callCounts {
		result.Calls = append(result.Calls, CallStat{State: key.state, Direction: key.direction, Count: count})
	}
	return result
}

func (c *Coordinator) CanAcceptIncoming(sessionID string) bool {
	session, err := c.session(sessionID)
	if err != nil {
		return false
	}
	state, err := c.rtc.State(sessionID)
	if err != nil || state != pion.PeerConnectionStateConnected {
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.connected && session.registration == string(sip.EventRegistered) &&
		(session.activeCall == nil || session.activeCall.Snapshot().State == call.StateEnded)
}

func (c *Coordinator) Disconnect(claims Claims) {
	session, err := c.session(claims.SessionID)
	if err != nil {
		return
	}
	session.mu.Lock()
	if session.claims.CapabilityID != claims.CapabilityID {
		session.mu.Unlock()
		return
	}
	session.connected = false
	session.generation++
	generation := session.generation
	session.mu.Unlock()
	c.observeLease("disconnected")
	go func() {
		timer := time.NewTimer(c.cfg.ReconnectGrace)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-c.closed:
			return
		}
		session.mu.Lock()
		if session.connected || session.generation != generation ||
			(session.activeCall != nil && session.activeCall.Snapshot().State != call.StateEnded) {
			session.mu.Unlock()
			return
		}
		session.mu.Unlock()
		c.removeSession(claims.SessionID)
	}()
}

func (c *Coordinator) OnICECandidate(sessionID string, candidate pion.ICECandidateInit) {
	event, err := protocol.NewEvent("webrtc.ice_candidate", candidate, c.now())
	if err == nil {
		c.publish(sessionID, event)
	}
}

func (c *Coordinator) OnWebRTCState(sessionID string, state pion.PeerConnectionState) {
	if state != pion.PeerConnectionStateFailed {
		return
	}
	session, err := c.session(sessionID)
	if err != nil {
		return
	}
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !c.isCurrentSession(session) {
		return
	}
	session.mu.Lock()
	machine := session.activeCall
	if machine == nil || machine.Snapshot().State == call.StateEnded {
		session.mu.Unlock()
		return
	}
	if !machine.RemoteHangup(call.EndMediaFailed, c.now()) {
		session.mu.Unlock()
		return
	}
	snapshot := machine.Snapshot()
	session.mu.Unlock()
	_ = c.store.SaveCall(context.Background(), sessionID, session.claims.AgentInstanceID, snapshot, c.now())
	_ = c.emitCallEnded(context.Background(), session, snapshot)
	_ = c.sip.Hangup(context.Background(), sessionID, snapshot.CallID)
	c.closeCallMedia(session, snapshot.CallID)
}

func (c *Coordinator) OnBrowserRTP(sessionID string, packet *rtp.Packet) {
	session, err := c.session(sessionID)
	if err != nil || packet == nil {
		return
	}
	session.mu.Lock()
	media := session.activeMedia
	machine := session.activeCall
	state := call.State("")
	if machine != nil {
		state = machine.Snapshot().State
	}
	session.mu.Unlock()
	if media == nil || state != call.StateConnected {
		return
	}
	media.toSIPMu.Lock()
	defer media.toSIPMu.Unlock()
	if err := media.toSIP.TranslateInto(&media.toSIPPacket, packet); err != nil {
		c.observeRTPBridgeError("browser_to_pbx")
		return
	}
	if err := c.media.Write(media.callID, &media.toSIPPacket); err != nil {
		c.observeRTPBridgeError("browser_to_pbx")
		return
	}
	c.observeRTPPacket("browser_to_pbx")
}

func (c *Coordinator) OnPBXRTP(callID string, packet *rtp.Packet) {
	if packet == nil {
		return
	}
	c.mu.RLock()
	sessionID := c.callOwners[callID]
	c.mu.RUnlock()
	if sessionID == "" {
		return
	}
	session, err := c.session(sessionID)
	if err != nil {
		return
	}
	session.mu.Lock()
	media := session.activeMedia
	earlyMediaStatusCode := session.earlyMediaStatusCode
	session.mu.Unlock()
	if media == nil || media.callID != callID {
		return
	}
	// TrackLocalStaticRTP is bound to the negotiated G.711 codec and Pion
	// rewrites every outgoing packet to that audio payload type. Forwarding a
	// PBX telephone-event packet on this track would therefore make the browser
	// decode RFC 4733 bytes as audio (typically a click or corrupted frame).
	// Browser-originated DTMF uses call.dtmf and is sent to SIP separately.
	if packet.PayloadType != media.sipAudioPayload {
		return
	}
	media.toBrowserMu.Lock()
	defer media.toBrowserMu.Unlock()
	if err := media.toBrowser.TranslateInto(&media.toBrowserPacket, packet); err != nil {
		c.observeRTPBridgeError("pbx_to_browser")
		return
	}
	if err := c.rtc.WriteRTP(sessionID, &media.toBrowserPacket); err != nil {
		c.observeRTPBridgeError("pbx_to_browser")
		return
	}
	c.observeRTPPacket("pbx_to_browser")
	// A 180 response asks the client to present local ringing. Continue
	// forwarding its RTP for compatibility, but only 183/other progress media
	// can replace browser-generated ringback.
	if earlyMediaStatusCode == 180 {
		media.audibleRTPStreak = 0
		return
	}
	if !gatewaymedia.HasAudibleG711(media.codec.Name, packet.Payload) {
		media.audibleRTPStreak = 0
		return
	}
	if media.audibleRTPStreak < earlyMediaAudiblePacketThreshold {
		media.audibleRTPStreak++
	}
	if media.audibleRTPStreak < earlyMediaAudiblePacketThreshold {
		return
	}
	publishEarlyMedia := false
	statusCode := 0
	session.mu.Lock()
	if session.activeMedia == media && session.earlyMediaCallID == callID &&
		session.earlyMediaAudibleCallID != callID && session.activeCall != nil &&
		session.activeCall.Snapshot().State == call.StateRinging &&
		session.earlyMediaStatusCode != 180 {
		session.earlyMediaAudibleCallID = callID
		statusCode = session.earlyMediaStatusCode
		publishEarlyMedia = true
	}
	session.mu.Unlock()
	if publishEarlyMedia {
		c.logger.Info("outbound SIP audible early media detected", "callId", callID,
			"sipStatusCode", statusCode)
		c.publishCallEarlyMedia(session, callID, statusCode)
	}
}

func (c *Coordinator) OnBrowserRTCP(sessionID string, packets []rtcp.Packet) {
	session, err := c.session(sessionID)
	if err != nil || len(packets) == 0 {
		return
	}
	session.mu.Lock()
	media := session.activeMedia
	session.mu.Unlock()
	if media == nil {
		return
	}
	c.observeRTCPQuality(media, "pbx_to_browser", packets)
	browserSource, sipTarget, browserReady := media.toSIP.SSRCMapping()
	pbxSource, _, pbxReady := media.toBrowser.SSRCMapping()
	if !browserReady || !pbxReady {
		return
	}
	rewritten, err := gatewaymedia.RewriteRTCP(
		packets, browserSource, sipTarget, media.codec.OutboundSSRC, pbxSource,
	)
	if err != nil {
		c.observeRTPBridgeError("browser_to_pbx")
		return
	}
	if len(rewritten) > 0 {
		if err := c.media.WriteRTCP(media.callID, rewritten); err != nil {
			c.observeRTPBridgeError("browser_to_pbx")
		}
	}
}

func (c *Coordinator) OnPBXRTCP(callID string, packets []rtcp.Packet) {
	if len(packets) == 0 {
		return
	}
	c.mu.RLock()
	sessionID := c.callOwners[callID]
	c.mu.RUnlock()
	if sessionID == "" {
		return
	}
	session, err := c.session(sessionID)
	if err != nil {
		return
	}
	session.mu.Lock()
	media := session.activeMedia
	session.mu.Unlock()
	if media == nil || media.callID != callID {
		return
	}
	c.observeRTCPQuality(media, "browser_to_pbx", packets)
	pbxSource, _, pbxReady := media.toBrowser.SSRCMapping()
	browserSource, sipTarget, browserReady := media.toSIP.SSRCMapping()
	if !pbxReady || !browserReady {
		return
	}
	rewritten, err := gatewaymedia.RewriteRTCP(
		packets, pbxSource, media.codec.OutboundSSRC, sipTarget, browserSource,
	)
	if err != nil {
		c.observeRTPBridgeError("pbx_to_browser")
		return
	}
	if len(rewritten) > 0 {
		if err := c.rtc.WriteRTCP(sessionID, rewritten); err != nil {
			c.observeRTPBridgeError("pbx_to_browser")
		}
	}
}

func (c *Coordinator) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	c.mu.Lock()
	sessions := c.sessions
	c.sessions = make(map[string]*runtimeSession)
	c.sipOwners = make(map[string]string)
	c.mu.Unlock()
	var result error
	sessionIDs := make([]string, 0, len(sessions))
	for id, session := range sessions {
		session.callMu.Lock()
		c.turn.RevokeSession(id)
		result = errors.Join(result, c.rtc.CloseSession(id))
		session.mu.Lock()
		if session.activeMedia != nil {
			result = errors.Join(result, c.media.CloseCall(session.activeMedia.callID))
			session.activeMedia = nil
		}
		session.sipConfig.Password = ""
		if session.live != nil {
			close(session.live)
			session.live = nil
		}
		session.mu.Unlock()
		session.callMu.Unlock()
		sessionIDs = append(sessionIDs, id)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()
	closed := make(chan error, len(sessionIDs))
	for _, id := range sessionIDs {
		go func() { closed <- c.sip.CloseSession(cleanupCtx, id) }()
	}
	for range sessionIDs {
		select {
		case closeErr := <-closed:
			result = errors.Join(result, closeErr)
		case <-cleanupCtx.Done():
			result = errors.Join(result, cleanupCtx.Err())
			return errors.Join(result, c.sip.Close())
		}
	}
	return errors.Join(result, c.sip.Close())
}

func (c *Coordinator) startRegistration(ctx context.Context, session *runtimeSession, envelope protocol.Envelope) ([]protocol.Envelope, error) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !c.isCurrentSession(session) {
		return nil, commandError{"session_not_found"}
	}
	identity, err := protocol.DecodePayload[protocol.CommandIdentity](envelope)
	if err != nil || protocol.ValidateCommandID(identity.CommandID) != nil {
		return nil, commandError{"invalid_registration_command"}
	}
	begin, err := c.store.BeginCommand(ctx, identity.CommandID, envelope.Payload, c.now())
	if err != nil {
		return nil, commandError{"command_journal_failed"}
	}
	if begin.Conflict {
		return nil, commandError{"command_duplicate_conflict"}
	}
	if begin.Duplicate {
		return c.duplicateResult(identity.CommandID, begin.Record), nil
	}
	session.mu.Lock()
	if session.registration == string(sip.EventRegistered) {
		session.mu.Unlock()
		return c.finishCommand(ctx, identity.CommandID, "registration.start")
	}
	if session.activeCall != nil && session.activeCall.Snapshot().State != call.StateEnded {
		session.mu.Unlock()
		_ = c.store.FinishCommand(ctx, identity.CommandID, store.CommandFailed, nil, "active_call_exists", c.now())
		return nil, commandError{"active_call_exists"}
	}
	session.registration = "registering"
	configuration := session.sipConfig
	session.mu.Unlock()
	if err := c.sip.Configure(ctx, session.claims.SessionID, configuration); err != nil {
		session.mu.Lock()
		session.registration = string(sip.EventRegistrationFailed)
		session.mu.Unlock()
		_ = c.store.FinishCommand(ctx, identity.CommandID, store.CommandFailed, nil, "sip_adapter_unavailable", c.now())
		return nil, commandError{"sip_adapter_unavailable"}
	}
	if err := c.sip.Register(ctx, session.claims.SessionID); err != nil {
		_ = c.store.FinishCommand(ctx, identity.CommandID, store.CommandFailed, nil, "registration_failed", c.now())
		return nil, commandError{"registration_failed"}
	}
	return c.finishCommand(ctx, identity.CommandID, "registration.start")
}

func (c *Coordinator) stopRegistration(ctx context.Context, session *runtimeSession, envelope protocol.Envelope) ([]protocol.Envelope, error) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !c.isCurrentSession(session) {
		return nil, commandError{"session_not_found"}
	}
	identity, err := protocol.DecodePayload[protocol.CommandIdentity](envelope)
	if err != nil || protocol.ValidateCommandID(identity.CommandID) != nil {
		return nil, commandError{"invalid_registration_command"}
	}
	begin, err := c.store.BeginCommand(ctx, identity.CommandID, envelope.Payload, c.now())
	if err != nil {
		return nil, commandError{"command_journal_failed"}
	}
	if begin.Conflict {
		return nil, commandError{"command_duplicate_conflict"}
	}
	if begin.Duplicate {
		return c.duplicateResult(identity.CommandID, begin.Record), nil
	}
	session.mu.Lock()
	hasActiveCall := session.activeCall != nil && session.activeCall.Snapshot().State != call.StateEnded
	session.mu.Unlock()
	if hasActiveCall {
		_ = c.store.FinishCommand(ctx, identity.CommandID, store.CommandFailed, nil, "active_call_exists", c.now())
		return nil, commandError{"active_call_exists"}
	}
	if err := c.sip.Unregister(ctx, session.claims.SessionID); err != nil {
		_ = c.store.FinishCommand(ctx, identity.CommandID, store.CommandFailed, nil, "unregister_failed", c.now())
		return nil, commandError{"unregister_failed"}
	}
	return c.finishCommand(ctx, identity.CommandID, "registration.stop")
}

func (c *Coordinator) startCall(ctx context.Context, session *runtimeSession, envelope protocol.Envelope) ([]protocol.Envelope, error) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !c.isCurrentSession(session) {
		return nil, commandError{"session_not_found"}
	}
	command, err := protocol.DecodePayload[protocol.StartCall](envelope)
	if err != nil || protocol.ValidateStartCall(command) != nil {
		return nil, commandError{"invalid_call_start"}
	}
	begin, err := c.store.BeginCommand(ctx, command.CommandID, envelope.Payload, c.now())
	if err != nil {
		return nil, commandError{"command_journal_failed"}
	}
	if begin.Conflict {
		return nil, commandError{"command_duplicate_conflict"}
	}
	if begin.Duplicate {
		return c.duplicateResult(command.CommandID, begin.Record), nil
	}
	capacity, err := c.store.CanStartCall(ctx, session.claims.SessionID)
	if err != nil || !capacity {
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "event_outbox_near_capacity", c.now())
		return nil, commandError{"event_outbox_near_capacity"}
	}
	session.mu.Lock()
	if session.registration != string(sip.EventRegistered) {
		session.mu.Unlock()
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "registration_required", c.now())
		return nil, commandError{"registration_required"}
	}
	if session.activeCall != nil && session.activeCall.Snapshot().State != call.StateEnded {
		session.mu.Unlock()
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "active_call_exists", c.now())
		return nil, commandError{"active_call_exists"}
	}
	if state, stateErr := c.rtc.State(session.claims.SessionID); stateErr != nil || state != pion.PeerConnectionStateConnected {
		session.mu.Unlock()
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "media_not_ready", c.now())
		return nil, commandError{"media_not_ready"}
	}
	if !c.reserveCallOwner(command.CallID, session.claims.SessionID) {
		session.mu.Unlock()
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "call_capacity_reached", c.now())
		return nil, commandError{"call_capacity_reached"}
	}
	machine, _ := call.NewOutbound(command.CallID, c.now())
	createdSnapshot := machine.Snapshot()
	session.activeCall = machine
	session.remoteParty = ""
	session.callContext = ""
	session.callCommand = ""
	session.mu.Unlock()
	if err := c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID, machine.Snapshot(), c.now()); err != nil {
		c.discardCall(session, machine)
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "call_persistence_failed", c.now())
		return nil, commandError{"call_persistence_failed"}
	}
	if err := machine.BeginAuthorization(); err != nil {
		c.releaseCallOwner(command.CallID)
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "call_state_conflict", c.now())
		return nil, commandError{"call_state_conflict"}
	}
	authorization, err := c.capability.VerifyCall(command.GatewayCapability, capability.CallBinding{
		SessionID: session.claims.SessionID, UserSessionID: session.claims.UserSessionID,
		CollectionID:        session.claims.CollectionID,
		CollectionBindingID: session.claims.CollectionBindingID, CallID: command.CallID,
		CallContextID: command.CallContextID,
	})
	if err != nil || authorization.Destination == "" {
		c.observeCapability("call", "rejected")
		machine.AuthorizationFailed(c.now())
		_ = c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID,
			machine.Snapshot(), c.now())
		c.discardCall(session, machine)
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil,
			"call_authorization_failed", c.now())
		return nil, commandError{"call_authorization_failed"}
	}
	session.mu.Lock()
	session.remoteParty = authorization.Destination
	session.callContext = command.CallContextID
	session.callCommand = command.CommandID
	session.mu.Unlock()
	createdPayload := callEventPayload(createdSnapshot, authorization.Destination,
		command.CallContextID, command.CommandID)
	if _, err := c.emit(ctx, session, "call.created", createdPayload,
		"call-created:"+command.CallID); err != nil {
		c.logger.Error("persist call.created event failed", "callId", command.CallID, "error", err)
		c.failCallBeforeInvite(ctx, session, machine, command.CommandID, "event_outbox_failed", false)
		return nil, commandError{"event_outbox_failed"}
	}
	consumed, err := c.store.ConsumeCapability(ctx, authorization.CapabilityID,
		time.Unix(authorization.ExpiresAt, 0).UTC(), c.now().UTC())
	if err != nil || !consumed {
		if err == nil && !consumed {
			c.observeCapability("call", "replayed")
		} else {
			c.observeCapability("call", "rejected")
		}
		machine.AuthorizationFailed(c.now())
		c.releaseCallOwner(command.CallID)
		_ = c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID, machine.Snapshot(), c.now())
		_ = c.emitCallEnded(ctx, session, machine.Snapshot())
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "call_authorization_failed", c.now())
		return nil, commandError{"call_authorization_failed"}
	}
	c.observeCapability("call", "accepted")
	if _, err := machine.AuthorizationSucceeded(); err != nil {
		c.releaseCallOwner(command.CallID)
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "call_state_conflict", c.now())
		return nil, commandError{"call_state_conflict"}
	}
	dialing := machine.Snapshot()
	if err := c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID, dialing, c.now()); err != nil {
		c.failCallBeforeInvite(ctx, session, machine, command.CommandID, "call_persistence_failed", true)
		return nil, commandError{"call_persistence_failed"}
	}
	if _, err := c.emit(ctx, session, "call.state_changed", dialing, "call-dialing:"+command.CallID); err != nil {
		c.logger.Error("persist outbound dialing event failed", "callId", command.CallID, "error", err)
		c.failCallBeforeInvite(ctx, session, machine, command.CommandID, "event_outbox_failed", true)
		return nil, commandError{"event_outbox_failed"}
	}
	localSDP, err := c.setupCallMedia(session, command.CallID, nil)
	if err != nil {
		c.logger.Error("setup outbound call media failed", "callId", command.CallID, "error", err)
		c.closeCallMedia(session, command.CallID)
		machine.RemoteHangup(call.EndMediaFailed, c.now())
		_ = c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID, machine.Snapshot(), c.now())
		_ = c.emitCallEnded(ctx, session, machine.Snapshot())
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "media_not_ready", c.now())
		return nil, commandError{"media_not_ready"}
	}
	if err := c.sip.StartCall(ctx, session.claims.SessionID, command.CallID, authorization.Destination, localSDP); err != nil {
		errorCode := "sip_adapter_unavailable"
		if errors.Is(err, sip.ErrRegistrationNotReady) {
			errorCode = "registration_required"
			session.mu.Lock()
			session.registration = string(sip.EventRegistrationFailed)
			session.mu.Unlock()
			c.logger.Warn("outbound call rejected because SIP registration is not ready",
				"callId", command.CallID, "sessionId", session.claims.SessionID)
		} else {
			c.logger.Error("start outbound SIP call failed", "callId", command.CallID, "error", err)
		}
		c.closeCallMedia(session, command.CallID)
		machine.RemoteHangup(call.EndSIPFailed, c.now())
		_ = c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID, machine.Snapshot(), c.now())
		_ = c.emitCallEnded(ctx, session, machine.Snapshot())
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, errorCode, c.now())
		return nil, commandError{errorCode}
	}
	return c.finishCommand(ctx, command.CommandID, "call.start")
}

func (c *Coordinator) hangup(ctx context.Context, session *runtimeSession, envelope protocol.Envelope) ([]protocol.Envelope, error) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !c.isCurrentSession(session) {
		return nil, commandError{"session_not_found"}
	}
	command, begin, err := c.beginCallCommand(ctx, envelope)
	if err != nil {
		return nil, err
	}
	if begin.Duplicate {
		return c.duplicateResult(command.CommandID, begin.Record), nil
	}
	session.mu.Lock()
	machine := session.activeCall
	if machine == nil || machine.Snapshot().CallID != command.CallID {
		session.mu.Unlock()
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "call_not_found", c.now())
		return nil, commandError{"call_not_found"}
	}
	_, emitted := machine.BeginEnding()
	ending := machine.Snapshot()
	session.mu.Unlock()
	if !emitted {
		return c.finishCommand(ctx, command.CommandID, "call.hangup")
	}
	_ = c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID, ending, c.now())
	_, _ = c.emit(ctx, session, "call.state_changed", ending, "call-ending:"+command.CallID)
	sipErr := c.sip.Hangup(ctx, session.claims.SessionID, command.CallID)
	if machine.CompleteEnding(call.EndLocalHangup, c.now()) {
		snapshot := machine.Snapshot()
		_ = c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID, snapshot, c.now())
		_ = c.emitCallEnded(ctx, session, snapshot)
	}
	c.closeCallMedia(session, command.CallID)
	if sipErr != nil && !errors.Is(sipErr, sip.ErrSpikeGateNotPassed) {
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "sip_hangup_failed", c.now())
		return nil, commandError{"sip_hangup_failed"}
	}
	return c.finishCommand(ctx, command.CommandID, "call.hangup")
}

func (c *Coordinator) answer(ctx context.Context, session *runtimeSession, envelope protocol.Envelope) ([]protocol.Envelope, error) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !c.isCurrentSession(session) {
		return nil, commandError{"session_not_found"}
	}
	command, begin, err := c.beginCallCommand(ctx, envelope)
	if err != nil {
		return nil, err
	}
	if begin.Duplicate {
		return c.duplicateResult(command.CommandID, begin.Record), nil
	}
	session.mu.Lock()
	machine, media := session.activeCall, session.activeMedia
	if machine == nil || media == nil || machine.Snapshot().CallID != command.CallID {
		session.mu.Unlock()
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "call_not_found", c.now())
		return nil, commandError{"call_not_found"}
	}
	if _, transitionErr := machine.AnswerInbound(); transitionErr != nil {
		session.mu.Unlock()
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "call_state_conflict", c.now())
		return nil, commandError{"call_state_conflict"}
	}
	snapshot := machine.Snapshot()
	localSDP := append([]byte(nil), media.localSDP...)
	session.mu.Unlock()
	if err := c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID, snapshot, c.now()); err != nil {
		c.failCallBeforeAnswer(ctx, session, machine, command.CommandID, "call_persistence_failed")
		return nil, commandError{"call_persistence_failed"}
	}
	if _, err := c.emit(ctx, session, "call.state_changed", snapshot, "call-answering:"+command.CallID); err != nil {
		c.failCallBeforeAnswer(ctx, session, machine, command.CommandID, "event_outbox_failed")
		return nil, commandError{"event_outbox_failed"}
	}
	if err := c.sip.Answer(ctx, session.claims.SessionID, command.CallID, localSDP); err != nil {
		machine.RemoteHangup(call.EndSIPFailed, c.now())
		failed := machine.Snapshot()
		c.closeCallMedia(session, command.CallID)
		_ = c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID, failed, c.now())
		_ = c.emitCallEnded(ctx, session, failed)
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "sip_answer_failed", c.now())
		return nil, commandError{"sip_answer_failed"}
	}
	return c.finishCommand(ctx, command.CommandID, "call.answer")
}

func (c *Coordinator) reject(ctx context.Context, session *runtimeSession, envelope protocol.Envelope) ([]protocol.Envelope, error) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !c.isCurrentSession(session) {
		return nil, commandError{"session_not_found"}
	}
	command, begin, err := c.beginCallCommand(ctx, envelope)
	if err != nil {
		return nil, err
	}
	if begin.Duplicate {
		return c.duplicateResult(command.CommandID, begin.Record), nil
	}
	session.mu.Lock()
	machine := session.activeCall
	if machine == nil || machine.Snapshot().CallID != command.CallID || machine.Snapshot().Direction != call.Inbound {
		session.mu.Unlock()
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "call_not_found", c.now())
		return nil, commandError{"call_not_found"}
	}
	_, emitted := machine.BeginRejectInbound()
	ending := machine.Snapshot()
	session.mu.Unlock()
	if emitted {
		_ = c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID, ending, c.now())
		_, _ = c.emit(ctx, session, "call.state_changed", ending, "call-ending:"+command.CallID)
		sipErr := c.sip.Reject(ctx, session.claims.SessionID, command.CallID, 486)
		if machine.CompleteEnding(call.EndRejected, c.now()) {
			snapshot := machine.Snapshot()
			_ = c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID, snapshot, c.now())
			_ = c.emitCallEnded(ctx, session, snapshot)
		}
		c.closeCallMedia(session, command.CallID)
		if sipErr != nil {
			_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "sip_reject_failed", c.now())
			return nil, commandError{"sip_reject_failed"}
		}
	}
	return c.finishCommand(ctx, command.CommandID, "call.reject")
}

func (c *Coordinator) sendDTMF(ctx context.Context, session *runtimeSession, envelope protocol.Envelope) ([]protocol.Envelope, error) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !c.isCurrentSession(session) {
		return nil, commandError{"session_not_found"}
	}
	command, err := protocol.DecodePayload[protocol.DTMF](envelope)
	if err != nil || protocol.ValidateDTMF(command) != nil {
		return nil, commandError{"invalid_dtmf_command"}
	}
	begin, err := c.store.BeginCommand(ctx, command.CommandID, envelope.Payload, c.now())
	if err != nil {
		return nil, commandError{"command_journal_failed"}
	}
	if begin.Conflict {
		return nil, commandError{"command_duplicate_conflict"}
	}
	if begin.Duplicate {
		return c.duplicateResult(command.CommandID, begin.Record), nil
	}
	session.mu.Lock()
	machine, media := session.activeCall, session.activeMedia
	if machine == nil || media == nil || machine.Snapshot().CallID != command.CallID || machine.Snapshot().State != call.StateConnected {
		session.mu.Unlock()
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "call_not_connected", c.now())
		return nil, commandError{"call_not_connected"}
	}
	packets, buildErr := gatewaymedia.BuildDTMF(command.Digit[0], media.sipDTMFPayload, media.dtmfSSRC, media.dtmfSequence, media.dtmfTimestamp)
	if buildErr == nil {
		media.dtmfSequence += uint16(len(packets))
		media.dtmfTimestamp += 1600
	}
	callID := media.callID
	session.mu.Unlock()
	if buildErr != nil {
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "invalid_dtmf_command", c.now())
		return nil, commandError{"invalid_dtmf_command"}
	}
	for _, packet := range packets {
		if err := c.media.Write(callID, packet); err != nil {
			_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "dtmf_send_failed", c.now())
			return nil, commandError{"dtmf_send_failed"}
		}
	}
	return c.finishCommand(ctx, command.CommandID, "call.dtmf")
}

func (c *Coordinator) beginCallCommand(ctx context.Context, envelope protocol.Envelope) (protocol.CallCommand, store.BeginCommandResult, error) {
	command, err := protocol.DecodePayload[protocol.CallCommand](envelope)
	if err != nil || protocol.ValidateCallCommand(command) != nil {
		return protocol.CallCommand{}, store.BeginCommandResult{}, commandError{"invalid_call_command"}
	}
	begin, err := c.store.BeginCommand(ctx, command.CommandID, envelope.Payload, c.now())
	if err != nil {
		return protocol.CallCommand{}, store.BeginCommandResult{}, commandError{"command_journal_failed"}
	}
	if begin.Conflict {
		return protocol.CallCommand{}, store.BeginCommandResult{}, commandError{"command_duplicate_conflict"}
	}
	return command, begin, nil
}

func (c *Coordinator) finishCommand(ctx context.Context, commandID, commandType string) ([]protocol.Envelope, error) {
	result := map[string]string{"commandId": commandID, "commandType": commandType, "status": "completed"}
	raw, _ := json.Marshal(result)
	if err := c.store.FinishCommand(ctx, commandID, store.CommandCompleted, raw, "", c.now()); err != nil {
		return nil, commandError{"command_journal_failed"}
	}
	event, _ := protocol.NewEvent("command_result", result, c.now())
	return []protocol.Envelope{event}, nil
}

func (c *Coordinator) duplicateResult(commandID string, record store.CommandRecord) []protocol.Envelope {
	payload := map[string]any{
		"commandId": commandID, "status": string(record.Status), "duplicate": true,
	}
	if len(record.Result) > 0 {
		payload["recordedResult"] = json.RawMessage(record.Result)
	}
	event, _ := protocol.NewEvent("command_result", payload, c.now())
	return []protocol.Envelope{event}
}

func (c *Coordinator) pumpSIPEvents() {
	for {
		select {
		case <-c.closed:
			return
		case event, ok := <-c.sip.Events():
			if !ok {
				return
			}
			c.applySIPEvent(event)
		}
	}
}

func (c *Coordinator) applySIPEvent(event sip.Event) {
	session, err := c.session(event.SessionID)
	if err != nil {
		return
	}
	switch event.Type {
	case sip.EventRegistering, sip.EventRegistrationRetrying, sip.EventRegistered,
		sip.EventUnregistered, sip.EventRegistrationFailed:
		session.callMu.Lock()
		if !c.isCurrentSession(session) {
			session.callMu.Unlock()
			return
		}
		session.mu.Lock()
		session.registration = string(event.Type)
		state := session.registration
		session.mu.Unlock()
		_, eventErr := c.emit(context.Background(), session, "registration.state_changed", map[string]any{
			"state": state, "sipStatusCode": event.StatusCode,
		}, "")
		session.callMu.Unlock()
		if eventErr != nil {
			c.logger.Error("persist registration state event failed", "error", eventErr)
			c.terminateSession(session, call.EndAPIUnavailable)
		}
	case sip.EventIncoming:
		c.applyIncomingCall(session, event)
	case sip.EventTrying, sip.EventRinging, sip.EventEarlyMedia, sip.EventConnected,
		sip.EventEnded, sip.EventFailed:
		c.applyCallEvent(session, event)
	}
}

func (c *Coordinator) applyIncomingCall(session *runtimeSession, event sip.Event) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !c.isCurrentSession(session) {
		return
	}
	codec, err := c.rtc.Codec(event.SessionID)
	if err != nil {
		c.logger.Warn("inbound SIP call rejected", "callId", event.CallID,
			"reason", "webrtc_codec_unavailable", "sipStatusCode", 480)
		_ = c.sip.Reject(context.Background(), event.SessionID, event.CallID, 480)
		return
	}
	remote, err := sip.ParseRemoteSDPForCodec(event.RemoteSDP, codec.Name)
	if err != nil {
		reason := "invalid_sdp"
		if errors.Is(err, sip.ErrNoCodecIntersection) {
			reason = "no_codec_intersection"
		}
		c.logger.Warn("inbound SIP call rejected", "callId", event.CallID,
			"reason", reason, "sipStatusCode", 488,
			"webrtcCodec", codec.Name)
		_ = c.sip.Reject(context.Background(), event.SessionID, event.CallID, 488)
		return
	}
	c.logger.Debug("inbound SIP media negotiated", "callId", event.CallID,
		"codec", remote.Codec, "audioPayloadType", remote.AudioPayloadType,
		"dtmfPayloadType", remote.DTMFPayloadType)
	if state, stateErr := c.rtc.State(event.SessionID); stateErr != nil || state != pion.PeerConnectionStateConnected {
		c.logger.Warn("inbound SIP call rejected", "callId", event.CallID,
			"reason", "webrtc_not_ready", "sipStatusCode", 480)
		_ = c.sip.Reject(context.Background(), event.SessionID, event.CallID, 480)
		return
	}
	session.mu.Lock()
	if !session.connected || (session.activeCall != nil && session.activeCall.Snapshot().State != call.StateEnded) {
		session.mu.Unlock()
		c.logger.Warn("inbound SIP call rejected", "callId", event.CallID,
			"reason", "active_call_or_session_unavailable", "sipStatusCode", 486)
		_ = c.sip.Reject(context.Background(), event.SessionID, event.CallID, 486)
		return
	}
	machine, _ := call.NewInbound(event.CallID, c.now())
	created := machine.Snapshot()
	if err := machine.MarkIncoming(); err != nil {
		session.mu.Unlock()
		c.logger.Error("inbound SIP call rejected", "callId", event.CallID,
			"reason", "call_state_invalid", "sipStatusCode", 500)
		_ = c.sip.Reject(context.Background(), event.SessionID, event.CallID, 500)
		return
	}
	session.activeCall = machine
	session.remoteParty = event.RemoteParty
	session.callContext = ""
	session.callCommand = ""
	session.mu.Unlock()
	if !c.reserveCallOwner(event.CallID, event.SessionID) {
		session.mu.Lock()
		session.activeCall = nil
		session.mu.Unlock()
		c.logger.Warn("inbound SIP call rejected", "callId", event.CallID,
			"reason", "call_capacity_exhausted", "sipStatusCode", 486)
		_ = c.sip.Reject(context.Background(), event.SessionID, event.CallID, 486)
		return
	}
	if _, err := c.setupCallMedia(session, event.CallID, &remote); err != nil {
		c.logger.Warn("inbound SIP call rejected", "callId", event.CallID,
			"reason", "media_setup_failed", "sipStatusCode", 488)
		machine.RemoteHangup(call.EndMediaFailed, c.now())
		_ = c.sip.Reject(context.Background(), event.SessionID, event.CallID, 488)
		c.closeCallMedia(session, event.CallID)
		return
	}
	if err := c.media.SetRemote(event.CallID,
		&net.UDPAddr{IP: net.ParseIP(remote.IP), Port: int(remote.Port)},
		&net.UDPAddr{IP: net.ParseIP(remote.RTCPIP), Port: int(remote.RTCPPort)}); err != nil {
		c.logger.Warn("inbound SIP call rejected", "callId", event.CallID,
			"reason", "media_remote_invalid", "sipStatusCode", 488)
		machine.RemoteHangup(call.EndMediaFailed, c.now())
		_ = c.sip.Reject(context.Background(), event.SessionID, event.CallID, 488)
		c.closeCallMedia(session, event.CallID)
		return
	}
	incoming := machine.Snapshot()
	if err := c.store.SaveCall(context.Background(), event.SessionID, session.claims.AgentInstanceID, incoming, c.now()); err != nil {
		c.logger.Error("persist inbound call failed", "callId", event.CallID,
			"reason", "call_store_unavailable")
		machine.RemoteHangup(call.EndAPIUnavailable, c.now())
		c.discardCall(session, machine)
		_ = c.sip.Reject(context.Background(), event.SessionID, event.CallID, 500)
		c.closeCallMedia(session, event.CallID)
		return
	}
	payload := map[string]any{
		"callId": created.CallID, "direction": created.Direction, "state": created.State,
		"createdAtUtc": created.CreatedAt, "revision": created.Revision,
		"remoteParty": event.RemoteParty,
	}
	if _, err := c.emit(context.Background(), session, "call.created", payload, "call-created:"+event.CallID); err != nil {
		c.logger.Error("persist inbound call.created event failed", "callId", event.CallID,
			"reason", "event_outbox_unavailable")
		machine.RemoteHangup(call.EndAPIUnavailable, c.now())
		terminal := machine.Snapshot()
		_ = c.store.SaveCall(context.Background(), event.SessionID, session.claims.AgentInstanceID, terminal, c.now())
		_ = c.sip.Reject(context.Background(), event.SessionID, event.CallID, 500)
		c.closeCallMedia(session, event.CallID)
		return
	}
	if _, err := c.emit(context.Background(), session, "call.state_changed", incoming, "call-incoming:"+event.CallID); err != nil {
		c.logger.Error("persist inbound incoming event failed", "callId", event.CallID,
			"reason", "event_outbox_unavailable")
		machine.RemoteHangup(call.EndAPIUnavailable, c.now())
		terminal := machine.Snapshot()
		_ = c.store.SaveCall(context.Background(), event.SessionID, session.claims.AgentInstanceID, terminal, c.now())
		_ = c.emitCallEnded(context.Background(), session, terminal)
		_ = c.sip.Reject(context.Background(), event.SessionID, event.CallID, 500)
		c.closeCallMedia(session, event.CallID)
	}
}

func (c *Coordinator) applyCallEvent(session *runtimeSession, event sip.Event) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !c.isCurrentSession(session) {
		return
	}
	session.mu.Lock()
	machine := session.activeCall
	if machine == nil || machine.Snapshot().CallID != event.CallID {
		session.mu.Unlock()
		return
	}
	eventType := "call.state_changed"
	closeMedia := false
	hangupSIP := false
	previousRevision := machine.Snapshot().Revision
	switch event.Type {
	case sip.EventTrying:
		if err := machine.MarkTrying(); err != nil {
			session.mu.Unlock()
			return
		}
	case sip.EventRinging:
		if err := machine.MarkRinging(); err != nil {
			session.mu.Unlock()
			return
		}
	case sip.EventEarlyMedia:
		if err := machine.MarkRinging(); err != nil {
			session.mu.Unlock()
			return
		}
		media := session.activeMedia
		if media == nil || media.callID != event.CallID {
			c.logger.Warn("outbound SIP early media ignored", "callId", event.CallID,
				"reason", "media_not_ready", "sipStatusCode", event.StatusCode)
			break
		}
		remote, err := sip.ParseRemoteSDPForCodec(event.RemoteSDP, media.codec.Name)
		if err != nil {
			c.logger.Warn("outbound SIP early media ignored", "callId", event.CallID,
				"reason", "invalid_sdp", "sipStatusCode", event.StatusCode)
			break
		}
		if err := c.media.SetRemote(event.CallID,
			&net.UDPAddr{IP: net.ParseIP(remote.IP), Port: int(remote.Port)},
			&net.UDPAddr{IP: net.ParseIP(remote.RTCPIP), Port: int(remote.RTCPPort)}); err != nil {
			c.logger.Warn("outbound SIP early media ignored", "callId", event.CallID,
				"reason", "media_remote_invalid", "sipStatusCode", event.StatusCode)
			break
		}
		session.earlyMediaCallID = event.CallID
		session.earlyMediaStatusCode = event.StatusCode
		c.logger.Info("outbound SIP early media activated", "callId", event.CallID,
			"sipStatusCode", event.StatusCode, "codec", remote.Codec)
	case sip.EventConnected:
		hadEarlyMedia := session.earlyMediaCallID == event.CallID
		session.earlyMediaCallID = ""
		session.earlyMediaAudibleCallID = ""
		session.earlyMediaStatusCode = 0
		media := session.activeMedia
		snapshot := machine.Snapshot()
		mediaReady := media != nil && media.callID == event.CallID
		if mediaReady && snapshot.Direction == call.Outbound {
			if len(event.RemoteSDP) == 0 {
				mediaReady = hadEarlyMedia
			} else {
				remote, err := sip.ParseRemoteSDPForCodec(event.RemoteSDP, media.codec.Name)
				mediaReady = err == nil && c.media.SetRemote(event.CallID,
					&net.UDPAddr{IP: net.ParseIP(remote.IP), Port: int(remote.Port)},
					&net.UDPAddr{IP: net.ParseIP(remote.RTCPIP), Port: int(remote.RTCPPort)}) == nil
			}
		}
		// Inbound RTP/RTCP and codec were validated and bound from the INVITE in
		// applyIncomingCall. A UAS 200 response does not contain a new remote SDP,
		// so parsing event.RemoteSDP here would incorrectly terminalize every
		// answered inbound call as media_failed.
		if !mediaReady {
			machine.RemoteHangup(call.EndMediaFailed, c.now())
			eventType = "call.ended"
			closeMedia = true
			hangupSIP = true
		} else {
			if err := machine.Connected(c.now()); err != nil {
				session.mu.Unlock()
				return
			}
		}
	case sip.EventEnded:
		machine.RemoteHangup(endReason(event.Reason), c.now())
		eventType = "call.ended"
		closeMedia = true
	case sip.EventFailed:
		machine.RemoteHangup(failedEndReason(event.Reason), c.now())
		eventType = "call.ended"
		closeMedia = true
	}
	snapshot := machine.Snapshot()
	session.mu.Unlock()
	if snapshot.Revision == previousRevision {
		return
	}
	if err := c.store.SaveCall(context.Background(), session.claims.SessionID,
		session.claims.AgentInstanceID, snapshot, c.now()); err != nil {
		c.logger.Error("persist asynchronous call state failed", "callId", event.CallID,
			"state", snapshot.State, "reason", "call_store_unavailable")
	}
	if eventType == "call.ended" {
		if err := c.emitCallEnded(context.Background(), session, snapshot); err != nil {
			c.logger.Error("persist asynchronous call ended event failed", "callId", event.CallID,
				"reason", "event_outbox_unavailable")
		}
	} else {
		if _, err := c.emit(context.Background(), session, eventType, snapshot, ""); err != nil {
			c.logger.Error("persist asynchronous call state event failed", "callId", event.CallID,
				"state", snapshot.State, "reason", "event_outbox_unavailable")
		}
	}
	if hangupSIP {
		_ = c.sip.Hangup(context.Background(), session.claims.SessionID, event.CallID)
	}
	if closeMedia {
		c.closeCallMedia(session, event.CallID)
	}
}

func (c *Coordinator) setupCallMedia(
	session *runtimeSession, callID string, sipOffer *sip.MediaDescription,
) ([]byte, error) {
	codec, err := c.rtc.Codec(session.claims.SessionID)
	if err != nil {
		return nil, err
	}
	sipAudioPayload := uint8(0)
	switch codec.Name {
	case "PCMU":
		sipAudioPayload = 0
	case "PCMA":
		sipAudioPayload = 8
	default:
		return nil, errors.New("negotiated WebRTC codec is not supported by SIP P1")
	}
	sipDTMFPayload := uint8(101)
	if sipOffer != nil {
		if sipOffer.Codec != codec.Name || sipOffer.AudioPayloadType != sipAudioPayload {
			return nil, errors.New("SIP offer does not contain the negotiated WebRTC codec")
		}
		sipDTMFPayload = sipOffer.DTMFPayloadType
	}
	local, err := c.media.Allocate(callID)
	if err != nil {
		return nil, err
	}
	sipSSRC, browserSSRC := randomUint32(), randomUint32()
	dtmfSSRC := randomUint32()
	for dtmfSSRC == sipSSRC {
		dtmfSSRC = randomUint32()
	}
	toSIP, err := gatewaymedia.NewTranslator(gatewaymedia.Mapping{
		SourceAudioPayloadType: codec.AudioPayloadType, TargetAudioPayloadType: sipAudioPayload,
		SourceDTMFPayloadType: codec.DTMFPayloadType, TargetDTMFPayloadType: sipDTMFPayload,
		TargetSSRC: sipSSRC, InitialSequence: randomUint16(), InitialTimestamp: randomUint32(),
	})
	if err != nil {
		_ = c.media.CloseCall(callID)
		return nil, err
	}
	toBrowser, err := gatewaymedia.NewTranslator(gatewaymedia.Mapping{
		SourceAudioPayloadType: sipAudioPayload, TargetAudioPayloadType: codec.AudioPayloadType,
		SourceDTMFPayloadType: sipDTMFPayload, TargetDTMFPayloadType: codec.DTMFPayloadType,
		TargetSSRC: browserSSRC, InitialSequence: randomUint16(), InitialTimestamp: randomUint32(),
	})
	if err != nil {
		_ = c.media.CloseCall(callID)
		return nil, err
	}
	localSDP, err := (sip.AsteriskProfile{}).BuildSDP(sip.MediaDescription{
		IP: local.IP.String(), Port: uint16(local.Port), RTCPIP: local.IP.String(),
		RTCPPort: uint16(local.Port + 1), AudioPayloadType: sipAudioPayload,
		DTMFPayloadType: sipDTMFPayload, Codec: codec.Name,
	})
	if err != nil {
		_ = c.media.CloseCall(callID)
		return nil, err
	}
	session.mu.Lock()
	session.activeMedia = &callMedia{
		callID: callID, codec: codec, localSDP: localSDP, toSIP: toSIP, toBrowser: toBrowser,
		sipAudioPayload: sipAudioPayload, sipDTMFPayload: sipDTMFPayload,
		dtmfSSRC: dtmfSSRC, dtmfSequence: randomUint16(), dtmfTimestamp: randomUint32(),
		browserLoss: make(map[uint32]uint32), pbxLoss: make(map[uint32]uint32),
		toSIPPacket:     rtp.Packet{Payload: make([]byte, 0, 160)},
		toBrowserPacket: rtp.Packet{Payload: make([]byte, 0, 160)},
	}
	session.mu.Unlock()
	return append([]byte(nil), localSDP...), nil
}

func (c *Coordinator) closeCallMedia(session *runtimeSession, callID string) {
	session.mu.Lock()
	if session.activeMedia != nil && session.activeMedia.callID == callID {
		session.activeMedia = nil
	}
	if session.earlyMediaCallID == callID || session.earlyMediaAudibleCallID == callID {
		session.earlyMediaCallID = ""
		session.earlyMediaAudibleCallID = ""
		session.earlyMediaStatusCode = 0
	}
	session.mu.Unlock()
	_ = c.media.CloseCall(callID)
	c.mu.Lock()
	delete(c.callOwners, callID)
	c.mu.Unlock()
}

func (c *Coordinator) discardCall(session *runtimeSession, machine *call.Machine) {
	session.mu.Lock()
	if session.activeCall == machine {
		session.activeCall = nil
	}
	session.mu.Unlock()
	c.releaseCallOwner(machine.Snapshot().CallID)
}

func (c *Coordinator) failCallBeforeInvite(
	ctx context.Context,
	session *runtimeSession,
	machine *call.Machine,
	commandID string,
	errorCode string,
	lifecycleStarted bool,
) {
	machine.RemoteHangup(call.EndAPIUnavailable, c.now())
	snapshot := machine.Snapshot()
	_ = c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID, snapshot, c.now())
	if lifecycleStarted {
		_ = c.emitCallEnded(ctx, session, snapshot)
	}
	c.closeCallMedia(session, snapshot.CallID)
	_ = c.store.FinishCommand(ctx, commandID, store.CommandFailed, nil, errorCode, c.now())
}

func (c *Coordinator) failCallBeforeAnswer(
	ctx context.Context,
	session *runtimeSession,
	machine *call.Machine,
	commandID string,
	errorCode string,
) {
	machine.RemoteHangup(call.EndAPIUnavailable, c.now())
	snapshot := machine.Snapshot()
	_ = c.store.SaveCall(ctx, session.claims.SessionID, session.claims.AgentInstanceID, snapshot, c.now())
	_ = c.emitCallEnded(ctx, session, snapshot)
	_ = c.sip.Reject(ctx, session.claims.SessionID, snapshot.CallID, 500)
	c.closeCallMedia(session, snapshot.CallID)
	_ = c.store.FinishCommand(ctx, commandID, store.CommandFailed, nil, errorCode, c.now())
}

func (c *Coordinator) reserveCallOwner(callID, sessionID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.callOwners[callID]; exists || len(c.callOwners) >= c.cfg.MaxCalls {
		return false
	}
	c.callOwners[callID] = sessionID
	return true
}

func (c *Coordinator) releaseCallOwner(callID string) {
	c.mu.Lock()
	delete(c.callOwners, callID)
	c.mu.Unlock()
}

func randomUint32() uint32 {
	var raw [4]byte
	_, _ = rand.Read(raw[:])
	value := binary.BigEndian.Uint32(raw[:])
	if value == 0 {
		return 1
	}
	return value
}

func randomUint16() uint16 {
	var raw [2]byte
	_, _ = rand.Read(raw[:])
	return binary.BigEndian.Uint16(raw[:])
}

func endReason(reason string) call.EndReason {
	switch reason {
	case "cancelled":
		return call.EndCancelled
	case "busy":
		return call.EndBusy
	case "no_answer":
		return call.EndNoAnswer
	default:
		return call.EndRemoteHangup
	}
}

func failedEndReason(reason string) call.EndReason {
	switch reason {
	case "cancelled":
		return call.EndCancelled
	case "busy":
		return call.EndBusy
	case "rejected":
		return call.EndRejected
	case "no_answer":
		return call.EndNoAnswer
	default:
		return call.EndSIPFailed
	}
}

func (c *Coordinator) monitorLease(sessionID, connectionCapabilityID string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-c.closed:
			return
		}
		session, err := c.session(sessionID)
		if err != nil {
			return
		}
		now := c.now()
		session.mu.Lock()
		if session.claims.CapabilityID != connectionCapabilityID {
			session.mu.Unlock()
			return
		}
		expires := session.leaseExpires
		session.mu.Unlock()
		if !now.Before(expires) {
			c.observeLease("expired")
			c.terminateSession(session, call.EndLeaseRevoked)
			return
		}
	}
}

func (c *Coordinator) renewSession(
	ctx context.Context,
	session *runtimeSession,
	connection Claims,
	envelope protocol.Envelope,
) ([]protocol.Envelope, error) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !c.isCurrentSession(session) {
		return nil, commandError{"session_not_found"}
	}
	command, err := protocol.DecodePayload[protocol.SessionRenew](envelope)
	if err != nil || protocol.ValidateSessionRenew(command) != nil {
		return nil, commandError{"invalid_session_renewal"}
	}
	begin, err := c.store.BeginCommand(ctx, command.CommandID, envelope.Payload, c.now())
	if err != nil {
		return nil, commandError{"command_journal_failed"}
	}
	if begin.Conflict {
		return nil, commandError{"command_duplicate_conflict"}
	}
	if begin.Duplicate {
		return c.duplicateResult(command.CommandID, begin.Record), nil
	}
	session.mu.Lock()
	if !sameConnectionClaims(session.claims, connection) {
		session.mu.Unlock()
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "session_connection_stale", c.now())
		return nil, commandError{"session_connection_stale"}
	}
	current := session.claims
	currentSIP := session.sipConfig
	session.mu.Unlock()
	verified, err := c.capability.VerifySession(command.SessionCapability, current.CollectionID)
	if err != nil || verified.SessionID != current.SessionID || verified.UserSessionID != current.UserSessionID ||
		verified.CollectionBindingID != current.CollectionBindingID || verified.AgentInstanceID != current.AgentInstanceID {
		c.observeCapability("session_renewal", "rejected")
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "session_renewal_rejected", c.now())
		return nil, commandError{"session_renewal_rejected"}
	}
	expiresAt := time.Unix(verified.ExpiresAt, 0).UTC()
	consumed, err := c.store.ConsumeCapability(ctx, verified.CapabilityID, expiresAt, c.now().UTC())
	if err != nil || !consumed {
		c.observeCapability("session_renewal", "replayed")
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "session_renewal_rejected", c.now())
		return nil, commandError{"session_renewal_rejected"}
	}
	leaseExpires := time.Unix(verified.LeaseExpiresAt, 0).UTC()
	renewedSIP := sip.Configuration{
		Host: verified.SIP.Host, Port: uint16(verified.SIP.Port),
		Transport: sip.Transport(verified.SIP.Transport), Username: verified.SIP.Username,
		Password: verified.SIP.Password, Realm: verified.SIP.Realm,
	}
	if !sameSIPConfiguration(currentSIP, renewedSIP) {
		c.observeCapability("session_renewal", "rejected")
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "session_sip_binding_changed", c.now())
		return nil, commandError{"session_sip_binding_changed"}
	}
	if err := c.store.BindSession(ctx, current.SessionID, current.AgentInstanceID,
		current.UserSessionID, current.CollectionID, current.CollectionBindingID, c.now()); err != nil {
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil,
			"session_binding_persistence_failed", c.now())
		return nil, commandError{"session_binding_persistence_failed"}
	}
	if _, err := c.turn.ExtendSession(current.SessionID, leaseExpires, c.now()); err != nil {
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "session_renewal_rejected", c.now())
		return nil, commandError{"session_renewal_rejected"}
	}
	session.mu.Lock()
	if !sameConnectionClaims(session.claims, connection) || !sameSIPConfiguration(session.sipConfig, currentSIP) {
		session.mu.Unlock()
		_ = c.store.FinishCommand(ctx, command.CommandID, store.CommandFailed, nil, "session_connection_stale", c.now())
		return nil, commandError{"session_connection_stale"}
	}
	session.claims.ExpiresAtUTC = leaseExpires
	session.leaseExpires = leaseExpires
	session.mu.Unlock()
	c.observeCapability("session_renewal", "accepted")
	c.observeLease("refreshed")
	return c.finishCommand(ctx, command.CommandID, "session.renew")
}

func sameSIPConfiguration(left, right sip.Configuration) bool {
	return left.Host == right.Host && left.Port == right.Port && left.Transport == right.Transport &&
		left.Username == right.Username && left.Password == right.Password && left.Realm == right.Realm
}

func sameConnectionClaims(left, right Claims) bool {
	return left.CapabilityID == right.CapabilityID && left.SessionID == right.SessionID &&
		left.UserSessionID == right.UserSessionID && left.CollectionID == right.CollectionID &&
		left.CollectionBindingID == right.CollectionBindingID && left.AgentInstanceID == right.AgentInstanceID
}

func sameSessionIdentity(left, right Claims) bool {
	return left.SessionID == right.SessionID && left.UserSessionID == right.UserSessionID &&
		left.CollectionID == right.CollectionID && left.CollectionBindingID == right.CollectionBindingID &&
		left.AgentInstanceID == right.AgentInstanceID
}

func (c *Coordinator) observeCommand(commandType, outcome string) {
	if c.observer != nil {
		c.observer.ObserveCommand(commandType, outcome)
	}
}

func (c *Coordinator) observeCapability(kind, outcome string) {
	if c.observer != nil {
		c.observer.ObserveCapability(kind, outcome)
	}
}

func (c *Coordinator) observeLease(outcome string) {
	if c.observer != nil {
		c.observer.ObserveLease(outcome)
	}
}

func (c *Coordinator) observeRTPPacket(direction string) {
	if c.observer != nil {
		c.observer.ObserveRTPPacket(direction)
	}
}

func (c *Coordinator) observeRTPBridgeError(direction string) {
	if c.observer != nil {
		c.observer.ObserveRTPBridgeError(direction)
	}
}

func (c *Coordinator) observeRTCPQuality(media *callMedia, direction string, packets []rtcp.Packet) {
	if c.observer == nil {
		return
	}
	media.qualityMu.Lock()
	last := media.browserLoss
	if direction == "browser_to_pbx" {
		last = media.pbxLoss
	}
	for _, packet := range packets {
		var reports []rtcp.ReceptionReport
		switch value := packet.(type) {
		case *rtcp.SenderReport:
			reports = value.Reports
		case *rtcp.ReceiverReport:
			reports = value.Reports
		}
		for _, report := range reports {
			previous := last[report.SSRC]
			lost := report.TotalLost
			if lost >= previous {
				lost -= previous
			}
			last[report.SSRC] = report.TotalLost
			c.observer.ObserveRTPQuality(direction, lost, report.Jitter)
		}
	}
	media.qualityMu.Unlock()
}

func (c *Coordinator) terminateSession(session *runtimeSession, reason call.EndReason) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !c.isCurrentSession(session) {
		return
	}
	session.mu.Lock()
	machine := session.activeCall
	var snapshot call.Snapshot
	if machine != nil && machine.RemoteHangup(reason, c.now()) {
		snapshot = machine.Snapshot()
	}
	session.registration = "idle"
	session.mu.Unlock()
	if snapshot.CallID != "" {
		_ = c.store.SaveCall(context.Background(), session.claims.SessionID, session.claims.AgentInstanceID, snapshot, c.now())
		_ = c.emitCallEnded(context.Background(), session, snapshot)
		_ = c.sip.Hangup(context.Background(), session.claims.SessionID, snapshot.CallID)
		c.closeCallMedia(session, snapshot.CallID)
	}
	_ = c.sip.Unregister(context.Background(), session.claims.SessionID)
	c.removeSessionLocked(session.claims.SessionID, session)
}

func (c *Coordinator) emitCallEnded(ctx context.Context, session *runtimeSession, snapshot call.Snapshot) error {
	session.eventMu.Lock()
	defer session.eventMu.Unlock()
	if _, err := c.emitLocked(ctx, session, "call.state_changed", snapshot, "call-ended-state:"+snapshot.CallID); err != nil {
		return err
	}
	_, err := c.emitLocked(ctx, session, "call.ended", snapshot, "call-ended:"+snapshot.CallID)
	return err
}

func (c *Coordinator) emit(ctx context.Context, session *runtimeSession, eventType string, payload any, eventKey string) (protocol.Envelope, error) {
	session.eventMu.Lock()
	defer session.eventMu.Unlock()
	return c.emitLocked(ctx, session, eventType, payload, eventKey)
}

func (c *Coordinator) emitLocked(ctx context.Context, session *runtimeSession, eventType string, payload any, eventKey string) (protocol.Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return protocol.Envelope{}, err
	}
	stored, err := c.store.AppendEvent(ctx, session.claims.SessionID, session.claims.AgentInstanceID, eventKey, eventType, raw, c.now())
	if err != nil {
		return protocol.Envelope{}, err
	}
	if snapshot, ok := payload.(call.Snapshot); ok && c.observer != nil {
		switch {
		case eventType == "call.state_changed" && snapshot.State == call.StateConnected &&
			snapshot.ConnectedAt != nil:
			c.observer.ObserveCallConnected(string(snapshot.Direction),
				snapshot.ConnectedAt.Sub(snapshot.CreatedAt))
		case eventType == "call.ended":
			c.observer.ObserveCallEnded(string(snapshot.Direction), string(snapshot.EndReason))
		}
	}
	envelope, err := replayEnvelope(stored)
	if err == nil {
		c.publish(session.claims.SessionID, envelope)
	}
	return envelope, err
}

func replayEnvelope(event store.Event) (protocol.Envelope, error) {
	var data any
	if err := json.Unmarshal(event.Payload, &data); err != nil {
		return protocol.Envelope{}, err
	}
	payload := map[string]any{"sequence": event.Sequence, "data": data}
	if event.AgentInstanceID != "" {
		payload["agentInstanceId"] = event.AgentInstanceID
	}
	if event.SessionID != "" {
		payload["agentSessionId"] = event.SessionID
	}
	if event.CollectionID != "" {
		payload["collectionId"] = event.CollectionID
	}
	if event.CollectionBindingID != "" {
		payload["collectionBindingId"] = event.CollectionBindingID
	}
	envelope, err := protocol.NewEvent(event.Type, payload, event.CreatedAt)
	if err != nil {
		return protocol.Envelope{}, err
	}
	// The business event ID must survive reconnect and process restart. Deriving it
	// from the durable outbox identity keeps replay idempotent without another secret.
	envelope.MessageID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf(
		"%s:%d:%s", event.SessionID, event.Sequence, event.Type))).String()
	return envelope, nil
}

func (c *Coordinator) publish(sessionID string, event protocol.Envelope) {
	session, err := c.session(sessionID)
	if err != nil {
		return
	}
	session.mu.Lock()
	live := session.live
	if live == nil {
		session.mu.Unlock()
		return
	}
	select {
	case live <- event:
		session.mu.Unlock()
	default:
		// Never silently drop signaling. Closing the stream forces a reconnect,
		// where durable events are replayed and WebRTC is negotiated afresh.
		close(live)
		session.live = nil
		session.connected = false
		session.mu.Unlock()
		c.logger.Warn("runtime live event queue exhausted; reconnect required", "sessionId", sessionID, "eventType", event.Type)
	}
}

func (c *Coordinator) publishCallEarlyMedia(session *runtimeSession, callID string, statusCode int) {
	envelope, err := protocol.NewEvent("call.early_media", map[string]any{
		"callId": callID, "sipStatusCode": statusCode,
	}, c.now())
	if err != nil {
		return
	}
	c.publish(session.claims.SessionID, envelope)
}

func (c *Coordinator) closeLiveStream(session *runtimeSession, expected chan protocol.Envelope) {
	session.mu.Lock()
	if session.live == expected && expected != nil {
		close(expected)
		session.live = nil
		session.connected = false
	}
	session.mu.Unlock()
}

func (c *Coordinator) snapshotLocked(session *runtimeSession) map[string]any {
	value := map[string]any{
		"sessionId": session.claims.SessionID, "collectionId": session.claims.CollectionID,
		"registration": session.registration, "leaseExpiresAtUtc": session.leaseExpires,
	}
	if session.activeCall != nil {
		snapshot := session.activeCall.Snapshot()
		if snapshot.State != call.StateEnded {
			value["call"] = snapshot
			value["earlyMedia"] = session.earlyMediaAudibleCallID == snapshot.CallID
			if session.remoteParty != "" {
				value["remoteParty"] = session.remoteParty
			}
			if session.callContext != "" {
				value["callContextId"] = session.callContext
			}
			if session.callCommand != "" {
				value["commandId"] = session.callCommand
			}
		}
	}
	return value
}

func callEventPayload(snapshot call.Snapshot, remoteParty, callContextID, commandID string) map[string]any {
	payload := map[string]any{
		"callId": snapshot.CallID, "direction": snapshot.Direction, "state": snapshot.State,
		"createdAtUtc": snapshot.CreatedAt, "revision": snapshot.Revision,
	}
	if remoteParty != "" {
		payload["remoteParty"] = remoteParty
	}
	if callContextID != "" {
		payload["callContextId"] = callContextID
	}
	if commandID != "" {
		payload["commandId"] = commandID
	}
	return payload
}

func (c *Coordinator) session(sessionID string) (*runtimeSession, error) {
	c.mu.RLock()
	session := c.sessions[sessionID]
	c.mu.RUnlock()
	if session == nil {
		return nil, fmt.Errorf("runtime session %s not found", sessionID)
	}
	return session, nil
}

func (c *Coordinator) isCurrentSession(target *runtimeSession) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, session := range c.sessions {
		if session == target {
			return true
		}
	}
	return false
}

func (c *Coordinator) removeSession(sessionID string) {
	c.mu.RLock()
	session := c.sessions[sessionID]
	c.mu.RUnlock()
	if session == nil {
		return
	}
	session.callMu.Lock()
	defer session.callMu.Unlock()
	c.removeSessionLocked(sessionID, session)
}

// removeSessionLocked retires a session atomically with the call lifecycle.
// The caller must hold session.callMu so no command or SIP event can enter
// between terminalization and removal from the authoritative registry.
func (c *Coordinator) removeSessionLocked(sessionID string, session *runtimeSession) {
	c.mu.Lock()
	if c.sessions[sessionID] != session {
		c.mu.Unlock()
		return
	}
	delete(c.sessions, sessionID)
	if c.sipOwners[session.sipConfig.Username] == sessionID {
		delete(c.sipOwners, session.sipConfig.Username)
	}
	for callID, owner := range c.callOwners {
		if owner == sessionID {
			delete(c.callOwners, callID)
		}
	}
	c.mu.Unlock()
	// The session is no longer discoverable, so no new event producer can start.
	// Wait for an in-flight durable append before a replacement session performs
	// its agent-wide replay, otherwise the event could fall between the query and
	// the retired live stream.
	session.eventMu.Lock()
	session.eventMu.Unlock()
	session.mu.Lock()
	if session.live != nil {
		close(session.live)
		session.live = nil
	}
	session.connected = false
	session.mu.Unlock()
	c.turn.RevokeSession(sessionID)
	_ = c.rtc.CloseSession(sessionID)
	_ = c.sip.CloseSession(context.Background(), sessionID)
	session.mu.Lock()
	if session.activeMedia != nil {
		_ = c.media.CloseCall(session.activeMedia.callID)
		session.activeMedia = nil
	}
	session.sipConfig.Password = ""
	session.mu.Unlock()
}
