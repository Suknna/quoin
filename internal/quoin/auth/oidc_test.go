package auth_test

// Fake-IdP integration coverage of the OIDC channel (ADR-0010): the full
// authorization-code + PKCE round-trip against an httptest IdP that signs
// real RS256 ID Tokens, the identities(issuer, subject) mapping with JIT
// provisioning and username-collision suffixing, replay/mismatch rejections
// and the IdP-unavailable behavior of the lazy discovery.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

// fakeIdP is a minimal OIDC provider: discovery, JWKS and a token endpoint
// issuing RS256 ID tokens bound to the minted authorization codes.
type fakeIdP struct {
	server *httptest.Server
	key    *rsa.PrivateKey

	mu         sync.Mutex
	codes      map[string]fakeGrant
	nextSub    int
	claimsHook func(claims map[string]any)
}

type fakeGrant struct {
	state       string
	nonce       string
	challenge   string
	sub         string
	username    string
	name        string
	email       string
	emailVerify bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key, codes: map[string]fakeGrant{}}
	mux := http.NewServeMux()
	discovery := func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, map[string]any{
			"issuer":                                idp.server.URL,
			"authorization_endpoint":                idp.server.URL + "/authorize",
			"token_endpoint":                        idp.server.URL + "/token",
			"jwks_uri":                              idp.server.URL + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}
	mux.HandleFunc("GET /.well-known/openid-configuration", discovery)
	mux.HandleFunc("GET /jwks", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test-key",
					"n": base64.RawURLEncoding.EncodeToString(idp.key.N.Bytes()),
					"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
				},
			},
		})
	})
	mux.HandleFunc("POST /token", func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			http.Error(writer, "bad form", http.StatusBadRequest)
			return
		}
		code := request.PostFormValue("code")
		verifier := request.PostFormValue("code_verifier")
		idp.mu.Lock()
		grant, ok := idp.codes[code]
		if ok {
			delete(idp.codes, code)
		}
		idp.mu.Unlock()
		if !ok {
			http.Error(writer, "unknown code", http.StatusBadRequest)
			return
		}

		challenge := base64.RawURLEncoding.EncodeToString(sum256([]byte(verifier)))
		if challenge != grant.challenge {
			http.Error(writer, "pkce mismatch", http.StatusBadRequest)
			return
		}
		idToken, err := idp.signIDToken(grant)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(writer, map[string]any{
			"access_token": "at-" + code,
			"token_type":   "Bearer",
			"id_token":     idToken,
		})
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *fakeIdP) mint(t *testing.T, grant fakeGrant) string {
	t.Helper()
	idp.mu.Lock()
	defer idp.mu.Unlock()
	code := fmt.Sprintf("code-%d", idp.nextSub)
	idp.nextSub++
	idp.codes[code] = grant
	return code
}

func (idp *fakeIdP) signIDToken(grant fakeGrant) (string, error) {
	now := time.Now().UTC()
	claims := map[string]any{
		"iss":                idp.server.URL,
		"sub":                grant.sub,
		"aud":                "quoin-client",
		"exp":                now.Add(5 * time.Minute).Unix(),
		"iat":                now.Unix(),
		"nonce":              grant.nonce,
		"preferred_username": grant.username,
		"name":               grant.name,
		"email":              grant.email,
		"email_verified":     grant.emailVerify,
	}
	if idp.claimsHook != nil {
		idp.claimsHook(claims)
	}
	header := base64URL(map[string]any{"alg": "RS256", "kid": "test-key"})
	body := base64URL(claims)
	signingInput := header + "." + body
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func base64URL(value any) string {
	encoded, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func sum256(data []byte) []byte {
	digest := sha256.Sum256(data)
	return digest[:]
}

func writeJSON(writer http.ResponseWriter, body any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(body)
}

// newOIDCFixture boots the auth service with an unlocked administrator and
// the provider pointed at the fake IdP.
func newOIDCFixture(t *testing.T) (*auth.Service, *auth.OIDCProvider, *fakeIdP, *sql.DB) {
	t.Helper()
	service, db := newAuthService(t)
	_, _ = initializeAdminDrive(t, service)
	idp := newFakeIdP(t)
	provider := &auth.OIDCProvider{
		Service:      service,
		Issuer:       idp.server.URL,
		ClientID:     "quoin-client",
		ClientSecret: "quoin-secret",
		RedirectURL:  "https://quoin.example.com/api/v1/auth/oidc/callback",
		StateKey:     []byte(strings.Repeat("s", 32)),
	}
	return service, provider, idp, db
}

func countUsers(t *testing.T, db *sql.DB) int {
	t.Helper()
	var users int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	return users
}

func disableUser(t *testing.T, db *sql.DB, username string) error {
	t.Helper()
	_, err := db.Exec(`UPDATE users SET enabled=0,auth_revision=auth_revision+1,row_version=row_version+1 WHERE username=?`, username)
	return err
}

// driveBeginLogin starts a login and extracts the intent fields from the
// authorize redirect.
func driveBeginLogin(t *testing.T, provider *auth.OIDCProvider, returnTo string) (auth.LoginIntent, url.Values) {
	t.Helper()
	intent, err := provider.BeginLogin(auth.ForceDiscoveryRetry(context.Background()), returnTo)
	if err != nil {
		t.Fatalf("begin login: %v", err)
	}
	target, err := url.Parse(intent.Redirect)
	if err != nil {
		t.Fatal(err)
	}
	return intent, target.Query()
}

func TestOIDCLoginJITProvisionsAndIssuesSession(t *testing.T) {
	ctx := auth.ForceDiscoveryRetry(context.Background())
	service, provider, idp, db := newOIDCFixture(t)
	intent, query := driveBeginLogin(t, provider, "/alerts/list")
	if query.Get("state") == "" || query.Get("nonce") == "" ||
		query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize URL misses the round-trip bindings: %s", intent.Redirect)
	}
	code := idp.mint(t, fakeGrant{
		state: query.Get("state"), nonce: query.Get("nonce"),
		challenge: query.Get("code_challenge"),
		sub:       "subject-jane", username: "Jane", name: "Jane Doe",
		email: "jane@corp.example.com", emailVerify: true,
	})
	result, returnTo, err := provider.CompleteLogin(ctx, auth.LoginCompletion{
		StateCookie: intent.StateCookie, State: query.Get("state"), Code: code, UserAgent: "Test on Linux",
	})
	if err != nil {
		t.Fatalf("complete login: %v", err)
	}
	if returnTo != "/alerts/list" {
		t.Fatalf("returnTo = %q, want the sanitized deep link", returnTo)
	}
	if result.User.Username != "jane" || result.User.Role != "operator" || result.User.AuthSource != "oidc" {
		t.Fatalf("JIT projection = %+v", result.User)
	}
	if result.User.PasswordChangeRequired || !result.User.Initialized {
		t.Fatalf("JIT user must be a full operator: %+v", result.User)
	}
	// The session bearer authenticates; the email claim landed as a
	// display-only contact; the audit trail names both channel operations.
	session, err := service.Authenticate(ctx, result.Bearer)
	if err != nil {
		t.Fatalf("oidc session: %v", err)
	}
	if session.User.AuthSource != "oidc" {
		t.Fatalf("session auth source = %q", session.User.AuthSource)
	}
	var identities int
	if err := db.QueryRow(`SELECT COUNT(*) FROM identities WHERE subject='subject-jane'`).Scan(&identities); err != nil || identities != 1 {
		t.Fatalf("identities rows = %d err=%v", identities, err)
	}
	var contacts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM user_contacts WHERE channel='email' AND target='jane@corp.example.com'`).Scan(&contacts); err != nil || contacts != 1 {
		t.Fatalf("display contact rows = %d err=%v", contacts, err)
	}
	var jit, logins int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='auth.oidc.jit' AND outcome='success'`).Scan(&jit); err != nil || jit != 1 {
		t.Fatalf("jit audit = %d err=%v", jit, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='auth.login.oidc' AND outcome='success'`).Scan(&logins); err != nil || logins != 1 {
		t.Fatalf("login audit = %d err=%v", logins, err)
	}
}

func TestOIDCSecondLoginReusesIdentityWithoutReprovision(t *testing.T) {
	ctx := auth.ForceDiscoveryRetry(context.Background())
	_, provider, idp, db := newOIDCFixture(t)
	for i := 0; i < 2; i++ {
		intent, query := driveBeginLogin(t, provider, "/")
		code := idp.mint(t, fakeGrant{
			state: query.Get("state"), nonce: query.Get("nonce"), challenge: query.Get("code_challenge"),
			sub: "subject-jane", username: "Jane", name: "Jane Doe", email: "jane@corp.example.com",
		})
		result, _, err := provider.CompleteLogin(ctx, auth.LoginCompletion{
			StateCookie: intent.StateCookie, State: query.Get("state"), Code: code,
		})
		if err != nil {
			t.Fatalf("login %d: %v", i, err)
		}
		_ = result
	}
	if users := countUsers(t, db); users != 2 { // admin + jane
		t.Fatalf("users = %d, the identity must not re-provision", users)
	}
}

func TestOIDCStateMismatchIsRejected(t *testing.T) {
	ctx := auth.ForceDiscoveryRetry(context.Background())
	_, provider, idp, _ := newOIDCFixture(t)
	intent, query := driveBeginLogin(t, provider, "/")
	code := idp.mint(t, fakeGrant{
		state: "forged-state", nonce: query.Get("nonce"), challenge: query.Get("code_challenge"),
		sub: "subject-jane", username: "Jane",
	})
	if _, _, err := provider.CompleteLogin(ctx, auth.LoginCompletion{
		StateCookie: intent.StateCookie, State: "forged-state", Code: code,
	}); !errors.Is(err, auth.ErrOIDCStateInvalid) {
		t.Fatalf("forged state must be rejected, got %v", err)
	}
}

func TestOIDCNonceMismatchIsRejected(t *testing.T) {
	ctx := auth.ForceDiscoveryRetry(context.Background())
	_, provider, idp, _ := newOIDCFixture(t)
	intent, query := driveBeginLogin(t, provider, "/")
	code := idp.mint(t, fakeGrant{
		state: query.Get("state"), nonce: "wrong-nonce", challenge: query.Get("code_challenge"),
		sub: "subject-jane", username: "Jane",
	})
	if _, _, err := provider.CompleteLogin(ctx, auth.LoginCompletion{
		StateCookie: intent.StateCookie, State: query.Get("state"), Code: code,
	}); !errors.Is(err, auth.ErrOIDCRejected) {
		t.Fatalf("nonce mismatch must be rejected, got %v", err)
	}
}

func TestOIDCDisabledAccountIsRejected(t *testing.T) {
	ctx := auth.ForceDiscoveryRetry(context.Background())
	_, provider, idp, db := newOIDCFixture(t)
	intent, query := driveBeginLogin(t, provider, "/")
	code := idp.mint(t, fakeGrant{
		state: query.Get("state"), nonce: query.Get("nonce"), challenge: query.Get("code_challenge"),
		sub: "subject-jane", username: "Jane",
	})
	if _, _, err := provider.CompleteLogin(ctx, auth.LoginCompletion{
		StateCookie: intent.StateCookie, State: query.Get("state"), Code: code,
	}); err != nil {
		t.Fatal(err)
	}
	if err := disableUser(t, db, "jane"); err != nil {
		t.Fatal(err)
	}
	intent, query = driveBeginLogin(t, provider, "/")
	code = idp.mint(t, fakeGrant{
		state: query.Get("state"), nonce: query.Get("nonce"), challenge: query.Get("code_challenge"),
		sub: "subject-jane", username: "Jane",
	})
	if _, _, err := provider.CompleteLogin(ctx, auth.LoginCompletion{
		StateCookie: intent.StateCookie, State: query.Get("state"), Code: code,
	}); !errors.Is(err, auth.ErrAccountDisabled) {
		t.Fatalf("disabled account must be rejected despite the IdP verdict, got %v", err)
	}
}

func TestOIDCUsernameCollisionSuffixesDeterministically(t *testing.T) {
	ctx := auth.ForceDiscoveryRetry(context.Background())
	service, provider, idp, db := newOIDCFixture(t)
	// A local account already owns the name.
	admin := mustSession(t, service)
	if _, _, err := service.CreateUser(ctx, admin, auth.CreateUserInput{
		ClientCommandID: "cmd-create-jane", Digest: auth.DigestCommand("user.create", map[string]any{"username": "jane"}),
		Username: "jane", DisplayName: "Local Jane", Role: "operator", Password: "Local jane passphrase 2027!",
		Contacts: []auth.ContactInput{{Channel: "email", Target: "jane@local.test"}},
	}); err != nil {
		t.Fatal(err)
	}
	intent, query := driveBeginLogin(t, provider, "/")
	code := idp.mint(t, fakeGrant{
		state: query.Get("state"), nonce: query.Get("nonce"), challenge: query.Get("code_challenge"),
		sub: "subject-jane", username: "Jane",
	})
	result, _, err := provider.CompleteLogin(ctx, auth.LoginCompletion{
		StateCookie: intent.StateCookie, State: query.Get("state"), Code: code,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.User.Username != "jane1" {
		t.Fatalf("collision suffix = %q, want jane1", result.User.Username)
	}
	if users := countUsers(t, db); users != 3 { // admin + local jane + oidc jane1
		t.Fatalf("users = %d", users)
	}
}

func TestOIDCProviderDownFailsWithoutBlocking(t *testing.T) {
	service, _, _, _ := newOIDCFixture(t)
	dead := &auth.OIDCProvider{
		Service:  service,
		Issuer:   "http://127.0.0.1:1", // nothing listens here
		StateKey: []byte(strings.Repeat("s", 32)),
	}
	ctx := context.Background()
	if _, err := dead.BeginLogin(ctx, "/"); !errors.Is(err, auth.ErrOIDCProviderDown) {
		t.Fatalf("dead IdP must answer ErrOIDCProviderDown, got %v", err)
	}
	// The local channel keeps working through the same outage.
	if _, _, bearer := loginPassword(t, service, "admin", fixtureAdminPassword); bearer == "" {
		t.Fatal("local login must survive the IdP outage")
	}
}
