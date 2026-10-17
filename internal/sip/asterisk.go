package sip

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

type AsteriskProfile struct{}

func (AsteriskProfile) ValidateConfiguration(cfg Configuration) error {
	if net.ParseIP(cfg.Host) == nil && strings.TrimSpace(cfg.Host) == "" {
		return errors.New("SIP host is required")
	}
	if cfg.Port == 0 || cfg.Username == "" || cfg.Password == "" {
		return errors.New("SIP port, username and password are required")
	}
	if cfg.Transport != TransportUDP && cfg.Transport != TransportTCP {
		return errors.New("SIP transport must be udp or tcp")
	}
	return nil
}

func (AsteriskProfile) BuildSDP(media MediaDescription) ([]byte, error) {
	if net.ParseIP(media.IP) == nil || media.Port == 0 || media.Port == 65535 {
		return nil, errors.New("valid RTP IP and port are required")
	}
	if media.RTCPIP == "" {
		media.RTCPIP = media.IP
	}
	if media.RTCPPort == 0 {
		media.RTCPPort = media.Port + 1
	}
	if net.ParseIP(media.RTCPIP) == nil {
		return nil, errors.New("valid RTCP IP and port are required")
	}
	codec := strings.ToUpper(media.Codec)
	staticPayload := uint8(0)
	switch codec {
	case "PCMU":
		staticPayload = 0
	case "PCMA":
		staticPayload = 8
	default:
		return nil, errors.New("P1 supports only PCMU and PCMA")
	}
	if media.AudioPayloadType != staticPayload {
		return nil, fmt.Errorf("%s must use static payload type %d toward Asterisk", codec, staticPayload)
	}
	if media.DTMFPayloadType == staticPayload {
		return nil, errors.New("DTMF payload type must differ from audio")
	}
	addressFamily := "IP4"
	if strings.Contains(media.IP, ":") {
		addressFamily = "IP6"
	}
	lines := []string{
		"v=0",
		"o=debt-flow 0 0 IN " + addressFamily + " " + media.IP,
		"s=Debt Flow SIP Gateway",
		"c=IN " + addressFamily + " " + media.IP,
		"t=0 0",
		"m=audio " + strconv.Itoa(int(media.Port)) + " RTP/AVP " +
			strconv.Itoa(int(media.AudioPayloadType)) + " " + strconv.Itoa(int(media.DTMFPayloadType)),
		"a=rtcp:" + strconv.Itoa(int(media.RTCPPort)) + " IN " + addressFamily + " " + media.RTCPIP,
		"a=rtpmap:" + strconv.Itoa(int(media.AudioPayloadType)) + " " + codec + "/8000",
		"a=rtpmap:" + strconv.Itoa(int(media.DTMFPayloadType)) + " telephone-event/8000",
		"a=fmtp:" + strconv.Itoa(int(media.DTMFPayloadType)) + " 0-16",
		"a=ptime:20",
		"a=sendrecv",
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n"), nil
}

func (AsteriskProfile) NormalizeFailure(status int) string {
	switch status {
	case 401, 403, 407:
		return "authentication_failed"
	case 404, 484:
		return "invalid_destination"
	case 480, 408:
		return "no_answer"
	case 486, 600:
		return "busy"
	case 487:
		return "cancelled"
	case 488, 415:
		return "codec_not_supported"
	case 500, 502, 503, 504:
		return "provider_unavailable"
	default:
		return "sip_failed"
	}
}
