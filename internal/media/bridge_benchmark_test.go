package media

import (
	"sync/atomic"
	"testing"

	"github.com/pion/rtp"
)

func BenchmarkRTPTranslate(b *testing.B) {
	translator, err := NewTranslator(Mapping{
		SourceAudioPayloadType: 0, TargetAudioPayloadType: 8,
		SourceDTMFPayloadType: 101, TargetDTMFPayloadType: 110,
		TargetSSRC: 99, InitialSequence: 1000, InitialTimestamp: 8000,
	})
	if err != nil {
		b.Fatal(err)
	}
	packet := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 1, Timestamp: 160, SSRC: 42},
		Payload: make([]byte, 160),
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(packet.Payload)))
	b.ResetTimer()
	for index := range b.N {
		packet.SequenceNumber = uint16(index)
		packet.Timestamp = uint32(index * 160)
		if _, err := translator.Translate(packet); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRTPTranslate200ConcurrentCalls(b *testing.B) {
	const calls = 200
	translators := make([]*Translator, calls)
	for index := range translators {
		value, err := NewTranslator(Mapping{
			SourceAudioPayloadType: 0, TargetAudioPayloadType: 8,
			SourceDTMFPayloadType: 101, TargetDTMFPayloadType: 110,
			TargetSSRC: uint32(index + 1), InitialSequence: 1000, InitialTimestamp: 8000,
		})
		if err != nil {
			b.Fatal(err)
		}
		translators[index] = value
	}
	var next atomic.Uint64
	b.ReportAllocs()
	b.SetBytes(160)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		index := int(next.Add(1)-1) % calls
		packet := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 0},
			Payload: make([]byte, 160),
		}
		sequence := uint16(index)
		for pb.Next() {
			packet.SequenceNumber = sequence
			packet.Timestamp = uint32(sequence) * 160
			packet.SSRC = uint32(index + 100)
			if _, err := translators[index].Translate(packet); err != nil {
				b.Fatal(err)
			}
			sequence++
			index = (index + 1) % calls
		}
	})
}
