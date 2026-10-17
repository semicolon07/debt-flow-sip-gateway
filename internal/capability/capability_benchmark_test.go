package capability

import (
	"testing"
	"time"
)

func BenchmarkVerifySessionCapability(b *testing.B) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	codec, err := New("gateway-1", "issuer", "audience", map[string][]byte{
		"key-1": []byte("01234567890123456789012345678901"),
	})
	if err != nil {
		b.Fatal(err)
	}
	defer codec.Close()
	codec.now = func() time.Time { return now }
	token, err := codec.Seal("key-1", SessionClaims{
		Version: 1, Type: SessionType, Issuer: "issuer", Audience: "audience",
		GatewayID: "gateway-1", CapabilityID: "capability-1",
		IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(),
		ExpiresAt: now.Add(time.Minute).Unix(), LeaseExpiresAt: now.Add(2 * time.Minute).Unix(),
		SessionID: "session-1", UserSessionID: "user-session-1", CollectionID: "collection-1",
		CollectionBindingID: "phonebind-1", AgentInstanceID: "agent-1", ProtocolVersion: 1,
		SIP: SIPBootstrap{Host: "pbx.internal", Port: 5060, Transport: "udp", Username: "1001", Password: "secret"},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(token)))
	b.ResetTimer()
	for range b.N {
		if _, err := codec.VerifySession(token, "collection-1"); err != nil {
			b.Fatal(err)
		}
	}
}
