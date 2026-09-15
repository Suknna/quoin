package app

// Shared real-authentication fixture for the legacy internal tests (package
// app). The two-step login replaced the one-shot password login, so these
// helpers drive the real production path: the pending bootstrap administrator
// is initialized through the real flow steps, and every login goes through
// the real HTTP flow (password start, OTP challenge, code verify) or the
// equivalent service calls — with the TEST-ONLY recording stubSender
// capturing the OTP codes. No direct-login shortcut exists here.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

const (
	scenarioAdminEmail        = "admin@quoin.test"
	scenarioSessionCookieName = "__Host-quoin-session"
)

// newScenarioAuth opens the real database for an already-bootstrapped config
// and wires a fresh auth service to a recording sender with the pending
// bootstrap administrator seeded. The caller owns the database lifetime.
func newScenarioAuth(t *testing.T, dataDirectory, rootKeyFile string) (*bootstrap.Database, *auth.Service, *stubSender) {
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
	sender := &stubSender{}
	if err := service.ConfigureAuth(auth.AuthConfig{OTPKey: bytes.Repeat([]byte{0x41}, 32), Sender: sender}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureBootstrapAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	return database, service, sender
}

// scenarioInitializeAdmin completes the real admin initialization through the
// service — the same steps the HTTP wizard drives (formal password plus a
// verified contact).
func scenarioInitializeAdmin(t *testing.T, service *auth.Service, sender *stubSender, formalPassword string) {
	t.Helper()
	ctx := context.Background()
	flow, _, err := service.StartAdminInitialization(ctx, "admin", "admin")
	if err != nil {
		t.Fatalf("start admin initialization: %v", err)
	}
	if err := service.SetFlowPassword(ctx, flow.Bearer, formalPassword); err != nil {
		t.Fatalf("set initialization password: %v", err)
	}
	masked, err := service.RegisterFlowContact(ctx, flow.Bearer, "email", scenarioAdminEmail)
	if err != nil {
		t.Fatalf("register initialization contact: %v", err)
	}
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, masked.Locator); err != nil {
		t.Fatalf("send initialization challenge: %v", err)
	}
	if err := service.VerifyFlowChallenge(ctx, flow.Bearer, sender.lastCode()); err != nil {
		t.Fatalf("verify initialization challenge: %v", err)
	}
	if err := service.CompleteAdminInitialization(ctx, flow.Bearer); err != nil {
		t.Fatalf("complete admin initialization: %v", err)
	}
}

// scenarioTwoStepLogin performs the real two-step login at the service level
// and returns the session bearer.
func scenarioTwoStepLogin(t *testing.T, service *auth.Service, sender *stubSender, username, password string) string {
	t.Helper()
	ctx := context.Background()
	flow, _, err := service.StartAuthentication(ctx, username, password, "scenario-agent")
	if err != nil {
		t.Fatalf("start authentication: %v", err)
	}
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, flow.Contacts[0].Locator); err != nil {
		t.Fatalf("send login challenge: %v", err)
	}
	result, err := service.CompleteLogin(ctx, flow.Bearer, sender.lastCode())
	if err != nil {
		t.Fatalf("complete login: %v", err)
	}
	return result.Bearer
}

// scenarioLoginCookie performs the real two-step login over an HTTP handler
// (recorder style) and returns the issued session cookie.
func scenarioLoginCookie(t *testing.T, handler http.Handler, origin, username, password string, sender *stubSender) *http.Cookie {
	t.Helper()
	post := func(path, body string, cookies map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", origin)
		for name, value := range cookies {
			request.AddCookie(&http.Cookie{Name: name, Value: value})
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
		return recorder
	}
	start := post("/api/v1/auth/login", `{"username":"`+username+`","password":`+scenarioQuote(password)+`}`, nil)
	var flow struct {
		Contacts []struct {
			ID string `json:"id"`
		} `json:"contacts"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &flow); err != nil {
		t.Fatal(err)
	}
	if len(flow.Contacts) == 0 {
		t.Fatalf("login flow without a deliverable contact: %s", start.Body.String())
	}
	flowCookie := scenarioSetCookie(start.Result().Cookies(), flowCookieName)
	if flowCookie == nil {
		t.Fatalf("login issued no %s cookie", flowCookieName)
	}
	cookies := map[string]string{flowCookieName: flowCookie.Value}
	post("/api/v1/auth/flow/challenge", `{"contactId":"`+flow.Contacts[0].ID+`"}`, cookies)
	verified := post("/api/v1/auth/flow/verify", `{"code":"`+sender.lastCode()+`"}`, cookies)
	if session := scenarioSetCookie(verified.Result().Cookies(), scenarioSessionCookieName); session != nil {
		return session
	}
	t.Fatal("verified login issued no session cookie")
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
