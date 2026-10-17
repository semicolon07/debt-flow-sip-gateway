package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDecodeAuthenticate(t *testing.T) {
	payload := Authenticate{
		CollectionID: "collection", SessionCapability: "capability", PortalVersion: "1.0.0",
		SupportedProtocolVersions: []int{Version},
	}
	rawPayload, _ := json.Marshal(payload)
	raw, _ := json.Marshal(Envelope{
		ProtocolVersion: Version, Kind: KindCommand, MessageID: uuid.NewString(),
		SentAtUTC: time.Now().UTC(), Type: "session.authenticate", Payload: rawPayload,
	})
	envelope, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodePayload[Authenticate](envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAuthenticate(got); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeRejectsUnknownEnvelopeField(t *testing.T) {
	raw := `{"protocolVersion":1,"kind":"command","messageId":"` + uuid.NewString() +
		`","sentAtUtc":"2026-10-17T08:00:00Z","type":"state.get","payload":{},"secret":"leak"}`
	if _, err := Decode([]byte(raw)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestDecodeRejectsTrailingJSON(t *testing.T) {
	raw := `{"protocolVersion":1,"kind":"command","messageId":"` + uuid.NewString() +
		`","sentAtUtc":"2026-10-17T08:00:00Z","type":"state.get","payload":{}} {}`
	if _, err := Decode([]byte(raw)); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("expected trailing JSON error, got %v", err)
	}
}

func TestStartCallHasNoRawDestination(t *testing.T) {
	payload := json.RawMessage(`{"commandId":"` + uuid.NewString() + `","callId":"` +
		uuid.NewString() + `","callContextId":"ctx","destination":"sip:100@example.test"}`)
	_, err := DecodePayload[StartCall](Envelope{Type: "call.start", Payload: payload})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected raw destination to be rejected, got %v", err)
	}
}

func TestValidateDTMF(t *testing.T) {
	valid := DTMF{CommandID: uuid.NewString(), CallID: uuid.NewString(), Digit: "#"}
	if err := ValidateDTMF(valid); err != nil {
		t.Fatal(err)
	}
	valid.Digit = "12"
	if err := ValidateDTMF(valid); err == nil {
		t.Fatal("expected multi-digit DTMF to fail")
	}
}
