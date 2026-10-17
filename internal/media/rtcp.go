package media

import (
	"errors"

	"github.com/pion/rtcp"
)

// RewriteRTCP clones a compound RTCP packet and rewrites the sender and
// reported media SSRCs to match the RTP translations on the destination leg.
// Unsupported video-oriented feedback is dropped because P1 is audio-only.
func RewriteRTCP(packets []rtcp.Packet, senderFrom, senderTo, reportFrom, reportTo uint32) ([]rtcp.Packet, error) {
	if senderFrom == 0 || senderTo == 0 || reportFrom == 0 || reportTo == 0 {
		return nil, errors.New("complete RTCP SSRC mappings are required")
	}
	raw, err := rtcp.Marshal(packets)
	if err != nil {
		return nil, err
	}
	cloned, err := rtcp.Unmarshal(raw)
	if err != nil {
		return nil, err
	}
	result := make([]rtcp.Packet, 0, len(cloned))
	for _, packet := range cloned {
		switch value := packet.(type) {
		case *rtcp.SenderReport:
			value.SSRC = replaceSSRC(value.SSRC, senderFrom, senderTo)
			rewriteReports(value.Reports, reportFrom, reportTo)
			result = append(result, value)
		case *rtcp.ReceiverReport:
			value.SSRC = replaceSSRC(value.SSRC, senderFrom, senderTo)
			rewriteReports(value.Reports, reportFrom, reportTo)
			result = append(result, value)
		case *rtcp.SourceDescription:
			for index := range value.Chunks {
				value.Chunks[index].Source = replaceSSRC(value.Chunks[index].Source, senderFrom, senderTo)
			}
			result = append(result, value)
		case *rtcp.Goodbye:
			for index, source := range value.Sources {
				value.Sources[index] = replaceEither(source, senderFrom, senderTo, reportFrom, reportTo)
			}
			result = append(result, value)
		case *rtcp.TransportLayerNack:
			value.SenderSSRC = replaceSSRC(value.SenderSSRC, senderFrom, senderTo)
			value.MediaSSRC = replaceSSRC(value.MediaSSRC, reportFrom, reportTo)
			result = append(result, value)
		}
	}
	return result, nil
}

func rewriteReports(reports []rtcp.ReceptionReport, from, to uint32) {
	for index := range reports {
		reports[index].SSRC = replaceSSRC(reports[index].SSRC, from, to)
	}
}

func replaceSSRC(value, from, to uint32) uint32 {
	if value == from {
		return to
	}
	return value
}

func replaceEither(value, firstFrom, firstTo, secondFrom, secondTo uint32) uint32 {
	if value == firstFrom {
		return firstTo
	}
	return replaceSSRC(value, secondFrom, secondTo)
}
