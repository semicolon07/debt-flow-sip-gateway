package media

import (
	"testing"

	"github.com/pion/rtp"
)

func TestTranslatorRewritesWithoutDecodingPayload(t *testing.T) {
	translator, err := NewTranslator(Mapping{
		SourceAudioPayloadType: 0, TargetAudioPayloadType: 8,
		SourceDTMFPayloadType: 101, TargetDTMFPayloadType: 110,
		TargetSSRC: 42, InitialSequence: 1000, InitialTimestamp: 9000,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte{1, 2, 3, 4}
	translated, err := translator.Translate(&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 65535, Timestamp: 100},
		Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if translated.PayloadType != 8 || translated.SSRC != 42 || translated.SequenceNumber != 1000 || translated.Timestamp != 9000 {
		t.Fatalf("unexpected translated header: %+v", translated.Header)
	}
	if &translated.Payload[0] == &payload[0] {
		t.Fatal("payload must be copied before asynchronous forwarding")
	}
	second, err := translator.Translate(&rtp.Packet{
		Header: rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 0, Timestamp: 260},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.SequenceNumber != 1001 || second.Timestamp != 9160 {
		t.Fatalf("wrap-safe translation failed: %+v", second.Header)
	}
}

func TestTranslatorTranslateIntoReusesPayloadStorage(t *testing.T) {
	translator, err := NewTranslator(Mapping{
		SourceAudioPayloadType: 0, TargetAudioPayloadType: 8,
		SourceDTMFPayloadType: 101, TargetDTMFPayloadType: 110,
		TargetSSRC: 42, InitialSequence: 1000, InitialTimestamp: 9000,
	})
	if err != nil {
		t.Fatal(err)
	}
	destination := &rtp.Packet{Payload: make([]byte, 0, 160)}
	source := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 7, Timestamp: 160, SSRC: 99},
		Payload: []byte{1, 2, 3, 4},
	}
	if err := translator.TranslateInto(destination, source); err != nil {
		t.Fatal(err)
	}
	if destination.PayloadType != 8 || destination.SSRC != 42 || string(destination.Payload) != string(source.Payload) {
		t.Fatalf("unexpected translated packet: %+v", destination)
	}
	if &destination.Payload[0] == &source.Payload[0] {
		t.Fatal("destination payload aliases source")
	}
	firstPayload := &destination.Payload[0]
	if err := translator.TranslateInto(destination, source); err != nil {
		t.Fatal(err)
	}
	if &destination.Payload[0] != firstPayload {
		t.Fatal("destination payload capacity was not reused")
	}
}

func TestBuildDTMFRepeatsTerminalPacket(t *testing.T) {
	packets, err := BuildDTMF('#', 101, 42, 100, 9000)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) != 7 {
		t.Fatalf("expected 7 packets, got %d", len(packets))
	}
	for index, packet := range packets {
		ended := packet.Payload[1]&0x80 != 0
		if ended != (index >= 4) {
			t.Fatalf("unexpected end bit at packet %d", index)
		}
	}
}
