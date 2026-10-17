//go:build windows

package security

import (
	"bytes"
	"testing"
)

func TestMachineProtectorRoundTrip(t *testing.T) {
	protector := NewMachineProtector()
	plain := []byte("sip-password")
	cipher, err := protector.Protect(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cipher, plain) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := protector.Unprotect(cipher)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round trip mismatch: %q", got)
	}
}
