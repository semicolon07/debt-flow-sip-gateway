package media

import "strings"

const (
	minimumG711ActivitySamples = 20
	minimumG711ActiveLevel     = 256
)

// HasAudibleG711 reports whether a G.711 payload contains enough signal to
// replace locally generated ringback. Providers commonly send valid RTP
// packets containing digital silence before their ringback or announcement
// begins, so packet presence alone is not evidence of audible early media.
func HasAudibleG711(codec string, payload []byte) bool {
	if len(payload) < minimumG711ActivitySamples {
		return false
	}
	decode, ok := g711Decoder(codec)
	if !ok {
		return false
	}
	var energy int64
	activeSamples := 0
	for _, encoded := range payload {
		sample := int64(decode(encoded))
		absolute := sample
		if absolute < 0 {
			absolute = -absolute
		}
		if absolute >= minimumG711ActiveLevel {
			activeSamples++
		}
		energy += sample * sample
	}
	minimumEnergy := int64(minimumG711ActiveLevel * minimumG711ActiveLevel)
	return activeSamples*5 >= len(payload) && energy/int64(len(payload)) >= minimumEnergy
}

func g711Decoder(codec string) (func(byte) int16, bool) {
	switch {
	case strings.EqualFold(codec, "PCMU"):
		return decodeMuLaw, true
	case strings.EqualFold(codec, "PCMA"):
		return decodeALaw, true
	default:
		return nil, false
	}
}

func decodeMuLaw(encoded byte) int16 {
	value := ^encoded
	magnitude := ((int(value)&0x0f)<<3 + 0x84) << ((value & 0x70) >> 4)
	if value&0x80 != 0 {
		return int16(0x84 - magnitude)
	}
	return int16(magnitude - 0x84)
}

func decodeALaw(encoded byte) int16 {
	value := encoded ^ 0x55
	magnitude := int(value&0x0f) << 4
	segment := (value & 0x70) >> 4
	switch segment {
	case 0:
		magnitude += 8
	case 1:
		magnitude += 0x108
	default:
		magnitude += 0x108
		magnitude <<= segment - 1
	}
	if value&0x80 != 0 {
		return int16(magnitude)
	}
	return int16(-magnitude)
}
