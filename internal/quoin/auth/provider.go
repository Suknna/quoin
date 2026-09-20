package auth

// AuthProvider registry (ADR-0010 / docs/authentication-design.md §4/§7).
// Login channels are declared by the `authentication` deployment config and
// materialize as explicit, version-shipped provider implementations — the
// registry is the single source the public /api/v1/auth/config endpoint
// projects, so the login page renders from configuration with no per-channel
// branching. Providers own their whole channel dance (credential verify or
// SSO redirect round-trip) and hand the HTTP layer only cookies to set and a
// location to send the browser to.

import (
	"context"
	"errors"
)

// Provider kinds persisted in the registry and exposed on /api/v1/auth/config.
const (
	ProviderKindLocal = "local"
	ProviderKindOIDC  = "oidc"
)

var (
	// ErrProviderUnknown marks a registry lookup for an unregistered kind.
	ErrProviderUnknown = errors.New("auth: login provider is not registered")
	// ErrProviderNotRedirect marks BeginLogin on a non-redirect channel (the
	// local form posts credentials instead of following a redirect dance).
	ErrProviderNotRedirect = errors.New("auth: login provider does not redirect")
)

// ProviderDescriptor is the non-secret channel projection behind
// /api/v1/auth/config: the login page renders entirely from these fields.
type ProviderDescriptor struct {
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
	Visible bool   `json:"visible,omitempty"`
	Label   string `json:"label,omitempty"`
	IconURL string `json:"iconUrl,omitempty"`
}

// LoginIntent is the browser-facing outcome of BeginLogin for redirect-based
// channels: the IdP authorize URL to navigate to plus the opaque short-lived
// state credential the HTTP layer must store as a short-TTL HttpOnly cookie.
type LoginIntent struct {
	Redirect     string
	StateCookie  string
	CookieMaxAge int // seconds; 0 means the intent carries no state cookie
}

// LoginCompletion carries one channel-agnostic completion attempt. Exactly
// one shape is populated per provider kind: the local credential POST or the
// OIDC callback (echoed state cookie + query state + authorization code).
type LoginCompletion struct {
	Username  string
	Password  string
	UserAgent string

	StateCookie string
	State       string
	Code        string
}

// Provider is one configured login channel. BeginLogin starts a redirect
// dance (SSO); non-redirect channels return ErrProviderNotRedirect and the
// HTTP layer serves the credential form instead. CompleteLogin finishes the
// channel and issues the full session. Logout is channel-uniform local
// revocation (ADR-0010: no RP-initiated IdP logout).
type Provider interface {
	Descriptor() ProviderDescriptor
	BeginLogin(ctx context.Context, returnTo string) (LoginIntent, error)
	// CompleteLogin finishes the channel and additionally returns the
	// sanitized same-origin return-to target the login intent carried.
	CompleteLogin(ctx context.Context, completion LoginCompletion) (LoginResult, string, error)
	Logout(ctx context.Context, session Session) error
}

// LocalProvider is the emergency password channel: single-step, no redirect.
type LocalProvider struct {
	Service *Service
	// Enabled/Visible mirror authentication.local from the deployment config.
	Enabled bool
	Visible bool
}

func (p LocalProvider) Descriptor() ProviderDescriptor {
	return ProviderDescriptor{Kind: ProviderKindLocal, Enabled: p.Enabled, Visible: p.Visible}
}

func (p LocalProvider) BeginLogin(ctx context.Context, returnTo string) (LoginIntent, error) {
	return LoginIntent{}, ErrProviderNotRedirect
}

func (p LocalProvider) CompleteLogin(ctx context.Context, completion LoginCompletion) (LoginResult, string, error) {
	result, err := p.Service.LoginWithPassword(ctx, completion.Username, completion.Password, completion.UserAgent)
	return result, "", err
}

func (p LocalProvider) Logout(ctx context.Context, session Session) error {
	return p.Service.Logout(ctx, session)
}

// Registry holds the assembled providers keyed by kind.
type Registry struct {
	providers map[string]Provider
	kinds     []string
}

func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}}
}

// Register installs one provider; a duplicate kind is a startup error.
func (r *Registry) Register(provider Provider) error {
	kind := provider.Descriptor().Kind
	if _, exists := r.providers[kind]; exists {
		return errors.New("auth: duplicate login provider " + kind)
	}
	r.providers[kind] = provider
	r.kinds = append(r.kinds, kind)
	return nil
}

func (r *Registry) Get(kind string) (Provider, error) {
	provider, ok := r.providers[kind]
	if !ok {
		return nil, ErrProviderUnknown
	}
	return provider, nil
}

// Kinds returns the registration order (stable for config projection).
func (r *Registry) Kinds() []string { return r.kinds }

// Descriptors projects every registered channel for /api/v1/auth/config.
func (r *Registry) Descriptors() []ProviderDescriptor {
	descriptors := make([]ProviderDescriptor, 0, len(r.kinds))
	for _, kind := range r.kinds {
		descriptors = append(descriptors, r.providers[kind].Descriptor())
	}
	return descriptors
}
