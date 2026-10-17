package protocol

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func BenchmarkDecodeCallStart(b *testing.B) {
	payload, _ := json.Marshal(StartCall{
		CommandID: uuid.NewString(), CallID: uuid.NewString(),
		CallContextID: "phonectx_benchmark", GatewayCapability: "dfcap1.benchmark",
	})
	raw, _ := json.Marshal(Envelope{
		ProtocolVersion: Version, Kind: KindCommand, MessageID: uuid.NewString(),
		SentAtUTC: time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC),
		Type:      "call.start", Payload: payload,
	})
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		envelope, err := Decode(raw)
		if err != nil {
			b.Fatal(err)
		}
		value, err := DecodePayload[StartCall](envelope)
		if err != nil || ValidateStartCall(value) != nil {
			b.Fatalf("decode call.start: %v", err)
		}
	}
}
