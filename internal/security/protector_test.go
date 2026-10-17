package security

import "testing"

func TestZero(t *testing.T) {
	value := []byte("secret")
	Zero(value)
	for _, b := range value {
		if b != 0 {
			t.Fatal("secret buffer was not zeroed")
		}
	}
}
