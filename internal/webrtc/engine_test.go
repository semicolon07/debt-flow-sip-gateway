package webrtc

import (
	"testing"

	pion "github.com/pion/webrtc/v4"
)

func TestSelectCodecUsesOfferOrder(t *testing.T) {
	offer := "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111 8 0 126\r\na=rtpmap:111 opus/48000/2\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:126 telephone-event/8000\r\n"
	codec, err := selectCodec(offer)
	if err != nil {
		t.Fatal(err)
	}
	if codec.capability.MimeType != mimePCMA || codec.payloadType != 8 || codec.dtmfPayloadType != 126 {
		t.Fatalf("expected PCMA, got %+v", codec)
	}
}

func TestSelectCodecRejectsWrongClockRate(t *testing.T) {
	offer := "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 96 101\r\na=rtpmap:96 PCMU/48000\r\na=rtpmap:101 telephone-event/8000\r\n"
	if _, err := selectCodec(offer); err == nil {
		t.Fatal("expected wrong G.711 clock rate to be rejected")
	}
}

func TestSelectCodecRejectsMissingTelephoneEvent(t *testing.T) {
	offer := "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 0\r\na=rtpmap:0 PCMU/8000\r\n"
	if _, err := selectCodec(offer); err == nil {
		t.Fatal("expected missing RFC 4733 codec to be rejected")
	}
}

func TestSelectCodecIgnoresMappingsOutsideSelectedAudioSection(t *testing.T) {
	offer := "v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 0 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111 126\r\na=rtpmap:111 opus/48000/2\r\na=rtpmap:126 telephone-event/8000\r\n"
	if _, err := selectCodec(offer); err == nil {
		t.Fatal("codec mappings from a video section were accepted for audio")
	}
}

func TestSelectCodecRejectsOpusOnly(t *testing.T) {
	offer := "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=rtpmap:111 opus/48000/2\r\n"
	if _, err := selectCodec(offer); err == nil {
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
