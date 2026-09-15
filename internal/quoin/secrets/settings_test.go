package secrets

import (
	"bytes"
	"testing"
)

func TestSettingsEnvelopeBindsIdentityAndRevision(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	envelope, err := SealSetting(key, "auth.delivery", 2, 1, []byte("private-value"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := OpenSetting(key, "auth.delivery", 2, 1, envelope)
	if err != nil || string(plain) != "private-value" {
		t.Fatalf("round trip: %q %v", plain, err)
	}
	for _, tc := range []struct {
		purpose  string
		revision int64
		binding  int
	}{{"other", 2, 1}, {"auth.delivery", 3, 1}, {"auth.delivery", 2, 2}} {
		if _, err := OpenSetting(key, tc.purpose, tc.revision, tc.binding, envelope); err == nil {
			t.Fatal("accepted a different identity")
		}
	}
	envelope.Ciphertext[0] ^= 1
	if _, err := OpenSetting(key, "auth.delivery", 2, 1, envelope); err == nil {
		t.Fatal("accepted tampering")
	}
}

func TestSettingsEnvelopeRejectsMalformedInputs(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	for _, envelope := range []*Envelope{nil, {}, {Nonce: []byte{1}, Ciphertext: make([]byte, 32)}} {
		if _, err := OpenSetting(key, "auth.delivery", 1, 1, envelope); err == nil {
			t.Fatal("accepted malformed envelope")
		}
	}
	if _, err := SealSetting(key, "", 1, 1, nil); err == nil {
		t.Fatal("accepted empty purpose")
	}
	if _, err := SealSetting(key, "auth.delivery", 0, 1, nil); err == nil {
		t.Fatal("accepted zero revision")
	}
	if _, err := SealSetting(key[:16], "auth.delivery", 1, 1, nil); err == nil {
		t.Fatal("accepted short key")
	}
}
