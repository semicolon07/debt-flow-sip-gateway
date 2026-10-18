package sip

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"

	sipmessage "github.com/emiago/sipgo/sip"
)

// sanitizedSIPTracer intentionally records only routing-neutral transaction
// metadata. Raw SIP packets, addresses, URIs, SDP and authorization headers
// must never enter the operational log.
type sanitizedSIPTracer struct {
	logger *slog.Logger
}

func (t *sanitizedSIPTracer) SIPTraceRead(transport, _, _ string, packet []byte) {
	t.trace("read", transport, packet)
}

func (t *sanitizedSIPTracer) SIPTraceWrite(transport, _, _ string, packet []byte) {
	t.trace("write", transport, packet)
}

func (t *sanitizedSIPTracer) trace(direction, transport string, packet []byte) {
	if t == nil || t.logger == nil {
		return
	}
	message, err := sipmessage.ParseMessage(packet)
	if err != nil {
		t.logger.Debug("SIP wire message", "direction", direction, "transport", transport,
			"kind", "parse_failed", "bytes", len(packet))
		return
	}
	attributes := []any{
		"direction", direction,
		"transport", transport,
		"bytes", len(packet),
		"callRef", sipMessageRef(message),
	}
	if cseq := message.CSeq(); cseq != nil {
		attributes = append(attributes, "cseq", cseq.SeqNo, "cseqMethod", cseq.MethodName)
	}
	switch value := message.(type) {
	case *sipmessage.Request:
		attributes = append(attributes, "kind", "request", "method", value.Method)
	case *sipmessage.Response:
		attributes = append(attributes, "kind", "response", "sipStatusCode", value.StatusCode)
	default:
		attributes = append(attributes, "kind", "unknown")
	}
	t.logger.Debug("SIP wire message", attributes...)
}

func sipMessageRef(message sipmessage.Message) string {
	if message == nil || message.CallID() == nil {
		return "unavailable"
	}
	return shortSIPCallRef(message.CallID().Value())
}

func shortSIPCallRef(value string) string {
	if value == "" {
		return "unavailable"
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}
