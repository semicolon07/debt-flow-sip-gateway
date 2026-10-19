package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debt-flow/sip-gateway/internal/call"
	"github.com/debt-flow/sip-gateway/internal/capability"
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

type fakeCapabilityVerifier struct {
	destination  string
	userSession  string
	sessionID    string
	sipBootstrap capability.SIPBootstrap
	mu           sync.Mutex
	redeems      int
}

type recordingObserver struct {
	mu             sync.Mutex
	connected      int
	ended          int
	packets        map[string]int
	qualityReports map[string]int
	bridgeErrors   map[string]int
}

func (*recordingObserver) ObserveCommand(string, string)    {}
func (*recordingObserver) ObserveCapability(string, string) {}
func (*recordingObserver) ObserveLease(string)              {}

func (o *recordingObserver) ObserveCallConnected(string, time.Duration) {
	o.mu.Lock()
	o.connected++
	o.mu.Unlock()
}

func (o *recordingObserver) ObserveCallEnded(string, string) {
	o.mu.Lock()
	o.ended++
	o.mu.Unlock()
}

func (o *recordingObserver) ObserveRTPPacket(direction string) {
	o.mu.Lock()
	o.packets[direction]++
	o.mu.Unlock()
}

func (o *recordingObserver) ObserveRTPQuality(direction string, _, _ uint32) {
	o.mu.Lock()
	o.qualityReports[direction]++
	o.mu.Unlock()
}

func (o *recordingObserver) ObserveRTPBridgeError(direction string) {
	o.mu.Lock()
	o.bridgeErrors[direction]++
	o.mu.Unlock()
}

func (f *fakeCapabilityVerifier) VerifySession(_ string, collectionID string) (capability.SessionClaims, error) {
	f.mu.Lock()
	f.redeems++
	capabilityID := fmt.Sprintf("capability-%d", f.redeems)
	userSession := f.userSession
	if userSession == "" {
		userSession = "user-session-1"
	}
	sessionID := f.sessionID
	if sessionID == "" {
		sessionID = "session-1"
	}
	sipBootstrap := f.sipBootstrap
	if sipBootstrap.Host == "" {
		sipBootstrap = capability.SIPBootstrap{
			Host: "pbx.internal", Port: 5060, Transport: "udp",
			Username: "100", Password: "password",
		}
	}
	f.mu.Unlock()
	return capability.SessionClaims{
		CapabilityID: capabilityID, SessionID: sessionID, CollectionID: collectionID,
		CollectionBindingID: "phonebind-1", AgentInstanceID: "agent-1",
		UserSessionID: userSession, ExpiresAt: time.Now().Add(time.Minute).Unix(),
		LeaseExpiresAt: time.Now().Add(2 * time.Minute).Unix(),
		SIP:            sipBootstrap,
	}, nil
}

func TestCoordinatorRejectsSessionIDReuseAcrossUserSessions(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator := newTestCoordinator(t, verifier, newFakeSIP())
	authentication := protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	}
	if _, err := coordinator.Redeem(context.Background(), authentication); err != nil {
		t.Fatal(err)
	}
	verifier.mu.Lock()
	verifier.userSession = "attacker-user-session"
	verifier.mu.Unlock()
	if _, err := coordinator.Redeem(context.Background(), authentication); err == nil {
		t.Fatal("gateway session ID was reused across different authenticated user sessions")
	}
}

func TestCoordinatorEnforcesSessionCapacityBeforeWebRTCCreation(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator := newTestCoordinator(t, verifier, newFakeSIP())
	coordinator.cfg.MaxSessions = 1
	authentication := protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	}
	if _, err := coordinator.Redeem(context.Background(), authentication); err != nil {
		t.Fatal(err)
	}
	verifier.mu.Lock()
	verifier.sessionID = "session-2"
	verifier.sipBootstrap = capability.SIPBootstrap{
		Host: "pbx.internal", Port: 5060, Transport: "udp", Username: "101", Password: "password",
	}
	verifier.mu.Unlock()
	if _, err := coordinator.Redeem(context.Background(), authentication); err == nil {
		t.Fatal("session over the configured capacity was accepted")
	}
	if stats := coordinator.Stats(); stats.AuthenticatedSessions != 1 {
		t.Fatalf("authenticated sessions=%d, want 1", stats.AuthenticatedSessions)
	}
}

func TestCoordinatorAtomicallyTakesOverAnIdleSIPIdentity(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	sipRuntime := newFakeSIP()
	coordinator := newTestCoordinator(t, verifier, sipRuntime)
	authentication := protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	}
	first, err := coordinator.Redeem(context.Background(), authentication)
	if err != nil {
		t.Fatal(err)
	}
	_, firstLive, err := coordinator.Connect(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	verifier.mu.Lock()
	verifier.sessionID = "session-2"
	verifier.userSession = "user-session-2"
	verifier.mu.Unlock()
	second, err := coordinator.Redeem(context.Background(), authentication)
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID == first.SessionID {
		t.Fatal("expected a new logical session")
	}
	select {
	case _, open := <-firstLive:
		if open {
			t.Fatal("superseded runtime stream remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("superseded runtime stream was not closed")
	}
	if _, err := coordinator.session(first.SessionID); err == nil {
		t.Fatal("superseded SIP identity remained addressable")
	}
	sipRuntime.mu.Lock()
	defer sipRuntime.mu.Unlock()
	if len(sipRuntime.closed) != 1 || sipRuntime.closed[0] != first.SessionID {
		t.Fatalf("unexpected closed SIP sessions: %v", sipRuntime.closed)
	}
}

func TestCoordinatorResumesSessionAndIgnoresStaleDisconnect(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator := newTestCoordinator(t, verifier, newFakeSIP())
	authentication := protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	}
	first, err := coordinator.Redeem(context.Background(), authentication)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.Redeem(context.Background(), authentication)
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID != second.SessionID || first.CapabilityID == second.CapabilityID {
		t.Fatalf("unexpected resumed claims: first=%+v second=%+v", first, second)
	}
	if _, _, err := coordinator.Connect(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: second.SessionID})
	coordinator.Disconnect(first)
	if !coordinator.CanAcceptIncoming(second.SessionID) {
		t.Fatal("stale disconnect marked the resumed session offline")
	}
}

func TestCoordinatorKeepsExistingConnectionUntilResumeCommits(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator := newTestCoordinator(t, verifier, newFakeSIP())
	authentication := protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	}
	stale, err := coordinator.Redeem(context.Background(), authentication)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	current, err := coordinator.Redeem(context.Background(), authentication)
	if err != nil {
		t.Fatal(err)
	}
	envelope := protocol.Envelope{Type: "state.get", Payload: json.RawMessage(`{}`)}
	if _, err := coordinator.Handle(context.Background(), stale, envelope); err != nil {
		t.Fatalf("healthy connection was invalidated before replacement committed: %v", err)
	}
	if _, _, err := coordinator.Connect(context.Background(), current); err != nil {
		t.Fatalf("current connection was rejected: %v", err)
	}
	if _, err := coordinator.Handle(context.Background(), stale, envelope); err == nil {
		t.Fatal("replaced connection was allowed to issue a command")
	}
	if _, err := coordinator.Handle(context.Background(), current, envelope); err != nil {
		t.Fatalf("current connection command was rejected: %v", err)
	}
}

func TestCoordinatorRejectsSIPBindingChangeOnSessionResume(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator := newTestCoordinator(t, verifier, newFakeSIP())
	authentication := protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	}
	if _, err := coordinator.Redeem(context.Background(), authentication); err != nil {
		t.Fatal(err)
	}
	verifier.mu.Lock()
	verifier.sipBootstrap = capability.SIPBootstrap{
		Host: "pbx.internal", Port: 5060, Transport: "udp",
		Username: "100", Password: "rotated-password",
	}
	verifier.mu.Unlock()
	if _, err := coordinator.Redeem(context.Background(), authentication); err == nil {
		t.Fatal("expected SIP binding change on a logical session to be rejected")
	}
}

func TestCoordinatorRejectsSIPBindingChangeOnLeaseRenewal(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator := newTestCoordinator(t, verifier, newFakeSIP())
	authentication := protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	}
	claims, err := coordinator.Redeem(context.Background(), authentication)
	if err != nil {
		t.Fatal(err)
	}
	verifier.mu.Lock()
	verifier.sipBootstrap = capability.SIPBootstrap{
		Host: "pbx.internal", Port: 5060, Transport: "tcp",
		Username: "100", Password: "password",
	}
	verifier.mu.Unlock()
	payload, _ := json.Marshal(protocol.SessionRenew{
		CommandID: uuid.NewString(), SessionCapability: "renewed-capability",
	})
	_, err = coordinator.Handle(context.Background(), claims,
		protocol.Envelope{Type: "session.renew", Payload: payload})
	var coded interface{ Code() string }
	if !errors.As(err, &coded) || coded.Code() != "session_sip_binding_changed" {
		t.Fatalf("expected immutable SIP binding error, got %v", err)
	}
}

func TestCoordinatorRejectsLeaseRenewalFromAReplacedConnection(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator := newTestCoordinator(t, verifier, newFakeSIP())
	authentication := protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	}
	stale, err := coordinator.Redeem(context.Background(), authentication)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	current, err := coordinator.Redeem(context.Background(), authentication)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.session(current.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(protocol.SessionRenew{
		CommandID: uuid.NewString(), SessionCapability: "renewed-capability",
	})
	_, err = coordinator.renewSession(context.Background(), session, stale,
		protocol.Envelope{Type: "session.renew", Payload: payload})
	var coded interface{ Code() string }
	if !errors.As(err, &coded) || coded.Code() != "session_connection_stale" {
		t.Fatalf("expected stale connection renewal error, got %v", err)
	}
}

func TestCoordinatorRenewsTheCurrentConnectionLease(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator := newTestCoordinator(t, verifier, newFakeSIP())
	authentication := protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	}
	claims, err := coordinator.Redeem(context.Background(), authentication)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.session(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	before := session.leaseExpires
	session.mu.Unlock()
	time.Sleep(1100 * time.Millisecond)
	payload, _ := json.Marshal(protocol.SessionRenew{
		CommandID: uuid.NewString(), SessionCapability: "renewed-capability",
	})
	events, err := coordinator.Handle(context.Background(), claims,
		protocol.Envelope{Type: "session.renew", Payload: payload})
	if err != nil || len(events) != 1 || events[0].Type != "command_result" {
		t.Fatalf("unexpected renewal result: events=%+v err=%v", events, err)
	}
	session.mu.Lock()
	after := session.leaseExpires
	session.mu.Unlock()
	if !after.After(before) {
		t.Fatalf("lease was not extended: before=%s after=%s", before, after)
	}
}

func TestReplayEnvelopeKeepsStableBusinessEventID(t *testing.T) {
	stored := store.Event{
		SessionID: "session-1", Sequence: 7, Type: "call.state_changed",
		Payload: []byte(`{"callId":"call-1"}`), CreatedAt: time.Now().UTC(),
	}
	first, err := replayEnvelope(stored)
	if err != nil {
		t.Fatal(err)
	}
	second, err := replayEnvelope(stored)
	if err != nil {
		t.Fatal(err)
	}
	if first.MessageID != second.MessageID {
		t.Fatalf("replay changed event identity: %s != %s", first.MessageID, second.MessageID)
	}
}

func (f *fakeCapabilityVerifier) VerifyCall(_ string, binding capability.CallBinding) (capability.CallClaims, error) {
	return capability.CallClaims{
		CapabilityID: "call-capability-" + binding.CallID, Destination: f.destination,
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}, nil
}

type fakeRTC struct {
	mu              sync.Mutex
	writes          []*rtp.Packet
	rtcp            []rtcp.Packet
	connectionState pion.PeerConnectionState
	codec           gatewaywebrtc.CodecInfo
	offerCodec      string
}

func (f *fakeRTC) WriteRTCP(_ string, packets []rtcp.Packet) error {
	f.mu.Lock()
	f.rtcp = append(f.rtcp, packets...)
	f.mu.Unlock()
	return nil
}

func (*fakeRTC) Open(string, gatewaywebrtc.TURNCredential) error { return nil }
func (f *fakeRTC) AcceptOffer(_, _, preferredCodec string) (string, error) {
	f.mu.Lock()
	f.offerCodec = preferredCodec
	f.mu.Unlock()
	return "answer", nil
}
func (*fakeRTC) AddCandidate(string, pion.ICECandidateInit) error { return nil }
func (f *fakeRTC) WriteRTP(_ string, packet *rtp.Packet) error {
	f.mu.Lock()
	f.writes = append(f.writes, packet)
	f.mu.Unlock()
	return nil
}
func (f *fakeRTC) Codec(string) (gatewaywebrtc.CodecInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.codec.Name != "" {
		return f.codec, nil
	}
	return gatewaywebrtc.CodecInfo{Name: "PCMU", AudioPayloadType: 0, DTMFPayloadType: 101}, nil
}

func (f *fakeRTC) State(string) (pion.PeerConnectionState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connectionState != pion.PeerConnectionStateUnknown {
		return f.connectionState, nil
	}
	return pion.PeerConnectionStateConnected, nil
}
func (*fakeRTC) CloseSession(string) error { return nil }

type fakeMedia struct {
	mu         sync.Mutex
	remote     *net.UDPAddr
	remoteRTCP *net.UDPAddr
	writes     []*rtp.Packet
	rtcp       []rtcp.Packet
	closed     []string
}

func (f *fakeMedia) WriteRTCP(_ string, packets []rtcp.Packet) error {
	f.mu.Lock()
	f.rtcp = append(f.rtcp, packets...)
	f.mu.Unlock()
	return nil
}

func (*fakeMedia) Allocate(string) (*net.UDPAddr, error) {
	return &net.UDPAddr{IP: net.ParseIP("10.0.0.10"), Port: 20000}, nil
}
func (f *fakeMedia) SetRemote(_ string, remoteRTP, remoteRTCP *net.UDPAddr) error {
	f.mu.Lock()
	f.remote = remoteRTP
	f.remoteRTCP = remoteRTCP
	f.mu.Unlock()
	return nil
}
func (f *fakeMedia) Write(_ string, packet *rtp.Packet) error {
	f.mu.Lock()
	f.writes = append(f.writes, packet)
	f.mu.Unlock()
	return nil
}
func (f *fakeMedia) CloseCall(callID string) error {
	f.mu.Lock()
	f.closed = append(f.closed, callID)
	f.mu.Unlock()
	return nil
}

type fakeSIP struct {
	mu                sync.Mutex
	events            chan sip.Event
	destination       string
	starts            int
	answers           int
	hangups           int
	rejects           []int
	closed            []string
	localSDP          []byte
	startErr          error
	answerEntered     chan struct{}
	answerRelease     chan struct{}
	hangupEntered     chan struct{}
	hangupRelease     chan struct{}
	unregisterEntered chan struct{}
	unregisterRelease chan struct{}
}

func newFakeSIP() *fakeSIP                                                  { return &fakeSIP{events: make(chan sip.Event, 8)} }
func (f *fakeSIP) Events() <-chan sip.Event                                 { return f.events }
func (*fakeSIP) Configure(context.Context, string, sip.Configuration) error { return nil }
func (*fakeSIP) Register(context.Context, string) error                     { return nil }
func (f *fakeSIP) Unregister(context.Context, string) error {
	f.mu.Lock()
	entered := f.unregisterEntered
	release := f.unregisterRelease
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	return nil
}
func (f *fakeSIP) StartCall(_ context.Context, _, _, destination string, localSDP []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destination = destination
	f.starts++
	f.localSDP = append([]byte(nil), localSDP...)
	return f.startErr
}
func (f *fakeSIP) Answer(_ context.Context, _, _ string, localSDP []byte) error {
	f.mu.Lock()
	f.answers++
	f.localSDP = append([]byte(nil), localSDP...)
	entered := f.answerEntered
	release := f.answerRelease
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	return nil
}
func (f *fakeSIP) Reject(_ context.Context, _, _ string, status int) error {
	f.mu.Lock()
	f.rejects = append(f.rejects, status)
	f.mu.Unlock()
	return nil
}
func (f *fakeSIP) Hangup(context.Context, string, string) error {
	f.mu.Lock()
	f.hangups++
	entered := f.hangupEntered
	release := f.hangupRelease
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	return nil
}
func (*fakeSIP) SendRTP(context.Context, string, string, *rtp.Packet) error { return nil }
func (*fakeSIP) SendDTMF(context.Context, string, string, byte) error       { return nil }
func (f *fakeSIP) CloseSession(_ context.Context, sessionID string) error {
	f.mu.Lock()
	f.closed = append(f.closed, sessionID)
	f.mu.Unlock()
	return nil
}
func (*fakeSIP) Close() error { return nil }

func newTestCoordinator(t *testing.T, verifier *fakeCapabilityVerifier, sipRuntime *fakeSIP) *Coordinator {
	coordinator, _, _ := newTestCoordinatorWithMedia(t, verifier, sipRuntime)
	return coordinator
}

func newTestCoordinatorWithMedia(t *testing.T, verifier *fakeCapabilityVerifier, sipRuntime *fakeSIP) (*Coordinator, *fakeRTC, *fakeMedia) {
	coordinator, rtc, media, _ := newTestCoordinatorWithStore(t, verifier, sipRuntime)
	return coordinator, rtc, media
}

func newTestCoordinatorWithStore(t *testing.T, verifier *fakeCapabilityVerifier, sipRuntime *fakeSIP) (*Coordinator, *fakeRTC, *fakeMedia, *store.Store) {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "gateway.db"), store.Limits{
		PerSession: 100, Global: 1000, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := gatewayturn.NewCredentials(2 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rtc := &fakeRTC{}
	media := &fakeMedia{}
	coordinator, err := NewCoordinator(CoordinatorConfig{
		NodeID: "node-1", MaxSessions: 100, MaxCalls: 25, TURNURLs: []string{"turn:10.0.0.10:3478"},
		LeaseRenewEvery: time.Hour, LeaseExpiresAfter: 2 * time.Minute,
		ReconnectGrace: time.Minute,
	}, verifier, database, credentials, rtc, media, sipRuntime, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = coordinator.Close()
		_ = database.Close()
	})
	return coordinator, rtc, media, database
}

func TestRegistrationStateChangesUseDurableOutbox(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator, _, _, database := newTestCoordinatorWithStore(t, verifier, newFakeSIP())
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID, StatusCode: 200})
	events, err := database.PendingEvents(context.Background(), claims.SessionID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "registration.state_changed" {
		t.Fatalf("registration event was not durable: %+v", events)
	}
}

func TestCoordinatorConfirmsDurableEventACKOnlyAfterStoreCommit(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator, _, _, database := newTestCoordinatorWithStore(t, verifier, newFakeSIP())
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventRegistered, SessionID: claims.SessionID, StatusCode: 200,
	})
	commandID := uuid.NewString()
	payload, err := json.Marshal(protocol.EventsACK{CommandID: commandID, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	events, err := coordinator.Handle(context.Background(), claims, protocol.Envelope{
		Type: "events.ack", Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "command_result" {
		t.Fatalf("unexpected ACK result: %+v", events)
	}
	var result struct {
		CommandID   string `json:"commandId"`
		CommandType string `json:"commandType"`
		Status      string `json:"status"`
		Sequence    uint64 `json:"sequence"`
	}
	if err := json.Unmarshal(events[0].Payload, &result); err != nil {
		t.Fatal(err)
	}
	if result.CommandID != commandID || result.CommandType != "events.ack" ||
		result.Status != "completed" || result.Sequence != 1 {
		t.Fatalf("unexpected ACK payload: %+v", result)
	}
	pending, err := database.PendingEvents(context.Background(), claims.SessionID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("ACK result was returned before outbox commit: %+v", pending)
	}
	initial, _, err := coordinator.Connect(context.Background(), claims)
	if err != nil {
		t.Fatal(err)
	}
	var welcome struct {
		AcknowledgedThrough uint64 `json:"acknowledgedThrough"`
	}
	if err := json.Unmarshal(initial[0].Payload, &welcome); err != nil {
		t.Fatal(err)
	}
	if welcome.AcknowledgedThrough != 1 {
		t.Fatalf("reconnect lost committed ACK cursor: %+v", welcome)
	}
}

func TestCoordinatorPreservesActiveCallCodecDuringWebRTCReconnect(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator, rtc, _ := newTestCoordinatorWithMedia(t, verifier, newFakeSIP())
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.session(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.activeMedia = &callMedia{codec: gatewaywebrtc.CodecInfo{Name: "PCMA"}}
	session.mu.Unlock()
	payload, err := json.Marshal(protocol.WebRTCOffer{SDP: "offer"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := coordinator.Handle(context.Background(), claims, protocol.Envelope{
		Type: "webrtc.offer", Payload: payload,
	})
	if err != nil || len(events) != 1 || events[0].Type != "webrtc.answer" {
		t.Fatalf("unexpected WebRTC answer: events=%+v err=%v", events, err)
	}
	rtc.mu.Lock()
	preferred := rtc.offerCodec
	rtc.mu.Unlock()
	if preferred != "PCMA" {
		t.Fatalf("reconnect preferred codec=%q, want PCMA", preferred)
	}
}

func TestCoordinatorAcceptsLegacyDurableEventACKWithoutResult(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator := newTestCoordinator(t, verifier, newFakeSIP())
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := coordinator.store.AppendEvent(context.Background(), claims.SessionID,
		claims.AgentInstanceID, "legacy-ack", "registration.state_changed", []byte(`{"state":"registered"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(protocol.EventsACK{Sequence: stored.Sequence})
	if err != nil {
		t.Fatal(err)
	}
	events, err := coordinator.Handle(context.Background(), claims, protocol.Envelope{
		Type: "events.ack", Payload: payload,
	})
	if err != nil || len(events) != 0 {
		t.Fatalf("legacy ACK changed behavior: events=%+v err=%v", events, err)
	}
}

func TestCoordinatorBridgesOutboundEarlyMediaBeforeAnswer(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator, rtc, media := newTestCoordinatorWithMedia(t, api, newFakeSIP())
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, live, err := coordinator.Connect(context.Background(), claims)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
	callID := uuid.NewString()
	startPayload, _ := json.Marshal(protocol.StartCall{
		CommandID: uuid.NewString(), CallID: callID, CallContextID: "context-1", GatewayCapability: "call-capability",
	})
	if _, err := coordinator.Handle(context.Background(), claims,
		protocol.Envelope{Type: "call.start", Payload: startPayload}); err != nil {
		t.Fatal(err)
	}
	remoteSDP := []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 0 101\r\na=rtcp:30500 IN IP4 10.0.0.21\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\n")
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventEarlyMedia, SessionID: claims.SessionID, CallID: callID,
		StatusCode: 183, RemoteSDP: remoteSDP,
	})
	foundEarlyMediaEvent := false
	deadline := time.After(2 * time.Second)
	for !foundEarlyMediaEvent {
		select {
		case event := <-live:
			foundEarlyMediaEvent = event.Type == "call.early_media"
		case <-deadline:
			t.Fatal("timed out waiting for live call.early_media event")
		}
	}
	coordinator.OnPBXRTP(callID, &rtp.Packet{
		Header: rtp.Header{Version: 2, PayloadType: 0, SSRC: 22}, Payload: []byte{3, 4},
	})
	coordinator.OnBrowserRTP(claims.SessionID, &rtp.Packet{
		Header: rtp.Header{Version: 2, PayloadType: 0, SSRC: 11}, Payload: []byte{1, 2},
	})

	media.mu.Lock()
	remote := media.remote
	remoteRTCP := media.remoteRTCP
	browserWriteCount := len(media.writes)
	media.mu.Unlock()
	if remote == nil || remote.Port != 20500 || remoteRTCP == nil || remoteRTCP.Port != 30500 {
		t.Fatalf("early media remote RTP=%v RTCP=%v", remote, remoteRTCP)
	}
	if browserWriteCount != 0 {
		t.Fatalf("browser microphone leaked %d RTP packets before answer", browserWriteCount)
	}
	rtc.mu.Lock()
	writeCount := len(rtc.writes)
	rtc.mu.Unlock()
	if writeCount != 1 {
		t.Fatalf("early media WebRTC writes=%d, want 1", writeCount)
	}
	updatedSDP := []byte("v=0\r\nc=IN IP4 10.0.0.22\r\nm=audio 20600 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\n")
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventEarlyMedia, SessionID: claims.SessionID, CallID: callID,
		StatusCode: 183, RemoteSDP: updatedSDP,
	})
	media.mu.Lock()
	remote = media.remote
	media.mu.Unlock()
	if remote == nil || remote.Port != 20600 {
		t.Fatalf("updated early media remote=%v, want port 20600", remote)
	}
	select {
	case event := <-live:
		if event.Type == "call.early_media" {
			t.Fatal("duplicate early media activation event was published")
		}
	default:
	}
	session, err := coordinator.session(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	snapshot := coordinator.snapshotLocked(session)
	state := session.activeCall.Snapshot().State
	earlyMediaCallID := session.earlyMediaCallID
	session.mu.Unlock()
	if state != call.StateRinging || earlyMediaCallID != callID || snapshot["earlyMedia"] != true {
		t.Fatalf("early media state=%s callID=%q snapshot=%+v", state, earlyMediaCallID, snapshot)
	}

	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventConnected, SessionID: claims.SessionID, CallID: callID,
	})
	session.mu.Lock()
	state = session.activeCall.Snapshot().State
	earlyMediaCallID = session.earlyMediaCallID
	session.mu.Unlock()
	if state != call.StateConnected || earlyMediaCallID != "" {
		t.Fatalf("answered early media state=%s earlyMediaCallID=%q", state, earlyMediaCallID)
	}
	coordinator.OnBrowserRTP(claims.SessionID, &rtp.Packet{
		Header: rtp.Header{Version: 2, PayloadType: 0, SSRC: 11}, Payload: []byte{1, 2},
	})
	media.mu.Lock()
	writeCount = len(media.writes)
	media.mu.Unlock()
	if writeCount != 1 {
		t.Fatalf("connected browser RTP writes=%d, want 1", writeCount)
	}
}

func TestCoordinatorIgnoresMalformedEarlyMediaWithoutEndingCall(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator, _, media := newTestCoordinatorWithMedia(t, api, newFakeSIP())
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
	callID := uuid.NewString()
	startPayload, _ := json.Marshal(protocol.StartCall{
		CommandID: uuid.NewString(), CallID: callID, CallContextID: "context-1", GatewayCapability: "call-capability",
	})
	if _, err := coordinator.Handle(context.Background(), claims,
		protocol.Envelope{Type: "call.start", Payload: startPayload}); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventEarlyMedia, SessionID: claims.SessionID, CallID: callID,
		StatusCode: 183, RemoteSDP: []byte("not-sdp"),
	})

	session, err := coordinator.session(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	state := session.activeCall.Snapshot().State
	earlyMediaCallID := session.earlyMediaCallID
	mediaPresent := session.activeMedia != nil
	session.mu.Unlock()
	media.mu.Lock()
	remote := media.remote
	media.mu.Unlock()
	if state != call.StateRinging || earlyMediaCallID != "" || !mediaPresent || remote != nil {
		t.Fatalf("malformed early media state=%s callID=%q media=%v remote=%v",
			state, earlyMediaCallID, mediaPresent, remote)
	}

	validSDP := []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\n")
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventEarlyMedia, SessionID: claims.SessionID, CallID: callID,
		StatusCode: 183, RemoteSDP: validSDP,
	})
	session.mu.Lock()
	earlyMediaCallID = session.earlyMediaCallID
	session.mu.Unlock()
	media.mu.Lock()
	remote = media.remote
	media.mu.Unlock()
	if earlyMediaCallID != callID || remote == nil || remote.Port != 20500 {
		t.Fatalf("recovered early media callID=%q remote=%v", earlyMediaCallID, remote)
	}

	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventFailed, SessionID: claims.SessionID, CallID: callID,
		StatusCode: 603, Reason: "rejected",
	})
	session.mu.Lock()
	state = session.activeCall.Snapshot().State
	earlyMediaCallID = session.earlyMediaCallID
	mediaPresent = session.activeMedia != nil
	session.mu.Unlock()
	if state != call.StateEnded || earlyMediaCallID != "" || mediaPresent {
		t.Fatalf("terminal early media state=%s callID=%q media=%v",
			state, earlyMediaCallID, mediaPresent)
	}
}

func TestCoordinatorBridgesRTPAndSendsDTMF(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	sipRuntime := newFakeSIP()
	coordinator, rtc, media := newTestCoordinatorWithMedia(t, api, sipRuntime)
	observer := &recordingObserver{
		packets: make(map[string]int), qualityReports: make(map[string]int),
		bridgeErrors: make(map[string]int),
	}
	coordinator.SetObserver(observer)
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
	callID := uuid.NewString()
	startPayload, _ := json.Marshal(protocol.StartCall{
		CommandID: uuid.NewString(), CallID: callID, CallContextID: "context-1", GatewayCapability: "call-capability",
	})
	if _, err := coordinator.Handle(context.Background(), claims, protocol.Envelope{Type: "call.start", Payload: startPayload}); err != nil {
		t.Fatal(err)
	}
	remoteSDP := []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 0 101\r\na=rtcp:30500 IN IP4 10.0.0.21\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\n")
	coordinator.applySIPEvent(sip.Event{Type: sip.EventConnected, SessionID: claims.SessionID, CallID: callID, RemoteSDP: remoteSDP})

	coordinator.OnBrowserRTP(claims.SessionID, &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SSRC: 11}, Payload: []byte{1, 2}})
	coordinator.OnPBXRTP(callID, &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SSRC: 22}, Payload: []byte{3, 4}})
	coordinator.OnBrowserRTCP(claims.SessionID, []rtcp.Packet{&rtcp.ReceiverReport{
		SSRC: 11, Reports: []rtcp.ReceptionReport{{SSRC: rtc.writes[0].SSRC, TotalLost: 2, Jitter: 80}},
	}})
	coordinator.OnPBXRTCP(callID, []rtcp.Packet{&rtcp.ReceiverReport{
		SSRC: 22, Reports: []rtcp.ReceptionReport{{SSRC: media.writes[0].SSRC, TotalLost: 1, Jitter: 40}},
	}})
	dtmfPayload, _ := json.Marshal(protocol.DTMF{CommandID: uuid.NewString(), CallID: callID, Digit: "#"})
	if _, err := coordinator.Handle(context.Background(), claims, protocol.Envelope{Type: "call.dtmf", Payload: dtmfPayload}); err != nil {
		t.Fatal(err)
	}
	hangupPayload, _ := json.Marshal(protocol.CallCommand{CommandID: uuid.NewString(), CallID: callID})
	if _, err := coordinator.Handle(context.Background(), claims,
		protocol.Envelope{Type: "call.hangup", Payload: hangupPayload}); err != nil {
		t.Fatal(err)
	}

	media.mu.Lock()
	defer media.mu.Unlock()
	if media.remote == nil || media.remote.Port != 20500 || media.remoteRTCP == nil ||
		media.remoteRTCP.Port != 30500 || !media.remoteRTCP.IP.Equal(net.ParseIP("10.0.0.21")) ||
		len(media.writes) != 8 || len(media.rtcp) != 1 {
		t.Fatalf("unexpected PBX media state: remote=%v rtcp=%v writes=%d",
			media.remote, media.remoteRTCP, len(media.writes))
	}
	if media.writes[0].SSRC == media.writes[1].SSRC {
		t.Fatal("concurrent RFC 4733 events must use a separate SSRC from the audio stream")
	}
	for _, packet := range media.writes[1:] {
		if packet.SSRC != media.writes[1].SSRC || packet.PayloadType != 101 {
			t.Fatalf("inconsistent DTMF stream: %+v", packet.Header)
		}
	}
	rtc.mu.Lock()
	defer rtc.mu.Unlock()
	if len(rtc.writes) != 1 || len(rtc.rtcp) != 1 || string(rtc.writes[0].Payload) != string([]byte{3, 4}) {
		t.Fatalf("unexpected browser RTP writes: %+v", rtc.writes)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.connected != 1 || observer.ended != 1 ||
		observer.packets["browser_to_pbx"] != 1 || observer.packets["pbx_to_browser"] != 1 ||
		observer.qualityReports["browser_to_pbx"] != 1 || observer.qualityReports["pbx_to_browser"] != 1 ||
		len(observer.bridgeErrors) != 0 {
		t.Fatalf("unexpected lifecycle/media observations: %+v", observer)
	}
}

func TestCoordinatorTerminatesTheSIPCallWhenWebRTCFails(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	sipRuntime := newFakeSIP()
	coordinator, rtc, _ := newTestCoordinatorWithMedia(t, api, sipRuntime)
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
	callID := uuid.NewString()
	payload, _ := json.Marshal(protocol.StartCall{
		CommandID: uuid.NewString(), CallID: callID, CallContextID: "context-1", GatewayCapability: "call-capability",
	})
	if _, err := coordinator.Handle(context.Background(), claims, protocol.Envelope{Type: "call.start", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	rtc.mu.Lock()
	rtc.connectionState = pion.PeerConnectionStateFailed
	rtc.mu.Unlock()
	coordinator.OnWebRTCState(claims.SessionID, pion.PeerConnectionStateFailed)
	session, err := coordinator.session(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	snapshot := session.activeCall.Snapshot()
	session.mu.Unlock()
	if snapshot.State != call.StateEnded || snapshot.EndReason != call.EndMediaFailed {
		t.Fatalf("unexpected terminal call snapshot: %+v", snapshot)
	}
	sipRuntime.mu.Lock()
	defer sipRuntime.mu.Unlock()
	if sipRuntime.hangups != 1 {
		t.Fatalf("WebRTC failure produced %d SIP hangups", sipRuntime.hangups)
	}
}

func TestCoordinatorAcceptsInboundOnlyForHealthyPortal(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	sipRuntime := newFakeSIP()
	coordinator, _, _, database := newTestCoordinatorWithStore(t, api, sipRuntime)
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
	if !coordinator.CanAcceptIncoming(claims.SessionID) {
		t.Fatal("expected healthy registered portal session to accept inbound calls")
	}
	callID := uuid.NewString()
	remoteSDP := []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\n")
	coordinator.applySIPEvent(sip.Event{Type: sip.EventIncoming, SessionID: claims.SessionID, CallID: callID, RemoteSDP: remoteSDP})
	answerPayload, _ := json.Marshal(protocol.CallCommand{CommandID: uuid.NewString(), CallID: callID})
	if _, err := coordinator.Handle(context.Background(), claims, protocol.Envelope{Type: "call.answer", Payload: answerPayload}); err != nil {
		t.Fatal(err)
	}
	events, err := database.PendingEvents(context.Background(), claims.SessionID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Type != "registration.state_changed" ||
		events[1].Type != "call.created" || events[2].Type != "call.state_changed" ||
		events[3].Type != "call.state_changed" {
		t.Fatalf("unexpected inbound event order: %+v", events)
	}
	var incoming call.Snapshot
	if err := json.Unmarshal(events[2].Payload, &incoming); err != nil {
		t.Fatal(err)
	}
	if incoming.State != call.StateIncoming {
		t.Fatalf("durable state=%q, want %q", incoming.State, call.StateIncoming)
	}
	var answering call.Snapshot
	if err := json.Unmarshal(events[3].Payload, &answering); err != nil {
		t.Fatal(err)
	}
	if answering.State != call.StateAnswering {
		t.Fatalf("durable state=%q, want %q", answering.State, call.StateAnswering)
	}
	sipRuntime.mu.Lock()
	defer sipRuntime.mu.Unlock()
	if sipRuntime.answers != 1 || len(sipRuntime.localSDP) == 0 {
		t.Fatalf("expected one SDP answer, got answers=%d sdp=%q", sipRuntime.answers, sipRuntime.localSDP)
	}
}

func TestCoordinatorSelectsInboundCodecIntersectionRegardlessOfOfferOrder(t *testing.T) {
	tests := []struct {
		name           string
		browserCodec   gatewaywebrtc.CodecInfo
		sipPayloads    string
		sipAttributes  string
		wantAnswerLine string
		wantDTMF       uint8
	}{
		{
			name: "browser PCMU with PBX preferring PCMA", browserCodec: gatewaywebrtc.CodecInfo{
				Name: "PCMU", AudioPayloadType: 0, DTMFPayloadType: 101,
			},
			sipPayloads: "8 0 126", sipAttributes: "a=rtpmap:8 PCMA/8000\r\n" +
				"a=rtpmap:0 PCMU/8000\r\na=rtpmap:126 telephone-event/8000\r\n",
			wantAnswerLine: "RTP/AVP 0 126", wantDTMF: 126,
		},
		{
			name: "browser PCMA with PBX preferring PCMU", browserCodec: gatewaywebrtc.CodecInfo{
				Name: "PCMA", AudioPayloadType: 8, DTMFPayloadType: 110,
			},
			sipPayloads: "0 8 112", sipAttributes: "a=rtpmap:0 PCMU/8000\r\n" +
				"a=rtpmap:8 PCMA/8000\r\na=rtpmap:112 telephone-event/8000\r\n",
			wantAnswerLine: "RTP/AVP 8 112", wantDTMF: 112,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
			sipRuntime := newFakeSIP()
			coordinator, rtc, _ := newTestCoordinatorWithMedia(t, api, sipRuntime)
			rtc.codec = test.browserCodec
			claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
				CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
				SupportedProtocolVersions: []int{1},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
				t.Fatal(err)
			}
			coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})

			callID := uuid.NewString()
			remoteSDP := []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP " +
				test.sipPayloads + "\r\n" + test.sipAttributes)
			coordinator.applySIPEvent(sip.Event{
				Type: sip.EventIncoming, SessionID: claims.SessionID, CallID: callID, RemoteSDP: remoteSDP,
			})
			answerPayload, _ := json.Marshal(protocol.CallCommand{CommandID: uuid.NewString(), CallID: callID})
			if _, err := coordinator.Handle(context.Background(), claims,
				protocol.Envelope{Type: "call.answer", Payload: answerPayload}); err != nil {
				t.Fatal(err)
			}

			sipRuntime.mu.Lock()
			answerSDP := string(sipRuntime.localSDP)
			rejects := append([]int(nil), sipRuntime.rejects...)
			sipRuntime.mu.Unlock()
			if len(rejects) != 0 {
				t.Fatalf("compatible inbound offer was rejected: %v", rejects)
			}
			if !strings.Contains(answerSDP, test.wantAnswerLine) {
				t.Fatalf("answer SDP %q does not contain %q", answerSDP, test.wantAnswerLine)
			}
			session, err := coordinator.session(claims.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			session.mu.Lock()
			media := session.activeMedia
			session.mu.Unlock()
			if media == nil || media.sipDTMFPayload != test.wantDTMF {
				t.Fatalf("SIP DTMF payload=%v, want %d", media, test.wantDTMF)
			}
		})
	}
}

func TestCoordinatorRejectsInboundWithoutCodecIntersection(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	sipRuntime := newFakeSIP()
	coordinator, _, _ := newTestCoordinatorWithMedia(t, api, sipRuntime)
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventIncoming, SessionID: claims.SessionID, CallID: uuid.NewString(),
		RemoteSDP: []byte("v=0\r\nc=IN IP4 10.0.0.20\r\n" +
			"m=audio 20500 RTP/AVP 8 101\r\na=rtpmap:8 PCMA/8000\r\n" +
			"a=rtpmap:101 telephone-event/8000\r\n"),
	})
	sipRuntime.mu.Lock()
	rejects := append([]int(nil), sipRuntime.rejects...)
	sipRuntime.mu.Unlock()
	if len(rejects) != 1 || rejects[0] != 488 {
		t.Fatalf("inbound without codec intersection rejects=%v, want [488]", rejects)
	}
}

func TestCoordinatorAnswersInboundWithExistingInviteMediaAndBridgesAudio(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	sipRuntime := newFakeSIP()
	coordinator, rtc, media, database := newTestCoordinatorWithStore(t, api, sipRuntime)
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})

	callID := uuid.NewString()
	remoteSDP := []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 0 101\r\na=rtcp:30500 IN IP4 10.0.0.21\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\n")
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventIncoming, SessionID: claims.SessionID, CallID: callID,
		RemoteSDP: remoteSDP, RemoteParty: "0812345678",
	})
	answerPayload, _ := json.Marshal(protocol.CallCommand{CommandID: uuid.NewString(), CallID: callID})
	if _, err := coordinator.Handle(context.Background(), claims,
		protocol.Envelope{Type: "call.answer", Payload: answerPayload}); err != nil {
		t.Fatal(err)
	}

	// SIPGoRuntime.Answer emits connected without RemoteSDP because the remote
	// offer was already supplied by the inbound INVITE.
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventConnected, SessionID: claims.SessionID, CallID: callID, StatusCode: 200,
	})

	session, err := coordinator.session(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	snapshot := session.activeCall.Snapshot()
	activeMedia := session.activeMedia
	session.mu.Unlock()
	if snapshot.State != call.StateConnected || snapshot.Direction != call.Inbound ||
		snapshot.ConnectedAt == nil || activeMedia == nil || activeMedia.callID != callID {
		t.Fatalf("unexpected connected inbound call: snapshot=%+v media=%+v", snapshot, activeMedia)
	}

	coordinator.OnBrowserRTP(claims.SessionID, &rtp.Packet{
		Header: rtp.Header{Version: 2, PayloadType: 0, SSRC: 11}, Payload: []byte{1, 2},
	})
	coordinator.OnPBXRTP(callID, &rtp.Packet{
		Header: rtp.Header{Version: 2, PayloadType: 0, SSRC: 22}, Payload: []byte{3, 4},
	})
	media.mu.Lock()
	if media.remote == nil || media.remote.Port != 20500 || media.remoteRTCP == nil ||
		media.remoteRTCP.Port != 30500 || len(media.writes) != 1 || len(media.closed) != 0 {
		media.mu.Unlock()
		t.Fatalf("unexpected inbound PBX media state: remote=%v rtcp=%v writes=%d closed=%v",
			media.remote, media.remoteRTCP, len(media.writes), media.closed)
	}
	media.mu.Unlock()
	rtc.mu.Lock()
	if len(rtc.writes) != 1 || string(rtc.writes[0].Payload) != string([]byte{3, 4}) {
		rtc.mu.Unlock()
		t.Fatalf("unexpected inbound browser RTP: %+v", rtc.writes)
	}
	rtc.mu.Unlock()

	events, err := database.PendingEvents(context.Background(), claims.SessionID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	states := make([]call.State, 0, 4)
	for _, event := range events {
		if event.Type != "call.created" && event.Type != "call.state_changed" {
			continue
		}
		var eventSnapshot call.Snapshot
		if err := json.Unmarshal(event.Payload, &eventSnapshot); err != nil {
			t.Fatal(err)
		}
		states = append(states, eventSnapshot.State)
	}
	want := []call.State{call.StateCreated, call.StateIncoming, call.StateAnswering, call.StateConnected}
	if fmt.Sprint(states) != fmt.Sprint(want) {
		t.Fatalf("inbound lifecycle=%v, want %v", states, want)
	}

	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventEnded, SessionID: claims.SessionID, CallID: callID, Reason: "remote_hangup",
	})
	session.mu.Lock()
	snapshot = session.activeCall.Snapshot()
	session.mu.Unlock()
	if snapshot.State != call.StateEnded || snapshot.EndReason != call.EndRemoteHangup {
		t.Fatalf("unexpected inbound terminal state: %+v", snapshot)
	}
	media.mu.Lock()
	defer media.mu.Unlock()
	if len(media.closed) != 1 || media.closed[0] != callID {
		t.Fatalf("inbound media close=%v, want [%s]", media.closed, callID)
	}
}

func TestCoordinatorSerializesRemoteCancelWithInboundAnswer(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	sipRuntime := newFakeSIP()
	sipRuntime.answerEntered = make(chan struct{}, 1)
	sipRuntime.answerRelease = make(chan struct{})
	coordinator, _, _, database := newTestCoordinatorWithStore(t, api, sipRuntime)
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
	callID := uuid.NewString()
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventIncoming, SessionID: claims.SessionID, CallID: callID,
		RemoteSDP:   []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\n"),
		RemoteParty: "0812345678",
	})
	answerPayload, _ := json.Marshal(protocol.CallCommand{CommandID: uuid.NewString(), CallID: callID})
	answerDone := make(chan error, 1)
	go func() {
		_, handleErr := coordinator.Handle(context.Background(), claims,
			protocol.Envelope{Type: "call.answer", Payload: answerPayload})
		answerDone <- handleErr
	}()
	select {
	case <-sipRuntime.answerEntered:
	case <-time.After(time.Second):
		t.Fatal("answer did not reach the SIP adapter")
	}

	cancelDone := make(chan struct{})
	go func() {
		coordinator.applySIPEvent(sip.Event{
			Type: sip.EventEnded, SessionID: claims.SessionID, CallID: callID, Reason: "cancelled",
		})
		close(cancelDone)
	}()
	select {
	case <-cancelDone:
		t.Fatal("remote CANCEL mutated the call while answer persistence was still in progress")
	case <-time.After(50 * time.Millisecond):
	}
	close(sipRuntime.answerRelease)
	select {
	case err := <-answerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("answer command did not complete")
	}
	select {
	case <-cancelDone:
	case <-time.After(time.Second):
		t.Fatal("remote CANCEL did not resume after answer persistence completed")
	}

	session, err := coordinator.session(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	snapshot := session.activeCall.Snapshot()
	session.mu.Unlock()
	if snapshot.State != call.StateEnded || snapshot.EndReason != call.EndCancelled {
		t.Fatalf("unexpected answer/CANCEL terminal state: %+v", snapshot)
	}
	events, err := database.PendingEvents(context.Background(), claims.SessionID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var states []call.State
	for _, event := range events {
		if event.Type != "call.state_changed" {
			continue
		}
		var eventSnapshot call.Snapshot
		if err := json.Unmarshal(event.Payload, &eventSnapshot); err != nil {
			t.Fatal(err)
		}
		states = append(states, eventSnapshot.State)
	}
	want := []call.State{call.StateIncoming, call.StateAnswering, call.StateEnded}
	if fmt.Sprint(states) != fmt.Sprint(want) {
		t.Fatalf("answer/CANCEL lifecycle=%v, want %v", states, want)
	}
}

func TestCoordinatorRemovesTerminatedSessionBeforeWaitingCommandCanRun(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	sipRuntime := newFakeSIP()
	sipRuntime.unregisterEntered = make(chan struct{}, 1)
	sipRuntime.unregisterRelease = make(chan struct{})
	coordinator := newTestCoordinator(t, api, sipRuntime)
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.session(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	terminated := make(chan struct{})
	go func() {
		coordinator.terminateSession(session, call.EndLeaseRevoked)
		close(terminated)
	}()
	select {
	case <-sipRuntime.unregisterEntered:
	case <-time.After(time.Second):
		t.Fatal("session termination did not reach SIP unregister")
	}

	payload, _ := json.Marshal(protocol.CommandIdentity{CommandID: uuid.NewString()})
	commandDone := make(chan error, 1)
	go func() {
		_, handleErr := coordinator.Handle(context.Background(), claims,
			protocol.Envelope{Type: "registration.start", Payload: payload})
		commandDone <- handleErr
	}()
	select {
	case err := <-commandDone:
		t.Fatalf("command entered a terminating session: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(sipRuntime.unregisterRelease)
	select {
	case <-terminated:
	case <-time.After(time.Second):
		t.Fatal("session termination did not complete")
	}
	select {
	case err := <-commandDone:
		var commandFailure commandError
		if !errors.As(err, &commandFailure) ||
			(commandFailure.code != "session_not_found" && commandFailure.code != "session_connection_stale") {
			t.Fatalf("waiting command error=%v, want stale/not found", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting command did not leave after session removal")
	}
	if _, err := coordinator.session(claims.SessionID); err == nil {
		t.Fatal("terminated session remains discoverable")
	}
}

func TestCoordinatorEmitsOneTerminalEventWhenLocalAndRemoteHangupRace(t *testing.T) {
	type fixture struct {
		coordinator *Coordinator
		claims      Claims
		callID      string
		sipRuntime  *fakeSIP
		media       *fakeMedia
		database    *store.Store
	}
	setup := func(t *testing.T, sipRuntime *fakeSIP) fixture {
		t.Helper()
		coordinator, _, media, database := newTestCoordinatorWithStore(
			t, &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}, sipRuntime,
		)
		claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
			CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
			SupportedProtocolVersions: []int{1},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
			t.Fatal(err)
		}
		coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
		callID := uuid.NewString()
		startPayload, _ := json.Marshal(protocol.StartCall{
			CommandID: uuid.NewString(), CallID: callID, CallContextID: "context-1",
			GatewayCapability: "call-capability",
		})
		if _, err := coordinator.Handle(context.Background(), claims,
			protocol.Envelope{Type: "call.start", Payload: startPayload}); err != nil {
			t.Fatal(err)
		}
		coordinator.applySIPEvent(sip.Event{
			Type: sip.EventConnected, SessionID: claims.SessionID, CallID: callID,
			RemoteSDP: []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\n"),
		})
		return fixture{coordinator, claims, callID, sipRuntime, media, database}
	}
	assertTerminalOnce := func(t *testing.T, value fixture, wantReason call.EndReason, wantSIPHangups int) {
		t.Helper()
		session, err := value.coordinator.session(value.claims.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		session.mu.Lock()
		snapshot := session.activeCall.Snapshot()
		session.mu.Unlock()
		if snapshot.State != call.StateEnded || snapshot.EndReason != wantReason {
			t.Fatalf("terminal snapshot=%+v, want ended/%s", snapshot, wantReason)
		}
		events, err := value.database.PendingEvents(context.Background(), value.claims.SessionID, 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		ended := 0
		for _, event := range events {
			if event.Type == "call.ended" {
				ended++
			}
		}
		if ended != 1 {
			t.Fatalf("call.ended count=%d, want 1", ended)
		}
		value.media.mu.Lock()
		closed := append([]string(nil), value.media.closed...)
		value.media.mu.Unlock()
		if len(closed) != 1 || closed[0] != value.callID {
			t.Fatalf("media closed=%v, want [%s]", closed, value.callID)
		}
		value.sipRuntime.mu.Lock()
		hangups := value.sipRuntime.hangups
		value.sipRuntime.mu.Unlock()
		if hangups != wantSIPHangups {
			t.Fatalf("SIP hangups=%d, want %d", hangups, wantSIPHangups)
		}
	}

	t.Run("local hangup owns terminal transition", func(t *testing.T) {
		sipRuntime := newFakeSIP()
		sipRuntime.hangupEntered = make(chan struct{}, 1)
		sipRuntime.hangupRelease = make(chan struct{})
		value := setup(t, sipRuntime)
		payload, _ := json.Marshal(protocol.CallCommand{CommandID: uuid.NewString(), CallID: value.callID})
		localDone := make(chan error, 1)
		go func() {
			_, err := value.coordinator.Handle(context.Background(), value.claims,
				protocol.Envelope{Type: "call.hangup", Payload: payload})
			localDone <- err
		}()
		select {
		case <-sipRuntime.hangupEntered:
		case <-time.After(time.Second):
			t.Fatal("local hangup did not reach SIP adapter")
		}
		remoteDone := make(chan struct{})
		go func() {
			value.coordinator.applySIPEvent(sip.Event{
				Type: sip.EventEnded, SessionID: value.claims.SessionID,
				CallID: value.callID, Reason: "remote_hangup",
			})
			close(remoteDone)
		}()
		select {
		case <-remoteDone:
			t.Fatal("remote BYE bypassed serialized local hangup")
		case <-time.After(50 * time.Millisecond):
		}
		close(sipRuntime.hangupRelease)
		if err := <-localDone; err != nil {
			t.Fatal(err)
		}
		select {
		case <-remoteDone:
		case <-time.After(time.Second):
			t.Fatal("remote BYE did not resume")
		}
		assertTerminalOnce(t, value, call.EndLocalHangup, 1)
	})

	t.Run("remote hangup owns terminal transition", func(t *testing.T) {
		value := setup(t, newFakeSIP())
		value.coordinator.applySIPEvent(sip.Event{
			Type: sip.EventEnded, SessionID: value.claims.SessionID,
			CallID: value.callID, Reason: "remote_hangup",
		})
		payload, _ := json.Marshal(protocol.CallCommand{CommandID: uuid.NewString(), CallID: value.callID})
		if _, err := value.coordinator.Handle(context.Background(), value.claims,
			protocol.Envelope{Type: "call.hangup", Payload: payload}); err != nil {
			t.Fatal(err)
		}
		assertTerminalOnce(t, value, call.EndRemoteHangup, 0)
	})
}

func TestCoordinatorRejectsInboundAndClosesMedia(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	sipRuntime := newFakeSIP()
	coordinator, _, media := newTestCoordinatorWithMedia(t, api, sipRuntime)
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
	callID := uuid.NewString()
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventIncoming, SessionID: claims.SessionID, CallID: callID,
		RemoteSDP:   []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\n"),
		RemoteParty: "0812345678",
	})
	rejectPayload, _ := json.Marshal(protocol.CallCommand{CommandID: uuid.NewString(), CallID: callID})
	if _, err := coordinator.Handle(context.Background(), claims,
		protocol.Envelope{Type: "call.reject", Payload: rejectPayload}); err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.session(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	snapshot := session.activeCall.Snapshot()
	session.mu.Unlock()
	if snapshot.State != call.StateEnded || snapshot.EndReason != call.EndRejected {
		t.Fatalf("unexpected rejected inbound call: %+v", snapshot)
	}
	sipRuntime.mu.Lock()
	if len(sipRuntime.rejects) != 1 || sipRuntime.rejects[0] != 486 {
		sipRuntime.mu.Unlock()
		t.Fatalf("SIP reject statuses=%v, want [486]", sipRuntime.rejects)
	}
	sipRuntime.mu.Unlock()
	media.mu.Lock()
	defer media.mu.Unlock()
	if len(media.closed) != 1 || media.closed[0] != callID {
		t.Fatalf("rejected inbound media close=%v, want [%s]", media.closed, callID)
	}
}

func TestCoordinatorHandlesRemoteCancelWhileInboundIsRinging(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator, _, media := newTestCoordinatorWithMedia(t, api, newFakeSIP())
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
	callID := uuid.NewString()
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventIncoming, SessionID: claims.SessionID, CallID: callID,
		RemoteSDP:   []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\n"),
		RemoteParty: "0812345678",
	})
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventEnded, SessionID: claims.SessionID, CallID: callID, Reason: "cancelled",
	})
	session, err := coordinator.session(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	snapshot := session.activeCall.Snapshot()
	session.mu.Unlock()
	if snapshot.State != call.StateEnded || snapshot.EndReason != call.EndCancelled {
		t.Fatalf("unexpected cancelled inbound call: %+v", snapshot)
	}
	media.mu.Lock()
	defer media.mu.Unlock()
	if len(media.closed) != 1 || media.closed[0] != callID {
		t.Fatalf("cancelled inbound media close=%v, want [%s]", media.closed, callID)
	}
}

func TestCoordinatorEmitsDesktopAgentCompatibleOutboundLifecycle(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator, _, _, database := newTestCoordinatorWithStore(t, api, newFakeSIP())
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
	callID := uuid.NewString()
	startPayload, _ := json.Marshal(protocol.StartCall{
		CommandID: uuid.NewString(), CallID: callID, CallContextID: "context-1", GatewayCapability: "call-capability",
	})
	if _, err := coordinator.Handle(context.Background(), claims,
		protocol.Envelope{Type: "call.start", Payload: startPayload}); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventTrying, SessionID: claims.SessionID, CallID: callID})
	// Retransmitted provisional responses must not create duplicate lifecycle
	// records; the desktop agent only emits when its state actually changes.
	coordinator.applySIPEvent(sip.Event{Type: sip.EventTrying, SessionID: claims.SessionID, CallID: callID})
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRinging, SessionID: claims.SessionID, CallID: callID})
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRinging, SessionID: claims.SessionID, CallID: callID})
	remoteSDP := []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\n")
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventConnected, SessionID: claims.SessionID, CallID: callID, RemoteSDP: remoteSDP,
	})
	hangupPayload, _ := json.Marshal(protocol.CallCommand{CommandID: uuid.NewString(), CallID: callID})
	if _, err := coordinator.Handle(context.Background(), claims,
		protocol.Envelope{Type: "call.hangup", Payload: hangupPayload}); err != nil {
		t.Fatal(err)
	}
	events, err := database.PendingEvents(context.Background(), claims.SessionID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{
		"registration.state_changed", "call.created", "call.state_changed", "call.state_changed",
		"call.state_changed", "call.state_changed", "call.state_changed", "call.state_changed", "call.ended",
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(wantTypes), events)
	}
	var createdMetadata struct {
		RemoteParty string `json:"remoteParty"`
		CallContext string `json:"callContextId"`
		CommandID   string `json:"commandId"`
	}
	if err := json.Unmarshal(events[1].Payload, &createdMetadata); err != nil {
		t.Fatal(err)
	}
	if createdMetadata.RemoteParty != "sip:canonical@pbx.internal" ||
		createdMetadata.CallContext != "context-1" || createdMetadata.CommandID == "" {
		t.Fatalf("outbound replay metadata is incomplete: %+v", createdMetadata)
	}
	wantStates := []call.State{
		call.StateCreated, call.StateDialing, call.StateTrying, call.StateRinging,
		call.StateConnected, call.StateEnding, call.StateEnded, call.StateEnded,
	}
	for index, event := range events {
		if event.Type != wantTypes[index] {
			t.Fatalf("event %d type=%q, want %q", index, event.Type, wantTypes[index])
		}
		if index == 0 {
			continue
		}
		var snapshot call.Snapshot
		if err := json.Unmarshal(event.Payload, &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.State != wantStates[index-1] {
			t.Fatalf("event %d state=%q, want %q", index, snapshot.State, wantStates[index-1])
		}
	}
}

func TestCoordinatorPreservesNormalizedOutboundFailureReason(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator := newTestCoordinator(t, api, newFakeSIP())
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
	callID := uuid.NewString()
	payload, _ := json.Marshal(protocol.StartCall{
		CommandID: uuid.NewString(), CallID: callID, CallContextID: "context-1",
		GatewayCapability: "call-capability",
	})
	if _, err := coordinator.Handle(context.Background(), claims,
		protocol.Envelope{Type: "call.start", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{
		Type: sip.EventFailed, SessionID: claims.SessionID, CallID: callID,
		StatusCode: 486, Reason: "busy",
	})
	session, err := coordinator.session(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	snapshot := session.activeCall.Snapshot()
	session.mu.Unlock()
	if snapshot.State != call.StateEnded || snapshot.EndReason != call.EndBusy {
		t.Fatalf("outbound failure=%+v, want ended/busy", snapshot)
	}
}

func TestCoordinatorMapsOutboundTerminalReasonsToAgentContract(t *testing.T) {
	for reason, expected := range map[string]call.EndReason{
		"busy":      call.EndBusy,
		"no_answer": call.EndNoAnswer,
		"cancelled": call.EndCancelled,
		"rejected":  call.EndRejected,
		"unknown":   call.EndSIPFailed,
	} {
		if actual := failedEndReason(reason); actual != expected {
			t.Fatalf("failedEndReason(%q)=%q, want %q", reason, actual, expected)
		}
	}
}

func TestCoordinatorUsesCanonicalAuthorizedDestinationAndDeduplicates(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	sipRuntime := newFakeSIP()
	coordinator, _, _, database := newTestCoordinatorWithStore(t, api, sipRuntime)
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	initial, _, err := coordinator.Connect(context.Background(), claims)
	if err != nil || len(initial) != 2 || initial[0].Type != "session.welcome" || initial[1].Type != "gateway.snapshot" {
		t.Fatalf("unexpected connect result: %+v %v", initial, err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
	commandID := uuid.NewString()
	payload, _ := json.Marshal(protocol.StartCall{
		CommandID: commandID, CallID: uuid.NewString(), CallContextID: "context-1", GatewayCapability: "call-capability",
	})
	envelope := protocol.Envelope{Type: "call.start", Payload: payload}
	result, err := coordinator.Handle(context.Background(), claims, envelope)
	if err != nil || len(result) != 1 || result[0].Type != "command_result" {
		t.Fatalf("unexpected call result: %+v %v", result, err)
	}
	events, err := database.PendingEvents(context.Background(), claims.SessionID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Type != "registration.state_changed" ||
		events[1].Type != "call.created" || events[2].Type != "call.state_changed" {
		t.Fatalf("unexpected durable event order: %+v", events)
	}
	var dialing call.Snapshot
	if err := json.Unmarshal(events[2].Payload, &dialing); err != nil {
		t.Fatal(err)
	}
	if dialing.State != call.StateDialing {
		t.Fatalf("durable state=%q, want %q", dialing.State, call.StateDialing)
	}
	sipRuntime.mu.Lock()
	if sipRuntime.destination != api.destination || sipRuntime.starts != 1 {
		t.Fatalf("unexpected SIP start destination=%q starts=%d", sipRuntime.destination, sipRuntime.starts)
	}
	sipRuntime.mu.Unlock()
	if _, err := coordinator.Handle(context.Background(), claims, envelope); err != nil {
		t.Fatal(err)
	}
	sipRuntime.mu.Lock()
	defer sipRuntime.mu.Unlock()
	if sipRuntime.starts != 1 {
		t.Fatalf("duplicate command started %d SIP calls", sipRuntime.starts)
	}
}

func TestCoordinatorConnectReplaysDurableEventsBeforeAuthoritativeSnapshot(t *testing.T) {
	coordinator, _, _, database := newTestCoordinatorWithStore(
		t, &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}, newFakeSIP(),
	)
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := database.AppendEvent(
		context.Background(), claims.SessionID, claims.AgentInstanceID,
		"replay-order", "call.state_changed", []byte(`{"callId":"old-call","state":"ringing"}`), time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}

	initial, _, err := coordinator.Connect(context.Background(), claims)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial) != 3 {
		t.Fatalf("initial event count=%d, want 3", len(initial))
	}
	if initial[0].Type != "session.welcome" || initial[1].Type != "call.state_changed" ||
		initial[2].Type != "gateway.snapshot" {
		t.Fatalf("unexpected initial order: %q, %q, %q", initial[0].Type, initial[1].Type, initial[2].Type)
	}
	expectedReplay, err := replayEnvelope(stored)
	if err != nil {
		t.Fatal(err)
	}
	if initial[1].MessageID != expectedReplay.MessageID {
		t.Fatalf("replayed message ID=%q, want %q", initial[1].MessageID, expectedReplay.MessageID)
	}
}

func TestCoordinatorReplaysUncommittedEventsAfterBrowserSessionRefresh(t *testing.T) {
	verifier := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	coordinator, _, _, database := newTestCoordinatorWithStore(t, verifier, newFakeSIP())
	authentication := protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	}
	first, err := coordinator.Redeem(context.Background(), authentication)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := database.AppendEvent(context.Background(), first.SessionID,
		first.AgentInstanceID, "refresh-replay", "call.state_changed",
		[]byte(`{"callId":"old-call","state":"ringing"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	verifier.mu.Lock()
	verifier.sessionID = "session-after-refresh"
	verifier.mu.Unlock()
	second, err := coordinator.Redeem(context.Background(), authentication)
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID == first.SessionID {
		t.Fatal("refresh did not create a replacement logical session")
	}
	initial, _, err := coordinator.Connect(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial) != 3 || initial[1].Type != "call.state_changed" {
		t.Fatalf("uncommitted event was not replayed to replacement session: %+v", initial)
	}
	var payload struct {
		Sequence            uint64 `json:"sequence"`
		AgentSessionID      string `json:"agentSessionId"`
		CollectionID        string `json:"collectionId"`
		CollectionBindingID string `json:"collectionBindingId"`
	}
	if err := json.Unmarshal(initial[1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Sequence != stored.Sequence || payload.AgentSessionID != first.SessionID ||
		payload.CollectionID != first.CollectionID ||
		payload.CollectionBindingID != first.CollectionBindingID {
		t.Fatalf("replayed event lost original binding: %+v", payload)
	}
}

func TestCoordinatorSnapshotOmitsTerminalCall(t *testing.T) {
	coordinator := newTestCoordinator(t, &fakeCapabilityVerifier{}, newFakeSIP())
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.session(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := call.NewOutbound(uuid.NewString(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !machine.RemoteHangup(call.EndCancelled, time.Now().UTC()) {
		t.Fatal("expected call to become terminal")
	}
	session.mu.Lock()
	session.activeCall = machine
	snapshot := coordinator.snapshotLocked(session)
	session.mu.Unlock()
	if _, exists := snapshot["call"]; exists {
		t.Fatal("terminal call must not be exposed as active in gateway snapshot")
	}
}

func TestCoordinatorRequiresRegistrationBeforeStartingCall(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	sipRuntime := newFakeSIP()
	coordinator := newTestCoordinator(t, api, sipRuntime)
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(protocol.StartCall{
		CommandID: uuid.NewString(), CallID: uuid.NewString(), CallContextID: "context-1",
		GatewayCapability: "call-capability",
	})
	_, err = coordinator.Handle(context.Background(), claims, protocol.Envelope{Type: "call.start", Payload: payload})
	var coded interface{ Code() string }
	if !errors.As(err, &coded) || coded.Code() != "registration_required" {
		t.Fatalf("expected registration_required, got %v", err)
	}
	sipRuntime.mu.Lock()
	defer sipRuntime.mu.Unlock()
	if sipRuntime.starts != 0 {
		t.Fatalf("unregistered session started %d SIP calls", sipRuntime.starts)
	}
}

func TestCoordinatorReconcilesStaleRegisteredStateWhenRuntimeRejectsCall(t *testing.T) {
	api := &fakeCapabilityVerifier{destination: "sip:canonical@pbx.internal"}
	sipRuntime := newFakeSIP()
	sipRuntime.startErr = sip.ErrRegistrationNotReady
	coordinator := newTestCoordinator(t, api, sipRuntime)
	claims, err := coordinator.Redeem(context.Background(), protocol.Authenticate{
		CollectionID: "collection-1", SessionCapability: "capability", PortalVersion: "1",
		SupportedProtocolVersions: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.Connect(context.Background(), claims); err != nil {
		t.Fatal(err)
	}
	coordinator.applySIPEvent(sip.Event{Type: sip.EventRegistered, SessionID: claims.SessionID})
	payload, _ := json.Marshal(protocol.StartCall{
		CommandID: uuid.NewString(), CallID: uuid.NewString(), CallContextID: "context-1",
		GatewayCapability: "call-capability",
	})
	_, err = coordinator.Handle(context.Background(), claims,
		protocol.Envelope{Type: "call.start", Payload: payload})
	var coded interface{ Code() string }
	if !errors.As(err, &coded) || coded.Code() != "registration_required" {
		t.Fatalf("expected registration_required, got %v", err)
	}
	session, err := coordinator.session(claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	registration := session.registration
	session.mu.Unlock()
	if registration != string(sip.EventRegistrationFailed) {
		t.Fatalf("registration=%q, want %q", registration, sip.EventRegistrationFailed)
	}
}
