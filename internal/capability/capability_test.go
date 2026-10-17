package capability

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/debt-flow/sip-gateway/internal/config"
)

type unusedProtector struct{}

func (unusedProtector) Protect([]byte) ([]byte, error)   { panic("unexpected call") }
func (unusedProtector) Unprotect([]byte) ([]byte, error) { panic("unexpected call") }

type recordingProtector struct{ plaintext []byte }

// These immutable vectors were emitted by the .NET
// AesGcmPhoneGatewayCapabilityIssuer using the fixed test key and clock below.
// They guard the cross-language envelope, AAD and JSON claim contract.
const dotNetSessionCapabilityVector = "dfcap1.a2V5LTE.J9wC5WlZAiWNKI4g.EBT_lsU7zJ_1f7V7Frmos-OfhYaww9eQvE-bP2uc4_V4_DsYxNpenX-RKvxk7anDTKsqhCDf0YsneSySnKDJJwbqsBY75chjj0SRkp6PNBHXxktfB1dSs83syqRD7ugfjXRxZR1tpZnHEyoe3O7AxsMAXvVZh-DEcAHvf_O18ADJ9H7sjIdKDCkfaImg4ZbiHCWvKq0lu_ZDf0YFvpgEnjJE8MlnjF4axPqJaqoeK0Yth87N-oVTf80rdgv6EV4dJkCeBnzQc_uwLJpM93i3l1ykjjbkkzjgnaVOZPxEytjajv34WKY0sTfGZHEqH9RXx7864AHSRu_NYtJFmKshj3vFmVGI64ZqXNdnr8bQDuxLMzI78L9GSeL7bEU6Jj-UW5w5DdLZaOFtlGnVNtL-sGZxfmJEnbEjfeRyAhW4rEqZ-BH22YKIIGDk2teaD5v9KruG8LCKDLeCkd2ar5ZK39e2l-b7vNMo3wRVj5tO7W1dFesRXprONtebVO2AT-upFfx46LFWla9mWQ1VhyrjbhNZ5aN5qxXHNevVe3FyYBC0JuT699hkZzRAF0Nm19Us0LKMDaCB1cxM8RTiohkgraZgnHWF7SmO0AMfRD-BBCtELgNS0oBZkBh31EP1P_bPSti0g6m40EWEw11xraiQA3LDbzZ4PnYFMHuqxz9tEH8RDf_X7g86Qe7OOGZd3v8TdmFkmdN-0NFnhrlhxeirdVIAh6AGePkUjTiIv3wuUCy1.DEaM3JpLnH2wEzwtqISK4A"

const dotNetCallCapabilityVector = "dfcap1.a2V5LTE.6hyX5H0WibVe6kEv.eiWsGXOwhm6M8bmZI4Itwr79xEjGsxKMHx0MyQ5b3eL_LXu7pIZZlm-nLU0ZM2tI0D7N-xIlQ1bNlnndBqCQbQUuBe_q7DuKojtlW7CZE8CtVW-RbTKsaomEqh4p8jc6Toyhgf6vUBR9Ds4vQY8lFUMuiS8_IWavV0xUNF85qZoea-cTFuE7KffvjopzFQb0u4BBzwd7kR7Ib-IpvdxP5WgWxclpRHRlT83ull7XFDJzsisA9eFUmH6TjwelaGVTb5J6ROeU1NCgHU2DRUuPD4WvUokvoEXQrZH9WpFlR2oSJJNjBgX1VR2Sncx9tcMQCRn5KW6LFLAz84vHh4hvZOI7AKu72oEI8Qd1cgQJFD5JZhuqWsLbgtv0tMm5HxERM6pPLIcv9MOUBjbb-dEeDKeMeybfFYXs_tR3FM7NkTXbKX5B_rc6a5ie3DMIooVeMo2Kj3liA8BTLFJty4_n-N1g3EaecTUbuHJaaPLwYsFOW6M1sByTDoZzC1LiYRPdXw2K6NNMDbSxb4fkbSdLcJuXPshf-rom2M5tKPXKyX4HNGA7IeKYUPQSDEkyQP9MrQTKr8-pMvUhuY5-4QEDeI_8pkZs.9cj1QaSjluPoMtKgEbdchg"

func (*recordingProtector) Protect([]byte) ([]byte, error) { panic("unexpected call") }
func (p *recordingProtector) Unprotect([]byte) ([]byte, error) {
	return p.plaintext, nil
}

func testCodec(t *testing.T) (*Codec, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	codec, err := New("gateway-1", "debt-flow-web-api", "debt-flow-sip-gateway",
		map[string][]byte{"key-1": []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	codec.now = func() time.Time { return now }
	t.Cleanup(codec.Close)
	return codec, now
}

func TestSessionCapabilityRoundTrip(t *testing.T) {
	codec, now := testCodec(t)
	claims := SessionClaims{
		Version: 1, Type: SessionType, Issuer: "debt-flow-web-api", Audience: "debt-flow-sip-gateway",
		GatewayID: "gateway-1", CapabilityID: "cap-1", IssuedAt: now.Unix(), NotBefore: now.Unix(),
		ExpiresAt: now.Add(time.Minute).Unix(), LeaseExpiresAt: now.Add(2 * time.Minute).Unix(),
		SessionID: "session-1", UserSessionID: "user-session-1", CollectionID: "collection-1",
		CollectionBindingID: "phonebind-1", AgentInstanceID: "agent-1", ProtocolVersion: 1,
		SIP: SIPBootstrap{Host: "pbx.internal", Port: 5060, Transport: "udp", Username: "100", Password: "secret"},
	}
	token, err := codec.Seal("key-1", claims)
	if err != nil {
		t.Fatal(err)
	}
	got, err := codec.VerifySession(token, "collection-1")
	if err != nil || got.SIP.Password != "secret" || got.CapabilityID != "cap-1" {
		t.Fatalf("verify: %+v %v", got, err)
	}
	parts := strings.Split(token, ".")
	replacement := "A"
	if parts[3][0] == 'A' {
		replacement = "B"
	}
	parts[3] = replacement + parts[3][1:]
	if _, err := codec.VerifySession(strings.Join(parts, "."), "collection-1"); err == nil {
		t.Fatal("tampered capability was accepted")
	}
}

func TestVerifyDotNetCapabilityVectors(t *testing.T) {
	codec, _ := testCodec(t)
	session, err := codec.VerifySession(dotNetSessionCapabilityVector, "col-1")
	if err != nil {
		t.Fatalf("verify .NET session vector: %v", err)
	}
	if session.UserSessionID != "portal-session" || session.SessionID != "00000000-0000-0000-0000-000000000001" ||
		session.SIP.Password != "secret" {
		t.Fatalf("unexpected .NET session claims: %+v", session)
	}
	call, err := codec.VerifyCall(dotNetCallCapabilityVector, CallBinding{
		SessionID: "00000000-0000-0000-0000-000000000001", UserSessionID: "portal-session",
		CollectionID: "col-1", CollectionBindingID: "phonebind_1",
		CallID: "00000000-0000-0000-0000-000000000003", CallContextID: "phonectx_1",
	})
	if err != nil {
		t.Fatalf("verify .NET call vector: %v", err)
	}
	if call.Destination != "0812345678" {
		t.Fatalf("unexpected .NET call destination %q", call.Destination)
	}
}

func TestSessionCapabilityNormalizesTransport(t *testing.T) {
	codec, now := testCodec(t)
	claims := SessionClaims{
		Version: 1, Type: SessionType, Issuer: "debt-flow-web-api", Audience: "debt-flow-sip-gateway",
		GatewayID: "gateway-1", CapabilityID: "cap-1", IssuedAt: now.Unix(), NotBefore: now.Unix(),
		ExpiresAt: now.Add(time.Minute).Unix(), LeaseExpiresAt: now.Add(2 * time.Minute).Unix(),
		SessionID: "session-1", UserSessionID: "user-session-1", CollectionID: "collection-1",
		CollectionBindingID: "phonebind-1", AgentInstanceID: "agent-1", ProtocolVersion: 1,
		SIP: SIPBootstrap{Host: "pbx.internal", Port: 5060, Transport: "TCP", Username: "100", Password: "secret"},
	}
	token, err := codec.Seal("key-1", claims)
	if err != nil {
		t.Fatal(err)
	}
	got, err := codec.VerifySession(token, "collection-1")
	if err != nil || got.SIP.Transport != "tcp" {
		t.Fatalf("transport was not normalized: %q, %v", got.SIP.Transport, err)
	}
}

func TestSessionCapabilityRejectsAuthorityTimeAndProtocolMismatches(t *testing.T) {
	codec, now := testCodec(t)
	valid := func() SessionClaims {
		return SessionClaims{
			Version: 1, Type: SessionType, Issuer: "debt-flow-web-api", Audience: "debt-flow-sip-gateway",
			GatewayID: "gateway-1", CapabilityID: "cap-1", IssuedAt: now.Unix(), NotBefore: now.Unix(),
			ExpiresAt: now.Add(time.Minute).Unix(), LeaseExpiresAt: now.Add(2 * time.Minute).Unix(),
			SessionID: "session-1", UserSessionID: "user-session-1", CollectionID: "collection-1",
			CollectionBindingID: "phonebind-1", AgentInstanceID: "agent-1", ProtocolVersion: 1,
			SIP: SIPBootstrap{Host: "pbx.internal", Port: 5060, Transport: "udp", Username: "100", Password: "secret"},
		}
	}
	tests := []struct {
		name   string
		mutate func(*SessionClaims)
	}{
		{"version", func(value *SessionClaims) { value.Version = 2 }},
		{"type", func(value *SessionClaims) { value.Type = CallType }},
		{"issuer", func(value *SessionClaims) { value.Issuer = "attacker" }},
		{"audience", func(value *SessionClaims) { value.Audience = "other-service" }},
		{"gateway", func(value *SessionClaims) { value.GatewayID = "other-gateway" }},
		{"protocol", func(value *SessionClaims) { value.ProtocolVersion = 2 }},
		{"not-before", func(value *SessionClaims) { value.NotBefore = now.Add(clockSkew + time.Second).Unix() }},
		{"capability-ttl", func(value *SessionClaims) { value.ExpiresAt = now.Add(maxCapabilityTTL + time.Second).Unix() }},
		{"lease-ttl", func(value *SessionClaims) { value.LeaseExpiresAt = now.Add(maxSessionLeaseTTL + time.Second).Unix() }},
		{"sip-transport", func(value *SessionClaims) { value.SIP.Transport = "tls" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := valid()
			test.mutate(&claims)
			token, err := codec.Seal("key-1", claims)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := codec.VerifySession(token, "collection-1"); err == nil {
				t.Fatalf("session capability with mismatched %s was accepted", test.name)
			}
		})
	}
	claims := valid()
	token, err := codec.Seal("key-1", claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.VerifySession(token, "other-collection"); err == nil {
		t.Fatal("session capability for another collection was accepted")
	}
}

func TestCapabilityKeyRotationAcceptsOverlapAndRejectsRemovedKey(t *testing.T) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	oldKey := []byte("old-key-0123456789abcdef01234567")
	newKey := []byte("new-key-0123456789abcdef01234567")
	overlap, err := New("gateway-1", "debt-flow-web-api", "debt-flow-sip-gateway",
		map[string][]byte{"old": oldKey, "new": newKey})
	if err != nil {
		t.Fatal(err)
	}
	overlap.now = func() time.Time { return now }
	t.Cleanup(overlap.Close)
	claims := SessionClaims{
		Version: 1, Type: SessionType, Issuer: "debt-flow-web-api", Audience: "debt-flow-sip-gateway",
		GatewayID: "gateway-1", CapabilityID: "cap-1", IssuedAt: now.Unix(), NotBefore: now.Unix(),
		ExpiresAt: now.Add(time.Minute).Unix(), LeaseExpiresAt: now.Add(2 * time.Minute).Unix(),
		SessionID: "session-1", UserSessionID: "user-session-1", CollectionID: "collection-1",
		CollectionBindingID: "phonebind-1", AgentInstanceID: "agent-1", ProtocolVersion: 1,
		SIP: SIPBootstrap{Host: "pbx.internal", Port: 5060, Transport: "udp", Username: "100", Password: "secret"},
	}
	oldToken, err := overlap.Seal("old", claims)
	if err != nil {
		t.Fatal(err)
	}
	claims.CapabilityID = "cap-2"
	newToken, err := overlap.Seal("new", claims)
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"old": oldToken, "new": newToken} {
		if _, err := overlap.VerifySession(token, "collection-1"); err != nil {
			t.Fatalf("overlap rejected %s key: %v", name, err)
		}
	}
	rolled, err := New("gateway-1", "debt-flow-web-api", "debt-flow-sip-gateway",
		map[string][]byte{"new": newKey})
	if err != nil {
		t.Fatal(err)
	}
	rolled.now = func() time.Time { return now }
	t.Cleanup(rolled.Close)
	if _, err := rolled.VerifySession(oldToken, "collection-1"); err == nil || !strings.Contains(err.Error(), "unknown capability key") {
		t.Fatalf("removed key was not rejected: %v", err)
	}
	if _, err := rolled.VerifySession(newToken, "collection-1"); err != nil {
		t.Fatalf("active key was rejected after rotation: %v", err)
	}
}

func TestCallCapabilityRequiresExactBinding(t *testing.T) {
	codec, now := testCodec(t)
	claims := CallClaims{
		Version: 1, Type: CallType, Issuer: "debt-flow-web-api", Audience: "debt-flow-sip-gateway",
		GatewayID: "gateway-1", CapabilityID: "call-cap-1", IssuedAt: now.Unix(), NotBefore: now.Unix(),
		ExpiresAt: now.Add(time.Minute).Unix(), SessionID: "session-1", CollectionID: "collection-1",
		UserSessionID:       "user-session-1",
		CollectionBindingID: "phonebind-1", CallID: "call-1", CallContextID: "context-1",
		Destination: "sip:100@pbx.internal",
	}
	token, err := codec.Seal("key-1", claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.VerifyCall(token, CallBinding{SessionID: "other", UserSessionID: "user-session-1", CollectionID: "collection-1",
		CollectionBindingID: "phonebind-1", CallID: "call-1", CallContextID: "context-1"}); err == nil {
		t.Fatal("wrong session binding was accepted")
	}
	if _, err := codec.VerifyCall(token, CallBinding{SessionID: "session-1", UserSessionID: "other-user-session", CollectionID: "collection-1",
		CollectionBindingID: "phonebind-1", CallID: "call-1", CallContextID: "context-1"}); err == nil {
		t.Fatal("wrong user session binding was accepted")
	}
}

func TestExpiredCapabilityIsRejected(t *testing.T) {
	codec, now := testCodec(t)
	claims := CallClaims{
		Version: 1, Type: CallType, Issuer: "debt-flow-web-api", Audience: "debt-flow-sip-gateway",
		GatewayID: "gateway-1", CapabilityID: "call-cap-1", IssuedAt: now.Add(-2 * time.Minute).Unix(),
		NotBefore: now.Add(-2 * time.Minute).Unix(), ExpiresAt: now.Add(-time.Minute).Unix(),
		SessionID: "session-1", UserSessionID: "user-session-1", CollectionID: "collection-1", CollectionBindingID: "phonebind-1",
		CallID: "call-1", CallContextID: "context-1", Destination: "sip:100@pbx.internal",
	}
	token, err := codec.Seal("key-1", claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.VerifyCall(token, CallBinding{SessionID: "session-1", UserSessionID: "user-session-1", CollectionID: "collection-1",
		CollectionBindingID: "phonebind-1", CallID: "call-1", CallContextID: "context-1"}); err == nil {
		t.Fatal("expired capability was accepted")
	}
}

func TestLoadKeysRejectsDuplicateKeyIDAcrossStores(t *testing.T) {
	key := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	_, err := LoadKeys(config.Capability{
		ProtectedKeys: map[string]string{"key-1": "not-read-because-load-fails-first"},
		PlaintextKeys: map[string]string{"key-1": key}, AllowPlaintextKeys: true,
	}, unusedProtector{})
	if err == nil || !strings.Contains(err.Error(), "configured more than once") {
		t.Fatalf("expected duplicate key ID error, got %v", err)
	}
}

func TestLoadKeysZeroesAlreadyLoadedKeysWhenLaterKeyFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "protected-key.dpapi")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString([]byte("ciphertext"))), 0o600); err != nil {
		t.Fatal(err)
	}
	protector := &recordingProtector{plaintext: []byte("0123456789abcdef0123456789abcdef")}
	_, err := LoadKeys(config.Capability{
		ProtectedKeys: map[string]string{"protected": path},
		PlaintextKeys: map[string]string{"invalid": "not-base64"}, AllowPlaintextKeys: true,
	}, protector)
	if err == nil {
		t.Fatal("invalid later key was accepted")
	}
	for index, value := range protector.plaintext {
		if value != 0 {
			t.Fatalf("loaded key byte %d was not zeroed after failure", index)
		}
	}
}
