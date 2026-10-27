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

func TestContactUserIsStablePerSessionAndDistinctAcrossSessions(t *testing.T) {
	identity, err := IdentityOf(Configuration{
		Host: "pbx.example", Port: 5060, Transport: TransportUDP, Username: "100",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := contactUserFor("session-1", identity)
	if first != contactUserFor("session-1", identity) {
		t.Fatal("contact user changed for the same session identity")
	}
	if first == contactUserFor("session-2", identity) {
		t.Fatal("different sessions shared an inbound Contact user")
	}
	if strings.Contains(first, identity.Username) {
		t.Fatal("opaque Contact user exposed the SIP username")
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

func TestOutboundInviteUsesRegisteredSIPUsernameAsFromIdentity(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	if err := runtime.StartCall(context.Background(), "session-1", "outbound-identity", "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	from := headerValue(invite, "From")
	if !strings.Contains(from, "<sip:1129@127.0.0.1>") {
		t.Fatalf("outbound From identity = %q, want registered SIP username", from)
	}
	if !strings.Contains(strings.ToLower(from), ";tag=") {
		t.Fatalf("outbound From identity is missing dialog tag: %q", from)
	}
	if strings.Contains(from, "DebtFlowSipGateway") {
		t.Fatalf("outbound From identity leaked user-agent product name: %q", from)
	}

	writeSIPResponse(t, peer, gateway, invite, 603, "Decline", "identity-test")
	_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	failed := readRuntimeEvent(t, runtime, EventFailed)
	if failed.StatusCode != 603 || failed.Reason != "rejected" {
		t.Fatalf("terminal event=%+v", failed)
	}
}

func TestOutboundSessionProgressPublishesEarlyMediaSDP(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	const callID = "outbound-early-media"
	if err := runtime.StartCall(context.Background(), "session-1", callID, "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponseWithBody(t, peer, gateway, invite, 183, "Session Progress",
		"early-media", testInboundSDP)

	event := readRuntimeEvent(t, runtime, EventEarlyMedia)
	if event.CallID != callID || event.StatusCode != 183 || string(event.RemoteSDP) != testInboundSDP {
		t.Fatalf("early media event=%+v", event)
	}

	writeSIPResponse(t, peer, gateway, invite, 603, "Decline", "early-media")
	_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	failed := readRuntimeEvent(t, runtime, EventFailed)
	if failed.CallID != callID || failed.Reason != "rejected" {
		t.Fatalf("terminal event=%+v", failed)
	}
}

func TestOutboundReliableProvisionalResponseSendsPRACKBeforeEarlyMedia(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	const callID = "outbound-reliable-provisional"
	if err := runtime.StartCall(context.Background(), "session-1", callID, "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponseWithHeadersAndBody(t, peer, gateway, invite,
		183, "Session Progress", "reliable-progress",
		[]string{"Require: 100rel", "RSeq: 41"}, testInboundSDP)

	prack, prackTarget := readSIPRequestFromContaining(t, peer, "PRACK ")
	inviteCSeq := strings.Fields(headerValue(invite, "CSeq"))[0]
	wantRAck := "41 " + inviteCSeq + " INVITE"
	if rack := headerValue(prack, "RAck"); rack != wantRAck {
		t.Fatalf("PRACK RAck=%q, want %q", rack, wantRAck)
	}
	if _, err := peer.WriteTo([]byte(buildResponseForRequest(prack, 200, "OK")), prackTarget); err != nil {
		t.Fatal(err)
	}
	event := readRuntimeEvent(t, runtime, EventEarlyMedia)
	if event.CallID != callID || event.StatusCode != 183 {
		t.Fatalf("early media event=%+v", event)
	}

	writeSIPResponse(t, peer, gateway, invite, 486, "Busy Here", "reliable-progress")
	_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	failed := readRuntimeEvent(t, runtime, EventFailed)
	if failed.CallID != callID || failed.Reason != "busy" {
		t.Fatalf("terminal event=%+v", failed)
	}
}

func TestOutboundRingingWithSDPPublishesEarlyMedia(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	const callID = "outbound-ringing-early-media"
	if err := runtime.StartCall(context.Background(), "session-1", callID, "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponseWithBody(t, peer, gateway, invite, 180, "Ringing",
		"ringing-early-media", testInboundSDP)

	event := readRuntimeEvent(t, runtime, EventEarlyMedia)
	if event.CallID != callID || event.StatusCode != 180 || string(event.RemoteSDP) != testInboundSDP {
		t.Fatalf("early media event=%+v", event)
	}

	writeSIPResponse(t, peer, gateway, invite, 486, "Busy Here", "ringing-early-media")
	_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	failed := readRuntimeEvent(t, runtime, EventFailed)
	if failed.CallID != callID || failed.Reason != "busy" {
		t.Fatalf("terminal event=%+v", failed)
	}
}

func TestOutboundSessionProgressWithoutSDPFallsBackToRinging(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	const callID = "outbound-progress-without-sdp"
	if err := runtime.StartCall(context.Background(), "session-1", callID, "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponse(t, peer, gateway, invite, 183, "Session Progress", "progress-no-sdp")

	event := readRuntimeEvent(t, runtime, EventRinging)
	if event.CallID != callID || event.StatusCode != 183 || len(event.RemoteSDP) != 0 {
		t.Fatalf("ringing fallback event=%+v", event)
	}

	writeSIPResponse(t, peer, gateway, invite, 486, "Busy Here", "progress-no-sdp")
	_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	failed := readRuntimeEvent(t, runtime, EventFailed)
	if failed.CallID != callID || failed.Reason != "busy" {
		t.Fatalf("terminal event=%+v", failed)
	}
}

func TestOutboundInviteDigestRetryKeepsSessionIdentityAndClassifiesForbidden(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	if err := runtime.StartCall(context.Background(), "session-1", "outbound-auth", "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	challenge := strings.Replace(
		buildResponseForRequest(invite, 401, "Unauthorized"),
		"Content-Length: 0\r\n",
		"WWW-Authenticate: Digest realm=\"asterisk\", nonce=\"test-nonce\", algorithm=MD5\r\nContent-Length: 0\r\n",
		1,
	)
	if _, err := peer.WriteTo([]byte(challenge), gateway); err != nil {
		t.Fatal(err)
	}
	authenticatedInvite, authenticatedGateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	if from := headerValue(authenticatedInvite, "From"); !strings.Contains(from, "<sip:1129@127.0.0.1>") {
		t.Fatalf("authenticated outbound From identity = %q", from)
	}
	authorization := headerValue(authenticatedInvite, "Authorization")
	if !strings.Contains(authorization, `username="1129"`) || !strings.Contains(authorization, `realm="asterisk"`) {
		t.Fatalf("authenticated outbound INVITE has unexpected digest identity: %q", authorization)
	}
	if headerValue(authenticatedInvite, "CSeq") == headerValue(invite, "CSeq") {
		t.Fatalf("authenticated outbound INVITE did not advance CSeq: initial=%q authenticated=%q",
			headerValue(invite, "CSeq"), headerValue(authenticatedInvite, "CSeq"))
	}

	writeSIPResponse(t, peer, authenticatedGateway, authenticatedInvite, 403, "Forbidden", "auth-forbidden")
	_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	failed := readRuntimeEvent(t, runtime, EventFailed)
	if failed.StatusCode != 403 || failed.Reason != "forbidden" {
		t.Fatalf("terminal event=%+v", failed)
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

func TestOutboundRingingWithoutFinalResponseTimesOutAndAllowsImmediateRedial(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	runtime.cfg.OutboundTimeout = 50 * time.Millisecond

	const firstCallID = "outbound-timeout-1"
	if err := runtime.StartCall(context.Background(), "session-1", firstCallID, "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponse(t, peer, gateway, invite, 180, "Ringing", "timeout-test")
	if event := readRuntimeEvent(t, runtime, EventRinging); event.CallID != firstCallID {
		t.Fatalf("ringing event=%+v", event)
	}

	failed := readRuntimeEvent(t, runtime, EventFailed)
	if failed.CallID != firstCallID || failed.Reason != "no_answer" {
		t.Fatalf("timeout event=%+v", failed)
	}
	if _, err := runtime.call("session-1", firstCallID); err == nil {
		t.Fatal("timed-out outbound call still retained the session call slot")
	}

	// A missing final response must not retain an application-level call lock,
	// even while the old SIP transaction completes its CANCEL handshake.
	const secondCallID = "outbound-timeout-2"
	if err := runtime.StartCall(context.Background(), "session-1", secondCallID, "9001", []byte(testInboundSDP)); err != nil {
		t.Fatalf("immediate redial after timeout failed: %v", err)
	}
	secondInvite, secondGateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponse(t, peer, secondGateway, secondInvite, 603, "Decline", "timeout-redial")
	_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	secondFailed := readRuntimeEvent(t, runtime, EventFailed)
	if secondFailed.CallID != secondCallID || secondFailed.Reason != "rejected" {
		t.Fatalf("redial terminal event=%+v", secondFailed)
	}
}

func TestOutboundLateAnswerAfterTimeoutIsAckedAndEndedWithBye(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	runtime.cfg.OutboundTimeout = 50 * time.Millisecond

	const callID = "outbound-timeout-late-answer"
	if err := runtime.StartCall(context.Background(), "session-1", callID, "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	callValue, err := runtime.call("session-1", callID)
	if err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponse(t, peer, gateway, invite, 180, "Ringing", "timeout-late-answer")
	_ = readRuntimeEvent(t, runtime, EventRinging)
	failed := readRuntimeEvent(t, runtime, EventFailed)
	if failed.CallID != callID || failed.Reason != "no_answer" {
		t.Fatalf("timeout event=%+v", failed)
	}

	cancelRequest, gateway := readSIPRequestFromContaining(t, peer, "CANCEL ")
	writeSIPResponse(t, peer, gateway, cancelRequest, 200, "OK", "timeout-late-answer")
	writeSIPResponseWithBody(t, peer, gateway, invite, 200, "OK", "timeout-late-answer", testInboundSDP)
	_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	bye, gateway := readSIPRequestFromContaining(t, peer, "BYE ")
	writeSIPResponse(t, peer, gateway, bye, 200, "OK", "timeout-late-answer")
	select {
	case <-callValue.outboundDone:
	case <-time.After(2 * time.Second):
		t.Fatal("late answer after timeout did not complete ACK/BYE cleanup")
	}
	select {
	case event := <-runtime.Events():
		if event.Type == EventConnected {
			t.Fatalf("late answer resurrected timed-out call: %+v", event)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

func TestOutboundFinalResponseAndTimeoutAreIsolatedAcrossSessions(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	runtime.cfg.OutboundTimeout = 100 * time.Millisecond
	peerPort := peer.LocalAddr().(*net.UDPAddr).Port
	if err := runtime.Configure(context.Background(), "session-2", Configuration{
		Host: "127.0.0.1", Port: uint16(peerPort), Transport: TransportUDP,
		Username: "1130", Password: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	runtime.setRegistered("session-2", true)

	if err := runtime.StartCall(context.Background(), "session-1", "session-1-timeout", "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	firstInvite, firstGateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponse(t, peer, firstGateway, firstInvite, 180, "Ringing", "session-one")
	if event := readRuntimeEvent(t, runtime, EventRinging); event.CallID != "session-1-timeout" {
		t.Fatalf("session 1 ringing event=%+v", event)
	}

	if err := runtime.StartCall(context.Background(), "session-2", "session-2-decline", "9002", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	secondInvite, secondGateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponse(t, peer, secondGateway, secondInvite, 180, "Ringing", "session-two")
	if event := readRuntimeEvent(t, runtime, EventRinging); event.CallID != "session-2-decline" {
		t.Fatalf("session 2 ringing event=%+v", event)
	}
	writeSIPResponse(t, peer, secondGateway, secondInvite, 603, "Decline", "session-two")
	_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	declined := readRuntimeEvent(t, runtime, EventFailed)
	if declined.SessionID != "session-2" || declined.CallID != "session-2-decline" || declined.Reason != "rejected" {
		t.Fatalf("session 2 terminal event=%+v", declined)
	}

	timedOut := readRuntimeEvent(t, runtime, EventFailed)
	if timedOut.SessionID != "session-1" || timedOut.CallID != "session-1-timeout" || timedOut.Reason != "no_answer" {
		t.Fatalf("session 1 terminal event=%+v", timedOut)
	}
}

func TestOutboundDigestAuthenticationRetriesInviteWithAccountIdentity(t *testing.T) {
	tests := []struct {
		name            string
		status          int
		reason          string
		challengeHeader string
		authorization   string
	}{
		{
			name: "www-authenticate", status: 401, reason: "Unauthorized",
			challengeHeader: "WWW-Authenticate", authorization: "Authorization",
		},
		{
			name: "proxy-authenticate", status: 407, reason: "Proxy Authentication Required",
			challengeHeader: "Proxy-Authenticate", authorization: "Proxy-Authorization",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime, peer := newOutboundSIPTestRuntime(t)
			const callID = "outbound-digest-auth"
			if err := runtime.StartCall(
				context.Background(), "session-1", callID, "9001", []byte(testInboundSDP),
			); err != nil {
				t.Fatal(err)
			}
			firstInvite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
			writeSIPAuthChallenge(t, peer, gateway, firstInvite, test.status, test.reason,
				test.challengeHeader, `Digest realm="pbx.test", nonce="nonce-one", algorithm=MD5, qop="auth"`)
			authenticatedInvite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
			if headerValue(authenticatedInvite, test.authorization) == "" {
				t.Fatalf("authenticated INVITE has no %s: %q", test.authorization, authenticatedInvite)
			}
			if got := headerValue(authenticatedInvite, "From"); !strings.Contains(got, "sip:1129@") {
				t.Fatalf("authenticated INVITE From=%q", got)
			}
			if headerValue(authenticatedInvite, "Call-ID") != headerValue(firstInvite, "Call-ID") {
				t.Fatal("authenticated INVITE changed Call-ID")
			}
			if headerValue(authenticatedInvite, "Via") == headerValue(firstInvite, "Via") {
				t.Fatal("authenticated INVITE reused the original Via branch")
			}
			if headerValue(authenticatedInvite, "CSeq") == headerValue(firstInvite, "CSeq") {
				t.Fatal("authenticated INVITE did not increment CSeq")
			}
			writeSIPResponse(t, peer, gateway, authenticatedInvite, 180, "Ringing", "digest-auth")
			if event := readRuntimeEvent(t, runtime, EventRinging); event.CallID != callID {
				t.Fatalf("ringing event=%+v", event)
			}
			writeSIPResponseWithBody(t, peer, gateway, authenticatedInvite, 200, "OK",
				"digest-auth", testInboundSDP)
			_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
			if event := readRuntimeEvent(t, runtime, EventConnected); event.CallID != callID {
				t.Fatalf("connected event=%+v", event)
			}
		})
	}
}

func TestOutboundDigestAuthenticationRetriesStaleNonce(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	const callID = "outbound-stale-digest-auth"
	if err := runtime.StartCall(
		context.Background(), "session-1", callID, "9001", []byte(testInboundSDP),
	); err != nil {
		t.Fatal(err)
	}
	firstInvite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPAuthChallenge(t, peer, gateway, firstInvite, 401, "Unauthorized", "WWW-Authenticate",
		`Digest realm="pbx.test", nonce="nonce-one", algorithm=MD5, qop="auth"`)
	secondInvite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPAuthChallenge(t, peer, gateway, secondInvite, 401, "Unauthorized", "WWW-Authenticate",
		`Digest realm="pbx.test", nonce="nonce-two", algorithm=MD5, qop="auth", stale=true`)
	thirdInvite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	if headerValue(thirdInvite, "Authorization") == "" {
		t.Fatal("stale nonce retry has no Authorization header")
	}
	writeSIPResponseWithBody(t, peer, gateway, thirdInvite, 200, "OK", "digest-stale", testInboundSDP)
	_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	if event := readRuntimeEvent(t, runtime, EventConnected); event.CallID != callID {
		t.Fatalf("connected event=%+v", event)
	}
}

func TestConcurrentSessionsDoNotCrossDigestIdentities(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	peerPort := peer.LocalAddr().(*net.UDPAddr).Port
	if err := runtime.Configure(context.Background(), "session-2", Configuration{
		Host: "127.0.0.1", Port: uint16(peerPort), Transport: TransportUDP,
		Username: "3100", Password: "second-secret",
	}); err != nil {
		t.Fatal(err)
	}
	runtime.setRegistered("session-2", true)
	for _, call := range []struct {
		sessionID string
		callID    string
	}{
		{sessionID: "session-1", callID: "digest-session-1129"},
		{sessionID: "session-2", callID: "digest-session-3100"},
	} {
		if err := runtime.StartCall(context.Background(), call.sessionID, call.callID,
			"9001", []byte(testInboundSDP)); err != nil {
			t.Fatal(err)
		}
	}

	initial := make(map[string]struct {
		request string
		target  net.Addr
	}, 2)
	for range 2 {
		request, target := readSIPRequestFromContaining(t, peer, "INVITE ")
		username := sipUsernameFromHeader(headerValue(request, "From"))
		initial[username] = struct {
			request string
			target  net.Addr
		}{request: request, target: target}
	}
	for _, username := range []string{"1129", "3100"} {
		value, ok := initial[username]
		if !ok {
			t.Fatalf("missing initial INVITE for %s: %+v", username, initial)
		}
		writeSIPAuthChallenge(t, peer, value.target, value.request, 401, "Unauthorized",
			"WWW-Authenticate", `Digest realm="pbx.test", nonce="nonce-`+username+`", algorithm=MD5, qop="auth"`)
	}

	for range 2 {
		request, target := readSIPRequestFromContaining(t, peer, "INVITE ")
		username := sipUsernameFromHeader(headerValue(request, "From"))
		authorization := headerValue(request, "Authorization")
		if !strings.Contains(authorization, `username="`+username+`"`) {
			t.Fatalf("digest identity crossed sessions: from=%q authorization=%q",
				headerValue(request, "From"), authorization)
		}
		writeSIPResponse(t, peer, target, request, 603, "Decline", "digest-"+username)
	}
	for range 2 {
		_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	}
	failed := map[string]bool{}
	for range 2 {
		event := readRuntimeEvent(t, runtime, EventFailed)
		failed[event.SessionID] = true
	}
	if !failed["session-1"] || !failed["session-2"] {
		t.Fatalf("terminal events crossed or missing: %+v", failed)
	}
}

func TestConfigureAllowsSameUsernameAcrossDifferentPBXIdentities(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	firstPort := peer.LocalAddr().(*net.UDPAddr).Port
	secondPort := firstPort + 1
	if firstPort == 65535 {
		secondPort = firstPort - 1
	}
	if err := runtime.Configure(context.Background(), "session-2", Configuration{
		Host: "127.0.0.1", Port: uint16(secondPort), Transport: TransportUDP,
		Username: "1129", Password: "branch-two-secret",
	}); err != nil {
		t.Fatalf("same username on another PBX was rejected: %v", err)
	}
	if err := runtime.Configure(context.Background(), "session-3", Configuration{
		Host: "127.0.0.1", Port: uint16(firstPort), Transport: TransportUDP,
		Username: "1129", Password: "duplicate-secret",
	}); err == nil {
		t.Fatal("same SIP identity was accepted for another session")
	}
	runtime.mu.RLock()
	first := runtime.sessions["session-1"]
	second := runtime.sessions["session-2"]
	runtime.mu.RUnlock()
	if first == nil || second == nil || first.contactUser == second.contactUser {
		t.Fatalf("sessions did not receive distinct Contact identities: first=%v second=%v", first, second)
	}
}

func TestTransportLevelFinalResponseFallbackEndsCallWhenTransactionDoesNotMatch(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	runtime.cfg.OutboundTimeout = 2 * time.Second

	const callID = "outbound-unmatched-final"
	if err := runtime.StartCall(context.Background(), "session-1", callID, "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponse(t, peer, gateway, invite, 180, "Ringing", "unmatched-final")
	if event := readRuntimeEvent(t, runtime, EventRinging); event.CallID != callID {
		t.Fatalf("ringing event=%+v", event)
	}

	// Preserve Call-ID/CSeq but deliberately change the Via branch. sipgo's
	// transaction layer cannot match this response, while the transport-level
	// fallback can still safely correlate it to this exact active call.
	response := buildResponseForRequest(invite, 603, "Decline")
	response = strings.Replace(response,
		"Via: "+headerValue(invite, "Via")+"\r\n",
		"Via: SIP/2.0/UDP 127.0.0.1:9;branch=z9hG4bK-unmatched-final;rport\r\n", 1)
	if _, err := peer.WriteTo([]byte(response), gateway); err != nil {
		t.Fatal(err)
	}

	failed := readRuntimeEvent(t, runtime, EventFailed)
	if failed.CallID != callID || failed.StatusCode != 603 || failed.Reason != "rejected" {
		t.Fatalf("transport fallback event=%+v", failed)
	}
	waitForCallRemoval(t, runtime, "session-1", callID)
}

func TestOutboundFinalResponsesEndRingingAndAllowImmediateRedial(t *testing.T) {
	tests := []struct {
		status int
		phrase string
		reason string
	}{
		{404, "Not Found", "invalid_destination"},
		{403, "Forbidden", "forbidden"},
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

func TestOutboundDeclineDoesNotBlockImmediateCallFromAnotherSession(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	peerPort := peer.LocalAddr().(*net.UDPAddr).Port
	if err := runtime.Configure(context.Background(), "session-2", Configuration{
		Host: "127.0.0.1", Port: uint16(peerPort), Transport: TransportUDP,
		Username: "1130", Password: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	runtime.setRegistered("session-2", true)

	if err := runtime.StartCall(context.Background(), "session-1", "cross-session-1", "9001", []byte(testInboundSDP)); err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	writeSIPResponse(t, peer, gateway, invite, 180, "Ringing", "cross-session-decline")
	_ = readRuntimeEvent(t, runtime, EventRinging)
	writeSIPResponse(t, peer, gateway, invite, 603, "Decline", "cross-session-decline")
	_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	failed := readRuntimeEvent(t, runtime, EventFailed)
	if failed.StatusCode != 603 || failed.Reason != "rejected" {
		t.Fatalf("terminal event=%+v", failed)
	}

	if err := runtime.StartCall(context.Background(), "session-2", "cross-session-2", "9001", []byte(testInboundSDP)); err != nil {
		t.Fatalf("cross-session immediate call failed: %v", err)
	}
	secondInvite, _ := readSIPRequestFromContaining(t, peer, "INVITE ")
	if headerValue(secondInvite, "Call-ID") == headerValue(invite, "Call-ID") {
		t.Fatal("cross-session call reused completed SIP Call-ID")
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

func TestEstablishedOutboundCallAcceptsCompatibleAndOfferlessReinvite(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	const applicationCallID = "outbound-reinvite"
	_, ack, gateway := establishOutboundTestCall(t, runtime, peer, applicationCallID, "remote-reinvite")
	_, peerPortText, err := net.SplitHostPort(peer.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	peerPort, err := strconv.Atoi(peerPortText)
	if err != nil {
		t.Fatal(err)
	}
	_, serverPortText, err := net.SplitHostPort(gateway.String())
	if err != nil {
		t.Fatal(err)
	}
	serverPort, err := strconv.Atoi(serverPortText)
	if err != nil {
		t.Fatal(err)
	}
	remoteTag := sipTagFromHeader(headerValue(ack, "To"))
	localTo := headerValue(ack, "From")
	sipCallID := headerValue(ack, "Call-ID")
	if remoteTag == "" || localTo == "" || sipCallID == "" {
		t.Fatalf("established dialog headers are incomplete: %q", ack)
	}

	updatedSDP := strings.Replace(testInboundSDP, "m=audio 20000", "m=audio 24000", 1)
	reinvite := buildInboundRequest("INVITE", serverPort, peerPort, remoteTag, localTo, sipCallID,
		"z9hG4bK-outbound-reinvite", "2 INVITE", updatedSDP)
	if _, err := peer.WriteTo([]byte(reinvite), gateway); err != nil {
		t.Fatal(err)
	}
	response := readSIPResponseContaining(t, peer, "SIP/2.0 ")
	if !strings.Contains(response, "200 OK") {
		t.Fatalf("re-INVITE was not accepted: %q", response)
	}
	if cseq := headerValue(response, "CSeq"); cseq != "2 INVITE" {
		t.Fatalf("re-INVITE response CSeq=%q", cseq)
	}
	mediaUpdated := readRuntimeEvent(t, runtime, EventMediaUpdated)
	if mediaUpdated.CallID != applicationCallID || string(mediaUpdated.RemoteSDP) != updatedSDP {
		t.Fatalf("media update event=%+v", mediaUpdated)
	}
	reinviteAck := buildInboundRequest("ACK", serverPort, peerPort, remoteTag, localTo, sipCallID,
		"z9hG4bK-outbound-reinvite-ack", "2 ACK", "")
	if _, err := peer.WriteTo([]byte(reinviteAck), gateway); err != nil {
		t.Fatal(err)
	}
	callValue, err := runtime.call("session-1", applicationCallID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		callValue.mu.RLock()
		pending := callValue.reinviteAck != nil
		callValue.mu.RUnlock()
		if !pending {
			break
		}
		time.Sleep(time.Millisecond)
	}
	callValue.mu.RLock()
	stillPending := callValue.reinviteAck != nil
	callValue.mu.RUnlock()
	if stillPending {
		t.Fatal("compatible re-INVITE ACK did not complete the transaction")
	}

	offerless := buildInboundRequest("INVITE", serverPort, peerPort, remoteTag, localTo, sipCallID,
		"z9hG4bK-outbound-offerless", "3 INVITE", "")
	if _, err := peer.WriteTo([]byte(offerless), gateway); err != nil {
		t.Fatal(err)
	}
	response = readSIPResponseContaining(t, peer, "200 OK")
	if cseq := headerValue(response, "CSeq"); cseq != "3 INVITE" || !strings.Contains(response, "m=audio") {
		t.Fatalf("offerless re-INVITE response is incomplete: %q", response)
	}
	answerSDP := strings.Replace(testInboundSDP, "m=audio 20000", "m=audio 25000", 1)
	offerlessAck := buildInboundRequest("ACK", serverPort, peerPort, remoteTag, localTo, sipCallID,
		"z9hG4bK-outbound-offerless-ack", "3 ACK", answerSDP)
	if _, err := peer.WriteTo([]byte(offerlessAck), gateway); err != nil {
		t.Fatal(err)
	}
	mediaUpdated = readRuntimeEvent(t, runtime, EventMediaUpdated)
	if mediaUpdated.CallID != applicationCallID || string(mediaUpdated.RemoteSDP) != answerSDP {
		t.Fatalf("offerless media update event=%+v", mediaUpdated)
	}

	remoteBye := buildInboundRequest("BYE", serverPort, peerPort, remoteTag, localTo, sipCallID,
		"z9hG4bK-outbound-reinvite-bye", "4 BYE", "")
	if _, err := peer.WriteTo([]byte(remoteBye), gateway); err != nil {
		t.Fatal(err)
	}
	readSIPResponseContaining(t, peer, "200 OK")
	ended := readRuntimeEvent(t, runtime, EventEnded)
	if ended.CallID != applicationCallID || ended.Reason != "remote_hangup" {
		t.Fatalf("terminal event=%+v", ended)
	}
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

func TestResolveSessionTargetRefreshesIPv4ARecordAndReusesCurrentTarget(t *testing.T) {
	addresses := []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("192.0.2.11")}
	calls := 0
	runtime := &SIPGoRuntime{cfg: SIPGoConfig{ResolveIPv4: func(context.Context, string) ([]net.IP, error) {
		address := addresses[calls]
		calls++
		return []net.IP{address}, nil
	}}}
	session := &sipgoSession{identity: Identity{
		Host: "pbx.branch.example", Port: 5060, Transport: TransportUDP, Username: "100",
	}}
	first, err := runtime.resolveSessionTarget(context.Background(), session, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runtime.resolveSessionTarget(context.Background(), session, true)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := runtime.resolveSessionTarget(context.Background(), session, false)
	if err != nil {
		t.Fatal(err)
	}
	if first != "192.0.2.10:5060" || second != "192.0.2.11:5060" || cached != second || calls != 2 {
		t.Fatalf("targets first=%q second=%q cached=%q calls=%d", first, second, cached, calls)
	}
}

func TestResolveSessionTargetRejectsMissingIPv4AndPropagatesCancellation(t *testing.T) {
	providerFailure := errors.New("resolver unavailable")
	for name, resolver := range map[string]func(context.Context, string) ([]net.IP, error){
		"empty":     func(context.Context, string) ([]net.IP, error) { return nil, nil },
		"aaaa-only": func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("2001:db8::10")}, nil },
		"error":     func(context.Context, string) ([]net.IP, error) { return nil, providerFailure },
	} {
		t.Run(name, func(t *testing.T) {
			runtime := &SIPGoRuntime{cfg: SIPGoConfig{ResolveIPv4: resolver}}
			session := &sipgoSession{identity: Identity{
				Host: "pbx.branch.example", Port: 5060, Transport: TransportUDP, Username: "100",
			}}
			_, err := runtime.resolveSessionTarget(context.Background(), session, true)
			if name == "error" {
				if !errors.Is(err, providerFailure) {
					t.Fatalf("resolver error=%v, want provider failure", err)
				}
			} else if !errors.Is(err, errSIPHostIPv4Unavailable) {
				t.Fatalf("resolver error=%v, want IPv4 unavailable", err)
			}
		})
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	runtime := &SIPGoRuntime{cfg: SIPGoConfig{ResolveIPv4: func(context.Context, string) ([]net.IP, error) {
		called = true
		return nil, nil
	}}}
	session := &sipgoSession{identity: Identity{
		Host: "pbx.branch.example", Port: 5060, Transport: TransportUDP, Username: "100",
	}}
	if _, err := runtime.resolveSessionTarget(cancelled, session, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled resolve error=%v", err)
	}
	if called {
		t.Fatal("resolver was called after cancellation")
	}
}

func TestHostnameUsesIPv4DestinationWhileKeepingConfiguredHostInUDPRequestURI(t *testing.T) {
	peer, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	resolverCalls := 0
	runtime, err := NewSIPGoRuntime(SIPGoConfig{
		ListenUDP: reservePacketAddress(t), ListenTCP: reserveTCPAddress(t), AdvertisedIP: "127.0.0.1",
		EventBuffer: 16,
		ResolveIPv4: func(context.Context, string) ([]net.IP, error) {
			resolverCalls++
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	peerPort := peer.LocalAddr().(*net.UDPAddr).Port
	if err := runtime.Configure(context.Background(), "session-1", Configuration{
		Host: "pbx.branch.example", Port: uint16(peerPort), Transport: TransportUDP,
		Username: "1129", Password: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	registerResult := make(chan error, 1)
	go func() { registerResult <- runtime.Register(context.Background(), "session-1") }()
	register, gateway := readSIPRequestFromContaining(t, peer, "REGISTER ")
	if !strings.HasPrefix(register, fmt.Sprintf("REGISTER sip:pbx.branch.example:%d SIP/2.0", peerPort)) {
		t.Fatalf("REGISTER request URI lost configured hostname: %q", strings.Split(register, "\r\n")[0])
	}
	writeSIPResponse(t, peer, gateway, register, 200, "OK", "hostname-register")
	if err := <-registerResult; err != nil {
		t.Fatal(err)
	}
	_ = readRuntimeEvent(t, runtime, EventRegistered)
	if err := runtime.StartCall(
		context.Background(), "session-1", "hostname-outbound", "9001", []byte(testInboundSDP),
	); err != nil {
		t.Fatal(err)
	}
	invite, gateway := readSIPRequestFromContaining(t, peer, "INVITE ")
	if !strings.Contains(strings.Split(invite, "\r\n")[0], "@pbx.branch.example:") {
		t.Fatalf("INVITE request URI lost configured hostname: %q", strings.Split(invite, "\r\n")[0])
	}
	writeSIPResponse(t, peer, gateway, invite, 603, "Decline", "hostname-outbound")
	_, _ = readSIPRequestFromContaining(t, peer, "ACK ")
	_ = readRuntimeEvent(t, runtime, EventFailed)
	runtime.cfg.ResolveIPv4 = func(context.Context, string) ([]net.IP, error) {
		return nil, errors.New("DNS must not run during unregister")
	}
	unregisterResult := make(chan error, 1)
	go func() { unregisterResult <- runtime.Unregister(context.Background(), "session-1") }()
	unregister, gateway := readSIPRequestFromContaining(t, peer, "REGISTER ")
	writeSIPResponse(t, peer, gateway, unregister, 200, "OK", "hostname-unregister")
	if err := <-unregisterResult; err != nil {
		t.Fatal(err)
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver calls=%d, want 1 registration lookup", resolverCalls)
	}
}

func TestHostnameRegistrationUsesExplicitIPv4DestinationOverTCP(t *testing.T) {
	pbx, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pbx.Close() })
	runtime, err := NewSIPGoRuntime(SIPGoConfig{
		ListenUDP: reservePacketAddress(t), ListenTCP: reserveTCPAddress(t), AdvertisedIP: "127.0.0.1",
		EventBuffer: 16,
		ResolveIPv4: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	pbxPort := pbx.Addr().(*net.TCPAddr).Port
	if err := runtime.Configure(context.Background(), "session-1", Configuration{
		Host: "pbx.tcp.example", Port: uint16(pbxPort), Transport: TransportTCP,
		Username: "1129", Password: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	registerResult := make(chan error, 1)
	go func() { registerResult <- runtime.Register(context.Background(), "session-1") }()
	peer, err := pbx.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	reader := bufio.NewReader(peer)
	register := readTCPResponseContaining(t, peer, reader, "REGISTER ")
	if !strings.HasPrefix(register, fmt.Sprintf("REGISTER sip:pbx.tcp.example:%d SIP/2.0", pbxPort)) {
		t.Fatalf("TCP REGISTER request URI lost configured hostname: %q", strings.Split(register, "\r\n")[0])
	}
	if _, err := io.WriteString(peer, buildResponseForRequest(register, 200, "OK")); err != nil {
		t.Fatal(err)
	}
	if err := <-registerResult; err != nil {
		t.Fatal(err)
	}
}

func TestInboundSessionSelectionSeparatesSameUsernameByPBXPeer(t *testing.T) {
	firstIdentity, err := IdentityOf(Configuration{
		Host: "192.0.2.10", Port: 5060, Transport: TransportUDP, Username: "100",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, err := IdentityOf(Configuration{
		Host: "192.0.2.11", Port: 5060, Transport: TransportUDP, Username: "100",
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &SIPGoRuntime{
		sessions: map[string]*sipgoSession{
			"first": {
				identity: firstIdentity, registered: true, registeredPeer: "192.0.2.10:5060",
				contactUser: "df-first",
			},
			"second": {
				identity: secondIdentity, registered: true, registeredPeer: "192.0.2.11:5060",
				contactUser: "df-second",
			},
		},
		contacts: map[string]string{"df-first": "first", "df-second": "second"},
	}
	request := sipmessage.NewRequest(sipmessage.INVITE, sipmessage.Uri{User: "100"})
	request.SetTransport("UDP")
	request.SetSource("192.0.2.11:5060")
	sessionID, _, registered := runtime.sessionForInboundRequest(request)
	if sessionID != "second" || !registered {
		t.Fatalf("inbound request selected session %q registered=%v, want second", sessionID, registered)
	}

	request.Recipient.User = "df-first"
	request.SetSource("198.51.100.20:5090")
	sessionID, _, registered = runtime.sessionForInboundRequest(request)
	if sessionID != "first" || !registered {
		t.Fatalf("unique registered Contact selected session %q registered=%v, want first", sessionID, registered)
	}

	request.Recipient.User = "100"
	request.SetSource("198.51.100.20:5090")
	sessionID, session, registered := runtime.sessionForInboundRequest(request)
	if sessionID != "" || session != nil || registered {
		t.Fatalf("ambiguous username fallback selected session %q registered=%v", sessionID, registered)
	}
}

func TestStartCallRejectsUnregisteredSessionBeforeSendingInvite(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	runtime.setRegistered("session-1", false)

	err := runtime.StartCall(
		context.Background(), "session-1", "unregistered-call", "9001", []byte(testInboundSDP),
	)
	if !errors.Is(err, ErrRegistrationNotReady) {
		t.Fatalf("StartCall error=%v, want %v", err, ErrRegistrationNotReady)
	}
	assertNoSIPPacket(t, peer, 100*time.Millisecond)
}

func TestStaleRegistrationGenerationCannotMutateReplacementSession(t *testing.T) {
	runtime, _, _, _, peerPort := newInboundSIPTestRuntime(t)
	oldSession, err := runtime.session("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Configure(context.Background(), "session-1", Configuration{
		Host: "127.0.0.1", Port: uint16(peerPort), Transport: TransportUDP,
		Username: "3100", Password: "replacement-secret",
	}); err != nil {
		t.Fatal(err)
	}

	if _, current := runtime.setRegisteredIfCurrent("session-1", oldSession, true); current {
		t.Fatal("stale registration generation unexpectedly mutated replacement session")
	}
	newSession, err := runtime.session("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if newSession == oldSession || newSession.registered {
		t.Fatalf("replacement session=%p old=%p registered=%v", newSession, oldSession, newSession.registered)
	}
}

func TestRegisterResponseFromReplacedSessionIsDiscarded(t *testing.T) {
	runtime, peer, _, _, peerPort := newInboundSIPTestRuntime(t)
	registerResult := make(chan error, 1)
	go func() {
		registerResult <- runtime.Register(context.Background(), "session-1")
	}()
	request, gateway := readSIPRequestFromContaining(t, peer, "REGISTER ")
	if err := runtime.Configure(context.Background(), "session-1", Configuration{
		Host: "127.0.0.1", Port: uint16(peerPort), Transport: TransportUDP,
		Username: "3100", Password: "replacement-secret",
	}); err != nil {
		t.Fatal(err)
	}
	writeSIPResponse(t, peer, gateway, request, 200, "OK", "register-replaced")

	select {
	case err := <-registerResult:
		if !errors.Is(err, errSIPSessionChanged) {
			t.Fatalf("Register error=%v, want %v", err, errSIPSessionChanged)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Register did not finish")
	}
	if _, _, err := runtime.registeredSession("session-1"); !errors.Is(err, ErrRegistrationNotReady) {
		t.Fatalf("replacement registration state error=%v", err)
	}
	assertNoRuntimeEvent(t, runtime, EventRegistered, 100*time.Millisecond)
}

func TestFirstRegistrationRefreshFailureIsAuthoritative(t *testing.T) {
	runtime, peer := newOutboundSIPTestRuntime(t)
	session, err := runtime.session("session-1")
	if err != nil {
		t.Fatal(err)
	}
	runtime.startRegistrationLoop("session-1", session, 5*time.Millisecond, false)
	request, gateway := readSIPRequestFromContaining(t, peer, "REGISTER ")
	writeSIPResponse(t, peer, gateway, request, 500, "Server Error", "refresh-failed")

	event := readRuntimeEvent(t, runtime, EventRegistrationFailed)
	if event.SessionID != "session-1" || event.Reason != "refresh_failed" {
		t.Fatalf("registration event=%+v", event)
	}
	if _, _, err := runtime.registeredSession("session-1"); !errors.Is(err, ErrRegistrationNotReady) {
		t.Fatalf("registration state error=%v, want %v", err, ErrRegistrationNotReady)
	}
	err = runtime.StartCall(
		context.Background(), "session-1", "call-after-refresh-failure", "9001", []byte(testInboundSDP),
	)
	if !errors.Is(err, ErrRegistrationNotReady) {
		t.Fatalf("StartCall error=%v, want %v", err, ErrRegistrationNotReady)
	}
}

func TestRegistrationRefreshFailureIsIsolatedAcrossSessions(t *testing.T) {
	runtime, peer, _, _, peerPort := newInboundSIPTestRuntime(t)
	if err := runtime.Configure(context.Background(), "session-2", Configuration{
		Host: "127.0.0.1", Port: uint16(peerPort), Transport: TransportUDP,
		Username: "3100", Password: "second-secret",
	}); err != nil {
		t.Fatal(err)
	}
	runtime.setRegistered("session-2", true)
	first, err := runtime.session("session-1")
	if err != nil {
		t.Fatal(err)
	}
	runtime.startRegistrationLoop("session-1", first, 5*time.Millisecond, false)
	request, gateway := readSIPRequestFromContaining(t, peer, "REGISTER ")
	writeSIPResponse(t, peer, gateway, request, 500, "Server Error", "session-one-refresh-failed")
	_ = readRuntimeEvent(t, runtime, EventRegistrationFailed)

	if _, _, err := runtime.registeredSession("session-2"); err != nil {
		t.Fatalf("session-1 refresh failure changed session-2 registration: %v", err)
	}
}

func TestCloseSessionDoesNotDeleteConcurrentReplacement(t *testing.T) {
	runtime, peer, _, _, peerPort := newInboundSIPTestRuntime(t)
	closeResult := make(chan error, 1)
	go func() {
		closeResult <- runtime.CloseSession(context.Background(), "session-1")
	}()
	request, gateway := readSIPRequestFromContaining(t, peer, "REGISTER ")
	if err := runtime.Configure(context.Background(), "session-1", Configuration{
		Host: "127.0.0.1", Port: uint16(peerPort), Transport: TransportUDP,
		Username: "3100", Password: "replacement-secret",
	}); err != nil {
		t.Fatal(err)
	}
	writeSIPResponse(t, peer, gateway, request, 200, "OK", "unregister-replaced")

	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CloseSession did not finish")
	}
	session, err := runtime.session("session-1")
	if err != nil {
		t.Fatalf("replacement session was deleted: %v", err)
	}
	if session.configuration.Username != "3100" {
		t.Fatalf("replacement username=%q", session.configuration.Username)
	}
}

func TestInboundInviteRejectsUnsupportedRequiredExtensionBeforeCreatingCall(t *testing.T) {
	runtime, peer, serverUDP, serverPort, peerPort := newInboundSIPTestRuntime(t)
	callID := "inbound-required-extension@example.test"
	invite := buildInboundRequest("INVITE", serverPort, peerPort, "from-required", "", callID,
		"z9hG4bK-required", "1 INVITE", testInboundSDP)
	invite = strings.Replace(invite, "Content-Length:", "Require: 100rel, timer\r\nContent-Length:", 1)
	if _, err := peer.WriteTo([]byte(invite), serverUDP); err != nil {
		t.Fatal(err)
	}
	response := readSIPResponseContaining(t, peer, "420 Bad Extension")
	unsupported := strings.ToLower(headerValue(response, "Unsupported"))
	if !strings.Contains(unsupported, "100rel") || !strings.Contains(unsupported, "timer") {
		t.Fatalf("Unsupported header=%q, want 100rel and timer", unsupported)
	}
	assertNoRuntimeEvent(t, runtime, EventIncoming, 100*time.Millisecond)
	runtime.mu.RLock()
	_, retained := runtime.sipCallID[callID]
	runtime.mu.RUnlock()
	if retained {
		t.Fatal("unsupported INVITE retained a SIP Call-ID mapping")
	}
}

func TestInboundInviteWithSameUsernameRoutesByRegisteredPBXPeer(t *testing.T) {
	udpAddress := reservePacketAddress(t)
	runtime, err := NewSIPGoRuntime(SIPGoConfig{
		ListenUDP: udpAddress, ListenTCP: reserveTCPAddress(t), AdvertisedIP: "127.0.0.1",
		EventBuffer: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	firstPeer, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstPeer.Close() })
	secondPeer, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondPeer.Close() })
	for _, session := range []struct {
		id   string
		peer net.PacketConn
	}{
		{id: "session-1", peer: firstPeer},
		{id: "session-2", peer: secondPeer},
	} {
		if err := runtime.Configure(context.Background(), session.id, Configuration{
			Host: "127.0.0.1", Port: uint16(session.peer.LocalAddr().(*net.UDPAddr).Port),
			Transport: TransportUDP, Username: "1129", Password: "secret",
		}); err != nil {
			t.Fatal(err)
		}
		runtime.setRegistered(session.id, true)
	}
	serverUDP, err := net.ResolveUDPAddr("udp", udpAddress)
	if err != nil {
		t.Fatal(err)
	}
	serverPort := mustPort(t, udpAddress)
	secondPort := secondPeer.LocalAddr().(*net.UDPAddr).Port
	invite := buildInboundRequest(
		"INVITE", serverPort, secondPort, "from-branch-two", "",
		"same-extension-branch-two@example.test", "z9hG4bK-branch-two", "1 INVITE", testInboundSDP,
	)
	if _, err := secondPeer.WriteTo([]byte(invite), serverUDP); err != nil {
		t.Fatal(err)
	}
	_ = readSIPResponseContaining(t, secondPeer, "180 Ringing")
	incoming := readRuntimeEvent(t, runtime, EventIncoming)
	if incoming.SessionID != "session-2" {
		t.Fatalf("inbound same-username call routed to %q, want session-2", incoming.SessionID)
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

func TestEstablishedInboundCallAcceptsCompatibleReinviteWithoutCreatingAnotherCall(t *testing.T) {
	runtime, peer, serverUDP, serverPort, peerPort := newInboundSIPTestRuntime(t)
	callID := "inbound-reinvite@example.test"
	fromTag := "from-reinvite"
	incoming, toValue := establishInboundTestCall(
		t, runtime, peer, serverUDP, serverPort, peerPort, callID, fromTag,
	)
	updatedSDP := strings.Replace(testInboundSDP, "m=audio 20000", "m=audio 21000", 1)
	reinvite := buildInboundRequest("INVITE", serverPort, peerPort, fromTag, toValue, callID,
		"z9hG4bK-reinvite", "2 INVITE", updatedSDP)
	if _, err := peer.WriteTo([]byte(reinvite), serverUDP); err != nil {
		t.Fatal(err)
	}
	response := readSIPResponseContaining(t, peer, "200 OK")
	if cseq := headerValue(response, "CSeq"); cseq != "2 INVITE" {
		t.Fatalf("re-INVITE response CSeq=%q", cseq)
	}
	mediaUpdated := readRuntimeEvent(t, runtime, EventMediaUpdated)
	if mediaUpdated.CallID != incoming.CallID || string(mediaUpdated.RemoteSDP) != updatedSDP {
		t.Fatalf("media update event=%+v", mediaUpdated)
	}
	ack := buildInboundRequest("ACK", serverPort, peerPort, fromTag, toValue, callID,
		"z9hG4bK-reinvite-ack", "2 ACK", "")
	if _, err := peer.WriteTo([]byte(ack), serverUDP); err != nil {
		t.Fatal(err)
	}
	assertNoRuntimeEvent(t, runtime, EventIncoming, 100*time.Millisecond)
	runtime.mu.RLock()
	callCount := len(runtime.calls)
	runtime.mu.RUnlock()
	if callCount != 1 {
		t.Fatalf("re-INVITE changed active call count to %d", callCount)
	}
	endInboundTestCall(t, runtime, peer, serverUDP, serverPort, peerPort,
		incoming, callID, fromTag, toValue, 3)
}

func TestEstablishedInboundCallAcceptsCompatibleUpdateAndRejectsCodecChange(t *testing.T) {
	runtime, peer, serverUDP, serverPort, peerPort := newInboundSIPTestRuntime(t)
	callID := "inbound-update@example.test"
	fromTag := "from-update"
	incoming, toValue := establishInboundTestCall(
		t, runtime, peer, serverUDP, serverPort, peerPort, callID, fromTag,
	)
	updatedSDP := strings.Replace(testInboundSDP, "m=audio 20000", "m=audio 22000", 1)
	update := buildInboundRequest("UPDATE", serverPort, peerPort, fromTag, toValue, callID,
		"z9hG4bK-update", "2 UPDATE", updatedSDP)
	if _, err := peer.WriteTo([]byte(update), serverUDP); err != nil {
		t.Fatal(err)
	}
	response := readSIPResponseContaining(t, peer, "200 OK")
	if cseq := headerValue(response, "CSeq"); cseq != "2 UPDATE" {
		t.Fatalf("UPDATE response CSeq=%q", cseq)
	}
	mediaUpdated := readRuntimeEvent(t, runtime, EventMediaUpdated)
	if mediaUpdated.CallID != incoming.CallID || string(mediaUpdated.RemoteSDP) != updatedSDP {
		t.Fatalf("media update event=%+v", mediaUpdated)
	}

	unsupportedSDP := strings.ReplaceAll(testInboundSDP,
		"m=audio 20000 RTP/AVP 0 101", "m=audio 23000 RTP/AVP 8 101")
	unsupportedSDP = strings.ReplaceAll(unsupportedSDP,
		"a=rtpmap:0 PCMU/8000", "a=rtpmap:8 PCMA/8000")
	update = buildInboundRequest("UPDATE", serverPort, peerPort, fromTag, toValue, callID,
		"z9hG4bK-update-codec", "3 UPDATE", unsupportedSDP)
	if _, err := peer.WriteTo([]byte(update), serverUDP); err != nil {
		t.Fatal(err)
	}
	response = readSIPResponseContaining(t, peer, "488 Not Acceptable Here")
	if cseq := headerValue(response, "CSeq"); cseq != "3 UPDATE" {
		t.Fatalf("codec rejection CSeq=%q", cseq)
	}
	assertNoRuntimeEvent(t, runtime, EventMediaUpdated, 100*time.Millisecond)
	endInboundTestCall(t, runtime, peer, serverUDP, serverPort, peerPort,
		incoming, callID, fromTag, toValue, 4)
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

func TestInboundTCPSeparatesSameUsernameByOpaqueContactAndPBXPeer(t *testing.T) {
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
	firstPeer, err := net.DialTimeout("tcp", tcpAddress, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstPeer.Close() })
	secondPeer, err := net.DialTimeout("tcp", tcpAddress, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondPeer.Close() })
	for _, session := range []struct {
		id   string
		peer net.Conn
	}{
		{id: "session-1", peer: firstPeer},
		{id: "session-2", peer: secondPeer},
	} {
		if err := runtime.Configure(context.Background(), session.id, Configuration{
			Host: "127.0.0.1", Port: uint16(session.peer.LocalAddr().(*net.TCPAddr).Port),
			Transport: TransportTCP, Username: "1129", Password: "secret",
		}); err != nil {
			t.Fatal(err)
		}
		runtime.setRegistered(session.id, true)
	}
	serverPort := mustPort(t, tcpAddress)
	secondPort := secondPeer.LocalAddr().(*net.TCPAddr).Port
	rewrittenInvite := strings.ReplaceAll(buildInboundRequest(
		"INVITE", serverPort, secondPort, "from-tcp-peer", "",
		"tcp-peer-routing@example.test", "z9hG4bK-tcp-peer", "1 INVITE", testInboundSDP,
	), "SIP/2.0/UDP", "SIP/2.0/TCP")
	if _, err := io.WriteString(secondPeer, rewrittenInvite); err != nil {
		t.Fatal(err)
	}
	secondReader := bufio.NewReader(secondPeer)
	_ = readTCPResponseContaining(t, secondPeer, secondReader, "180 Ringing")
	if incoming := readRuntimeEvent(t, runtime, EventIncoming); incoming.SessionID != "session-2" {
		t.Fatalf("rewritten Contact routed to %q, want session-2 by PBX peer", incoming.SessionID)
	}

	runtime.mu.RLock()
	firstContact := runtime.sessions["session-1"].contactUser
	runtime.mu.RUnlock()
	firstPort := firstPeer.LocalAddr().(*net.TCPAddr).Port
	opaqueInvite := strings.ReplaceAll(buildInboundRequest(
		"INVITE", serverPort, firstPort, "from-tcp-contact", "",
		"tcp-contact-routing@example.test", "z9hG4bK-tcp-contact", "1 INVITE", testInboundSDP,
	), "SIP/2.0/UDP", "SIP/2.0/TCP")
	opaqueInvite = strings.ReplaceAll(opaqueInvite, "sip:1129@", "sip:"+firstContact+"@")
	if _, err := io.WriteString(firstPeer, opaqueInvite); err != nil {
		t.Fatal(err)
	}
	firstReader := bufio.NewReader(firstPeer)
	_ = readTCPResponseContaining(t, firstPeer, firstReader, "180 Ringing")
	if incoming := readRuntimeEvent(t, runtime, EventIncoming); incoming.SessionID != "session-1" {
		t.Fatalf("opaque Contact routed to %q, want session-1", incoming.SessionID)
	}

	ambiguousPeer, err := net.DialTimeout("tcp", tcpAddress, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ambiguousPeer.Close() })
	ambiguousPort := ambiguousPeer.LocalAddr().(*net.TCPAddr).Port
	ambiguousInvite := strings.ReplaceAll(buildInboundRequest(
		"INVITE", serverPort, ambiguousPort, "from-tcp-ambiguous", "",
		"tcp-ambiguous@example.test", "z9hG4bK-tcp-ambiguous", "1 INVITE", testInboundSDP,
	), "SIP/2.0/UDP", "SIP/2.0/TCP")
	if _, err := io.WriteString(ambiguousPeer, ambiguousInvite); err != nil {
		t.Fatal(err)
	}
	ambiguousReader := bufio.NewReader(ambiguousPeer)
	_ = readTCPResponseContaining(t, ambiguousPeer, ambiguousReader, "480 Temporarily Unavailable")
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

func establishInboundTestCall(
	t *testing.T,
	runtime *SIPGoRuntime,
	peer net.PacketConn,
	serverUDP *net.UDPAddr,
	serverPort, peerPort int,
	callID, fromTag string,
) (Event, string) {
	t.Helper()
	invite := buildInboundRequest("INVITE", serverPort, peerPort, fromTag, "", callID,
		"z9hG4bK-"+fromTag+"-invite", "1 INVITE", testInboundSDP)
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
	ack := buildInboundRequest("ACK", serverPort, peerPort, fromTag, toValue, callID,
		"z9hG4bK-"+fromTag+"-ack", "1 ACK", "")
	if _, err := peer.WriteTo([]byte(ack), serverUDP); err != nil {
		t.Fatal(err)
	}
	connected := readRuntimeEvent(t, runtime, EventConnected)
	if connected.CallID != incoming.CallID {
		t.Fatalf("connected event=%+v", connected)
	}
	return incoming, toValue
}

func endInboundTestCall(
	t *testing.T,
	runtime *SIPGoRuntime,
	peer net.PacketConn,
	serverUDP *net.UDPAddr,
	serverPort, peerPort int,
	incoming Event,
	callID, fromTag, toValue string,
	cseq uint32,
) {
	t.Helper()
	bye := buildInboundRequest("BYE", serverPort, peerPort, fromTag, toValue, callID,
		"z9hG4bK-"+fromTag+"-bye", fmt.Sprintf("%d BYE", cseq), "")
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

func sipTagFromHeader(value string) string {
	for _, parameter := range strings.Split(value, ";")[1:] {
		name, tag, found := strings.Cut(strings.TrimSpace(parameter), "=")
		if found && strings.EqualFold(name, "tag") {
			return strings.TrimSpace(tag)
		}
	}
	return ""
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

func assertNoRuntimeEvent(
	t *testing.T, runtime *SIPGoRuntime, eventType EventType, duration time.Duration,
) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		select {
		case event := <-runtime.Events():
			if event.Type == eventType {
				t.Fatalf("unexpected runtime event %+v", event)
			}
		case <-timer.C:
			return
		}
	}
}

func assertNoSIPPacket(t *testing.T, peer net.PacketConn, duration time.Duration) {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(duration)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 4096)
	count, _, err := peer.ReadFrom(buffer)
	if err == nil {
		t.Fatalf("unexpected SIP packet: %s", string(buffer[:count]))
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("read SIP packet: %v", err)
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

func writeSIPAuthChallenge(
	t *testing.T,
	peer net.PacketConn,
	target net.Addr,
	request string,
	status int,
	reason string,
	headerName string,
	challenge string,
) {
	t.Helper()
	response := buildResponseForRequest(request, status, reason)
	response = strings.Replace(response, "Content-Length: 0\r\n",
		headerName+": "+challenge+"\r\nContent-Length: 0\r\n", 1)
	if _, err := peer.WriteTo([]byte(response), target); err != nil {
		t.Fatal(err)
	}
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
	writeSIPResponseWithHeadersAndBody(t, peer, target, request, status, reason, toTag, nil, body)
}

func writeSIPResponseWithHeadersAndBody(
	t *testing.T,
	peer net.PacketConn,
	target net.Addr,
	request string,
	status int,
	reason string,
	toTag string,
	headers []string,
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
	for _, header := range headers {
		extraHeaders += header + "\r\n"
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

func sipUsernameFromHeader(value string) string {
	lower := strings.ToLower(value)
	start := strings.Index(lower, "sip:")
	if start < 0 {
		return ""
	}
	start += len("sip:")
	end := strings.IndexByte(value[start:], '@')
	if end < 0 {
		return ""
	}
	return value[start : start+end]
}
