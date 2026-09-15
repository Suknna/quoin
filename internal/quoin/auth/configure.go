package auth

// Narrow delivery seam for authentication messages (docs/authentication-design.md §7/§8).
// The auth package owns flows, challenges and session issuance; a Sender only
// accepts deliveries and never decides authentication outcomes. The interface
// is deliberately minimal so the delivery integration can adapt any transport
// (SMTP, outbound webhook) through SenderFunc until its concrete type lands.

import (
	"context"
	"fmt"
	"sync"
)

// Message is one outbound authentication delivery. Variables carry the
// structured template fields (the verification code never enters logs,
// ledgers or audit bodies — the Sender receives it in memory only).
type Message struct {
	DeliveryID string
	Channel    string // "email" or "sms"
	Recipient  string
	Template   string
	Variables  map[string]string
}

// Sender accepts one delivery. An error means the outcome is unknown or
// failed; a nil error only means the delivery was accepted (never that the
// recipient received or verified anything).
type Sender interface {
	Send(ctx context.Context, message Message) error
}

// SenderFunc adapts a callback into a Sender (delivery integration bridge).
type SenderFunc func(ctx context.Context, message Message) error

// Send implements Sender.
func (fn SenderFunc) Send(ctx context.Context, message Message) error {
	return fn(ctx, message)
}

// AuthConfig carries the process-wide authentication configuration.
// OTPKey protects low-entropy verification codes with a keyed hash
// (HMAC-SHA256): a database leak alone must not allow offline code guessing.
type AuthConfig struct {
	OTPKey []byte
	Sender Sender
}

// OTPKeyMinLength is the minimum OTP key material in bytes.
const OTPKeyMinLength = 32

// ConfigureAuth installs the OTP keyed-hash key and the delivery sender.
// It must be called once during startup before any flow issues challenges.
func (service *Service) ConfigureAuth(config AuthConfig) error {
	if len(config.OTPKey) < OTPKeyMinLength {
		return fmt.Errorf("OTP key must contain at least %d bytes", OTPKeyMinLength)
	}
	key := make([]byte, len(config.OTPKey))
	copy(key, config.OTPKey)
	service.authMu.Lock()
	defer service.authMu.Unlock()
	service.otpKey = key
	service.sender = config.Sender
	return nil
}

// delivery returns the configured key and sender; ok is false while
// ConfigureAuth has not run, and every challenge path fails closed then.
func (service *Service) delivery() (key []byte, sender Sender, ok bool) {
	service.authMu.RLock()
	defer service.authMu.RUnlock()
	if service.otpKey == nil {
		return nil, nil, false
	}
	return service.otpKey, service.sender, true
}

// sync.RWMutex guards the configurable key/sender pair on Service.
type authConfigGuard = sync.RWMutex
