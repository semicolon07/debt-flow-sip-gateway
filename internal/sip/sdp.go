package sip

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

func ParseRemoteSDP(raw []byte) (MediaDescription, error) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var address string
	var port uint16
	var rtcpAddress string
	var rtcpPort uint16
	var payloadOrder []uint8
	codecs := make(map[uint8]string)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "c=IN IP4 ") || strings.HasPrefix(line, "c=IN IP6 ") {
			fields := strings.Fields(line)
			if len(fields) == 3 {
				address = strings.SplitN(fields[2], "/", 2)[0]
			}
		}
		if strings.HasPrefix(line, "m=audio ") {
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				parsedPort, _ := strconv.ParseUint(fields[1], 10, 16)
				port = uint16(parsedPort)
				for _, value := range fields[3:] {
					parsed, err := strconv.ParseUint(value, 10, 8)
					if err == nil {
						payloadOrder = append(payloadOrder, uint8(parsed))
					}
				}
			}
		}
		if strings.HasPrefix(line, "a=rtpmap:") {
			parts := strings.Fields(strings.TrimPrefix(line, "a=rtpmap:"))
			if len(parts) == 2 {
				payload, err := strconv.ParseUint(parts[0], 10, 8)
				if err == nil {
					codecs[uint8(payload)] = strings.ToUpper(strings.SplitN(parts[1], "/", 2)[0])
				}
			}
		}
		if strings.HasPrefix(line, "a=rtcp:") {
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
	if net.ParseIP(address) == nil || port == 0 || port == 65535 {
		return MediaDescription{}, errors.New("remote SDP has no valid audio connection address")
	}
	if rtcpPort == 0 {
		rtcpPort = port + 1
	}
	if rtcpAddress == "" {
		rtcpAddress = address
	}
	if net.ParseIP(rtcpAddress) == nil {
		return MediaDescription{}, errors.New("remote SDP has no valid RTCP connection address")
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
		if description.Codec == "" && (codec == "PCMU" || codec == "PCMA") {
			description.Codec = codec
			description.AudioPayloadType = payload
		}
		if codec == "TELEPHONE-EVENT" {
			description.DTMFPayloadType = payload
		}
	}
	if description.Codec == "" {
		return MediaDescription{}, errors.New("remote SDP has no PCMU/PCMA codec")
	}
	return description, nil
}
