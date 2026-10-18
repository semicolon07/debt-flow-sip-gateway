package webrtc

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
	"golang.org/x/net/ipv4"
)

const (
	mimePCMU           = "audio/PCMU"
	mimePCMA           = "audio/PCMA"
	mimeTelephoneEvent = "audio/telephone-event"
	iceUDPReadBuffer   = 8 << 20
	iceUDPWriteBuffer  = 4 << 20
)

type Config struct {
	UDPMuxListen string
	AdvertisedIP string
	TURNURLs     []string
	MaxSessions  int
}

type TURNCredential struct {
	Username string
	Password string
}

type CodecInfo struct {
	Name             string
	AudioPayloadType uint8
	DTMFPayloadType  uint8
}

type CandidateHandler func(sessionID string, candidate pion.ICECandidateInit)
type RTPHandler func(sessionID string, packet *rtp.Packet)
type RTCPHandler func(sessionID string, packets []rtcp.Packet)
type StateHandler func(sessionID string, state pion.PeerConnectionState)

type Engine struct {
	api         *pion.API
	udpConn     net.PacketConn
	udpMux      ice.UDPMux
	turnURLs    []string
	maxSessions int
	onCandidate CandidateHandler
	onRTP       RTPHandler
	onRTCP      RTCPHandler
	onState     StateHandler
	mu          sync.RWMutex
	peers       map[string]*peer
}

type peer struct {
	connection *pion.PeerConnection
	outbound   *pion.TrackLocalStaticRTP
	codec      pion.RTPCodecCapability
	codecInfo  CodecInfo
	mu         sync.RWMutex
}

type Stats struct {
	Sessions  int
	ICERoutes map[string]int
}

func New(cfg Config, onCandidate CandidateHandler, onRTP RTPHandler, onRTCP RTCPHandler, onState StateHandler) (*Engine, error) {
	if cfg.MaxSessions <= 0 {
		return nil, errors.New("WebRTC max sessions must be positive")
	}
	udpConn, err := net.ListenPacket("udp4", cfg.UDPMuxListen)
	if err != nil {
		return nil, fmt.Errorf("listen ICE UDP mux: %w", err)
	}
	if conn, ok := udpConn.(*net.UDPConn); ok {
		if err := conn.SetReadBuffer(iceUDPReadBuffer); err != nil {
			_ = udpConn.Close()
			return nil, fmt.Errorf("set ICE UDP read buffer: %w", err)
		}
		if err := conn.SetWriteBuffer(iceUDPWriteBuffer); err != nil {
			_ = udpConn.Close()
			return nil, fmt.Errorf("set ICE UDP write buffer: %w", err)
		}
	}
	if err := ipv4.NewPacketConn(udpConn).SetTOS(46 << 2); err != nil {
		slog.Warn("WebRTC UDP DSCP EF is unavailable; continuing without QoS marking", "error", err)
	}
	loggerFactory := logging.NewDefaultLoggerFactory()
	loggerFactory.DefaultLogLevel = logging.LogLevelDisabled
	loggerFactory.Writer = discardWriter{}
	udpMux := pion.NewICEUDPMux(loggerFactory.NewLogger("ice-udp-mux"), udpConn)

	media := &pion.MediaEngine{}
	for _, codec := range supportedCodecs() {
		if err := media.RegisterCodec(codec, pion.RTPCodecTypeAudio); err != nil {
			_ = udpMux.Close()
			_ = udpConn.Close()
			return nil, fmt.Errorf("register WebRTC codec: %w", err)
		}
	}
	registry := &interceptor.Registry{}
	if err := pion.RegisterDefaultInterceptors(media, registry); err != nil {
		_ = udpMux.Close()
		_ = udpConn.Close()
		return nil, fmt.Errorf("register WebRTC interceptors: %w", err)
	}
	settings := pion.SettingEngine{}
	settings.SetICEUDPMux(udpMux)
	if cfg.AdvertisedIP != "" {
		if net.ParseIP(cfg.AdvertisedIP) == nil {
			_ = udpMux.Close()
			_ = udpConn.Close()
			return nil, errors.New("invalid WebRTC advertised IP")
		}
		settings.SetNAT1To1IPs([]string{cfg.AdvertisedIP}, pion.ICECandidateTypeHost)
	}
	return &Engine{
		api: pion.NewAPI(
			pion.WithMediaEngine(media), pion.WithInterceptorRegistry(registry), pion.WithSettingEngine(settings),
		),
		udpConn: udpConn, udpMux: udpMux, turnURLs: append([]string(nil), cfg.TURNURLs...),
		maxSessions: cfg.MaxSessions, onCandidate: onCandidate, onRTP: onRTP, onRTCP: onRTCP,
		onState: onState, peers: make(map[string]*peer),
	}, nil
}

func (e *Engine) Open(sessionID string, credential TURNCredential) error {
	if sessionID == "" {
		return errors.New("sessionID is required")
	}
	e.mu.RLock()
	_, replacing := e.peers[sessionID]
	atCapacity := len(e.peers) >= e.maxSessions
	e.mu.RUnlock()
	if !replacing && atCapacity {
		return errors.New("WebRTC session capacity reached")
	}
	configuration := pion.Configuration{}
	if len(e.turnURLs) > 0 {
		configuration.ICEServers = []pion.ICEServer{{
			URLs: append([]string(nil), e.turnURLs...), Username: credential.Username,
			Credential: credential.Password, CredentialType: pion.ICECredentialTypePassword,
		}}
	}
	connection, err := e.api.NewPeerConnection(configuration)
	if err != nil {
		return err
	}
	p := &peer{connection: connection}
	connection.OnICECandidate(func(candidate *pion.ICECandidate) {
		if candidate != nil && e.onCandidate != nil {
			e.onCandidate(sessionID, candidate.ToJSON())
		}
	})
	connection.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		e.mu.RLock()
		current := e.peers[sessionID]
		e.mu.RUnlock()
		// Reconnect replaces the PeerConnection. Ignore the terminal callback from
		// the intentionally retired peer so it cannot tear down the resumed call.
		if current == p && e.onState != nil {
			e.onState(sessionID, state)
		}
	})
	connection.OnTrack(func(track *pion.TrackRemote, _ *pion.RTPReceiver) {
		mime := strings.ToLower(track.Codec().MimeType)
		if mime != strings.ToLower(mimePCMU) && mime != strings.ToLower(mimePCMA) {
			return
		}
		for {
			packet, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			if e.onRTP != nil {
				e.onRTP(sessionID, packet)
			}
		}
	})
	e.mu.Lock()
	previous := e.peers[sessionID]
	if previous == nil && len(e.peers) >= e.maxSessions {
		e.mu.Unlock()
		_ = connection.Close()
		return errors.New("WebRTC session capacity reached")
	}
	e.peers[sessionID] = p
	e.mu.Unlock()
	if previous != nil {
		_ = previous.connection.Close()
	}
	return nil
}

func (e *Engine) AcceptOffer(sessionID, sdp string) (string, error) {
	p, err := e.get(sessionID)
	if err != nil {
		return "", err
	}
	selected, err := selectCodec(sdp)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.outbound != nil {
		return "", errors.New("WebRTC offer already accepted for this session")
	}
	if err := p.connection.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: sdp}); err != nil {
		return "", fmt.Errorf("set WebRTC offer: %w", err)
	}
	track, err := pion.NewTrackLocalStaticRTP(selected.capability, "audio", "debt-flow-sip-gateway")
	if err != nil {
		return "", err
	}
	sender, err := p.connection.AddTrack(track)
	if err != nil {
		return "", err
	}
	go func() {
		for {
			packets, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			if len(packets) > 0 && e.onRTCP != nil {
				e.onRTCP(sessionID, packets)
			}
		}
	}()
	answer, err := p.connection.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	if err := p.connection.SetLocalDescription(answer); err != nil {
		return "", err
	}
	p.outbound = track
	p.codec = selected.capability
	p.codecInfo = CodecInfo{
		Name:             strings.TrimPrefix(strings.ToUpper(selected.capability.MimeType), "AUDIO/"),
		AudioPayloadType: selected.payloadType, DTMFPayloadType: selected.dtmfPayloadType,
	}
	return answer.SDP, nil
}

func (e *Engine) Codec(sessionID string) (CodecInfo, error) {
	p, err := e.get(sessionID)
	if err != nil {
		return CodecInfo{}, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.outbound == nil {
		return CodecInfo{}, errors.New("WebRTC codec is not negotiated")
	}
	return p.codecInfo, nil
}

func (e *Engine) State(sessionID string) (pion.PeerConnectionState, error) {
	p, err := e.get(sessionID)
	if err != nil {
		return pion.PeerConnectionStateClosed, err
	}
	return p.connection.ConnectionState(), nil
}

func (e *Engine) AddCandidate(sessionID string, candidate pion.ICECandidateInit) error {
	p, err := e.get(sessionID)
	if err != nil {
		return err
	}
	return p.connection.AddICECandidate(candidate)
}

func (e *Engine) WriteRTP(sessionID string, packet *rtp.Packet) error {
	p, err := e.get(sessionID)
	if err != nil {
		return err
	}
	p.mu.RLock()
	track := p.outbound
	p.mu.RUnlock()
	if track == nil {
		return errors.New("WebRTC outbound track is not ready")
	}
	return track.WriteRTP(packet)
}

func (e *Engine) WriteRTCP(sessionID string, packets []rtcp.Packet) error {
	if len(packets) == 0 {
		return nil
	}
	p, err := e.get(sessionID)
	if err != nil {
		return err
	}
	return p.connection.WriteRTCP(packets)
}

func (e *Engine) CloseSession(sessionID string) error {
	e.mu.Lock()
	p, exists := e.peers[sessionID]
	if exists {
		delete(e.peers, sessionID)
	}
	e.mu.Unlock()
	if !exists {
		return nil
	}
	return p.connection.Close()
}

func (e *Engine) SessionCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.peers)
}

func (e *Engine) Stats() Stats {
	e.mu.RLock()
	peers := make([]*peer, 0, len(e.peers))
	for _, current := range e.peers {
		peers = append(peers, current)
	}
	e.mu.RUnlock()
	result := Stats{Sessions: len(peers), ICERoutes: make(map[string]int)}
	for _, current := range peers {
		result.ICERoutes[selectedRoute(current.connection)]++
	}
	return result
}

func selectedRoute(connection *pion.PeerConnection) string {
	for _, sender := range connection.GetSenders() {
		transport := sender.Transport()
		if transport == nil || transport.ICETransport() == nil {
			continue
		}
		pair, err := transport.ICETransport().GetSelectedCandidatePair()
		if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
			continue
		}
		if pair.Local.Typ == pion.ICECandidateTypeRelay || pair.Remote.Typ == pion.ICECandidateTypeRelay {
			return "relay"
		}
		return "direct"
	}
	return "pending"
}

func (e *Engine) Close() error {
	e.mu.Lock()
	peers := e.peers
	e.peers = make(map[string]*peer)
	e.mu.Unlock()
	var result error
	for _, p := range peers {
		result = errors.Join(result, p.connection.Close())
	}
	result = errors.Join(result, e.udpMux.Close(), e.udpConn.Close())
	return result
}

func (e *Engine) get(sessionID string) (*peer, error) {
	e.mu.RLock()
	p := e.peers[sessionID]
	e.mu.RUnlock()
	if p == nil {
		return nil, errors.New("WebRTC session is not open")
	}
	return p, nil
}

func supportedCodecs() []pion.RTPCodecParameters {
	return []pion.RTPCodecParameters{
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: mimePCMU, ClockRate: 8000, Channels: 1}, PayloadType: 0},
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: mimePCMA, ClockRate: 8000, Channels: 1}, PayloadType: 8},
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: mimeTelephoneEvent, ClockRate: 8000, Channels: 1}, PayloadType: 101},
	}
}

type selectedCodec struct {
	capability      pion.RTPCodecCapability
	payloadType     uint8
	dtmfPayloadType uint8
}

func selectCodec(offer string) (selectedCodec, error) {
	lines := strings.Split(strings.ReplaceAll(offer, "\r\n", "\n"), "\n")
	type codecDescription struct {
		name     string
		clock    uint64
		channels uint64
	}
	payloads := make(map[string]codecDescription)
	var audioOrder []string
	inSelectedAudio := false
	audioSelected := false
	for _, line := range lines {
		if strings.HasPrefix(line, "m=") {
			inSelectedAudio = false
			fields := strings.Fields(line)
			if !audioSelected && len(fields) >= 4 && fields[0] == "m=audio" && fields[1] != "0" {
				audioOrder = append(audioOrder, fields[3:]...)
				inSelectedAudio = true
				audioSelected = true
			}
			continue
		}
		if inSelectedAudio && strings.HasPrefix(line, "a=rtpmap:") {
			value := strings.TrimPrefix(line, "a=rtpmap:")
			parts := strings.Fields(value)
			if len(parts) == 2 {
				payload, payloadErr := strconv.ParseUint(parts[0], 10, 7)
				format := strings.Split(parts[1], "/")
				if payloadErr != nil || len(format) < 2 || len(format) > 3 {
					return selectedCodec{}, errors.New("offer contains an invalid audio rtpmap")
				}
				clock, clockErr := strconv.ParseUint(format[1], 10, 32)
				channels := uint64(1)
				var channelsErr error
				if len(format) == 3 {
					channels, channelsErr = strconv.ParseUint(format[2], 10, 16)
				}
				if clockErr != nil || channelsErr != nil || clock == 0 || channels == 0 {
					return selectedCodec{}, errors.New("offer contains an invalid audio rtpmap")
				}
				key := strconv.FormatUint(payload, 10)
				if _, duplicate := payloads[key]; duplicate {
					return selectedCodec{}, errors.New("offer contains a duplicate audio rtpmap")
				}
				payloads[key] = codecDescription{
					name: strings.ToUpper(format[0]), clock: clock, channels: channels,
				}
			}
		}
	}
	if len(audioOrder) == 0 {
		return selectedCodec{}, errors.New("offer has no active audio media section")
	}
	var dtmfPayloadType uint8
	dtmfNegotiated := false
	for _, payload := range audioOrder {
		description := payloads[payload]
		if description.name == "TELEPHONE-EVENT" && description.clock == 8000 && description.channels == 1 {
			parsed, err := strconv.ParseUint(payload, 10, 7)
			if err == nil {
				dtmfPayloadType = uint8(parsed)
				dtmfNegotiated = true
				break
			}
		}
	}
	if !dtmfNegotiated {
		return selectedCodec{}, errors.New("offer has no RFC 4733 telephone-event/8000 codec")
	}
	for _, payload := range audioOrder {
		description, described := payloads[payload]
		if !described {
			switch payload {
			case strconv.Itoa(0):
				description = codecDescription{name: "PCMU", clock: 8000, channels: 1}
			case strconv.Itoa(8):
				description = codecDescription{name: "PCMA", clock: 8000, channels: 1}
			}
		}
		if description.clock != 8000 || description.channels != 1 {
			continue
		}
		switch description.name {
		case "PCMU":
			parsed, _ := strconv.ParseUint(payload, 10, 7)
			return selectedCodec{
				capability:  pion.RTPCodecCapability{MimeType: mimePCMU, ClockRate: 8000, Channels: 1},
				payloadType: uint8(parsed), dtmfPayloadType: dtmfPayloadType,
			}, nil
		case "PCMA":
			parsed, _ := strconv.ParseUint(payload, 10, 7)
			return selectedCodec{
				capability:  pion.RTPCodecCapability{MimeType: mimePCMA, ClockRate: 8000, Channels: 1},
				payloadType: uint8(parsed), dtmfPayloadType: dtmfPayloadType,
			}, nil
		}
	}
	return selectedCodec{}, errors.New("offer has no PCMU/PCMA codec intersection")
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
