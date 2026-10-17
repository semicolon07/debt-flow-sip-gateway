package protocol

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func FuzzDecodeEnvelope(f *testing.F) {
	validPayload, _ := json.Marshal(Authenticate{
		CollectionID: "collection", SessionCapability: "capability", PortalVersion: "1.0.0",
		SupportedProtocolVersions: []int{Version},
	})
	valid, _ := json.Marshal(Envelope{
		ProtocolVersion: Version, Kind: KindCommand, MessageID: uuid.NewString(),
		SentAtUTC: time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC),
		Type:      "session.authenticate", Payload: validPayload,
	})
	f.Add(valid)
	f.Add([]byte(`{"protocolVersion":1}`))
	f.Add([]byte("not-json"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64*1024 {
			t.Skip()
		}
		envelope, err := Decode(raw)
		if err != nil {
			return
		}
		switch envelope.Type {
		case "session.authenticate":
			value, decodeErr := DecodePayload[Authenticate](envelope)
			if decodeErr == nil {
				_ = ValidateAuthenticate(value)
			}
		case "session.renew":
			value, decodeErr := DecodePayload[SessionRenew](envelope)
			if decodeErr == nil {
				_ = ValidateSessionRenew(value)
			}
		case "call.start":
			value, decodeErr := DecodePayload[StartCall](envelope)
			if decodeErr == nil {
				_ = ValidateStartCall(value)
			}
		case "call.answer", "call.reject", "call.hangup":
			value, decodeErr := DecodePayload[CallCommand](envelope)
			if decodeErr == nil {
				_ = ValidateCallCommand(value)
			}
		case "call.dtmf":
			value, decodeErr := DecodePayload[DTMF](envelope)
			if decodeErr == nil {
				_ = ValidateDTMF(value)
			}
		}
	})
}
