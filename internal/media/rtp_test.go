package media

import (
	"net"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

func TestRTPManagerAcceptsConfiguredSource(t *testing.T) {
	probeRTP, probeRTCP := listenUDPPair(t, 30000, 39000)
	port := probeRTP.LocalAddr().(*net.UDPAddr).Port
	_ = probeRTP.Close()
	_ = probeRTCP.Close()
	received := make(chan *rtp.Packet, 2)
	manager, err := NewRTPManager(RTPConfig{
		ListenIP: net.ParseIP("127.0.0.1"), AdvertisedIP: net.ParseIP("127.0.0.1"),
		MinPort: uint16(port), MaxPort: uint16(port + 2),
	}, func(_ string, packet *rtp.Packet) { received <- packet }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	target, err := manager.Allocate("call-1")
	if err != nil {
		t.Fatal(err)
	}
	legitimate, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer legitimate.Close()
	if err := manager.SetRemote("call-1", legitimate.LocalAddr().(*net.UDPAddr),
		&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: legitimate.LocalAddr().(*net.UDPAddr).Port + 1}); err != nil {
		t.Fatal(err)
	}
	raw, _ := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0}, Payload: []byte{1}}).Marshal()
	if _, err := legitimate.WriteToUDP(raw, target); err != nil {
		t.Fatal(err)
	}
	select {
	case packet := <-received:
		if len(packet.Payload) != 1 || packet.Payload[0] != 1 {
			t.Fatalf("unexpected RTP packet: %+v", packet)
		}
	case <-time.After(time.Second):
		t.Fatal("expected RTP packet from configured source")
	}
}

func TestLearnSymmetricSourceRequiresExpectedIPAndMigratesStablePort(t *testing.T) {
	remote := &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 1000}
	learned := false
	candidatePort := 0
	var candidateCount uint8
	if decision := learnSymmetricSource(&remote, &learned, &candidatePort, &candidateCount,
		&net.UDPAddr{IP: net.ParseIP("10.0.0.2"), Port: 2000}); decision != sourceRejectedIP {
		t.Fatal("symmetric RTP learned an unexpected source IP")
	}
	if decision := learnSymmetricSource(&remote, &learned, &candidatePort, &candidateCount,
		&net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 2000}); decision != sourceLearned {
		t.Fatal("symmetric RTP did not learn a source port from the expected IP")
	}
	if remote.Port != 2000 || !learned {
		t.Fatalf("unexpected learned source: %v learned=%v", remote, learned)
	}
	for index := 1; index <= symmetricSourceMigrationPackets; index++ {
		decision := learnSymmetricSource(&remote, &learned, &candidatePort, &candidateCount,
			&net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 3000})
		if !decision.accepted() {
			t.Fatal("valid media from a new port was rejected during migration")
		}
		if index < symmetricSourceMigrationPackets && remote.Port != 2000 {
			t.Fatal("symmetric RTP migrated before the new source was stable")
		}
	}
	if remote.Port != 3000 {
		t.Fatalf("symmetric RTP did not migrate to stable source port: %v", remote)
	}
}

func TestRTPManagerLearnsSymmetricSourcePortOnce(t *testing.T) {
	manager, err := NewRTPManager(RTPConfig{
		ListenIP: net.ParseIP("127.0.0.1"), AdvertisedIP: net.ParseIP("127.0.0.1"),
		MinPort: 30000, MaxPort: 34000,
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	target, err := manager.Allocate("call-1")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer actual.Close()
	advertisedPort := 9
	if actual.LocalAddr().(*net.UDPAddr).Port == advertisedPort {
		advertisedPort++
	}
	advertised := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: advertisedPort}
	if err := manager.SetRemote("call-1", advertised, advertised); err != nil {
		t.Fatal(err)
	}
	inbound := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0}, Payload: []byte{1}}
	raw, _ := inbound.Marshal()
	if _, err := actual.WriteToUDP(raw, target); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.RLock()
		endpoint := manager.endpoints["call-1"]
		manager.mu.RUnlock()
		endpoint.mu.RLock()
		learned := endpoint.learnedRTP
		endpoint.mu.RUnlock()
		if learned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("symmetric RTP source port was not learned")
		}
		time.Sleep(time.Millisecond)
	}
	outbound := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0}, Payload: []byte{2}}
	if err := manager.Write("call-1", outbound); err != nil {
		t.Fatal(err)
	}
	if err := actual.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1500)
	if _, _, err := actual.ReadFromUDP(buffer); err != nil {
		t.Fatalf("outbound RTP was not sent to learned symmetric port: %v", err)
	}
}

func TestRTPManagerBridgesRTCPOnPairedPort(t *testing.T) {
	received := make(chan []rtcp.Packet, 1)
	manager, err := NewRTPManager(RTPConfig{
		ListenIP: net.ParseIP("127.0.0.1"), AdvertisedIP: net.ParseIP("127.0.0.1"),
		MinPort: 30000, MaxPort: 34000,
	}, nil, func(_ string, packets []rtcp.Packet) { received <- packets }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	target, err := manager.Allocate("call-1")
	if err != nil {
		t.Fatal(err)
	}
	rtpSource, rtcpSource := listenUDPPair(t, 35000, 39000)
	defer rtpSource.Close()
	defer rtcpSource.Close()
	if err := manager.SetRemote("call-1", rtpSource.LocalAddr().(*net.UDPAddr),
		rtcpSource.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	packet := &rtcp.ReceiverReport{SSRC: 10, Reports: []rtcp.ReceptionReport{{SSRC: 20}}}
	raw, err := rtcp.Marshal([]rtcp.Packet{packet})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rtcpSource.WriteToUDP(raw, &net.UDPAddr{IP: target.IP, Port: target.Port + 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case packets := <-received:
		if len(packets) != 1 || packets[0].(*rtcp.ReceiverReport).SSRC != 10 {
			t.Fatalf("unexpected inbound RTCP: %#v", packets)
		}
	case <-time.After(time.Second):
		t.Fatal("expected RTCP packet from configured source")
	}
	if err := manager.WriteRTCP("call-1", []rtcp.Packet{packet}); err != nil {
		t.Fatal(err)
	}
	if err := rtcpSource.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1500)
	n, _, err := rtcpSource.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	packets, err := rtcp.Unmarshal(buffer[:n])
	if err != nil || len(packets) != 1 {
		t.Fatalf("unexpected outbound RTCP: %#v err=%v", packets, err)
	}
}

func listenUDPPair(t *testing.T, minimum, maximum int) (*net.UDPConn, *net.UDPConn) {
	t.Helper()
	for port := minimum; port+1 <= maximum; port += 2 {
		rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
		if err != nil {
			continue
		}
		rtcpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port + 1})
		if err == nil {
			return rtpConn, rtcpConn
		}
		_ = rtpConn.Close()
	}
	t.Fatal("no consecutive UDP port pair available")
	return nil, nil
}
