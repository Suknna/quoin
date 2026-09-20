package app

// Shared real-authentication fixture for the internal tests (package app,
// ADR-0010 single-step surface). The bootstrap administrator is unlocked
// through the real path (initial random password + forced change) and every
// login goes through the real single-step HTTP call or the equivalent
// service call. No direct-login shortcut exists here.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

const scenarioSessionCookieName = "__Host-quoin-session"

// newScenarioAuth opens the real database for an already-bootstrapped config
// and wires a fresh auth service with the pending bootstrap administrator
// seeded. The caller owns the database lifetime.
func newScenarioAuth(t *testing.T, dataDirectory, rootKeyFile string) (*bootstrap.Database, *auth.Service, string) {
	t.Helper()
	database, err := bootstrap.OpenDatabase(context.Background(), dataDirectory, rootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	initial, err := auth.GenerateInitialPassword()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureBootstrapAdmin(context.Background(), initial, time.Now().UTC().Add(auth.InitialPasswordLifetime)); err != nil {
		t.Fatal(err)
	}
	return database, service, initial
}

// scenarioInitializeAdmin unlocks the bootstrap administrator through the
// service — the same steps the restricted HTTP session drives.
func scenarioInitializeAdmin(t *testing.T, service *auth.Service, initialPassword, formalPassword string) {
	t.Helper()
	ctx := context.Background()
	result, err := service.LoginWithPassword(ctx, "admin", initialPassword, "Test on Linux")
	if err != nil {
		t.Fatalf("bootstrap login: %v", err)
	}
	session, err := service.Authenticate(ctx, result.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(ctx, session, initialPassword, formalPassword); err != nil {
		t.Fatalf("forced password change: %v", err)
	}
}

// scenarioLogin performs the real single-step login at the service level and
// returns the session bearer.
func scenarioLogin(t *testing.T, service *auth.Service, username, password string) string {
	t.Helper()
	result, err := service.LoginWithPassword(context.Background(), username, password, "scenario-agent")
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	return result.Bearer
}

// scenarioLoginCookie performs the real single-step login over an HTTP
// handler (recorder style) and returns the issued session cookie.
func scenarioLoginCookie(t *testing.T, handler http.Handler, origin, username, password string) *http.Cookie {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"`+username+`","password":`+scenarioQuote(password)+`}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if session := scenarioSetCookie(recorder.Result().Cookies(), scenarioSessionCookieName); session != nil {
		return session
	}
	t.Fatal("login issued no session cookie")
	return nil
}

// scenarioSetCookie finds the cookie of the given name in a response's cookies.
func scenarioSetCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

// scenarioQuote renders a JSON string literal.
func scenarioQuote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

// scenarioRequestContext builds the request context exactly as the admission
// guard does for an authenticated user request: fresh correlation and request
// ids, the user actor with initiator equal to actor, the HTTP entry source
// and the real session reference. Direct handler calls that reach audited
// operations must inject it — an absent execution context must fail closed
// (ADR-0006), never fall back to an anonymous default.
func scenarioRequestContext(t *testing.T, session auth.Session) context.Context {
	t.Helper()
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := execution.NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: session.User.ID},
		Initiator:     execution.Principal{Kind: execution.PrincipalUser, ID: session.User.ID},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: requestID},
		Session:       execution.SessionRef{ID: session.ID, AuthRevision: session.User.AuthRevision},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}
