package sip

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestSanitizedSIPTracerDoesNotLogPacketContentsOrAddresses(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tracer := &sanitizedSIPTracer{logger: logger}
	packet := []byte("INVITE sip:0955948679@192.168.1.247 SIP/2.0\r\n" +
		"Via: SIP/2.0/UDP 192.168.1.245:5060;branch=z9hG4bK-secret\r\n" +
		"From: <sip:1129@192.168.1.247>;tag=secret-tag\r\n" +
		"To: <sip:0955948679@192.168.1.247>\r\n" +
		"Call-ID: secret-call-id\r\n" +
		"CSeq: 42 INVITE\r\n" +
		"Authorization: Digest username=\"1129\", response=\"secret-response\"\r\n" +
		"Content-Length: 4\r\n\r\nv=0\n")

	tracer.SIPTraceWrite("UDP", "192.168.1.245:5060", "192.168.1.247:5060", packet)

	logged := output.String()
	for _, secret := range []string{
		"0955948679", "192.168.1.245", "192.168.1.247", "secret-call-id",
		"secret-response", "secret-tag", "z9hG4bK-secret", "1129", "v=0",
	} {
		if strings.Contains(logged, secret) {
			t.Fatalf("sanitized SIP trace leaked %q: %s", secret, logged)
		}
	}
	for _, expected := range []string{
		`"direction":"write"`, `"transport":"UDP"`, `"kind":"request"`,
		`"method":"INVITE"`, `"cseq":42`, `"cseqMethod":"INVITE"`,
	} {
		if !strings.Contains(logged, expected) {
			t.Fatalf("sanitized SIP trace missing %s: %s", expected, logged)
		}
	}
}

func TestSanitizedSIPTracerLogsOnlySizeWhenParsingFails(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tracer := &sanitizedSIPTracer{logger: logger}

	tracer.SIPTraceRead("TCP", "local-secret", "remote-secret", []byte("password=secret"))

	logged := output.String()
	if !strings.Contains(logged, `"kind":"parse_failed"`) ||
		!strings.Contains(logged, `"bytes":15`) {
		t.Fatalf("unexpected parse failure trace: %s", logged)
	}
	for _, secret := range []string{"password", "secret", "local-secret", "remote-secret"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("parse failure trace leaked %q: %s", secret, logged)
		}
	}
}
