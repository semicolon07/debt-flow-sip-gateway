package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
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
}

func (f *fakeRTC) WriteRTCP(_ string, packets []rtcp.Packet) error {
	f.mu.Lock()
	f.rtcp = append(f.rtcp, packets...)
	f.mu.Unlock()
	return nil
}

func (*fakeRTC) Open(string, gatewaywebrtc.TURNCredential) error  { return nil }
func (*fakeRTC) AcceptOffer(string, string) (string, error)       { return "answer", nil }
func (*fakeRTC) AddCandidate(string, pion.ICECandidateInit) error { return nil }
func (f *fakeRTC) WriteRTP(_ string, packet *rtp.Packet) error {
	f.mu.Lock()
	f.writes = append(f.writes, packet)
	f.mu.Unlock()
	return nil
}
func (*fakeRTC) Codec(string) (gatewaywebrtc.CodecInfo, error) {
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
func (*fakeMedia) CloseCall(string) error { return nil }

type fakeSIP struct {
	mu          sync.Mutex
	events      chan sip.Event
	destination string
	starts      int
	answers     int
	hangups     int
	closed      []string
	localSDP    []byte
}

func newFakeSIP() *fakeSIP                                                  { return &fakeSIP{events: make(chan sip.Event, 8)} }
func (f *fakeSIP) Events() <-chan sip.Event                                 { return f.events }
func (*fakeSIP) Configure(context.Context, string, sip.Configuration) error { return nil }
func (*fakeSIP) Register(context.Context, string) error                     { return nil }
func (*fakeSIP) Unregister(context.Context, string) error                   { return nil }
func (f *fakeSIP) StartCall(_ context.Context, _, _, destination string, localSDP []byte) error {
	f.mu.Lock()
	f.destination = destination
	f.starts++
	f.localSDP = append([]byte(nil), localSDP...)
	f.mu.Unlock()
	return nil
}
func (f *fakeSIP) Answer(_ context.Context, _, _ string, localSDP []byte) error {
	f.mu.Lock()
	f.answers++
	f.localSDP = append([]byte(nil), localSDP...)
	f.mu.Unlock()
	return nil
}
func (*fakeSIP) Reject(context.Context, string, string, int) error { return nil }
func (f *fakeSIP) Hangup(context.Context, string, string) error {
	f.mu.Lock()
	f.hangups++
	f.mu.Unlock()
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
	sipRuntime.mu.Lock()
	defer sipRuntime.mu.Unlock()
	if sipRuntime.answers != 1 || len(sipRuntime.localSDP) == 0 {
		t.Fatalf("expected one SDP answer, got answers=%d sdp=%q", sipRuntime.answers, sipRuntime.localSDP)
	}
}

func TestCoordinatorUsesCanonicalAuthorizedDestinationAndDeduplicates(t *testing.T) {
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
