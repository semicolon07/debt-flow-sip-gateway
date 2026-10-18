package sip

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	sipmessage "github.com/emiago/sipgo/sip"
)

func TestSIPTransactionFailureKindIsSanitized(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{err: sipmessage.ErrTransactionCanceled, want: "cancelled"},
		{err: sipmessage.ErrTransactionTimeout, want: "timeout"},
		{err: sipmessage.ErrTransactionTransport, want: "transport"},
		{err: sipmessage.ErrTransactionTerminated, want: "terminated"},
		{err: errors.New("contains potentially sensitive transport details"), want: "other"},
	}
	for _, test := range tests {
		if got := sipTransactionFailureKind(test.err); got != test.want {
			t.Fatalf("failure kind=%q, want %q", got, test.want)
		}
	}
}

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

func TestOutboundRecipientBuildsSIPURIFromCanonicalPhoneNumber(t *testing.T) {
	recipient, err := outboundRecipient("0955948679", Configuration{
		Host: "192.168.1.247", Port: 5060, Transport: TransportUDP,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := recipient.String(), "sip:0955948679@192.168.1.247:5060"; got != want {
		t.Fatalf("recipient=%q, want %q", got, want)
	}
}

func TestOutboundRecipientPreservesExplicitSIPURI(t *testing.T) {
	recipient, err := outboundRecipient("sip:100@pbx.example.test:5060", Configuration{
		Host: "192.168.1.247", Port: 5060, Transport: TransportUDP,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := recipient.String(), "sip:100@pbx.example.test:5060"; got != want {
		t.Fatalf("recipient=%q, want %q", got, want)
	}
}

func TestOutboundRecipientRejectsNonCanonicalDestination(t *testing.T) {
	if _, err := outboundRecipient("100@example.test", Configuration{
		Host: "192.168.1.247", Port: 5060, Transport: TransportUDP,
	}); err == nil {
		t.Fatal("expected non-canonical destination to be rejected")
	}
}

func TestOutboundHangupWaitsForCancelCompletionAndAllowsImmediateRedial(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	const firstCallID = "outbound-cancel-1"
	if err := runtime.StartCall(context.Background(), "session-1", firstCallID, "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponse(t, peer, gateway, invite, 180, "Ringing", "remote-cancel")
	if event := readRuntimeEvent(t, runtime, EventRinging); event.CallID != firstCallID {
		t.Fatalf("ringing event=%+v", event)
	}

	hangupResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		hangupResult <- runtime.Hangup(ctx, "session-1", firstCallID)
	}()
	cancelRequest, gateway := readSIPRequestFromContaining(t, peer, "CANCEL ")
	select {
	case err := <-hangupResult:
		t.Fatalf("Hangup returned before CANCEL transaction completed: %v", err)
	default:
	}
	writeSIPResponse(t, peer, gateway, cancelRequest, 200, "OK", "remote-cancel")
	writeSIPResponse(t, peer, gateway, invite, 487, "Request Terminated", "remote-cancel")
	select {
	case err := <-hangupResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Hangup did not finish after SIP 200/487 cancellation handshake")
	}
	ended := readRuntimeEvent(t, runtime, EventEnded)
	if ended.CallID != firstCallID || ended.Reason != "cancelled" {
		t.Fatalf("ended event=%+v", ended)
	}
	waitForCallRemoval(t, runtime, "session-1", firstCallID)

	if err := runtime.StartCall(context.Background(), "session-1", "outbound-cancel-2", "9001", []byte(testInboundSDP)); err != nil {
		t.Fatalf("immediate redial failed: %v", err)
	}
	secondInvite, _ := readSIPRequestFromContaining(t, peer, "INVITE ")
	if headerValue(secondInvite, "Call-ID") == headerValue(invite, "Call-ID") {
		t.Fatal("immediate redial reused the completed SIP Call-ID")
	}
}

func TestOutboundFinalResponsesEndRingingAndAllowImmediateRedial(t *testing.T) {
	tests := []struct {
		status int
		phrase string
		reason string
	}{
		{404, "Not Found", "invalid_destination"},
		{408, "Request Timeout", "no_answer"},
		{480, "Temporarily Unavailable", "no_answer"},
		{486, "Busy Here", "busy"},
		{487, "Request Terminated", "cancelled"},
		{488, "Not Acceptable Here", "codec_not_supported"},
		{500, "Server Internal Error", "provider_unavailable"},
		{503, "Service Unavailable", "provider_unavailable"},
		{600, "Busy Everywhere", "busy"},
		{603, "Decline", "rejected"},
	}
	for _, test := range tests {
		t.Run(strconv.Itoa(test.status), func(t *testing.T) {
			runtime, peer := newOutboundSIPTestRuntime(t)
			firstCallID := fmt.Sprintf("outbound-final-%d-1", test.status)
			if err := runtime.StartCall(context.Background(), "session-1", firstCallID, "9001", []byte(testInboundSDP)); err != nil {
				t.Fatal(err)
			}
			invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
			remoteTag := fmt.Sprintf("remote-final-%d", test.status)
			writeSIPResponse(t, peer, gateway, invite, 180, "Ringing", remoteTag)
			if event := readRuntimeEvent(t, runtime, EventRinging); event.CallID != firstCallID {
				t.Fatalf("ringing event=%+v", event)
			}
			writeSIPResponse(t, peer, gateway, invite, test.status, test.phrase, remoteTag)
			ack, _ := readSIPRequestFromContaining(t, peer, "ACK ")
			if headerValue(ack, "Call-ID") != headerValue(invite, "Call-ID") {
				t.Fatalf("non-2xx ACK does not match INVITE: %q", ack)
			}
			failed := readRuntimeEvent(t, runtime, EventFailed)
			if failed.CallID != firstCallID || failed.StatusCode != test.status || failed.Reason != test.reason {
				t.Fatalf("terminal event=%+v", failed)
			}
			waitForCallRemoval(t, runtime, "session-1", firstCallID)
			assertImmediateOutboundRedial(t, runtime, peer,
				fmt.Sprintf("outbound-final-%d-2", test.status))
		})
	}
}

func TestOutboundTransactionFailuresUseStableEndReasons(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{err: sipmessage.ErrTransactionTimeout, want: "no_answer"},
		{err: sipmessage.ErrTransactionTransport, want: "provider_unavailable"},
		{err: sipmessage.ErrTransactionCanceled, want: "cancelled"},
		{err: sipmessage.ErrTransactionTerminated, want: "sip_failed"},
	} {
		if got := outboundFailureReason(0, test.err); got != test.want {
			t.Fatalf("outboundFailureReason(%v)=%q, want %q", test.err, got, test.want)
		}
	}
}

func TestOutboundConnectedLocalHangupWaitsForByeAndAllowsImmediateRedial(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	const firstCallID = "outbound-local-bye-1"
	_, _, _ = establishOutboundTestCall(t, runtime, peer, firstCallID, "remote-local-bye")

	hangupResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		hangupResult <- runtime.Hangup(ctx, "session-1", firstCallID)
	}()
	bye, gateway := readSIPRequestFromContaining(t, peer, "BYE ")
	select {
	case err := <-hangupResult:
		t.Fatalf("Hangup returned before BYE received a final response: %v", err)
	default:
	}
	writeSIPResponse(t, peer, gateway, bye, 200, "OK", "remote-local-bye")
	select {
	case err := <-hangupResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Hangup did not finish after BYE 200 OK")
	}
	waitForCallRemoval(t, runtime, "session-1", firstCallID)
	assertImmediateOutboundRedial(t, runtime, peer, "outbound-local-bye-2")
}

func TestOutboundConnectedRemoteByeEndsCallAndAllowsImmediateRedial(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	const firstCallID = "outbound-remote-bye-1"
	_, ack, gateway := establishOutboundTestCall(t, runtime, peer, firstCallID, "remote-bye")
	peerHost, peerPortText, err := net.SplitHostPort(peer.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	remoteBye := fmt.Sprintf("BYE sip:1129@%s SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP %s;branch=z9hG4bK-remote-bye;rport\r\n"+
		"Max-Forwards: 70\r\n"+
		"From: %s\r\n"+
		"To: %s\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: 2 BYE\r\n"+
		"Contact: <sip:9001@%s>\r\n"+
		"Content-Length: 0\r\n\r\n",
		gateway.String(), net.JoinHostPort(peerHost, peerPortText), headerValue(ack, "To"),
		headerValue(ack, "From"), headerValue(ack, "Call-ID"), net.JoinHostPort(peerHost, peerPortText))
	if _, err := peer.WriteTo([]byte(remoteBye), gateway); err != nil {
		t.Fatal(err)
	}
	response := readSIPResponseContaining(t, peer, "200 OK")
	if headerValue(response, "CSeq") != "2 BYE" {
		t.Fatalf("remote BYE response=%q", response)
	}
	ended := readRuntimeEvent(t, runtime, EventEnded)
	if ended.CallID != firstCallID || ended.Reason != "remote_hangup" {
		t.Fatalf("ended event=%+v", ended)
	}
	waitForCallRemoval(t, runtime, "session-1", firstCallID)
	assertImmediateOutboundRedial(t, runtime, peer, "outbound-remote-bye-2")
}

func TestOutboundLateAnswerAfterCancelIsAckedAndEndedWithByeBeforeRedial(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	const firstCallID = "outbound-late-answer-1"
	if err := runtime.StartCall(context.Background(), "session-1", firstCallID, "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponse(t, peer, gateway, invite, 180, "Ringing", "remote-late-answer")
	_ = readRuntimeEvent(t, runtime, EventRinging)

	hangupResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		hangupResult <- runtime.Hangup(ctx, "session-1", firstCallID)
	}()
	cancelRequest, gateway := readSIPRequestFromContaining(t, peer, "CANCEL ")
	writeSIPResponse(t, peer, gateway, cancelRequest, 200, "OK", "remote-late-answer")
	writeSIPResponseWithBody(t, peer, gateway, invite, 200, "OK", "remote-late-answer", testInboundSDP)
	_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	bye, gateway := readSIPRequestFromContaining(t, peer, "BYE ")
	writeSIPResponse(t, peer, gateway, bye, 200, "OK", "remote-late-answer")
	select {
	case err := <-hangupResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Hangup did not wait for late-answer ACK/BYE cleanup")
	}
	ended := readRuntimeEvent(t, runtime, EventEnded)
	if ended.CallID != firstCallID || ended.Reason != "cancelled" {
		t.Fatalf("ended event=%+v", ended)
	}
	waitForCallRemoval(t, runtime, "session-1", firstCallID)
	assertImmediateOutboundRedial(t, runtime, peer, "outbound-late-answer-2")
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

func TestInboundInviteCanBeAnsweredAfterInviteHandlerPublishesRinging(t *testing.T) {
	udpAddress := reservePacketAddress(t)
	tcpAddress := reserveTCPAddress(t)
	runtime, err := NewSIPGoRuntime(SIPGoConfig{
		ListenUDP: udpAddress, ListenTCP: tcpAddress, AdvertisedIP: "127.0.0.1",
		EventBuffer: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	peer, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	peerPort := peer.LocalAddr().(*net.UDPAddr).Port
	if err := runtime.Configure(context.Background(), "session-1", Configuration{
		Host: "127.0.0.1", Port: uint16(peerPort), Transport: TransportUDP,
		Username: "1129", Password: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	runtime.setRegistered("session-1", true)

	serverPort := mustPort(t, udpAddress)
	callID := "inbound-answer-regression@example.test"
	fromTag := "from-regression"
	invite := fmt.Sprintf("INVITE sip:1129@127.0.0.1:%d SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK-invite;rport\r\n"+
		"Max-Forwards: 70\r\n"+
		"From: <sip:9001@127.0.0.1>;tag=%s\r\n"+
		"To: <sip:1129@127.0.0.1>\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: 1 INVITE\r\n"+
		"Contact: <sip:9001@127.0.0.1:%d>\r\n"+
		"Content-Type: application/sdp\r\n"+
		"Content-Length: %d\r\n\r\n%s",
		serverPort, peerPort, fromTag, callID, peerPort, len(testInboundSDP), testInboundSDP)
	serverUDP, err := net.ResolveUDPAddr("udp", udpAddress)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.WriteTo([]byte(invite), serverUDP); err != nil {
		t.Fatal(err)
	}
	readSIPResponseContaining(t, peer, "180 Ringing")

	var incoming Event
	select {
	case incoming = <-runtime.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound event")
	}
	if incoming.Type != EventIncoming {
		t.Fatalf("event type=%s, want %s", incoming.Type, EventIncoming)
	}
	answerSDP, err := (AsteriskProfile{}).BuildSDP(MediaDescription{
		IP: "127.0.0.1", Port: 30000, RTCPPort: 30001,
		AudioPayloadType: 0, DTMFPayloadType: 101, Codec: "PCMU",
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := runtime.Answer(context.Background(), "session-1", incoming.CallID, answerSDP); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("Answer blocked the command path for %s", elapsed)
	}

	response := readSIPResponseContaining(t, peer, "200 OK")
	toValue := headerValue(response, "To")
	if toValue == "" || !strings.Contains(toValue, "tag=") {
		t.Fatalf("200 response lacks dialog To tag: %q", toValue)
	}
	ack := fmt.Sprintf("ACK sip:1129@127.0.0.1:%d SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK-ack;rport\r\n"+
		"Max-Forwards: 70\r\n"+
		"From: <sip:9001@127.0.0.1>;tag=%s\r\n"+
		"To: %s\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: 1 ACK\r\n"+
		"Contact: <sip:9001@127.0.0.1:%d>\r\n"+
		"Content-Length: 0\r\n\r\n",
		serverPort, peerPort, fromTag, toValue, callID, peerPort)
	if _, err := peer.WriteTo([]byte(ack), serverUDP); err != nil {
		t.Fatal(err)
	}
	select {
	case connected := <-runtime.Events():
		if connected.Type != EventConnected || connected.CallID != incoming.CallID {
			t.Fatalf("connected event=%+v", connected)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound connected event")
	}

	bye := fmt.Sprintf("BYE sip:1129@127.0.0.1:%d SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK-bye;rport\r\n"+
		"Max-Forwards: 70\r\n"+
		"From: <sip:9001@127.0.0.1>;tag=%s\r\n"+
		"To: %s\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: 2 BYE\r\n"+
		"Content-Length: 0\r\n\r\n",
		serverPort, peerPort, fromTag, toValue, callID)
	if _, err := peer.WriteTo([]byte(bye), serverUDP); err != nil {
		t.Fatal(err)
	}
	byeResponse := readSIPResponseContaining(t, peer, "200 OK")
	if cseq := headerValue(byeResponse, "CSeq"); cseq != "2 BYE" {
		t.Fatalf("BYE response CSeq=%q, want 2 BYE", cseq)
	}
	select {
	case ended := <-runtime.Events():
		if ended.Type != EventEnded || ended.CallID != incoming.CallID || ended.Reason != "remote_hangup" {
			t.Fatalf("ended event=%+v", ended)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound ended event")
	}
	waitForCallRemoval(t, runtime, "session-1", incoming.CallID)
}

func TestInboundInviteAnswerAndRemoteByeOverTCP(t *testing.T) {
	udpAddress := reservePacketAddress(t)
	tcpAddress := reserveTCPAddress(t)
	runtime, err := NewSIPGoRuntime(SIPGoConfig{
		ListenUDP: udpAddress, ListenTCP: tcpAddress, AdvertisedIP: "127.0.0.1",
		EventBuffer: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Configure(context.Background(), "session-1", Configuration{
		Host: "127.0.0.1", Port: 5060, Transport: TransportTCP,
		Username: "1129", Password: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	runtime.setRegistered("session-1", true)

	peer, err := net.DialTimeout("tcp", tcpAddress, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	peerPort := peer.LocalAddr().(*net.TCPAddr).Port
	serverPort := mustPort(t, tcpAddress)
	callID := "inbound-tcp-answer@example.test"
	fromTag := "from-tcp"
	invite := strings.ReplaceAll(buildInboundRequest("INVITE", serverPort, peerPort, fromTag, "",
		callID, "z9hG4bK-tcp-invite", "1 INVITE", testInboundSDP), "SIP/2.0/UDP", "SIP/2.0/TCP")
	if _, err := io.WriteString(peer, invite); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(peer)
	readTCPResponseContaining(t, peer, reader, "180 Ringing")
	incoming := readRuntimeEvent(t, runtime, EventIncoming)
	answerSDP, err := (AsteriskProfile{}).BuildSDP(MediaDescription{
		IP: "127.0.0.1", Port: 30000, RTCPPort: 30001,
		AudioPayloadType: 0, DTMFPayloadType: 101, Codec: "PCMU",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Answer(context.Background(), "session-1", incoming.CallID, answerSDP); err != nil {
		t.Fatal(err)
	}
	response := readTCPResponseContaining(t, peer, reader, "200 OK")
	toValue := headerValue(response, "To")
	ack := strings.ReplaceAll(buildInboundRequest("ACK", serverPort, peerPort, fromTag, toValue,
		callID, "z9hG4bK-tcp-ack", "1 ACK", ""), "SIP/2.0/UDP", "SIP/2.0/TCP")
	if _, err := io.WriteString(peer, ack); err != nil {
		t.Fatal(err)
	}
	connected := readRuntimeEvent(t, runtime, EventConnected)
	if connected.CallID != incoming.CallID {
		t.Fatalf("connected event=%+v", connected)
	}
	bye := strings.ReplaceAll(buildInboundRequest("BYE", serverPort, peerPort, fromTag, toValue,
		callID, "z9hG4bK-tcp-bye", "2 BYE", ""), "SIP/2.0/UDP", "SIP/2.0/TCP")
	if _, err := io.WriteString(peer, bye); err != nil {
		t.Fatal(err)
	}
	byeResponse := readTCPResponseContaining(t, peer, reader, "200 OK")
	if cseq := headerValue(byeResponse, "CSeq"); cseq != "2 BYE" {
		t.Fatalf("BYE response CSeq=%q, want 2 BYE", cseq)
	}
	ended := readRuntimeEvent(t, runtime, EventEnded)
	if ended.CallID != incoming.CallID || ended.Reason != "remote_hangup" {
		t.Fatalf("ended event=%+v", ended)
	}
	waitForCallRemoval(t, runtime, "session-1", incoming.CallID)
}

func TestInboundInviteCanBeRejectedAfterRinging(t *testing.T) {
	runtime, peer, serverUDP, serverPort, peerPort := newInboundSIPTestRuntime(t)
	callID := "inbound-reject-regression@example.test"
	fromTag := "from-reject"
	inviteBranch := "z9hG4bK-reject"
	invite := buildInboundRequest("INVITE", serverPort, peerPort, fromTag, "", callID,
		inviteBranch, "1 INVITE", testInboundSDP)
	if _, err := peer.WriteTo([]byte(invite), serverUDP); err != nil {
		t.Fatal(err)
	}
	readSIPResponseContaining(t, peer, "180 Ringing")
	incoming := readRuntimeEvent(t, runtime, EventIncoming)

	started := time.Now()
	if err := runtime.Reject(context.Background(), "session-1", incoming.CallID, sipmessage.StatusBusyHere); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("Reject blocked the command path for %s", elapsed)
	}
	response := readSIPResponseContaining(t, peer, "486 Busy Here")
	toValue := headerValue(response, "To")
	ack := buildInboundRequest("ACK", serverPort, peerPort, fromTag, toValue, callID,
		inviteBranch, "1 ACK", "")
	if _, err := peer.WriteTo([]byte(ack), serverUDP); err != nil {
		t.Fatal(err)
	}
	waitForCallRemoval(t, runtime, "session-1", incoming.CallID)
}

func TestInboundInviteRemoteCancelCompletesInviteTransaction(t *testing.T) {
	runtime, peer, serverUDP, serverPort, peerPort := newInboundSIPTestRuntime(t)
	callID := "inbound-cancel-regression@example.test"
	fromTag := "from-cancel"
	inviteBranch := "z9hG4bK-cancel"
	invite := buildInboundRequest("INVITE", serverPort, peerPort, fromTag, "", callID,
		inviteBranch, "1 INVITE", testInboundSDP)
	if _, err := peer.WriteTo([]byte(invite), serverUDP); err != nil {
		t.Fatal(err)
	}
	readSIPResponseContaining(t, peer, "180 Ringing")
	incoming := readRuntimeEvent(t, runtime, EventIncoming)

	cancel := buildInboundRequest("CANCEL", serverPort, peerPort, fromTag, "", callID,
		inviteBranch, "1 CANCEL", "")
	if _, err := peer.WriteTo([]byte(cancel), serverUDP); err != nil {
		t.Fatal(err)
	}
	responses := readSIPResponsesContaining(t, peer, "200 OK", "487 Request Terminated")
	terminated := responses["487 Request Terminated"]
	toValue := headerValue(terminated, "To")
	ack := buildInboundRequest("ACK", serverPort, peerPort, fromTag, toValue, callID,
		inviteBranch, "1 ACK", "")
	if _, err := peer.WriteTo([]byte(ack), serverUDP); err != nil {
		t.Fatal(err)
	}
	ended := readRuntimeEvent(t, runtime, EventEnded)
	if ended.CallID != incoming.CallID || ended.Reason != "cancelled" {
		t.Fatalf("ended event=%+v", ended)
	}
	waitForCallRemoval(t, runtime, "session-1", incoming.CallID)
}

func TestInboundLocalHangupWhileRingingTerminatesInvite(t *testing.T) {
	runtime, peer, serverUDP, serverPort, peerPort := newInboundSIPTestRuntime(t)
	callID := "inbound-local-hangup-ringing@example.test"
	fromTag := "from-local-hangup-ringing"
	inviteBranch := "z9hG4bK-local-hangup-ringing"
	invite := buildInboundRequest("INVITE", serverPort, peerPort, fromTag, "", callID,
		inviteBranch, "1 INVITE", testInboundSDP)
	if _, err := peer.WriteTo([]byte(invite), serverUDP); err != nil {
		t.Fatal(err)
	}
	readSIPResponseContaining(t, peer, "180 Ringing")
	incoming := readRuntimeEvent(t, runtime, EventIncoming)
	if err := runtime.Hangup(context.Background(), "session-1", incoming.CallID); err != nil {
		t.Fatal(err)
	}
	response := readSIPResponseContaining(t, peer, "487 Request Terminated")
	toValue := headerValue(response, "To")
	ack := buildInboundRequest("ACK", serverPort, peerPort, fromTag, toValue, callID,
		inviteBranch, "1 ACK", "")
	if _, err := peer.WriteTo([]byte(ack), serverUDP); err != nil {
		t.Fatal(err)
	}
	waitForCallRemoval(t, runtime, "session-1", incoming.CallID)
}

func TestInboundInviteIgnoresCancelAfterSendingFinalResponse(t *testing.T) {
	runtime, peer, serverUDP, serverPort, peerPort := newInboundSIPTestRuntime(t)
	callID := "inbound-late-cancel@example.test"
	fromTag := "from-late-cancel"
	inviteBranch := "z9hG4bK-late-cancel"
	invite := buildInboundRequest("INVITE", serverPort, peerPort, fromTag, "", callID,
		inviteBranch, "1 INVITE", testInboundSDP)
	if _, err := peer.WriteTo([]byte(invite), serverUDP); err != nil {
		t.Fatal(err)
	}
	readSIPResponseContaining(t, peer, "180 Ringing")
	incoming := readRuntimeEvent(t, runtime, EventIncoming)
	if err := runtime.Answer(context.Background(), "session-1", incoming.CallID, testAnswerSDP(t)); err != nil {
		t.Fatal(err)
	}
	response := readSIPResponseContaining(t, peer, "200 OK")
	toValue := headerValue(response, "To")

	// RFC 3261: CANCEL has no effect once the UAS has sent the final response.
	// The caller must ACK the 2xx and use BYE to end the dialog.
	cancel := buildInboundRequest("CANCEL", serverPort, peerPort, fromTag, "", callID,
		inviteBranch, "1 CANCEL", "")
	if _, err := peer.WriteTo([]byte(cancel), serverUDP); err != nil {
		t.Fatal(err)
	}
	cancelResponse := readSIPResponseContaining(t, peer, "200 OK")
	if cseq := headerValue(cancelResponse, "CSeq"); cseq != "1 CANCEL" {
		t.Fatalf("late CANCEL response CSeq=%q, want 1 CANCEL", cseq)
	}

	ack := buildInboundRequest("ACK", serverPort, peerPort, fromTag, toValue, callID,
		"z9hG4bK-late-cancel-ack", "1 ACK", "")
	if _, err := peer.WriteTo([]byte(ack), serverUDP); err != nil {
		t.Fatal(err)
	}
	connected := readRuntimeEvent(t, runtime, EventConnected)
	if connected.CallID != incoming.CallID {
		t.Fatalf("connected event=%+v", connected)
	}

	bye := buildInboundRequest("BYE", serverPort, peerPort, fromTag, toValue, callID,
		"z9hG4bK-late-cancel-bye", "2 BYE", "")
	if _, err := peer.WriteTo([]byte(bye), serverUDP); err != nil {
		t.Fatal(err)
	}
	readSIPResponseContaining(t, peer, "200 OK")
	ended := readRuntimeEvent(t, runtime, EventEnded)
	if ended.CallID != incoming.CallID || ended.Reason != "remote_hangup" {
		t.Fatalf("ended event=%+v", ended)
	}
	waitForCallRemoval(t, runtime, "session-1", incoming.CallID)
}

func TestInboundLocalHangupWhileWaitingForAckSendsByeAfterAck(t *testing.T) {
	runtime, peer, serverUDP, serverPort, peerPort := newInboundSIPTestRuntime(t)
	callID := "inbound-local-hangup-during-answer@example.test"
	fromTag := "from-local-hangup"
	inviteBranch := "z9hG4bK-local-hangup"
	invite := buildInboundRequest("INVITE", serverPort, peerPort, fromTag, "", callID,
		inviteBranch, "1 INVITE", testInboundSDP)
	if _, err := peer.WriteTo([]byte(invite), serverUDP); err != nil {
		t.Fatal(err)
	}
	readSIPResponseContaining(t, peer, "180 Ringing")
	incoming := readRuntimeEvent(t, runtime, EventIncoming)
	if err := runtime.Answer(context.Background(), "session-1", incoming.CallID, testAnswerSDP(t)); err != nil {
		t.Fatal(err)
	}
	response := readSIPResponseContaining(t, peer, "200 OK")
	toValue := headerValue(response, "To")
	if err := runtime.Hangup(context.Background(), "session-1", incoming.CallID); err != nil {
		t.Fatal(err)
	}

	ack := buildInboundRequest("ACK", serverPort, peerPort, fromTag, toValue, callID,
		"z9hG4bK-local-hangup-ack", "1 ACK", "")
	if _, err := peer.WriteTo([]byte(ack), serverUDP); err != nil {
		t.Fatal(err)
	}
	bye := readSIPRequestContaining(t, peer, "BYE ")
	if cseq := headerValue(bye, "CSeq"); !strings.HasSuffix(cseq, " BYE") {
		t.Fatalf("gateway BYE CSeq=%q", cseq)
	}
	if _, err := peer.WriteTo([]byte(buildResponseForRequest(bye, 200, "OK")), serverUDP); err != nil {
		t.Fatal(err)
	}
	waitForCallRemoval(t, runtime, "session-1", incoming.CallID)
}

const testInboundSDP = "v=0\r\n" +
	"o=pbx 0 0 IN IP4 127.0.0.1\r\n" +
	"s=PBX\r\n" +
	"c=IN IP4 127.0.0.1\r\n" +
	"t=0 0\r\n" +
	"m=audio 20000 RTP/AVP 0 101\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=rtpmap:101 telephone-event/8000\r\n" +
	"a=sendrecv\r\n"

func testAnswerSDP(t *testing.T) []byte {
	t.Helper()
	value, err := (AsteriskProfile{}).BuildSDP(MediaDescription{
		IP: "127.0.0.1", Port: 30000, RTCPPort: 30001,
		AudioPayloadType: 0, DTMFPayloadType: 101, Codec: "PCMU",
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func reservePacketAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.LocalAddr().String()
	_ = listener.Close()
	return address
}

func reserveTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

func mustPort(t *testing.T, address string) int {
	t.Helper()
	_, value, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func newInboundSIPTestRuntime(t *testing.T) (*SIPGoRuntime, net.PacketConn, *net.UDPAddr, int, int) {
	t.Helper()
	udpAddress := reservePacketAddress(t)
	runtime, err := NewSIPGoRuntime(SIPGoConfig{
		ListenUDP: udpAddress, ListenTCP: reserveTCPAddress(t), AdvertisedIP: "127.0.0.1",
		EventBuffer: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	peer, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	peerPort := peer.LocalAddr().(*net.UDPAddr).Port
	if err := runtime.Configure(context.Background(), "session-1", Configuration{
		Host: "127.0.0.1", Port: uint16(peerPort), Transport: TransportUDP,
		Username: "1129", Password: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	runtime.setRegistered("session-1", true)
	serverUDP, err := net.ResolveUDPAddr("udp", udpAddress)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, peer, serverUDP, mustPort(t, udpAddress), peerPort
}

func newOutboundSIPTestRuntime(t *testing.T) (*SIPGoRuntime, net.PacketConn) {
	t.Helper()
	runtime, peer, _, _, _ := newInboundSIPTestRuntime(t)
	return runtime, peer
}

func establishOutboundTestCall(
	t *testing.T,
	runtime *SIPGoRuntime,
	peer net.PacketConn,
	callID string,
	remoteTag string,
) (string, string, net.Addr) {
	t.Helper()
	if err := runtime.StartCall(context.Background(), "session-1", callID, "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponse(t, peer, gateway, invite, 180, "Ringing", remoteTag)
	if event := readRuntimeEvent(t, runtime, EventRinging); event.CallID != callID {
		t.Fatalf("ringing event=%+v", event)
	}
	writeSIPResponseWithBody(t, peer, gateway, invite, 200, "OK", remoteTag, testInboundSDP)
	ack, _ := readSIPRequestFromContaining(t, peer, "ACK ")
	if event := readRuntimeEvent(t, runtime, EventConnected); event.CallID != callID {
		t.Fatalf("connected event=%+v", event)
	}
	return invite, ack, gateway
}

func assertImmediateOutboundRedial(t *testing.T, runtime *SIPGoRuntime, peer net.PacketConn, callID string) {
	t.Helper()
	if err := runtime.StartCall(context.Background(), "session-1", callID, "9001", []byte(testInboundSDP)); err != nil {
		t.Fatalf("immediate redial failed: %v", err)
	}
	_, _ = readSIPRequestFromContaining(t, peer, "INVITE ")
}

func buildInboundRequest(method string, serverPort, peerPort int, fromTag, toValue, callID, branch, cseq, body string) string {
	if toValue == "" {
		toValue = fmt.Sprintf("<sip:1129@127.0.0.1:%d>", serverPort)
	}
	contentHeaders := ""
	if body != "" {
		contentHeaders = "Content-Type: application/sdp\r\n"
	}
	return fmt.Sprintf("%s sip:1129@127.0.0.1:%d SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP 127.0.0.1:%d;branch=%s;rport\r\n"+
		"Max-Forwards: 70\r\n"+
		"From: <sip:9001@127.0.0.1>;tag=%s\r\n"+
		"To: %s\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: %s\r\n"+
		"Contact: <sip:9001@127.0.0.1:%d>\r\n"+
		"%sContent-Length: %d\r\n\r\n%s",
		method, serverPort, peerPort, branch, fromTag, toValue, callID, cseq, peerPort,
		contentHeaders, len(body), body)
}

func buildResponseForRequest(request string, status int, reason string) string {
	return fmt.Sprintf("SIP/2.0 %d %s\r\n"+
		"Via: %s\r\n"+
		"From: %s\r\n"+
		"To: %s\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: %s\r\n"+
		"Content-Length: 0\r\n\r\n",
		status, reason, headerValue(request, "Via"), headerValue(request, "From"),
		headerValue(request, "To"), headerValue(request, "Call-ID"), headerValue(request, "CSeq"))
}

func readRuntimeEvent(t *testing.T, runtime *SIPGoRuntime, eventType EventType) Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-runtime.Events():
			if event.Type == eventType {
				return event
			}
		case <-deadline:
			t.Fatalf("timed out waiting for runtime event %s", eventType)
		}
	}
}

func waitForCallRemoval(t *testing.T, runtime *SIPGoRuntime, sessionID, callID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := runtime.call(sessionID, callID); err != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("call %s was not removed", callID)
}

func readSIPResponseContaining(t *testing.T, peer net.PacketConn, marker string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	buffer := make([]byte, 4096)
	for time.Now().Before(deadline) {
		if err := peer.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		count, _, err := peer.ReadFrom(buffer)
		if err != nil {
			t.Fatalf("read SIP response %q: %v", marker, err)
		}
		response := string(buffer[:count])
		if strings.Contains(response, marker) {
			return response
		}
	}
	t.Fatalf("timed out waiting for SIP response %q", marker)
	return ""
}

func readSIPRequestContaining(t *testing.T, peer net.PacketConn, marker string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	buffer := make([]byte, 4096)
	for time.Now().Before(deadline) {
		if err := peer.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		count, _, err := peer.ReadFrom(buffer)
		if err != nil {
			t.Fatalf("read SIP request %q: %v", marker, err)
		}
		request := string(buffer[:count])
		if strings.Contains(request, marker) {
			return request
		}
	}
	t.Fatalf("timed out waiting for SIP request %q", marker)
	return ""
}

func readSIPRequestFromContaining(t *testing.T, peer net.PacketConn, marker string) (string, net.Addr) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	buffer := make([]byte, 4096)
	for time.Now().Before(deadline) {
		if err := peer.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		count, source, err := peer.ReadFrom(buffer)
		if err != nil {
			t.Fatalf("read SIP request %q: %v", marker, err)
		}
		request := string(buffer[:count])
		if strings.Contains(request, marker) {
			return request, source
		}
	}
	t.Fatalf("timed out waiting for SIP request %q", marker)
	return "", nil
}

func writeSIPResponse(
	t *testing.T,
	peer net.PacketConn,
	target net.Addr,
	request string,
	status int,
	reason string,
	toTag string,
) {
	writeSIPResponseWithBody(t, peer, target, request, status, reason, toTag, "")
}

func writeSIPResponseWithBody(
	t *testing.T,
	peer net.PacketConn,
	target net.Addr,
	request string,
	status int,
	reason string,
	toTag string,
	body string,
) {
	t.Helper()
	to := headerValue(request, "To")
	if toTag != "" && !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=" + toTag
	}
	extraHeaders := ""
	if status >= 180 && status < 300 {
		extraHeaders += fmt.Sprintf("Contact: <sip:9001@%s;transport=udp>\r\n", peer.LocalAddr().String())
	}
	if body != "" {
		extraHeaders += "Content-Type: application/sdp\r\n"
	}
	response := fmt.Sprintf("SIP/2.0 %d %s\r\n"+
		"Via: %s\r\n"+
		"From: %s\r\n"+
		"To: %s\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: %s\r\n"+
		"%sContent-Length: %d\r\n\r\n%s",
		status, reason, headerValue(request, "Via"), headerValue(request, "From"),
		to, headerValue(request, "Call-ID"), headerValue(request, "CSeq"),
		extraHeaders, len(body), body)
	if _, err := peer.WriteTo([]byte(response), target); err != nil {
		t.Fatal(err)
	}
}

func readSIPResponsesContaining(t *testing.T, peer net.PacketConn, markers ...string) map[string]string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	responses := make(map[string]string, len(markers))
	buffer := make([]byte, 4096)
	for len(responses) < len(markers) {
		if err := peer.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		count, _, err := peer.ReadFrom(buffer)
		if err != nil {
			t.Fatalf("read SIP responses %v: %v", markers, err)
		}
		response := string(buffer[:count])
		for _, marker := range markers {
			if _, found := responses[marker]; !found && strings.Contains(response, marker) {
				responses[marker] = response
			}
		}
	}
	return responses
}

func readTCPResponseContaining(
	t *testing.T, peer net.Conn, reader *bufio.Reader, marker string,
) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := peer.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		var header strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read TCP SIP response %q: %v", marker, err)
			}
			header.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		message := header.String()
		contentLength := 0
		if value := headerValue(message, "Content-Length"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 || parsed > 64*1024 {
				t.Fatalf("invalid TCP SIP Content-Length %q", value)
			}
			contentLength = parsed
		}
		if contentLength > 0 {
			body := make([]byte, contentLength)
			if _, err := io.ReadFull(reader, body); err != nil {
				t.Fatalf("read TCP SIP response body: %v", err)
			}
			message += string(body)
		}
		if strings.Contains(message, marker) {
			return message
		}
	}
	t.Fatalf("timed out waiting for TCP SIP response %q", marker)
	return ""
}

func headerValue(message, name string) string {
	prefix := strings.ToLower(name) + ":"
	for _, line := range strings.Split(message, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}
