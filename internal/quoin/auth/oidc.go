package auth

// OIDC login channel (ADR-0010 / docs/authentication-design.md §4): the
// authorization-code + PKCE round-trip with state/nonce anti-replay, the
// token exchange and ID Token verification entirely server-side, and the
// identities(issuer, subject) mapping with JIT provisioning. Tokens are
// used once and discarded — no refresh token is ever stored. Discovery is
// lazy: an unreachable IdP never blocks startup or the local channel.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	oidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	// oidcStateCookieName carries the HMAC-signed login intent (state, nonce,
	// PKCE verifier, return-to) through the browser round-trip.
	oidcStateCookieName = "__Host-quoin-oidc-state"
	// oidcIntentTTL bounds the whole browser round-trip.
	oidcIntentTTL = 10 * time.Minute
	// oidcScope is fixed: the platform needs no offline access.
	oidcScope = "openid profile email"
)

// Sentinel errors of the OIDC channel (HTTP mapping lives in the app layer).
var (
	ErrOIDCStateInvalid  = errors.New("oidc login state is invalid, expired or does not match")
	ErrOIDCProviderDown  = errors.New("oidc identity provider is temporarily unavailable")
	ErrOIDCRejected      = errors.New("oidc login was rejected")
	ErrOIDCMisconfigured = errors.New("oidc login channel is misconfigured")
)

// oidcIntent is the signed cookie payload of one login round-trip.
type oidcIntent struct {
	State     string `json:"state"`
	Nonce     string `json:"nonce"`
	Verifier  string `json:"verifier"`
	ReturnTo  string `json:"returnTo"`
	ExpiresAt int64  `json:"exp"`
}

// OIDCProvider implements Provider over the generic OIDC authorization-code
// flow. StateKey is the HMAC key for the intent cookie (derived from the
// deployment root key in the app layer); ClientSecret is read from the
// secrets file at startup and never logged.
type OIDCProvider struct {
	Service      *Service
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Label        string
	IconURL      string
	StateKey     []byte

	mu         sync.Mutex
	oauth      *oauth2.Config
	verifier   *oidc.IDTokenVerifier
	lastErr    error
	retryAfter time.Time
}

func (p *OIDCProvider) Descriptor() ProviderDescriptor {
	label := p.Label
	if label == "" {
		if issuer := strings.TrimPrefix(p.Issuer, "https://"); issuer != "" {
			label = strings.SplitN(issuer, "/", 2)[0]
		}
	}
	return ProviderDescriptor{Kind: ProviderKindOIDC, Enabled: true, Label: label, IconURL: p.IconURL}
}

// BeginLogin resolves discovery (lazily, retrying on failure), mints the
// signed intent cookie and returns the IdP authorize redirect.
func (p *OIDCProvider) BeginLogin(ctx context.Context, returnTo string) (LoginIntent, error) {
	if len(p.StateKey) < 32 {
		return LoginIntent{}, ErrOIDCMisconfigured
	}
	config, _, err := p.resolved(ctx)
	if err != nil {
		return LoginIntent{}, err
	}
	state, err := randomToken(16)
	if err != nil {
		return LoginIntent{}, err
	}
	nonce, err := randomToken(16)
	if err != nil {
		return LoginIntent{}, err
	}
	verifier, err := randomToken(32)
	if err != nil {
		return LoginIntent{}, err
	}
	intent := oidcIntent{
		State: state, Nonce: nonce, Verifier: verifier, ReturnTo: sanitizeReturnTo(returnTo),
		ExpiresAt: time.Now().UTC().Add(oidcIntentTTL).Unix(),
	}
	encoded, err := p.sealIntent(intent)
	if err != nil {
		return LoginIntent{}, err
	}
	redirect := config.AuthCodeURL(state,
		oauth2.SetAuthURLParam("scope", oidcScope),
		oauth2.SetAuthURLParam("code_challenge", pkceChallenge(verifier)),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oidc.Nonce(nonce),
	)
	return LoginIntent{Redirect: redirect, StateCookie: encoded, CookieMaxAge: int(oidcIntentTTL.Seconds())}, nil
}

// CompleteLogin verifies the echoed state cookie, exchanges the code with
// the PKCE verifier, checks the ID Token (iss/aud/exp/nonce) and hands the
// mapped identity to the service for the identities lookup or JIT
// provisioning and the session issue.
func (p *OIDCProvider) CompleteLogin(ctx context.Context, completion LoginCompletion) (LoginResult, string, error) {
	if completion.Code == "" || completion.State == "" || completion.StateCookie == "" {
		return LoginResult{}, "", ErrOIDCStateInvalid
	}
	intent, err := p.unsealIntent(completion.StateCookie)
	if err != nil {
		return LoginResult{}, "", ErrOIDCStateInvalid
	}
	if !hmac.Equal([]byte(intent.State), []byte(completion.State)) {
		return LoginResult{}, "", ErrOIDCStateInvalid
	}
	config, verifier, err := p.resolved(ctx)
	if err != nil {
		return LoginResult{}, "", err
	}
	token, err := config.Exchange(ctx, completion.Code,
		oauth2.SetAuthURLParam("code_verifier", intent.Verifier),
	)
	if err != nil {
		return LoginResult{}, "", fmt.Errorf("%w: token exchange failed", ErrOIDCRejected)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return LoginResult{}, "", fmt.Errorf("%w: no id_token in token response", ErrOIDCRejected)
	}
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return LoginResult{}, "", fmt.Errorf("%w: id_token verification failed", ErrOIDCRejected)
	}
	if idToken.Nonce != intent.Nonce {
		return LoginResult{}, "", fmt.Errorf("%w: nonce mismatch", ErrOIDCRejected)
	}
	var claims struct {
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
		Email             string `json:"email"`
		EmailVerified     bool   `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return LoginResult{}, "", fmt.Errorf("%w: claims decode failed", ErrOIDCRejected)
	}
	result, err := p.Service.CompleteOIDCLogin(ctx,
		p.Issuer, idToken.Subject,
		claims.PreferredUsername, claims.Name,
		claims.Email, claims.EmailVerified,
		completion.UserAgent)
	if err != nil {
		return LoginResult{}, "", err
	}
	return result, intent.ReturnTo, nil

}

// Logout is the channel-uniform local revocation.
func (p *OIDCProvider) Logout(ctx context.Context, session Session) error {
	return p.Service.Logout(ctx, session)
}

// resolved lazily discovers the issuer endpoints and keeps retrying on
// failure (one attempt per retry window): the local channel and startup
// never wait on the IdP. Discovery holds the mutex while running; the
// cached fast path also locks because a background retry may be in flight.
func (p *OIDCProvider) resolved(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.oauth != nil {
		return p.oauth, p.verifier, nil
	}
	if p.lastErr != nil && time.Now().Before(p.retryAfter) && ctx.Value(oidcForceRetry{}) == nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrOIDCProviderDown, p.lastErr)
	}
	provider, err := oidc.NewProvider(ctx, p.Issuer)
	if err != nil {
		p.lastErr = err
		p.retryAfter = time.Now().Add(30 * time.Second)
		return nil, nil, fmt.Errorf("%w: %v", ErrOIDCProviderDown, err)
	}
	config := &oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		RedirectURL:  p.RedirectURL,
		Endpoint:     provider.Endpoint(),
	}
	p.oauth = config
	p.verifier = provider.Verifier(&oidc.Config{ClientID: p.ClientID})
	p.lastErr = nil
	return config, p.verifier, nil
}

// ResetDiscovery drops the cached discovery (tests swap fake IdPs).
func (p *OIDCProvider) ResetDiscovery() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.oauth = nil
	p.verifier = nil
	p.lastErr = nil
}

type oidcForceRetry struct{}

// ForceDiscoveryRetry marks a context as allowed to bypass the retry window
// (test seam).
func ForceDiscoveryRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, oidcForceRetry{}, true)
}

func (p *OIDCProvider) sealIntent(intent oidcIntent) (string, error) {
	body, err := json.Marshal(intent)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, p.StateKey)
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (p *OIDCProvider) unsealIntent(value string) (oidcIntent, error) {
	encoded, signature, ok := strings.Cut(value, ".")
	if !ok {
		return oidcIntent{}, errors.New("malformed intent")
	}
	mac := hmac.New(sha256.New, p.StateKey)
	mac.Write([]byte(encoded))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(signature)) {
		return oidcIntent{}, errors.New("intent signature mismatch")
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return oidcIntent{}, err
	}
	var intent oidcIntent
	if err := json.Unmarshal(body, &intent); err != nil {
		return oidcIntent{}, err
	}
	if time.Now().UTC().Unix() >= intent.ExpiresAt {
		return oidcIntent{}, errors.New("intent expired")
	}
	return intent, nil
}

// randomToken returns url-safe random bytes (hex-free, high entropy).
func randomToken(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// pkceChallenge derives the S256 code challenge of a verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// sanitizeReturnTo accepts only same-origin relative paths; anything else
// falls back to the workbench root. Open redirects are a hard red line.
func sanitizeReturnTo(returnTo string) string {
	if returnTo == "" || !strings.HasPrefix(returnTo, "/") || strings.HasPrefix(returnTo, "//") || strings.Contains(returnTo, "\\") {
		return "/"
	}
	return returnTo
}

var _ Provider = (*OIDCProvider)(nil)
