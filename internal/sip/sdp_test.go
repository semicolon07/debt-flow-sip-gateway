package sip

import "testing"

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
	if err == nil {
		t.Fatal("expected unsupported codec error")
	}
}
