package sip

import (
	"errors"
	"strings"
	"testing"
)

func TestParseRemoteSDP(t *testing.T) {
	description, err := ParseRemoteSDP([]byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 8 101\r\na=rtcp:30500 IN IP4 10.0.0.21\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:101 telephone-event/8000\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if description.IP != "10.0.0.20" || description.Port != 20500 || description.Codec != "PCMA" ||
		description.RTCPIP != "10.0.0.21" || description.RTCPPort != 30500 ||
		description.AudioPayloadType != 8 || description.DTMFPayloadType != 101 {
		t.Fatalf("unexpected description: %+v", description)
	}
}

func TestParseRemoteSDPDefaultsRTCPToPairedPort(t *testing.T) {
	description, err := ParseRemoteSDP([]byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 0\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if description.RTCPIP != "10.0.0.20" || description.RTCPPort != 20501 {
		t.Fatalf("unexpected RTCP endpoint: %+v", description)
	}
}

func TestParseRemoteSDPRejectsOpusOnly(t *testing.T) {
	_, err := ParseRemoteSDP([]byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 111\r\na=rtpmap:111 opus/48000/2\r\n"))
	if !errors.Is(err, ErrNoCodecIntersection) {
		t.Fatal("expected unsupported codec error")
	}
}

func TestParseRemoteSDPForCodecUsesIntersectionNotOfferOrder(t *testing.T) {
	raw := []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 8 0 126\r\n" +
		"a=rtpmap:8 PCMA/8000\r\na=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:126 telephone-event/8000\r\n")
	description, err := ParseRemoteSDPForCodec(raw, "PCMU")
	if err != nil {
		t.Fatal(err)
	}
	if description.Codec != "PCMU" || description.AudioPayloadType != 0 ||
		description.DTMFPayloadType != 126 {
		t.Fatalf("unexpected preferred codec selection: %+v", description)
	}
}

func TestParseRemoteSDPForCodecRejectsWhenPreferredCodecIsNotOffered(t *testing.T) {
	raw := []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n")
	if _, err := ParseRemoteSDPForCodec(raw, "PCMU"); !errors.Is(err, ErrNoCodecIntersection) {
		t.Fatalf("expected no codec intersection, got %v", err)
	}
}

func TestParseRemoteSDPRejectsDynamicPayloadForStaticG711(t *testing.T) {
	raw := []byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 96\r\na=rtpmap:96 PCMU/8000\r\n")
	if _, err := ParseRemoteSDP(raw); !errors.Is(err, ErrNoCodecIntersection) {
		t.Fatalf("expected invalid dynamic G.711 payload to be rejected, got %v", err)
	}
}

func TestParseRemoteSDPIgnoresConnectionAndCodecsFromLaterMediaSections(t *testing.T) {
	raw := []byte("v=0\r\nc=IN IP4 10.0.0.20\r\n" +
		"m=audio 20500 RTP/AVP 0 101\r\n" +
		"a=rtcp:20501 IN IP4 10.0.0.21\r\n" +
		"a=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\n" +
		"m=video 40000 RTP/AVP 96\r\nc=IN IP4 203.0.113.50\r\n" +
		"a=rtcp:40001 IN IP4 203.0.113.51\r\na=rtpmap:96 H264/90000\r\n")
	description, err := ParseRemoteSDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	if description.IP != "10.0.0.20" || description.Port != 20500 ||
		description.RTCPIP != "10.0.0.21" || description.RTCPPort != 20501 ||
		description.Codec != "PCMU" {
		t.Fatalf("later video section contaminated audio media: %+v", description)
	}
}

func TestParseRemoteSDPDoesNotEnableUnnegotiatedRTCPMux(t *testing.T) {
	raw := []byte("v=0\r\nc=IN IP4 10.0.0.20\r\n" +
		"m=audio 20500 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=rtcp-mux\r\n")
	description, err := ParseRemoteSDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	if description.RTCPPort != description.Port+1 {
		t.Fatalf("RTCP port=%d, want separate paired port %d", description.RTCPPort, description.Port+1)
	}
}

func TestParseRemoteSDPRejectsIPv6ForIPv4MediaRuntime(t *testing.T) {
	raw := []byte("v=0\r\nc=IN IP6 2001:db8::10\r\nm=audio 20500 RTP/AVP 0\r\n")
	if _, err := ParseRemoteSDP(raw); err == nil || !strings.Contains(err.Error(), "IPv4") {
		t.Fatalf("expected IPv6 media endpoint to be rejected, got %v", err)
	}
}
