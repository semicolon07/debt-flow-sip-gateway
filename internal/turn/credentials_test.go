package turn

import (
	"strings"
	"testing"
	"time"
)

func TestCredentialExpiresAndCanBeRevoked(t *testing.T) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	credentials, err := NewCredentials(2 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := credentials.Issue("session-1", now.Add(2*time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	userID, password, ok := credentials.Authenticate(issued.Username, now.Add(time.Minute))
	if !ok || userID != "session-1" || password != issued.Password {
		t.Fatalf("unexpected authentication: user=%q ok=%v", userID, ok)
	}
	if strings.Contains(issued.Username, "session-1") {
		t.Fatalf("TURN username leaks the session ID: %q", issued.Username)
	}
	if _, _, ok := credentials.Authenticate(issued.Username, issued.ExpiresAt); ok {
		t.Fatal("credential must expire at its deadline")
	}
	second, _ := credentials.Issue("session-1", now.Add(2*time.Minute), now)
	credentials.RevokeSession("session-1")
	if _, _, ok := credentials.Authenticate(second.Username, now); ok {
		t.Fatal("revoked session credential was accepted")
	}
}

func TestCredentialExtendsWithRenewedSessionLease(t *testing.T) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	credentials, err := NewCredentials(2 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := credentials.Issue("session-1", now.Add(90*time.Second), now)
	if err != nil {
		t.Fatal(err)
	}
	renewedAt := now.Add(time.Minute)
	extended, err := credentials.ExtendSession("session-1", renewedAt.Add(2*time.Minute), renewedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !extended.Equal(renewedAt.Add(2 * time.Minute)) {
		t.Fatalf("unexpected extended expiry: %v", extended)
	}
	if _, _, ok := credentials.Authenticate(issued.Username, issued.ExpiresAt); !ok {
		t.Fatal("existing TURN credential did not survive session renewal")
	}
	if _, _, ok := credentials.Authenticate(issued.Username, extended); ok {
		t.Fatal("extended credential did not expire at the renewed lease bound")
	}
}

func TestCredentialCannotOutliveConfiguredTTL(t *testing.T) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	credentials, _ := NewCredentials(time.Minute)
	issued, err := credentials.Issue("session-1", now.Add(time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if !issued.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("credential exceeded configured TTL: %v", issued.ExpiresAt)
	}
}

func TestClosedCredentialsRejectFurtherUse(t *testing.T) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	credentials, _ := NewCredentials(time.Minute)
	issued, err := credentials.Issue("session-1", now.Add(time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	credentials.Close()
	if _, _, ok := credentials.Authenticate(issued.Username, now); ok {
		t.Fatal("closed credentials authenticated a user")
	}
	if _, err := credentials.Issue("session-2", now.Add(time.Minute), now); err == nil {
		t.Fatal("closed credentials issued a new credential")
	}
	if _, err := credentials.ExtendSession("session-1", now.Add(time.Minute), now); err == nil {
		t.Fatal("closed credentials extended a session")
	}
}

func TestSessionReconnectRetainsOnlyTheCurrentCredential(t *testing.T) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	credentials, _ := NewCredentials(time.Minute)
	first, _ := credentials.Issue("session-1", now.Add(time.Minute), now)
	second, _ := credentials.Issue("session-1", now.Add(time.Minute), now)
	credentials.RetainSessionCredential("session-1", second.Username)
	if _, _, ok := credentials.Authenticate(first.Username, now); ok {
		t.Fatal("superseded TURN credential remained valid")
	}
	if _, _, ok := credentials.Authenticate(second.Username, now); !ok {
		t.Fatal("current TURN credential was revoked")
	}
	credentials.Revoke(second.Username)
	if _, _, ok := credentials.Authenticate(second.Username, now); ok {
		t.Fatal("explicitly revoked TURN credential remained valid")
	}
}
