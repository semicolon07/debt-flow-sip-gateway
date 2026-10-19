package webrtc

import (
	"testing"

	"github.com/pion/rtp"
)

func TestRTPStreamContinuityPreservesOneSourceTimeline(t *testing.T) {
	continuity := rtpStreamContinuity{}
	first := audioPacket(11, 40000, 900000, 160)
	continuity.rewrite(first, 0)
	second := audioPacket(11, 40001, 900160, 160)
	continuity.rewrite(second, 0)

	if first.SequenceNumber != 40000 || first.Timestamp != 900000 || !first.Marker {
		t.Fatalf("unexpected first packet: seq=%d ts=%d marker=%t", first.SequenceNumber, first.Timestamp, first.Marker)
	}
	if second.SequenceNumber != 40001 || second.Timestamp != 900160 {
		t.Fatalf("source timeline changed: seq=%d ts=%d", second.SequenceNumber, second.Timestamp)
	}
}

func TestRTPStreamContinuityJoinsConsecutiveCalls(t *testing.T) {
	continuity := rtpStreamContinuity{}
	firstCall := []*rtp.Packet{
		audioPacket(11, 65000, 420000, 160),
		audioPacket(11, 65001, 420160, 160),
	}
	for _, packet := range firstCall {
		continuity.rewrite(packet, 0)
	}
	secondCall := []*rtp.Packet{
		audioPacket(22, 77, 12, 160),
		audioPacket(22, 78, 172, 160),
	}
	for _, packet := range secondCall {
		continuity.rewrite(packet, 0)
	}

	if secondCall[0].SequenceNumber != 65002 || secondCall[0].Timestamp != 420320 || !secondCall[0].Marker {
		t.Fatalf("new call was not joined to the track timeline: seq=%d ts=%d marker=%t",
			secondCall[0].SequenceNumber, secondCall[0].Timestamp, secondCall[0].Marker)
	}
	if secondCall[1].SequenceNumber != 65003 || secondCall[1].Timestamp != 420480 {
		t.Fatalf("new call deltas were not preserved: seq=%d ts=%d",
			secondCall[1].SequenceNumber, secondCall[1].Timestamp)
	}
}

func TestRTPStreamContinuityIgnoresLatePacketsAtCallBoundary(t *testing.T) {
	continuity := rtpStreamContinuity{}
	for _, packet := range []*rtp.Packet{
		audioPacket(11, 100, 1000, 160),
		audioPacket(11, 102, 1320, 160),
		audioPacket(11, 101, 1160, 160), // reordered packet must not move the boundary backwards
	} {
		continuity.rewrite(packet, 0)
	}
	nextCall := audioPacket(22, 5, 5, 160)
	continuity.rewrite(nextCall, 0)

	if nextCall.SequenceNumber != 103 || nextCall.Timestamp != 1480 {
		t.Fatalf("late packet moved continuity backwards: seq=%d ts=%d", nextCall.SequenceNumber, nextCall.Timestamp)
	}
}

func TestRTPStreamContinuityWrapsSequenceAndTimestamp(t *testing.T) {
	continuity := rtpStreamContinuity{}
	last := audioPacket(11, 65535, ^uint32(0)-79, 160)
	continuity.rewrite(last, 0)
	nextCall := audioPacket(22, 10, 10, 160)
	continuity.rewrite(nextCall, 0)

	if nextCall.SequenceNumber != 0 || nextCall.Timestamp != 80 {
		t.Fatalf("wrap-around continuity failed: seq=%d ts=%d", nextCall.SequenceNumber, nextCall.Timestamp)
	}
}

func TestRTPStreamContinuitySoakAcrossManyCalls(t *testing.T) {
	continuity := rtpStreamContinuity{}
	var previousSequence uint16
	var previousTimestamp uint32
	for call := 0; call < 500; call++ {
		for index := 0; index < 50; index++ {
			packet := audioPacket(
				uint32(call+1),
				uint16(1234+call*997+index),
				uint32(5678+call*7919+index*160),
				160,
			)
			continuity.rewrite(packet, 0)
			if call != 0 || index != 0 {
				if packet.SequenceNumber != previousSequence+1 {
					t.Fatalf("call=%d packet=%d sequence gap: got=%d want=%d",
						call, index, packet.SequenceNumber, previousSequence+1)
				}
				if packet.Timestamp != previousTimestamp+160 {
					t.Fatalf("call=%d packet=%d timestamp gap: got=%d want=%d",
						call, index, packet.Timestamp, previousTimestamp+160)
				}
			}
			previousSequence = packet.SequenceNumber
			previousTimestamp = packet.Timestamp
		}
	}
}

func audioPacket(ssrc uint32, sequence uint16, timestamp uint32, payloadBytes int) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    0,
			SequenceNumber: sequence,
			Timestamp:      timestamp,
			SSRC:           ssrc,
		},
		Payload: make([]byte, payloadBytes),
	}
}
