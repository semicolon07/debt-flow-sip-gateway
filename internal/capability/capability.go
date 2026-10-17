package capability

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	TokenVersion       = "dfcap1"
	SessionType        = "session"
	CallType           = "call"
	maxCapabilityTTL   = 60 * time.Second
	maxSessionLeaseTTL = 120 * time.Second
	clockSkew          = 5 * time.Second
)

type SIPBootstrap struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Transport string `json:"transport"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Realm     string `json:"realm,omitempty"`
}

type SessionClaims struct {
	Version             int          `json:"v"`
	Type                string       `json:"type"`
	Issuer              string       `json:"iss"`
	Audience            string       `json:"aud"`
	GatewayID           string       `json:"gatewayId"`
	CapabilityID        string       `json:"capabilityId"`
	IssuedAt            int64        `json:"iat"`
	NotBefore           int64        `json:"nbf"`
	ExpiresAt           int64        `json:"exp"`
	LeaseExpiresAt      int64        `json:"leaseExp"`
	SessionID           string       `json:"sessionId"`
	UserSessionID       string       `json:"userSessionId"`
	CollectionID        string       `json:"collectionId"`
	CollectionBindingID string       `json:"collectionBindingId"`
	AgentInstanceID     string       `json:"agentInstanceId"`
	ProtocolVersion     int          `json:"protocolVersion"`
	SIP                 SIPBootstrap `json:"sip"`
}

type CallClaims struct {
	Version             int    `json:"v"`
	Type                string `json:"type"`
	Issuer              string `json:"iss"`
	Audience            string `json:"aud"`
	GatewayID           string `json:"gatewayId"`
	CapabilityID        string `json:"capabilityId"`
	IssuedAt            int64  `json:"iat"`
	NotBefore           int64  `json:"nbf"`
	ExpiresAt           int64  `json:"exp"`
	SessionID           string `json:"sessionId"`
	UserSessionID       string `json:"userSessionId"`
	CollectionID        string `json:"collectionId"`
	CollectionBindingID string `json:"collectionBindingId"`
	CallID              string `json:"callId"`
	CallContextID       string `json:"callContextId"`
	Destination         string `json:"destination"`
}

type CallBinding struct {
	SessionID           string
	UserSessionID       string
	CollectionID        string
	CollectionBindingID string
	CallID              string
	CallContextID       string
}

type Codec struct {
	gatewayID string
	issuer    string
	audience  string
	keys      map[string][]byte
	now       func() time.Time
}

func New(gatewayID, issuer, audience string, keys map[string][]byte) (*Codec, error) {
	if strings.TrimSpace(gatewayID) == "" || strings.TrimSpace(issuer) == "" || strings.TrimSpace(audience) == "" || len(keys) == 0 {
		return nil, errors.New("gateway ID, issuer, audience and capability keys are required")
	}
	owned := make(map[string][]byte, len(keys))
	for keyID, key := range keys {
		if strings.TrimSpace(keyID) == "" || len(key) != 32 {
			return nil, errors.New("capability keys must have a non-empty ID and contain exactly 32 bytes")
		}
		owned[keyID] = append([]byte(nil), key...)
	}
	return &Codec{gatewayID: gatewayID, issuer: issuer, audience: audience, keys: owned, now: time.Now}, nil
}

func (c *Codec) Close() {
	for _, key := range c.keys {
		for index := range key {
			key[index] = 0
		}
	}
}

func (c *Codec) Seal(keyID string, claims any) (string, error) {
	key, ok := c.keys[keyID]
	if !ok {
		return "", errors.New("unknown capability key")
	}
	plaintext, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	defer clear(plaintext)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	encodedKeyID := base64.RawURLEncoding.EncodeToString([]byte(keyID))
	aad := []byte(TokenVersion + "." + encodedKeyID)
	sealed := gcm.Seal(nil, nonce, plaintext, aad)
	ciphertext, tag := sealed[:len(sealed)-gcm.Overhead()], sealed[len(sealed)-gcm.Overhead():]
	return strings.Join([]string{TokenVersion, encodedKeyID,
		base64.RawURLEncoding.EncodeToString(nonce),
		base64.RawURLEncoding.EncodeToString(ciphertext),
		base64.RawURLEncoding.EncodeToString(tag)}, "."), nil
}

func (c *Codec) VerifySession(token, collectionID string) (SessionClaims, error) {
	var claims SessionClaims
	if err := c.open(token, &claims); err != nil {
		return SessionClaims{}, err
	}
	if claims.Type != SessionType || claims.CollectionID != collectionID || claims.ProtocolVersion != 1 ||
		blank(claims.CapabilityID, claims.SessionID, claims.UserSessionID, claims.CollectionID,
			claims.CollectionBindingID, claims.AgentInstanceID) {
		return SessionClaims{}, errors.New("invalid session capability binding")
	}
	if err := c.validateCommon(claims.Version, claims.Issuer, claims.Audience, claims.GatewayID,
		claims.IssuedAt, claims.NotBefore, claims.ExpiresAt); err != nil {
		return SessionClaims{}, err
	}
	now := c.now().UTC()
	leaseExpires := time.Unix(claims.LeaseExpiresAt, 0).UTC()
	issued := time.Unix(claims.IssuedAt, 0).UTC()
	if !leaseExpires.After(now) || leaseExpires.After(issued.Add(maxSessionLeaseTTL)) {
		return SessionClaims{}, errors.New("invalid session capability lease")
	}
	transport := strings.ToLower(claims.SIP.Transport)
	if blank(claims.SIP.Host, claims.SIP.Username, claims.SIP.Password) || claims.SIP.Port < 1 || claims.SIP.Port > 65535 ||
		(transport != "udp" && transport != "tcp") || tooLong(claims.SIP.Host, 255) ||
		tooLong(claims.SIP.Username, 255) || tooLong(claims.SIP.Password, 4096) || tooLong(claims.SIP.Realm, 255) {
		return SessionClaims{}, errors.New("invalid SIP bootstrap in session capability")
	}
	claims.SIP.Transport = transport
	return claims, nil
}

func (c *Codec) VerifyCall(token string, expected CallBinding) (CallClaims, error) {
	var claims CallClaims
	if err := c.open(token, &claims); err != nil {
		return CallClaims{}, err
	}
	if claims.Type != CallType || blank(claims.CapabilityID, claims.SessionID, claims.UserSessionID, claims.CollectionID,
		claims.CollectionBindingID, claims.CallID, claims.CallContextID, claims.Destination) ||
		claims.SessionID != expected.SessionID || claims.UserSessionID != expected.UserSessionID ||
		claims.CollectionID != expected.CollectionID ||
		claims.CollectionBindingID != expected.CollectionBindingID || claims.CallID != expected.CallID ||
		claims.CallContextID != expected.CallContextID || tooLong(claims.Destination, 512) ||
		strings.IndexFunc(claims.Destination, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return CallClaims{}, errors.New("invalid call capability binding")
	}
	if err := c.validateCommon(claims.Version, claims.Issuer, claims.Audience, claims.GatewayID,
		claims.IssuedAt, claims.NotBefore, claims.ExpiresAt); err != nil {
		return CallClaims{}, err
	}
	return claims, nil
}

func (c *Codec) open(token string, target any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 5 || parts[0] != TokenVersion {
		return errors.New("invalid capability format")
	}
	keyIDRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(keyIDRaw) == 0 {
		return errors.New("invalid capability key ID")
	}
	key, ok := c.keys[string(keyIDRaw)]
	if !ok {
		return errors.New("unknown capability key")
	}
	nonce, nonceErr := base64.RawURLEncoding.DecodeString(parts[2])
	ciphertext, cipherErr := base64.RawURLEncoding.DecodeString(parts[3])
	tag, tagErr := base64.RawURLEncoding.DecodeString(parts[4])
	if nonceErr != nil || cipherErr != nil || tagErr != nil {
		return errors.New("invalid capability encoding")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	if len(nonce) != gcm.NonceSize() || len(tag) != gcm.Overhead() {
		return errors.New("invalid capability cipher parameters")
	}
	plaintext, err := gcm.Open(nil, nonce, append(ciphertext, tag...), []byte(parts[0]+"."+parts[1]))
	if err != nil {
		return errors.New("capability authentication failed")
	}
	defer clear(plaintext)
	decoder := json.NewDecoder(bytes.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode capability: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("decode capability: trailing JSON value")
	}
	return nil
}

func (c *Codec) validateCommon(version int, issuer, audience, gatewayID string, issuedAt, notBefore, expiresAt int64) error {
	if version != 1 || issuer != c.issuer || audience != c.audience || gatewayID != c.gatewayID {
		return errors.New("capability authority mismatch")
	}
	now := c.now().UTC()
	issued := time.Unix(issuedAt, 0).UTC()
	notBeforeTime := time.Unix(notBefore, 0).UTC()
	expires := time.Unix(expiresAt, 0).UTC()
	if issued.After(now.Add(clockSkew)) || now.Add(clockSkew).Before(notBeforeTime) || !now.Before(expires.Add(clockSkew)) ||
		!expires.After(issued) || expires.After(issued.Add(maxCapabilityTTL)) {
		return errors.New("capability is not valid at this time")
	}
	return nil
}

func blank(values ...string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return true
		}
	}
	return false
}

func tooLong(value string, maximum int) bool { return len(value) > maximum }
