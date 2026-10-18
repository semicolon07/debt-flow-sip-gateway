package sip

import (
	"strings"
	"testing"
)

func TestAsteriskSDPIsG711AndRFC4733Only(t *testing.T) {
	profile := AsteriskProfile{}
	sdp, err := profile.BuildSDP(MediaDescription{
		IP: "10.0.0.10", Port: 20000, AudioPayloadType: 0,
		DTMFPayloadType: 101, Codec: "PCMU",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(sdp)
	for _, expected := range []string{
		"m=audio 20000 RTP/AVP 0 101", "a=rtcp:20001 IN IP4 10.0.0.10",
		"PCMU/8000", "telephone-event/8000",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("SDP missing %q: %s", expected, text)
		}
	}
	if strings.Contains(strings.ToLower(text), "opus") {
		t.Fatal("P1 Asterisk SDP must not advertise Opus")
	}
}

func TestAsteriskFailureNormalization(t *testing.T) {
	profile := AsteriskProfile{}
	if got := profile.NormalizeFailure(401); got != "authentication_failed" {
		t.Fatalf("expected authentication failure, got %s", got)
	}
	if got := profile.NormalizeFailure(403); got != "forbidden" {
		t.Fatalf("expected forbidden, got %s", got)
	}
	if got := profile.NormalizeFailure(486); got != "busy" {
		t.Fatalf("expected busy, got %s", got)
	}
	if got := profile.NormalizeFailure(488); got != "codec_not_supported" {
		t.Fatalf("expected codec failure, got %s", got)
	}
	if got := profile.NormalizeFailure(603); got != "rejected" {
		t.Fatalf("expected rejected, got %s", got)
	}
}

func TestNormalizeRemoteParty(t *testing.T) {
	for value, expected := range map[string]string{
		"+6621234567": "+6621234567",
		"*123#":       "*123#",
		"anonymous":   "unknown",
		"":            "unknown",
	} {
		if actual := normalizeRemoteParty(value); actual != expected {
			t.Fatalf("normalizeRemoteParty(%q) = %q, want %q", value, actual, expected)
		}
	}
}
