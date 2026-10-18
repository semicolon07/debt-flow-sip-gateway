package sip

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

var ErrNoCodecIntersection = errors.New("remote SDP has no compatible PCMU/PCMA codec")

func ParseRemoteSDP(raw []byte) (MediaDescription, error) {
	return parseRemoteSDP(raw, "")
}

// ParseRemoteSDPForCodec selects the exact G.711 codec already negotiated by
// WebRTC from every codec offered by the SIP peer. An inbound offer is allowed
// to list PCMU and PCMA in any preference order; no transcoding is performed.
func ParseRemoteSDPForCodec(raw []byte, preferredCodec string) (MediaDescription, error) {
	preferredCodec = strings.ToUpper(strings.TrimSpace(preferredCodec))
	if preferredCodec != "PCMU" && preferredCodec != "PCMA" {
		return MediaDescription{}, fmt.Errorf("unsupported preferred codec %q", preferredCodec)
	}
	return parseRemoteSDP(raw, preferredCodec)
}

func parseRemoteSDP(raw []byte, preferredCodec string) (MediaDescription, error) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var sessionAddress string
	var address string
	var port uint16
	var rtcpAddress string
	var rtcpPort uint16
	var payloadOrder []uint8
	codecs := make(map[uint8]string)
	mediaSection := "session"
	audioSelected := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "c=IN IP4 ") || strings.HasPrefix(line, "c=IN IP6 ") {
			fields := strings.Fields(line)
			if len(fields) == 3 {
				candidate := strings.SplitN(fields[2], "/", 2)[0]
				if mediaSection == "session" {
					sessionAddress = candidate
				} else if mediaSection == "audio" {
					address = candidate
				}
			}
		}
		if strings.HasPrefix(line, "m=") {
			fields := strings.Fields(line)
			if len(fields) >= 4 && fields[0] == "m=audio" && !audioSelected {
				mediaSection = "audio"
				audioSelected = true
				address = sessionAddress
				parsedPort, _ := strconv.ParseUint(fields[1], 10, 16)
				port = uint16(parsedPort)
				for _, value := range fields[3:] {
					parsed, err := strconv.ParseUint(value, 10, 8)
					if err == nil {
						payloadOrder = append(payloadOrder, uint8(parsed))
					}
				}
			} else {
				mediaSection = "other"
			}
		}
		if mediaSection == "audio" && strings.HasPrefix(line, "a=rtpmap:") {
			parts := strings.Fields(strings.TrimPrefix(line, "a=rtpmap:"))
			if len(parts) == 2 {
				payload, err := strconv.ParseUint(parts[0], 10, 8)
				if err == nil {
					codecs[uint8(payload)] = strings.ToUpper(strings.SplitN(parts[1], "/", 2)[0])
				}
			}
		}
		if mediaSection == "audio" && strings.HasPrefix(line, "a=rtcp:") {
			fields := strings.Fields(strings.TrimPrefix(line, "a=rtcp:"))
			if len(fields) > 0 {
				parsedPort, _ := strconv.ParseUint(fields[0], 10, 16)
				rtcpPort = uint16(parsedPort)
			}
			if len(fields) == 4 && fields[1] == "IN" &&
				(fields[2] == "IP4" || fields[2] == "IP6") {
				rtcpAddress = strings.SplitN(fields[3], "/", 2)[0]
			}
		}
	}
	if ip := net.ParseIP(address); ip == nil || ip.To4() == nil || port == 0 || port == 65535 {
		return MediaDescription{}, errors.New("remote SDP has no valid IPv4 audio connection address")
	}
	if rtcpPort == 0 {
		// The Gateway advertises separate RTP/RTCP sockets and deliberately does
		// not include rtcp-mux in its offer/answer. Merely receiving rtcp-mux in
		// the remote SDP is therefore not enough to negotiate mux.
		rtcpPort = port + 1
	}
	if rtcpAddress == "" {
		rtcpAddress = address
	}
	if ip := net.ParseIP(rtcpAddress); ip == nil || ip.To4() == nil {
		return MediaDescription{}, errors.New("remote SDP has no valid IPv4 RTCP connection address")
	}
	description := MediaDescription{
		IP: address, Port: port, RTCPIP: rtcpAddress, RTCPPort: rtcpPort, DTMFPayloadType: 101,
	}
	for _, payload := range payloadOrder {
		codec := codecs[payload]
		if codec == "" {
			if payload == 0 {
				codec = "PCMU"
			} else if payload == 8 {
				codec = "PCMA"
			}
		}
		isStaticG711 := (codec == "PCMU" && payload == 0) || (codec == "PCMA" && payload == 8)
		if description.Codec == "" && isStaticG711 && (preferredCodec == "" || codec == preferredCodec) {
			description.Codec = codec
			description.AudioPayloadType = payload
		}
		if codec == "TELEPHONE-EVENT" {
			description.DTMFPayloadType = payload
		}
	}
	if description.Codec == "" {
		return MediaDescription{}, ErrNoCodecIntersection
	}
	return description, nil
}
