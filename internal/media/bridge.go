package media

import (
	"errors"
	"fmt"
	"sync"

	"github.com/pion/rtp"
)

type Mapping struct {
	SourceAudioPayloadType uint8
	TargetAudioPayloadType uint8
	SourceDTMFPayloadType  uint8
	TargetDTMFPayloadType  uint8
	TargetSSRC             uint32
	InitialSequence        uint16
	InitialTimestamp       uint32
}

type Translator struct {
	mu              sync.Mutex
	mapping         Mapping
	initialized     bool
	firstSSRC       uint32
	firstSequence   uint16
	firstTimestamp  uint32
	targetSequence  uint16
	targetTimestamp uint32
	nextSequence    uint16
	nextTimestamp   uint32
	haveNextAudioTS bool
}

func NewTranslator(mapping Mapping) (*Translator, error) {
	if mapping.TargetSSRC == 0 {
		return nil, errors.New("target SSRC must be non-zero")
	}
	if mapping.SourceAudioPayloadType == mapping.SourceDTMFPayloadType ||
		mapping.TargetAudioPayloadType == mapping.TargetDTMFPayloadType {
		return nil, errors.New("audio and DTMF payload types must differ")
	}
	return &Translator{mapping: mapping}, nil
}

func (t *Translator) Translate(packet *rtp.Packet) (*rtp.Packet, error) {
	translated := &rtp.Packet{}
	if err := t.TranslateInto(translated, packet); err != nil {
		return nil, err
	}
	return translated, nil
}

// TranslateInto rewrites packet into dst while reusing dst's payload capacity.
// The caller owns dst and must not reuse it until the downstream synchronous
// write has returned.
func (t *Translator) TranslateInto(dst, packet *rtp.Packet) error {
	if dst == nil {
		return errors.New("destination RTP packet is required")
	}
	if packet == nil {
		return errors.New("RTP packet is required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	targetPayloadType, err := t.targetPayloadType(packet.PayloadType)
	if err != nil {
		return err
	}
	streamChanged := false
	if !t.initialized {
		t.firstSSRC = packet.SSRC
		t.firstSequence = packet.SequenceNumber
		t.firstTimestamp = packet.Timestamp
		t.targetSequence = t.mapping.InitialSequence
		t.targetTimestamp = t.mapping.InitialTimestamp
		t.nextSequence = t.mapping.InitialSequence
		t.nextTimestamp = t.mapping.InitialTimestamp
		t.initialized = true
		streamChanged = true
	} else if packet.SSRC != t.firstSSRC {
		// The source may legitimately change when early media and answered media
		// come from different PBX/provider legs, or when WebRTC reconnects during
		// an active call. Keep the target RTP stream continuous instead of
		// forwarding the new source's unrelated sequence/timestamp origin.
		t.firstSSRC = packet.SSRC
		t.firstSequence = packet.SequenceNumber
		t.firstTimestamp = packet.Timestamp
		t.targetSequence = t.nextSequence
		t.targetTimestamp = t.nextTimestamp
		streamChanged = true
	}
	dst.Header = packet.Header
	dst.Payload = append(dst.Payload[:0], packet.Payload...)
	dst.Raw = nil
	dst.PayloadType = targetPayloadType
	dst.SSRC = t.mapping.TargetSSRC
	dst.SequenceNumber = t.targetSequence + (packet.SequenceNumber - t.firstSequence)
	dst.Timestamp = t.targetTimestamp + (packet.Timestamp - t.firstTimestamp)
	if streamChanged {
		dst.Marker = true
	}
	if dst.SequenceNumber == t.nextSequence || rtpSequenceAhead(dst.SequenceNumber, t.nextSequence) {
		t.nextSequence = dst.SequenceNumber + 1
	}
	if packet.PayloadType == t.mapping.SourceAudioPayloadType && len(packet.Payload) > 0 {
		nextTimestamp := dst.Timestamp + uint32(len(packet.Payload))
		if !t.haveNextAudioTS || nextTimestamp == t.nextTimestamp || rtpTimestampAhead(nextTimestamp, t.nextTimestamp) {
			t.nextTimestamp = nextTimestamp
			t.haveNextAudioTS = true
		}
	}
	return nil
}

func rtpSequenceAhead(value, reference uint16) bool {
	return int16(value-reference) > 0
}

func rtpTimestampAhead(value, reference uint32) bool {
	return int32(value-reference) > 0
}

func (t *Translator) SSRCMapping() (source, target uint32, initialized bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.firstSSRC, t.mapping.TargetSSRC, t.initialized
}

func (t *Translator) targetPayloadType(source uint8) (uint8, error) {
	switch source {
	case t.mapping.SourceAudioPayloadType:
		return t.mapping.TargetAudioPayloadType, nil
	case t.mapping.SourceDTMFPayloadType:
		return t.mapping.TargetDTMFPayloadType, nil
	default:
		return 0, fmt.Errorf("unsupported RTP payload type %d", source)
	}
}

func BuildDTMF(digit byte, payloadType uint8, ssrc uint32, sequence uint16, timestamp uint32) ([]*rtp.Packet, error) {
	event, ok := dtmfEvent(digit)
	if !ok {
		return nil, errors.New("unsupported RFC 4733 DTMF digit")
	}
	const volume = 10
	durations := []uint16{160, 320, 480, 640, 800, 800, 800}
	packets := make([]*rtp.Packet, 0, len(durations))
	for index, duration := range durations {
		end := index >= len(durations)-3
		secondByte := byte(volume)
		if end {
			secondByte |= 0x80
		}
		packets = append(packets, &rtp.Packet{
			Header: rtp.Header{
				Version: 2, Marker: index == 0, PayloadType: payloadType,
				SequenceNumber: sequence + uint16(index), Timestamp: timestamp, SSRC: ssrc,
			},
			Payload: []byte{event, secondByte, byte(duration >> 8), byte(duration)},
		})
	}
	return packets, nil
}

func dtmfEvent(digit byte) (byte, bool) {
	switch {
	case digit >= '0' && digit <= '9':
		return digit - '0', true
	case digit == '*':
		return 10, true
	case digit == '#':
		return 11, true
	case digit >= 'A' && digit <= 'D':
		return 12 + digit - 'A', true
	default:
		return 0, false
	}
}
