package capability

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/debt-flow/sip-gateway/internal/config"
	"github.com/debt-flow/sip-gateway/internal/security"
)

func LoadKeys(value config.Capability, protector security.Protector) (map[string][]byte, error) {
	keys := make(map[string][]byte, len(value.ProtectedKeys)+len(value.PlaintextKeys))
	fail := func(err error) (map[string][]byte, error) {
		for _, key := range keys {
			security.Zero(key)
		}
		return nil, err
	}
	for keyID := range value.PlaintextKeys {
		if _, duplicate := value.ProtectedKeys[keyID]; duplicate {
			return fail(fmt.Errorf("capability key ID %q is configured more than once", keyID))
		}
	}
	for keyID, path := range value.ProtectedKeys {
		encoded, err := os.ReadFile(path)
		if err != nil {
			return fail(fmt.Errorf("read protected capability key %q: %w", keyID, err))
		}
		ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
		if err != nil {
			return fail(fmt.Errorf("decode protected capability key %q: %w", keyID, err))
		}
		plaintext, err := protector.Unprotect(ciphertext)
		security.Zero(ciphertext)
		if err != nil {
			return fail(fmt.Errorf("unprotect capability key %q: %w", keyID, err))
		}
		if len(plaintext) != 32 {
			security.Zero(plaintext)
			return fail(fmt.Errorf("capability key %q must contain exactly 32 bytes", keyID))
		}
		keys[keyID] = plaintext
	}
	for keyID, encoded := range value.PlaintextKeys {
		plaintext, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			plaintext, err = base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		}
		if err != nil || len(plaintext) != 32 {
			security.Zero(plaintext)
			return fail(fmt.Errorf("plaintext capability key %q must be base64-encoded 32 bytes", keyID))
		}
		keys[keyID] = plaintext
	}
	return keys, nil
}
