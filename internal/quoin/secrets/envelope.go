// Package secrets gains the credential envelope codec shared by the
// connections domain (T07+): AES-256-GCM sealing per SEC-KEY-003 with AAD
// binding the generation identity, and the plaintext typed secret carrier.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
)

// envelopeVersion is frozen by the schema (credential_generations.
// envelope_version = 1).
const EnvelopeVersion = 1

// Envelope is the AEAD wire/storage form: nonce + ciphertext||tag merged, as
// persisted in credential_generations.
type Envelope struct {
	Nonce      []byte // 12 bytes
	Ciphertext []byte // payload + 16-byte GCM tag
}

// TypedSecret is the decrypted connection secret in supervisor memory only.
type TypedSecret struct {
	Type string `json:"type"` // thanos | kubernetes | model_provider
	// Exactly one carrier is set per type; raw values never persist.
	Thanos        *ThanosSecret        `json:"thanos,omitempty"`
	Kubernetes    *KubernetesSecret    `json:"kubernetes,omitempty"`
	ModelProvider *ModelProviderSecret `json:"model_provider,omitempty"`
}

type ThanosSecret struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type KubernetesSecret struct {
	Kubeconfig string `json:"kubeconfig"`
}

type ModelProviderSecret struct {
	APIKey string `json:"apiKey"`
}

// EnvelopeAAD binds the ciphertext to the generation identity
// (DATA-CONN-004): connection locator, generation seq, connection type and
// envelope version.
func EnvelopeAAD(connectionID, generationSeq int64, connectionType string, version int, rootBindingRevision int) []byte {
	return []byte(fmt.Sprintf("quoin:credential:v%d:conn=%d:gen=%d:type=%s:rootrev=%d", version, connectionID, generationSeq, connectionType, rootBindingRevision))
}

// Seal encrypts the typed secret with the 32-byte root key.
func Seal(rootKey []byte, connectionID, generationSeq int64, connectionType string, rootBindingRevision int, secret *TypedSecret) (*Envelope, error) {
	if len(rootKey) != 32 {
		return nil, fmt.Errorf("root key must be 32 bytes")
	}
	plaintext, err := json.Marshal(secret)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(rootKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, EnvelopeAAD(connectionID, generationSeq, connectionType, EnvelopeVersion, rootBindingRevision))
	return &Envelope{Nonce: nonce, Ciphertext: ciphertext}, nil
}

// Open decrypts and validates the envelope; AAD mismatch or tampering fails
// closed (DATA-CONN-002: no fallback, no legacy revision retry).
func Open(rootKey []byte, connectionID, generationSeq int64, connectionType string, rootBindingRevision int, envelope *Envelope) (*TypedSecret, error) {
	if len(rootKey) != 32 {
		return nil, fmt.Errorf("root key must be 32 bytes")
	}
	block, err := aes.NewCipher(rootKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, envelope.Nonce, envelope.Ciphertext, EnvelopeAAD(connectionID, generationSeq, connectionType, EnvelopeVersion, rootBindingRevision))
	if err != nil {
		return nil, fmt.Errorf("credential envelope authentication failed")
	}
	var secret TypedSecret
	if err := json.Unmarshal(plaintext, &secret); err != nil {
		return nil, err
	}
	if secret.Type != connectionType {
		return nil, fmt.Errorf("credential type mismatch")
	}
	return &secret, nil
}
