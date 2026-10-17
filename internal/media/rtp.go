package media

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

type RTPConfig struct {
	ListenIP     net.IP
	AdvertisedIP net.IP
	MinPort      uint16
	MaxPort      uint16
}

type RTPPacketHandler func(callID string, packet *rtp.Packet)
type RTCPPacketHandler func(callID string, packets []rtcp.Packet)

type RTPManager struct {
	cfg       RTPConfig
	onPacket  RTPPacketHandler
	onRTCP    RTCPPacketHandler
	mu        sync.RWMutex
	endpoints map[string]*rtpEndpoint
}

type rtpEndpoint struct {
	conn        *net.UDPConn
	rtcpConn    *net.UDPConn
	local       *net.UDPAddr
	advertised  *net.UDPAddr
	mu          sync.RWMutex
	remote      *net.UDPAddr
	remoteRTCP  *net.UDPAddr
	learnedRTP  bool
	learnedRTCP bool
	cancel      context.CancelFunc
}

func NewRTPManager(cfg RTPConfig, onPacket RTPPacketHandler, onRTCP RTCPPacketHandler) (*RTPManager, error) {
	if cfg.ListenIP == nil || cfg.AdvertisedIP == nil || cfg.MinPort == 0 || cfg.MaxPort < cfg.MinPort {
		return nil, errors.New("invalid RTP manager configuration")
	}
	return &RTPManager{cfg: cfg, onPacket: onPacket, onRTCP: onRTCP, endpoints: make(map[string]*rtpEndpoint)}, nil
}

func (m *RTPManager) Allocate(callID string) (*net.UDPAddr, error) {
	if callID == "" {
		return nil, errors.New("callID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if endpoint := m.endpoints[callID]; endpoint != nil {
		return cloneUDPAddr(endpoint.advertised), nil
	}
	firstPort := int(m.cfg.MinPort)
	if firstPort%2 != 0 {
		firstPort++
	}
	for port := firstPort; port+1 <= int(m.cfg.MaxPort); port += 2 {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: m.cfg.ListenIP, Port: port})
		if err != nil {
			continue
		}
		rtcpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: m.cfg.ListenIP, Port: port + 1})
		if err != nil {
			_ = conn.Close()
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		endpoint := &rtpEndpoint{
			conn: conn, rtcpConn: rtcpConn, local: cloneUDPAddr(conn.LocalAddr().(*net.UDPAddr)),
			advertised: &net.UDPAddr{IP: append(net.IP(nil), m.cfg.AdvertisedIP...), Port: port},
			cancel:     cancel,
		}
		m.endpoints[callID] = endpoint
		go m.readLoop(ctx, callID, endpoint)
		go m.readRTCPLoop(ctx, callID, endpoint)
		return cloneUDPAddr(endpoint.advertised), nil
	}
	return nil, errors.New("RTP port range exhausted")
}

func (m *RTPManager) SetRemote(callID string, remoteRTP, remoteRTCP *net.UDPAddr) error {
	if remoteRTP == nil || remoteRTP.IP == nil || remoteRTP.Port == 0 {
		return errors.New("valid remote RTP address is required")
	}
	if remoteRTCP == nil || remoteRTCP.IP == nil || remoteRTCP.Port == 0 {
		return errors.New("valid remote RTCP address is required")
	}
	endpoint, err := m.get(callID)
	if err != nil {
		return err
	}
	endpoint.mu.Lock()
	endpoint.remote = cloneUDPAddr(remoteRTP)
	endpoint.remoteRTCP = cloneUDPAddr(remoteRTCP)
	endpoint.learnedRTP = false
	endpoint.learnedRTCP = false
	endpoint.mu.Unlock()
	return nil
}

func (m *RTPManager) WriteRTCP(callID string, packets []rtcp.Packet) error {
	if len(packets) == 0 {
		return nil
	}
	endpoint, err := m.get(callID)
	if err != nil {
		return err
	}
	endpoint.mu.RLock()
	remote := cloneUDPAddr(endpoint.remoteRTCP)
	endpoint.mu.RUnlock()
	if remote == nil {
		return errors.New("remote RTCP address is not set")
	}
	raw, err := rtcp.Marshal(packets)
	if err != nil {
		return err
	}
	_, err = endpoint.rtcpConn.WriteToUDP(raw, remote)
	return err
}

func (m *RTPManager) Write(callID string, packet *rtp.Packet) error {
	if packet == nil {
		return errors.New("RTP packet is required")
	}
	endpoint, err := m.get(callID)
	if err != nil {
		return err
	}
	endpoint.mu.RLock()
	remote := cloneUDPAddr(endpoint.remote)
	endpoint.mu.RUnlock()
	if remote == nil {
		return errors.New("remote RTP address is not set")
	}
	raw, err := packet.Marshal()
	if err != nil {
		return err
	}
	_, err = endpoint.conn.WriteToUDP(raw, remote)
	return err
}

func (m *RTPManager) CloseCall(callID string) error {
	m.mu.Lock()
	endpoint := m.endpoints[callID]
	delete(m.endpoints, callID)
	m.mu.Unlock()
	if endpoint == nil {
		return nil
	}
	endpoint.cancel()
	return errors.Join(endpoint.conn.Close(), endpoint.rtcpConn.Close())
}

func (m *RTPManager) Close() error {
	m.mu.Lock()
	endpoints := m.endpoints
	m.endpoints = make(map[string]*rtpEndpoint)
	m.mu.Unlock()
	var result error
	for _, endpoint := range endpoints {
		endpoint.cancel()
		result = errors.Join(result, endpoint.conn.Close(), endpoint.rtcpConn.Close())
	}
	return result
}

func (m *RTPManager) readRTCPLoop(ctx context.Context, callID string, endpoint *rtpEndpoint) {
	buffer := make([]byte, 1600)
	for {
		n, source, err := endpoint.rtcpConn.ReadFromUDP(buffer)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		endpoint.mu.Lock()
		accepted := learnSymmetricSource(&endpoint.remoteRTCP, &endpoint.learnedRTCP, source)
		endpoint.mu.Unlock()
		if !accepted {
			continue
		}
		packets, err := rtcp.Unmarshal(buffer[:n])
		if err == nil && m.onRTCP != nil {
			m.onRTCP(callID, packets)
		}
	}
}

func (m *RTPManager) readLoop(ctx context.Context, callID string, endpoint *rtpEndpoint) {
	buffer := make([]byte, 1600)
	for {
		n, source, err := endpoint.conn.ReadFromUDP(buffer)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		endpoint.mu.Lock()
		accepted := learnSymmetricSource(&endpoint.remote, &endpoint.learnedRTP, source)
		endpoint.mu.Unlock()
		if !accepted {
			continue
		}
		var packet rtp.Packet
		if err := packet.Unmarshal(buffer[:n]); err != nil {
			continue
		}
		if m.onPacket != nil {
			m.onPacket(callID, &packet)
		}
	}
}

func learnSymmetricSource(remote **net.UDPAddr, learned *bool, source *net.UDPAddr) bool {
	if *remote == nil || source == nil || !(*remote).IP.Equal(source.IP) {
		return false
	}
	if *learned {
		return (*remote).Port == source.Port
	}
	*remote = cloneUDPAddr(source)
	*learned = true
	return true
}

func (m *RTPManager) get(callID string) (*rtpEndpoint, error) {
	m.mu.RLock()
	endpoint := m.endpoints[callID]
	m.mu.RUnlock()
	if endpoint == nil {
		return nil, fmt.Errorf("RTP endpoint for call %s does not exist", callID)
	}
	return endpoint, nil
}

func cloneUDPAddr(value *net.UDPAddr) *net.UDPAddr {
	if value == nil {
		return nil
	}
	return &net.UDPAddr{IP: append(net.IP(nil), value.IP...), Port: value.Port, Zone: value.Zone}
}
