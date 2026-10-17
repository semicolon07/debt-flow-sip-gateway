package security

import "errors"

var ErrDPAPIUnavailable = errors.New("Windows DPAPI is unavailable on this operating system")

type Protector interface {
	Protect(plaintext []byte) ([]byte, error)
	Unprotect(ciphertext []byte) ([]byte, error)
}

func Zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
