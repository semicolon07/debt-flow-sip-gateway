package sip

import (
	"net"
	"strings"
	"testing"
	"time"

	sipmessage "github.com/emiago/sipgo/sip"
)

func TestNewSIPGoRuntimeFailsSynchronouslyWhenAListenerCannotBind(t *testing.T) {
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpListener.Close()
	address := tcpListener.Addr().String()
	runtime, err := NewSIPGoRuntime(SIPGoConfig{
		ListenUDP: address, ListenTCP: address, AdvertisedIP: "127.0.0.1",
	})
	if runtime != nil {
		_ = runtime.Close()
		t.Fatal("runtime was returned despite a SIP listener bind failure")
	}
	if err == nil || !strings.Contains(err.Error(), "listen SIP TCP") {
		t.Fatalf("unexpected constructor error: %v", err)
	}
	// The UDP socket opened before the TCP failure must be released during
	// constructor rollback, otherwise a restart would still need manual cleanup.
	udp, err := net.ListenPacket("udp", address)
	if err != nil {
		t.Fatalf("UDP listener leaked after constructor failure: %v", err)
	}
	_ = udp.Close()
}

func TestBuildContactUsesSelectedTransportListener(t *testing.T) {
	cfg := SIPGoConfig{
		ListenUDP: "0.0.0.0:5060", ListenTCP: "0.0.0.0:5070", AdvertisedIP: "10.0.0.20",
	}
	for _, test := range []struct {
		transport Transport
		port      int
	}{
		{transport: TransportUDP, port: 5060},
		{transport: TransportTCP, port: 5070},
	} {
		contact, err := buildContact(cfg, Configuration{Username: "1001", Transport: test.transport})
		if err != nil {
			t.Fatal(err)
		}
		if contact.Address.Port != test.port {
			t.Fatalf("%s contact port = %d, want %d", test.transport, contact.Address.Port, test.port)
		}
		if got := contact.Address.UriParams.GetOr("transport", ""); got != string(test.transport) {
			t.Fatalf("%s contact transport = %q", test.transport, got)
		}
	}
}

func TestSetURITransportOverridesCanonicalDestination(t *testing.T) {
	var parsed sipmessage.Uri
	if err := sipmessage.ParseUri("sip:0812345678@pbx.internal:5060;transport=udp", &parsed); err != nil {
		t.Fatal(err)
	}
	setURITransport(&parsed, TransportTCP)
	if got := parsed.UriParams.GetOr("transport", ""); got != "tcp" {
		t.Fatalf("destination transport = %q, want tcp", got)
	}
}

func TestRegistrationRetryDelayIsExponentiallyBounded(t *testing.T) {
	for failures, expected := range map[int]time.Duration{
		0: time.Second, 1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second,
		6: 30 * time.Second, 20: 30 * time.Second,
	} {
		if actual := registrationRetryDelay(failures); actual != expected {
			t.Fatalf("registrationRetryDelay(%d) = %s, want %s", failures, actual, expected)
		}
	}
}
