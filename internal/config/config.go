package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultHTTPListen    = "127.0.0.1:8080"
	defaultMetricsListen = "127.0.0.1:9090"
	defaultOrigin        = "https://debt-flow.uniga.app"
)

type Config struct {
	NodeID        string        `yaml:"nodeId"`
	HTTP          HTTP          `yaml:"http"`
	Logging       Logging       `yaml:"logging"`
	Capability    Capability    `yaml:"capability"`
	Storage       Storage       `yaml:"storage"`
	ICE           ICE           `yaml:"ice"`
	TURN          TURN          `yaml:"turn"`
	SIP           SIP           `yaml:"sip"`
	Limits        Limits        `yaml:"limits"`
	Lease         Lease         `yaml:"lease"`
	ShutdownGrace time.Duration `yaml:"shutdownGrace"`
}

type HTTP struct {
	Listen        string `yaml:"listen"`
	MetricsListen string `yaml:"metricsListen"`
	// AllowedOrigin is the legacy single-origin setting. AllowedOrigins takes
	// precedence when it is non-empty so existing deployments remain valid.
	AllowedOrigin               string        `yaml:"allowedOrigin"`
	AllowedOrigins              []string      `yaml:"allowedOrigins"`
	AllowInsecureLoopbackOrigin bool          `yaml:"allowInsecureLoopbackOrigin"`
	AuthTimeout                 time.Duration `yaml:"authTimeout"`
	Heartbeat                   time.Duration `yaml:"heartbeat"`
	IdleTimeout                 time.Duration `yaml:"idleTimeout"`
}

type Logging struct {
	Path       string `yaml:"path"`
	Level      string `yaml:"level"`
	MaxBytes   int64  `yaml:"maxBytes"`
	MaxBackups int    `yaml:"maxBackups"`
}

type Capability struct {
	Issuer             string            `yaml:"issuer"`
	Audience           string            `yaml:"audience"`
	ProtectedKeys      map[string]string `yaml:"protectedKeys"`
	PlaintextKeys      map[string]string `yaml:"plaintextKeys"`
	AllowPlaintextKeys bool              `yaml:"allowPlaintextKeys"`
}

type Storage struct {
	Path              string `yaml:"path"`
	MaxBytes          int64  `yaml:"maxBytes"`
	RetentionDays     int    `yaml:"retentionDays"`
	MaxPendingGlobal  int    `yaml:"maxPendingGlobal"`
	MaxPendingSession int    `yaml:"maxPendingSession"`
}

type ICE struct {
	UDPMuxListen string `yaml:"udpMuxListen"`
	AdvertisedIP string `yaml:"advertisedIp"`
}

type TURN struct {
	Enabled        bool          `yaml:"enabled"`
	ListenUDP      string        `yaml:"listenUdp"`
	ListenTCP      string        `yaml:"listenTcp"`
	Realm          string        `yaml:"realm"`
	AdvertisedIP   string        `yaml:"advertisedIp"`
	RelayMinPort   uint16        `yaml:"relayMinPort"`
	RelayMaxPort   uint16        `yaml:"relayMaxPort"`
	CredentialTTL  time.Duration `yaml:"credentialTtl"`
	MaxAllocations int           `yaml:"maxAllocations"`
	BytesPerSecond int           `yaml:"bytesPerSecond"`
}

type SIP struct {
	SpikeApproved   bool   `yaml:"spikeApproved"`
	ListenUDP       string `yaml:"listenUdp"`
	ListenTCP       string `yaml:"listenTcp"`
	RTPAdvertisedIP string `yaml:"rtpAdvertisedIp"`
	RTPMinPort      uint16 `yaml:"rtpMinPort"`
	RTPMaxPort      uint16 `yaml:"rtpMaxPort"`
}

type Limits struct {
	MaxMessageBytes   int64         `yaml:"maxMessageBytes"`
	MessagesPerWindow int           `yaml:"messagesPerWindow"`
	MessageWindow     time.Duration `yaml:"messageWindow"`
	WriterQueue       int           `yaml:"writerQueue"`
	MaxSessions       int           `yaml:"maxSessions"`
	MaxCalls          int           `yaml:"maxCalls"`
}

type Lease struct {
	RenewEvery     time.Duration `yaml:"renewEvery"`
	ExpiresAfter   time.Duration `yaml:"expiresAfter"`
	ReconnectGrace time.Duration `yaml:"reconnectGrace"`
}

func Default() Config {
	return Config{
		HTTP: HTTP{
			Listen: defaultHTTPListen, MetricsListen: defaultMetricsListen,
			AllowedOrigin: defaultOrigin, AuthTimeout: 5 * time.Second,
			Heartbeat: 15 * time.Second, IdleTimeout: 45 * time.Second,
		},
		Logging: Logging{Path: defaultLogPath(), Level: "info", MaxBytes: 10 << 20, MaxBackups: 5},
		Capability: Capability{
			Issuer: "debt-flow-web-api", Audience: "debt-flow-sip-gateway",
		},
		Storage: Storage{
			Path: filepath.Join("data", "gateway.db"), MaxBytes: 1 << 30,
			RetentionDays: 30, MaxPendingGlobal: 100_000, MaxPendingSession: 10_000,
		},
		ICE: ICE{UDPMuxListen: "0.0.0.0:22000"},
		TURN: TURN{
			Enabled: true, ListenUDP: "0.0.0.0:3478", ListenTCP: "0.0.0.0:3478",
			Realm: "sip-gateway.uniga.app", RelayMinPort: 30_000, RelayMaxPort: 30_999,
			CredentialTTL: 2 * time.Minute, MaxAllocations: 250, BytesPerSecond: 256 << 10,
		},
		SIP: SIP{
			ListenUDP: "0.0.0.0:5060", ListenTCP: "0.0.0.0:5060",
			RTPAdvertisedIP: "127.0.0.1", RTPMinPort: 20_000, RTPMaxPort: 21_999,
		},
		Limits: Limits{
			MaxMessageBytes: 64 << 10, MessagesPerWindow: 60,
			MessageWindow: 10 * time.Second, WriterQueue: 256,
			MaxSessions: 250, MaxCalls: 200,
		},
		Lease: Lease{
			RenewEvery: 45 * time.Second, ExpiresAfter: 120 * time.Second,
			ReconnectGrace: 60 * time.Second,
		},
		ShutdownGrace: 15 * time.Second,
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(b)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// EffectiveAllowedOrigins returns a detached allowlist. The plural setting is
// authoritative when present; otherwise the legacy single-origin value is used.
func (h HTTP) EffectiveAllowedOrigins() []string {
	if len(h.AllowedOrigins) > 0 {
		return append([]string(nil), h.AllowedOrigins...)
	}
	if h.AllowedOrigin == "" {
		return nil
	}
	return []string{h.AllowedOrigin}
}

func (c Config) Validate() error {
	var problems []error
	for label, address := range map[string]string{
		"http.listen": c.HTTP.Listen, "http.metricsListen": c.HTTP.MetricsListen,
		"ice.udpMuxListen": c.ICE.UDPMuxListen, "turn.listenUdp": c.TURN.ListenUDP,
		"turn.listenTcp": c.TURN.ListenTCP, "sip.listenUdp": c.SIP.ListenUDP,
		"sip.listenTcp": c.SIP.ListenTCP,
	} {
		_, rawPort, err := net.SplitHostPort(address)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s must be host:port: %w", label, err))
			continue
		}
		port, portErr := strconv.ParseUint(rawPort, 10, 16)
		if portErr != nil || port == 0 {
			problems = append(problems, fmt.Errorf("%s must use a numeric port from 1 to 65535", label))
		}
	}
	problems = append(problems, c.validatePortTopology()...)
	if c.HTTP.Listen != "127.0.0.1:8080" {
		problems = append(problems, errors.New("http.listen must be 127.0.0.1:8080 in P1"))
	}
	if c.HTTP.MetricsListen != "127.0.0.1:9090" {
		problems = append(problems, errors.New("http.metricsListen must be 127.0.0.1:9090 in P1"))
	}
	if strings.TrimSpace(c.Logging.Path) == "" || c.Logging.MaxBytes < 1<<20 ||
		c.Logging.MaxBackups < 1 || c.Logging.MaxBackups > 20 {
		problems = append(problems, errors.New("logging path, maxBytes (>= 1 MiB) and maxBackups (1-20) are required"))
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, errors.New("logging.level must be debug, info, warn, or error"))
	}
	origins := c.HTTP.EffectiveAllowedOrigins()
	if len(origins) == 0 {
		problems = append(problems, errors.New("http.allowedOrigins must contain at least one origin"))
	}
	hasInsecureLoopbackOrigin := false
	seenOrigins := make(map[string]struct{}, len(origins))
	for _, configuredOrigin := range origins {
		if configuredOrigin == "*" {
			if _, duplicate := seenOrigins[configuredOrigin]; duplicate {
				problems = append(problems, errors.New("http.allowedOrigins must not contain duplicate origins"))
			}
			seenOrigins[configuredOrigin] = struct{}{}
			continue
		}
		origin, err := url.Parse(configuredOrigin)
		validOriginShape := err == nil && origin.Host != "" && origin.Path == "" && origin.RawQuery == "" &&
			origin.Fragment == "" && origin.User == nil
		secureOrigin := validOriginShape && origin.Scheme == "https"
		insecureLoopbackOrigin := validOriginShape && origin.Scheme == "http" && isLoopbackHost(origin.Hostname())
		if !secureOrigin && !(c.HTTP.AllowInsecureLoopbackOrigin && insecureLoopbackOrigin) {
			problems = append(problems, fmt.Errorf(
				"http origin %q must be HTTPS without a path, or an explicitly enabled HTTP loopback origin",
				configuredOrigin))
		}
		if insecureLoopbackOrigin {
			hasInsecureLoopbackOrigin = true
		}
		if _, duplicate := seenOrigins[configuredOrigin]; duplicate {
			problems = append(problems, errors.New("http.allowedOrigins must not contain duplicate origins"))
		}
		seenOrigins[configuredOrigin] = struct{}{}
	}
	if c.HTTP.AllowInsecureLoopbackOrigin && !hasInsecureLoopbackOrigin {
		problems = append(problems, errors.New(
			"http.allowInsecureLoopbackOrigin requires an HTTP localhost or loopback entry"))
	}
	if c.NodeID == "" || c.Capability.Issuer == "" || c.Capability.Audience == "" {
		problems = append(problems, errors.New("nodeId and capability issuer/audience are required"))
	}
	if len(c.Capability.ProtectedKeys) == 0 && len(c.Capability.PlaintextKeys) == 0 {
		problems = append(problems, errors.New("at least one capability key is required"))
	}
	if len(c.Capability.PlaintextKeys) > 0 && !c.Capability.AllowPlaintextKeys {
		problems = append(problems, errors.New("plaintext capability keys require allowPlaintextKeys=true"))
	}
	for keyID, path := range c.Capability.ProtectedKeys {
		if strings.TrimSpace(keyID) == "" || strings.TrimSpace(path) == "" {
			problems = append(problems, errors.New("capability protected key IDs and paths must be non-empty"))
		}
	}
	for keyID, value := range c.Capability.PlaintextKeys {
		if strings.TrimSpace(keyID) == "" || strings.TrimSpace(value) == "" {
			problems = append(problems, errors.New("capability plaintext key IDs and values must be non-empty"))
		}
		if _, duplicate := c.Capability.ProtectedKeys[keyID]; duplicate {
			problems = append(problems, fmt.Errorf("capability key ID %q cannot be both protected and plaintext", keyID))
		}
	}
	if c.Storage.Path == "" || c.Storage.MaxBytes <= 0 || c.Storage.RetentionDays <= 0 {
		problems = append(problems, errors.New("storage path, maxBytes and retentionDays are required"))
	}
	if c.Storage.MaxPendingSession <= 0 || c.Storage.MaxPendingGlobal < c.Storage.MaxPendingSession {
		problems = append(problems, errors.New("storage pending-event limits are invalid"))
	}
	if !c.TURN.Enabled {
		problems = append(problems, errors.New("embedded TURN must be enabled in P1"))
	} else {
		if ip := net.ParseIP(c.TURN.AdvertisedIP); ip == nil || ip.To4() == nil {
			problems = append(problems, errors.New("turn.advertisedIp must be a valid IPv4 address when TURN is enabled"))
		}
		if c.TURN.Realm == "" || !validPortRange(c.TURN.RelayMinPort, c.TURN.RelayMaxPort) {
			problems = append(problems, errors.New("TURN realm and relay port range are required"))
		}
		if c.TURN.CredentialTTL <= 0 || c.TURN.MaxAllocations <= 0 || c.TURN.BytesPerSecond < 1600 {
			problems = append(problems,
				errors.New("TURN credential TTL, allocation limit and bytesPerSecond (>= 1600) are required"))
		}
	}
	if !validPortRange(c.SIP.RTPMinPort, c.SIP.RTPMaxPort) {
		problems = append(problems, errors.New("SIP RTP port range is invalid"))
	} else if rtpPairCapacity(c.SIP.RTPMinPort, c.SIP.RTPMaxPort) < c.Limits.MaxCalls {
		problems = append(problems, errors.New("SIP RTP port range must provide one RTP/RTCP pair per maximum call"))
	}
	if ip := net.ParseIP(c.SIP.RTPAdvertisedIP); ip == nil || ip.To4() == nil {
		problems = append(problems, errors.New("sip.rtpAdvertisedIp must be a valid IPv4 address"))
	}
	if ip := net.ParseIP(c.ICE.AdvertisedIP); ip == nil || ip.To4() == nil {
		problems = append(problems, errors.New("ice.advertisedIp must be a valid IPv4 address"))
	}
	if c.Limits.MaxMessageBytes != 64<<10 || c.Limits.MessagesPerWindow <= 0 ||
		c.Limits.MessageWindow <= 0 || c.Limits.WriterQueue <= 0 ||
		c.Limits.MaxSessions <= 0 || c.Limits.MaxCalls <= 0 {
		problems = append(problems, errors.New("protocol limits are invalid; P1 message limit is 64 KiB"))
	}
	if c.Limits.MaxCalls > c.Limits.MaxSessions {
		problems = append(problems, errors.New("maximum calls cannot exceed maximum sessions"))
	}
	if validPortRange(c.TURN.RelayMinPort, c.TURN.RelayMaxPort) &&
		int(c.TURN.RelayMaxPort)-int(c.TURN.RelayMinPort)+1 < c.TURN.MaxAllocations {
		problems = append(problems, errors.New("TURN relay port range must provide one port per maximum allocation"))
	}
	if c.Lease.RenewEvery != 45*time.Second || c.Lease.ExpiresAfter != 120*time.Second ||
		c.Lease.ReconnectGrace != 60*time.Second {
		problems = append(problems, errors.New("P1 lease intervals must be 45s renew, 120s expiry and 60s reconnect grace"))
	}
	if c.HTTP.AuthTimeout != 5*time.Second || c.HTTP.Heartbeat != 15*time.Second || c.HTTP.IdleTimeout != 45*time.Second {
		problems = append(problems, errors.New("P1 WSS intervals must be 5s auth, 15s heartbeat and 45s idle timeout"))
	}
	if c.ShutdownGrace != 15*time.Second {
		problems = append(problems, errors.New("P1 shutdownGrace must be 15s"))
	}
	return errors.Join(problems...)
}

type namedPort struct {
	name string
	port uint16
}

type namedPortRange struct {
	name    string
	minimum uint16
	maximum uint16
}

func (c Config) validatePortTopology() []error {
	udp := configuredPorts([]struct{ name, address string }{
		{"ice.udpMuxListen", c.ICE.UDPMuxListen},
		{"turn.listenUdp", c.TURN.ListenUDP},
		{"sip.listenUdp", c.SIP.ListenUDP},
	})
	tcp := configuredPorts([]struct{ name, address string }{
		{"http.listen", c.HTTP.Listen},
		{"http.metricsListen", c.HTTP.MetricsListen},
		{"turn.listenTcp", c.TURN.ListenTCP},
		{"sip.listenTcp", c.SIP.ListenTCP},
	})
	ranges := []namedPortRange{
		{"sip.rtp", c.SIP.RTPMinPort, c.SIP.RTPMaxPort},
		{"turn.relay", c.TURN.RelayMinPort, c.TURN.RelayMaxPort},
	}
	var problems []error
	problems = append(problems, duplicatePorts("UDP", udp)...)
	problems = append(problems, duplicatePorts("TCP", tcp)...)
	if validPortRange(ranges[0].minimum, ranges[0].maximum) &&
		validPortRange(ranges[1].minimum, ranges[1].maximum) &&
		rangesOverlap(ranges[0], ranges[1]) {
		problems = append(problems, errors.New("sip.rtp and turn.relay UDP port ranges must not overlap"))
	}
	for _, endpoint := range udp {
		for _, portRange := range ranges {
			if validPortRange(portRange.minimum, portRange.maximum) &&
				endpoint.port >= portRange.minimum && endpoint.port <= portRange.maximum {
				problems = append(problems, fmt.Errorf("%s UDP port conflicts with %s range", endpoint.name, portRange.name))
			}
		}
	}
	return problems
}

func configuredPorts(values []struct{ name, address string }) []namedPort {
	result := make([]namedPort, 0, len(values))
	for _, value := range values {
		_, rawPort, err := net.SplitHostPort(value.address)
		if err != nil {
			continue
		}
		port, err := strconv.ParseUint(rawPort, 10, 16)
		if err != nil || port == 0 {
			continue
		}
		result = append(result, namedPort{name: value.name, port: uint16(port)})
	}
	return result
}

func duplicatePorts(protocol string, values []namedPort) []error {
	var problems []error
	for index, left := range values {
		for _, right := range values[index+1:] {
			if left.port == right.port {
				problems = append(problems,
					fmt.Errorf("%s and %s must not share %s port %d", left.name, right.name, protocol, left.port))
			}
		}
	}
	return problems
}

func rangesOverlap(left, right namedPortRange) bool {
	return left.minimum <= right.maximum && right.minimum <= left.maximum
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validPortRange(minimum, maximum uint16) bool {
	return minimum > 0 && maximum >= minimum
}

func rtpPairCapacity(minimum, maximum uint16) int {
	if !validPortRange(minimum, maximum) {
		return 0
	}
	first := int(minimum)
	if first%2 != 0 {
		first++
	}
	return (int(maximum) - first + 1) / 2
}

func defaultLogPath() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\DebtFlow\SipGateway\logs\gateway.jsonl`
	}
	return filepath.Join("logs", "gateway.jsonl")
}
