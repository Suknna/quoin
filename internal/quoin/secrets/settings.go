package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
)

var ErrSettingEnvelope = errors.New("setting envelope authentication failed")

func settingCipher(rootKey []byte, purpose string, revision int64, rootBinding int) (cipher.AEAD, []byte, error) {
	if len(rootKey) != 32 || purpose == "" || len(purpose) > 200 || revision < 1 || rootBinding < 1 {
		return nil, nil, ErrSettingEnvelope
	}
	block, err := aes.NewCipher(rootKey)
	if err != nil {
		return nil, nil, ErrSettingEnvelope
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, ErrSettingEnvelope
	}
	// A distinct, structured domain prevents ciphertext substitution with connection credentials.
	aad, err := json.Marshal(struct {
		Domain      string
		Purpose     string
		Revision    int64
		RootBinding int
	}{"quoin:setting:v1", purpose, revision, rootBinding})
	if err != nil {
		return nil, nil, ErrSettingEnvelope
	}
	return aead, aad, nil
}

func SealSetting(rootKey []byte, purpose string, revision int64, rootBinding int, plaintext []byte) (*Envelope, error) {
	aead, aad, err := settingCipher(rootKey, purpose, revision, rootBinding)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrSettingEnvelope
	}
	return &Envelope{Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, plaintext, aad)}, nil
}

func OpenSetting(rootKey []byte, purpose string, revision int64, rootBinding int, envelope *Envelope) ([]byte, error) {
	aead, aad, err := settingCipher(rootKey, purpose, revision, rootBinding)
	if err != nil {
		return nil, err
	}
	if envelope == nil || len(envelope.Nonce) != aead.NonceSize() || len(envelope.Ciphertext) < aead.Overhead() {
		return nil, ErrSettingEnvelope
	}
	plain, err := aead.Open(nil, envelope.Nonce, envelope.Ciphertext, aad)
	if err != nil {
		return nil, ErrSettingEnvelope
	}
	return plain, nil
}
