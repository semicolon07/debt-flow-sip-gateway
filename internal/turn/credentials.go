package turn

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

type Credential struct {
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	ExpiresAt time.Time `json:"expiresAtUtc"`
}

type credentialRecord struct {
	sessionID string
	expiresAt time.Time
}

type Credentials struct {
	mu      sync.RWMutex
	secret  []byte
	ttl     time.Duration
	records map[string]credentialRecord
	closed  bool
}

func NewCredentials(ttl time.Duration) (*Credentials, error) {
	if ttl <= 0 {
		return nil, errors.New("TURN credential TTL must be positive")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return &Credentials{secret: secret, ttl: ttl, records: make(map[string]credentialRecord)}, nil
}

func (c *Credentials) Issue(sessionID string, leaseExpiresAt, now time.Time) (Credential, error) {
	if sessionID == "" {
		return Credential{}, errors.New("sessionID is required")
	}
	expiresAt, err := c.boundedExpiry(leaseExpiresAt, now)
	if err != nil {
		return Credential{}, err
	}
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return Credential{}, err
	}
	username := "dfturn1:" + base64.RawURLEncoding.EncodeToString(random)
	clear(random)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Credential{}, errors.New("TURN credentials are closed")
	}
	password := c.passwordLocked(username)
	c.records[username] = credentialRecord{sessionID: sessionID, expiresAt: expiresAt}
	c.pruneLocked(now)
	c.mu.Unlock()
	return Credential{Username: username, Password: password, ExpiresAt: expiresAt}, nil
}

func (c *Credentials) ExtendSession(sessionID string, leaseExpiresAt, now time.Time) (time.Time, error) {
	expiresAt, err := c.boundedExpiry(leaseExpiresAt, now)
	if err != nil {
		return time.Time{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return time.Time{}, errors.New("TURN credentials are closed")
	}
	for username, record := range c.records {
		if record.sessionID == sessionID {
			record.expiresAt = expiresAt
			c.records[username] = record
		}
	}
	c.pruneLocked(now)
	return expiresAt, nil
}

func (c *Credentials) Authenticate(username string, now time.Time) (userID, password string, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return "", "", false
	}
	record, found := c.records[username]
	if !found || !now.Before(record.expiresAt) {
		return "", "", false
	}
	return record.sessionID, c.passwordLocked(username), true
}

func (c *Credentials) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.secret)
	clear(c.records)
	c.closed = true
}

func (c *Credentials) RevokeSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for username, record := range c.records {
		if record.sessionID == sessionID {
			delete(c.records, username)
		}
	}
}

func (c *Credentials) Revoke(username string) {
	c.mu.Lock()
	delete(c.records, username)
	c.mu.Unlock()
}

func (c *Credentials) RetainSessionCredential(sessionID, username string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for current, record := range c.records {
		if record.sessionID == sessionID && current != username {
			delete(c.records, current)
		}
	}
}

func (c *Credentials) passwordLocked(username string) string {
	h := hmac.New(sha256.New, c.secret)
	_, _ = h.Write([]byte(username))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (c *Credentials) pruneLocked(now time.Time) {
	for username, record := range c.records {
		if !now.Before(record.expiresAt) {
			delete(c.records, username)
		}
	}
}

func (c *Credentials) boundedExpiry(leaseExpiresAt, now time.Time) (time.Time, error) {
	now = now.UTC()
	expiresAt := leaseExpiresAt.UTC()
	if !expiresAt.After(now) {
		return time.Time{}, errors.New("TURN credential lease expiry must be in the future")
	}
	maximum := now.Add(c.ttl)
	if expiresAt.After(maximum) {
		expiresAt = maximum
	}
	return expiresAt, nil
}
