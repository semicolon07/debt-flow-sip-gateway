package poc_test

import (
	"os"
	"strings"
	"testing"
)

func TestPOCCoversGatewayProtocolWithoutPersistingSecrets(t *testing.T) {
	html := read(t, "sip-gateway-poc.html")
	script := read(t, "sip-gateway-poc.js")
	if !strings.Contains(html, `src="./sip-gateway-poc.js"`) {
		t.Fatal("POC HTML does not load its controller")
	}
	for _, command := range []string{
		"session.authenticate", "session.renew", "state.get", "events.ack",
		"registration.start", "registration.stop", "webrtc.offer", "webrtc.ice_candidate",
		"call.start", "call.answer", "call.reject", "call.hangup", "call.dtmf",
	} {
		if !strings.Contains(script, command) {
			t.Errorf("POC is missing protocol command %s", command)
		}
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage", "indexedDB"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("POC must not persist short-lived secrets through %s", forbidden)
		}
	}
	for _, required := range []string{
		"sensitiveKey", "[redacted]", "credentials: 'include'", "iceTransportPolicy",
		"getUserMedia", "setSinkId", "getStats", "sourceKind: 'manual'",
		"AES-GCM", "generateStandaloneSessionCapability", "generateStandaloneCallCapability",
		"canonicalStandaloneDestination", "createSilentAudioStream", "createConstantSource", "MediaStreamDestination",
		"Every standalone attempt represents a new call", "waitForWebRTCMedia", "WebRTC media negotiation completed",
		"do not block SDP offer creation on the audio hardware state", "void context.resume()",
		"No microphone detected; switched to Silent audio automatically", "negotiatePeer",
		"Inbound ringtone started", "Outbound ringback started", "enableCallTones",
		"state.peer.connectionState !== 'connected'", "registration.stop",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("POC is missing required safety or test behavior %s", required)
		}
	}
	for _, field := range []string{
		`id="sipHost"`, `id="sipPort"`, `id="sipTransport"`, `id="sipRealm"`,
		`id="sipUsername"`, `id="sipPassword"`, `id="standaloneKey"`, `id="useSilentAudio"`,
		`id="enableCallTones"`, `id="answer" disabled`, `id="reject" disabled`,
		`id="remoteParty"`,
	} {
		if !strings.Contains(html, field) {
			t.Errorf("POC is missing standalone field %s", field)
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
