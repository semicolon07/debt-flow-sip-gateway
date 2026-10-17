package sip

import (
	"testing"

	sipmessage "github.com/emiago/sipgo/sip"
)

func FuzzSIPParserBoundary(f *testing.F) {
	f.Add([]byte("OPTIONS sip:100@example.test SIP/2.0\r\nVia: SIP/2.0/UDP 10.0.0.1:5060;branch=z9hG4bK-1\r\nFrom: <sip:100@example.test>;tag=1\r\nTo: <sip:100@example.test>\r\nCall-ID: seed@example.test\r\nCSeq: 1 OPTIONS\r\nContent-Length: 0\r\n\r\n"))
	f.Add([]byte("INVITE"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64*1024 {
			t.Skip()
		}
		_, _ = sipmessage.ParseMessage(raw)
	})
}

func FuzzParseRemoteSDP(f *testing.F) {
	f.Add([]byte("v=0\r\nc=IN IP4 10.0.0.20\r\nm=audio 20500 RTP/AVP 0 101\r\na=rtpmap:101 telephone-event/8000\r\n"))
	f.Add([]byte("v=0\r\nm=audio 0 RTP/AVP 111\r\n"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64*1024 {
			t.Skip()
		}
		_, _ = ParseRemoteSDP(raw)
	})
}
