package auth

// Second-factor source seam (docs/authentication-design.md §7): the core owns
// the challenge lifecycle — keyed storage bound to flow/user/contact/revision,
// single consumption, resend supersession and rate limits — while a registered
// source owns producing the secret the user must present back. The built-in
// source generates numeric OTPs; deterministic or algorithmic sources (test
// providers, TOTP-style windows) register at startup without any core change.
// Non-code interaction shapes (WebAuthn-style challenge/response) need the
// challenge contract to grow a per-factor payload and are deliberately out of
// scope here.

import (
	"context"
	"errors"
)

// FactorSource produces the response secret for one challenge of its channel.
type FactorSource interface {
	Secret(ctx context.Context) (string, error)
}

// Factor channels (user_contacts.channel CHECK); every channel must have a
// source, defaulting to the built-in numeric generator.
var factorChannels = map[string]bool{ChannelEmail: true, ChannelSMS: true}

// RegisterFactorSource installs the source for one channel. Startup-only:
// call once after NewService, before serving; concurrent reconfiguration is
// not supported. A nil source is rejected.
func (service *Service) RegisterFactorSource(channel string, source FactorSource) error {
	if !factorChannels[channel] {
		return errors.New("auth: unknown factor channel")
	}
	if source == nil {
		return errors.New("auth: factor source must not be nil")
	}
	service.authMu.Lock()
	defer service.authMu.Unlock()
	if service.factors == nil {
		service.factors = map[string]FactorSource{}
	}
	service.factors[channel] = source
	return nil
}

// factorFor returns the registered source for a channel or the built-in
// numeric generator.
func (service *Service) factorFor(channel string) FactorSource {
	service.authMu.RLock()
	defer service.authMu.RUnlock()
	if source, ok := service.factors[channel]; ok {
		return source
	}
	return numericFactorSource{}
}

// numericFactorSource is the built-in uniform numeric OTP generator.
type numericFactorSource struct{}

func (numericFactorSource) Secret(context.Context) (string, error) {
	return generateOTP()
}
