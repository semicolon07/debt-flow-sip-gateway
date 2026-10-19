package webrtc

import (
	"testing"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

func TestSelectCodecUsesOfferOrder(t *testing.T) {
	offer := "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111 8 0 126\r\na=rtpmap:111 opus/48000/2\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:126 telephone-event/8000\r\n"
	codec, err := selectCodec(offer, "")
	if err != nil {
		t.Fatal(err)
	}
	if codec.capability.MimeType != mimePCMA || codec.payloadType != 8 || codec.dtmfPayloadType != 126 {
		t.Fatalf("expected PCMA, got %+v", codec)
	}
}

func TestSelectCodecPreservesActiveCallCodecAcrossReconnect(t *testing.T) {
	offer := "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 8 0 126\r\n" +
		"a=rtpmap:8 PCMA/8000\r\na=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:126 telephone-event/8000\r\n"
	codec, err := selectCodec(offer, "PCMU")
	if err != nil {
		t.Fatal(err)
	}
	if codec.capability.MimeType != mimePCMU || codec.payloadType != 0 {
		t.Fatalf("active-call codec changed during reconnect: %+v", codec)
	}
	withoutPCMU := "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 8 126\r\n" +
		"a=rtpmap:8 PCMA/8000\r\na=rtpmap:126 telephone-event/8000\r\n"
	if _, err := selectCodec(withoutPCMU, "PCMU"); err == nil {
		t.Fatal("offer without the active-call codec was accepted")
	}
}

func TestSelectCodecRejectsWrongClockRate(t *testing.T) {
	offer := "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 96 101\r\na=rtpmap:96 PCMU/48000\r\na=rtpmap:101 telephone-event/8000\r\n"
	if _, err := selectCodec(offer, ""); err == nil {
		t.Fatal("expected wrong G.711 clock rate to be rejected")
	}
}

func TestSelectCodecRejectsMissingTelephoneEvent(t *testing.T) {
	offer := "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 0\r\na=rtpmap:0 PCMU/8000\r\n"
	if _, err := selectCodec(offer, ""); err == nil {
		t.Fatal("expected missing RFC 4733 codec to be rejected")
	}
}

func TestSelectCodecIgnoresMappingsOutsideSelectedAudioSection(t *testing.T) {
	offer := "v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 0 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111 126\r\na=rtpmap:111 opus/48000/2\r\na=rtpmap:126 telephone-event/8000\r\n"
	if _, err := selectCodec(offer, ""); err == nil {
		t.Fatal("codec mappings from a video section were accepted for audio")
	}
}

func TestSelectCodecRejectsOpusOnly(t *testing.T) {
	offer := "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=rtpmap:111 opus/48000/2\r\n"
	if _, err := selectCodec(offer, ""); err == nil {
		t.Fatal("expected codec intersection error")
	}
}

func TestOpenAtomicallyReplacesAnExistingPeer(t *testing.T) {
	engine, err := New(Config{UDPMuxListen: "127.0.0.1:0", MaxSessions: 1}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.Open("session-1", TURNCredential{}); err != nil {
		t.Fatal(err)
	}
	engine.mu.RLock()
	previous := engine.peers["session-1"]
	engine.mu.RUnlock()
	if err := engine.Open("session-1", TURNCredential{}); err != nil {
		t.Fatal(err)
	}
	engine.mu.RLock()
	current := engine.peers["session-1"]
	engine.mu.RUnlock()
	if current == previous || engine.SessionCount() != 1 {
		t.Fatal("existing peer was not atomically replaced")
	}
	if state := previous.connection.ConnectionState(); state != pion.PeerConnectionStateClosed {
		t.Fatalf("previous peer state=%s, want closed", state)
	}
}

func TestAcceptOfferPublishesTheActualOutboundSSRC(t *testing.T) {
	engine, err := New(Config{UDPMuxListen: "127.0.0.1:0", MaxSessions: 1}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.Open("session-1", TURNCredential{}); err != nil {
		t.Fatal(err)
	}
	browserMedia := &pion.MediaEngine{}
	for _, codec := range supportedCodecs() {
		if err := browserMedia.RegisterCodec(codec, pion.RTPCodecTypeAudio); err != nil {
			t.Fatal(err)
		}
	}
	browser, err := pion.NewAPI(pion.WithMediaEngine(browserMedia)).NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	if _, err := browser.AddTransceiverFromKind(pion.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	offer, err := browser.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := browser.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.AcceptOffer("session-1", offer.SDP, ""); err != nil {
		t.Fatal(err)
	}
	codec, err := engine.Codec("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if codec.OutboundSSRC == 0 {
		t.Fatal("negotiated WebRTC outbound SSRC is missing")
	}
}

func TestFailedPeerReplacementKeepsTheExistingPeer(t *testing.T) {
	engine, err := New(Config{UDPMuxListen: "127.0.0.1:0", MaxSessions: 1}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.Open("session-1", TURNCredential{}); err != nil {
		t.Fatal(err)
	}
	engine.mu.RLock()
	previous := engine.peers["session-1"]
	engine.mu.RUnlock()
	engine.turnURLs = []string{"not-a-turn-url"}
	if err := engine.Open("session-1", TURNCredential{Username: "user", Password: "password"}); err == nil {
		t.Fatal("invalid replacement TURN URL was accepted")
	}
	engine.mu.RLock()
	current := engine.peers["session-1"]
	engine.mu.RUnlock()
	if current != previous || previous.connection.ConnectionState() == pion.PeerConnectionStateClosed {
		t.Fatal("failed replacement disrupted the existing peer")
	}
}

func TestRetiredPeerCallbacksCannotEnterTheCurrentSession(t *testing.T) {
	current := &peer{active: true}
	retired := &peer{}
	var candidates, rtpPackets, rtcpBatches, states int
	engine := &Engine{
		peers:       map[string]*peer{"session-1": current},
		onCandidate: func(string, pion.ICECandidateInit) { candidates++ },
		onRTP:       func(string, *rtp.Packet) { rtpPackets++ },
		onRTCP:      func(string, []rtcp.Packet) { rtcpBatches++ },
		onState:     func(string, pion.PeerConnectionState) { states++ },
	}

	engine.deliverCandidate("session-1", retired, pion.ICECandidateInit{Candidate: "retired"})
	engine.deliverRTP("session-1", retired, &rtp.Packet{})
	engine.deliverRTCP("session-1", retired, []rtcp.Packet{&rtcp.ReceiverReport{}})
	engine.deliverState("session-1", retired, pion.PeerConnectionStateFailed)
	if candidates != 0 || rtpPackets != 0 || rtcpBatches != 0 || states != 0 {
		t.Fatalf("retired peer callbacks escaped: candidate=%d rtp=%d rtcp=%d state=%d",
			candidates, rtpPackets, rtcpBatches, states)
	}

	engine.deliverCandidate("session-1", current, pion.ICECandidateInit{Candidate: "current"})
	engine.deliverRTP("session-1", current, &rtp.Packet{})
	engine.deliverRTCP("session-1", current, []rtcp.Packet{&rtcp.ReceiverReport{}})
	engine.deliverState("session-1", current, pion.PeerConnectionStateConnected)
	if candidates != 1 || rtpPackets != 1 || rtcpBatches != 1 || states != 1 {
		t.Fatalf("current peer callbacks were lost: candidate=%d rtp=%d rtcp=%d state=%d",
			candidates, rtpPackets, rtcpBatches, states)
	}
}
