package media

import "testing"

func TestHasAudibleG711RejectsDigitalSilence(t *testing.T) {
	tests := []struct {
		name    string
		codec   string
		payload []byte
	}{
		{name: "pcmu", codec: "PCMU", payload: repeatedByte(0xff, 160)},
		{name: "pcma positive", codec: "PCMA", payload: repeatedByte(0xd5, 160)},
		{name: "pcma negative", codec: "PCMA", payload: repeatedByte(0x55, 160)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if HasAudibleG711(test.codec, test.payload) {
				t.Fatal("digital silence was classified as audible")
			}
		})
	}
}

func TestHasAudibleG711DetectsSignal(t *testing.T) {
	for _, test := range []struct {
		codec   string
		payload []byte
	}{
		{codec: "PCMU", payload: repeatedByte(0x00, 160)},
		{codec: "PCMA", payload: repeatedByte(0x00, 160)},
	} {
		if !HasAudibleG711(test.codec, test.payload) {
			t.Fatalf("%s signal was classified as silent", test.codec)
		}
	}
}

func TestHasAudibleG711RejectsUnsupportedOrTruncatedPayload(t *testing.T) {
	if HasAudibleG711("opus", repeatedByte(0x00, 160)) {
		t.Fatal("unsupported codec was classified as audible")
	}
	if HasAudibleG711("PCMU", repeatedByte(0x00, minimumG711ActivitySamples-1)) {
		t.Fatal("truncated payload was classified as audible")
	}
}

func repeatedByte(value byte, count int) []byte {
	payload := make([]byte, count)
	for index := range payload {
		payload[index] = value
	}
	return payload
}
