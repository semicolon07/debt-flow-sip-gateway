package sip

import (
	"errors"
	"net"
	"strings"
)

// Identity is the canonical SIP address-of-record ownership boundary inside a
// Gateway process. Username is deliberately case-sensitive; the remaining
// routing fields are normalized from server-owned configuration.
type Identity struct {
	Transport Transport
	Host      string
	Port      uint16
	Realm     string
	Username  string
}

func IdentityOf(configuration Configuration) (Identity, error) {
	host, err := canonicalConfiguredHost(configuration.Host)
	if err != nil {
		return Identity{}, err
	}
	transport := Transport(strings.ToLower(string(configuration.Transport)))
	if transport != TransportUDP && transport != TransportTCP {
		return Identity{}, errors.New("SIP transport must be udp or tcp")
	}
	if configuration.Port == 0 || configuration.Username == "" {
		return Identity{}, errors.New("SIP port and username are required")
	}
	return Identity{
		Transport: transport,
		Host:      host,
		Port:      configuration.Port,
		Realm:     strings.TrimSpace(configuration.Realm),
		Username:  configuration.Username,
	}, nil
}

func canonicalConfiguredHost(value string) (string, error) {
	host := strings.TrimSpace(value)
	if host == "" {
		return "", errors.New("SIP host is required")
	}
	if ip := net.ParseIP(host); ip != nil {
		ipv4 := ip.To4()
		if ipv4 == nil {
			return "", errors.New("SIP host must be IPv4 or an ASCII hostname")
		}
		return ipv4.String(), nil
	}
	if strings.HasSuffix(host, ".") {
		host = strings.TrimSuffix(host, ".")
	}
	host = strings.ToLower(host)
	if len(host) == 0 || len(host) > 253 || strings.ContainsAny(host, ":/[]@") {
		return "", errors.New("SIP host must be IPv4 or an ASCII hostname")
	}
	for _, character := range host {
		if character > 127 {
			return "", errors.New("SIP hostname must contain ASCII characters only")
		}
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || !isHostnameAlphaNumeric(label[0]) ||
			!isHostnameAlphaNumeric(label[len(label)-1]) {
			return "", errors.New("SIP hostname is invalid")
		}
		for index := 1; index < len(label)-1; index++ {
			if !isHostnameAlphaNumeric(label[index]) && label[index] != '-' {
				return "", errors.New("SIP hostname is invalid")
			}
		}
	}
	return host, nil
}

func isHostnameAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
}
