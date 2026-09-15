package operations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

// fakeSink records access facts and can be told to fail.
type fakeSink struct {
	mu      sync.Mutex
	facts   []AccessFact
	failAll bool
}

func (sink *fakeSink) RecordAccess(_ context.Context, fact AccessFact) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.failAll {
		return errors.New("audit storage unavailable")
	}
	sink.facts = append(sink.facts, fact)
	return nil
}

func (sink *fakeSink) recorded() []AccessFact {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]AccessFact(nil), sink.facts...)
}

// resolveSession mirrors the auth seam: clean sentinel for unknown
// credentials, infrastructure failure for database faults.
func resolveSession(ctx context.Context, credential string) (Subject, error) {
	switch credential {
	case "valid-session":
		return Subject{UserID: 7, Role: "admin", Initialized: true, SessionID: 101, AuthRevision: 3}, nil
	case "operator-session":
		return Subject{UserID: 8, Role: "operator", Initialized: true, SessionID: 102, AuthRevision: 1}, nil
	case "restricted-session":
		return Subject{UserID: 9, Role: "admin", Initialized: true, PasswordChangeRequired: true, SessionID: 103, AuthRevision: 2}, nil
	case "uninitialized-session":
		return Subject{UserID: 10, Role: "admin", Initialized: false, SessionID: 104, AuthRevision: 1}, nil
	case "broken-session":
		return Subject{}, errors.New("database unavailable")
	}
	return Subject{}, fmt.Errorf("bad credential: %w", ErrUnauthenticated)
}

func resolveFlow(ctx context.Context, credential string) (FlowIdentity, error) {
	switch credential {
	case "login-flow":
		return FlowIdentity{Type: "login", CorrelationID: "login-flow-correlation", UserID: 42}, nil
	case "init-flow":
		return FlowIdentity{Type: "admin_initialize", CorrelationID: "init-flow-correlation", UserID: 33}, nil
	case "userless-flow":
		return FlowIdentity{Type: "login", CorrelationID: "userless-correlation"}, nil
	case "recovery-flow":
		return FlowIdentity{Type: "recovery", CorrelationID: "recovery-flow-correlation", UserID: 7}, nil
	case "expired-flow":
		return FlowIdentity{}, fmt.Errorf("expired: %w", ErrUnauthenticated)
	case "broken-flow":
		return FlowIdentity{}, errors.New("database unavailable")
	}
	return FlowIdentity{}, fmt.Errorf("unknown flow: %w", ErrUnauthenticated)
}

var testDeclarations = []Declaration{
	{ID: "getCurrentUser", Method: http.MethodGet, Path: "/api/v1/auth/me", Level: LevelSession, Kind: KindQuery, ObjectType: "user"},
	{ID: "getAdminAbout", Method: http.MethodGet, Path: "/api/v1/admin/about", Level: LevelAdmin, Kind: KindQuery},
	{ID: "listBusinessContext", Method: http.MethodGet, Path: "/api/v1/business-context", Level: LevelFull, Kind: KindQuery},
	{ID: "readAuthenticationFlow", Method: http.MethodGet, Path: "/api/v1/auth/flow", Level: LevelFlow, Kind: KindQuery, ObjectType: "auth_flow"},
	{ID: "startAuthentication", Method: http.MethodPost, Path: "/api/v1/auth/login", Level: LevelPublic, Kind: KindCommand},
	{ID: "readAuthDelivery", Method: http.MethodGet, Path: "/api/v1/auth/flow/delivery", Level: LevelFlowOrAdmin, Kind: KindQuery, ObjectType: "auth_delivery"},
}

func newTestRegistry(t *testing.T, extra ...Declaration) *AccessRegistry {
	t.Helper()
	registry, err := NewAccessRegistry(append(append([]Declaration(nil), testDeclarations...), extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func mustAdmission(t *testing.T, registry *AccessRegistry, sink Sink) *Admission {
	t.Helper()
	admission, err := NewAdmission(AdmissionDeps{
		Registry: registry, Sessions: resolveSession, Flows: resolveFlow, Sink: sink,
		OnSinkError: func(AccessFact, error) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	return admission
}

// testInput/testOutput keep the huma.Register handler shape uniform.
type testInput struct {
	Session string `cookie:"__Host-quoin-session"`
	Flow    string `cookie:"__Host-quoin-flow"`
}

type testOutput struct {
	Body string `json:"body"`
}

func okHandler(t *testing.T) func(ctx context.Context, input *testInput) (*testOutput, error) {
	t.Helper()
	return func(ctx context.Context, input *testInput) (*testOutput, error) {
		if _, ok := execution.FromContext(ctx); !ok {
			return nil, errors.New("execution metadata missing in handler context")
		}
		return &testOutput{Body: "ok"}, nil
	}
}

// newTestAPI builds a Huma API with the guard installed before registration,
// mirroring the NewHandler adoption order.
func newTestAPI(t *testing.T, admission *Admission, declaration Declaration, handler func(ctx context.Context, input *testInput) (*testOutput, error)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	config := huma.DefaultConfig("test", "1.0.0")
	config.OpenAPIPath, config.DocsPath, config.SchemasPath = "", "", ""
	api := humago.New(mux, config)
	api.UseMiddleware(admission.HumaMiddleware())
	huma.Register(api, huma.Operation{Method: declaration.Method, Path: declaration.Path, OperationID: declaration.ID}, handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func doGet(t *testing.T, server *httptest.Server, path string, cookies ...string) *http.Response {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
	for _, cookie := range cookies {
		request.Header.Add("Cookie", cookie)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

const (
	sessionCookie   = "__Host-quoin-session=valid-session"
	operatorCookie  = "__Host-quoin-session=operator-session"
	restrictedCook  = "__Host-quoin-session=restricted-session"
	uninitCookie    = "__Host-quoin-session=uninitialized-session"
	brokenCookie    = "__Host-quoin-session=broken-session"
	expiredCookie   = "__Host-quoin-session=expired-credential"
	loginFlowCook   = "__Host-quoin-flow=login-flow"
	initFlowCookie  = "__Host-quoin-flow=init-flow"
	expiredFlowCook = "__Host-quoin-flow=expired-flow"
	brokenFlowCook  = "__Host-quoin-flow=broken-flow"
)

func TestValidateSurfaceRejectsUndeclaredOperation(t *testing.T) {
	registry, err := NewAccessRegistry()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	config := huma.DefaultConfig("test", "1.0.0")
	config.OpenAPIPath, config.DocsPath, config.SchemasPath = "", "", ""
	api := humago.New(mux, config)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/login", OperationID: "login"}, func(ctx context.Context, input *struct{}) (*struct{}, error) {
		return nil, nil
	})
	err = registry.ValidateSurface(api)
	if err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("missing declaration must fail construction validation, got %v", err)
	}
}

func TestValidateSurfaceRejectsMethodPathDrift(t *testing.T) {
	registry, err := NewAccessRegistry(Declaration{ID: "login", Method: http.MethodGet, Path: "/api/v1/auth/login", Level: LevelPublic, Kind: KindCommand})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	config := huma.DefaultConfig("test", "1.0.0")
	config.OpenAPIPath, config.DocsPath, config.SchemasPath = "", "", ""
	api := humago.New(mux, config)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/login", OperationID: "login"}, func(ctx context.Context, input *struct{}) (*struct{}, error) {
		return nil, nil
	})
	err = registry.ValidateSurface(api)
	if err == nil || !strings.Contains(err.Error(), "declared as GET") {
		t.Fatalf("declaration drift must fail validation, got %v", err)
	}
}

func TestValidateSurfaceRejectsUnregisteredDeclaration(t *testing.T) {
	registry, err := NewAccessRegistry(Declaration{ID: "retiredOperation", Method: http.MethodPost, Path: "/api/v1/retired", Level: LevelAdmin, Kind: KindCommand})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	config := huma.DefaultConfig("test", "1.0.0")
	config.OpenAPIPath, config.DocsPath, config.SchemasPath = "", "", ""
	api := humago.New(mux, config)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/login", OperationID: "login"}, func(ctx context.Context, input *struct{}) (*struct{}, error) {
		return nil, nil
	})
	if err := registry.ValidateSurface(api); err == nil {
		t.Fatal("a non-planned declaration without its registered route must fail validation")
	}
}

func TestValidateSurfaceAcceptsPlannedDeclarationAndExactCoverage(t *testing.T) {
	registry, err := NewAccessRegistry(
		Declaration{ID: "login", Method: http.MethodPost, Path: "/api/v1/auth/login", Level: LevelPublic, Kind: KindCommand},
		Declaration{ID: "futureOperation", Method: http.MethodPost, Path: "/api/v1/future", Level: LevelAdmin, Kind: KindCommand, Planned: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	config := huma.DefaultConfig("test", "1.0.0")
	config.OpenAPIPath, config.DocsPath, config.SchemasPath = "", "", ""
	api := humago.New(mux, config)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/login", OperationID: "login"}, func(ctx context.Context, input *struct{}) (*struct{}, error) {
		return nil, nil
	})
	if err := registry.ValidateSurface(api); err != nil {
		t.Fatalf("planned declarations must not fail surface validation: %v", err)
	}
	if planned := registry.RemainingPlanned(); len(planned) != 1 || planned[0] != "futureOperation" {
		t.Fatalf("RemainingPlanned = %v, want [futureOperation]", planned)
	}
}

func TestNewAdmissionFailsClosedOnMissingDependencies(t *testing.T) {
	registry := newTestRegistry(t)
	if _, err := NewAdmission(AdmissionDeps{Registry: registry, Sessions: resolveSession}); err == nil {
		t.Fatal("missing sink must fail construction")
	}
	if _, err := NewAdmission(AdmissionDeps{Registry: registry, Sink: &fakeSink{}}); err == nil {
		t.Fatal("missing session resolver must fail construction")
	}
	flowless, err := NewAccessRegistry(Declaration{ID: "getCurrentUser", Method: http.MethodGet, Path: "/api/v1/auth/me", Level: LevelSession, Kind: KindQuery})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAdmission(AdmissionDeps{Registry: flowless, Sessions: resolveSession, Sink: &fakeSink{}}); err != nil {
		t.Fatalf("flow resolver is only required with flow declarations: %v", err)
	}
	if _, err := NewAdmission(AdmissionDeps{Registry: registry, Sessions: resolveSession, Sink: &fakeSink{}}); err == nil {
		t.Fatal("flow declarations without a flow resolver must fail construction")
	}
}

func TestGuardEnforcesLevels(t *testing.T) {
	sessionLevel := testDeclarations[0]
	adminLevel := testDeclarations[1]
	fullLevel := testDeclarations[2]

	t.Run("session level admits any valid session", func(t *testing.T) {
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), &fakeSink{}), sessionLevel, okHandler(t))
		for _, credential := range []string{sessionCookie, restrictedCook, operatorCookie} {
			response := doGet(t, server, sessionLevel.Path, credential)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", credential, response.StatusCode)
			}
		}
	})

	t.Run("session level rejects anonymous with 401 and no audit row", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), sessionLevel, okHandler(t))
		response := doGet(t, server, sessionLevel.Path)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", response.StatusCode)
		}
		if facts := sink.recorded(); len(facts) != 0 {
			t.Fatalf("anonymous rejections must stay out of the business audit table, got %+v", facts)
		}
	})

	t.Run("session resolution failure answers 503", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), sessionLevel, okHandler(t))
		response := doGet(t, server, sessionLevel.Path, brokenCookie)
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", response.StatusCode)
		}
		if facts := sink.recorded(); len(facts) != 0 {
			t.Fatalf("unresolved identity must not be attributed, got %+v", facts)
		}
	})

	t.Run("admin level rejects non-admin with recorded denial", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), adminLevel, okHandler(t))
		response := doGet(t, server, adminLevel.Path, operatorCookie)
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", response.StatusCode)
		}
		facts := sink.recorded()
		if len(facts) != 1 || facts[0].Outcome != OutcomeDenied || facts[0].Status != http.StatusForbidden || facts[0].ActorUserID != 8 {
			t.Fatalf("denial facts = %+v, want one denied record for user 8", facts)
		}
	})

	t.Run("full level rejects restricted and uninitialized sessions", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), fullLevel, okHandler(t))
		for _, credential := range []string{restrictedCook, uninitCookie} {
			response := doGet(t, server, fullLevel.Path, credential)
			response.Body.Close()
			if response.StatusCode != http.StatusForbidden {
				t.Fatalf("%s: status = %d, want 403", credential, response.StatusCode)
			}
		}
		response := doGet(t, server, fullLevel.Path, expiredCookie)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expired credential: status = %d, want 401", response.StatusCode)
		}
	})
}

func TestGuardRecordsGrantedAccessAndEnrichesContext(t *testing.T) {
	sink := &fakeSink{}
	declaration := testDeclarations[0]
	var handlerCorrelation, handlerRequestID string
	server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, func(ctx context.Context, input *testInput) (*testOutput, error) {
		meta, ok := execution.FromContext(ctx)
		if !ok {
			return nil, errors.New("execution metadata missing in handler context")
		}
		handlerCorrelation, handlerRequestID = meta.CorrelationID, meta.Source.RequestID
		if meta.Actor.Kind != execution.PrincipalUser || meta.Actor.ID != 7 {
			t.Errorf("handler actor = %+v, want user 7", meta.Actor)
		}
		if meta.Source.Kind != execution.SourceHTTP {
			t.Errorf("handler source = %+v, want http", meta.Source)
		}
		if meta.Session.ID != 101 || meta.Session.AuthRevision != 3 {
			t.Errorf("handler session reference = %+v, want session 101 at revision 3", meta.Session)
		}
		return &testOutput{Body: "ok"}, nil
	})

	request, _ := http.NewRequest(http.MethodGet, server.URL+declaration.Path, nil)
	// Client-supplied identifiers must never be adopted as the correlation.
	request.Header.Set("Cookie", sessionCookie)
	request.Header.Set("X-Correlation-Id", "client-forged-correlation")
	request.Header.Set("X-Request-Id", "client-forged-request")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	facts := sink.recorded()
	if len(facts) != 1 {
		t.Fatalf("recorded %d access facts, want exactly the pre-dispatch one", len(facts))
	}
	fact := facts[0]
	if fact.Outcome != OutcomeGranted || fact.OperationID != declaration.ID || fact.ActorUserID != 7 {
		t.Errorf("access fact = %+v", fact)
	}
	if fact.CorrelationID == "" || fact.CorrelationID == "client-forged-correlation" {
		t.Errorf("correlation id must be server-generated, got %q", fact.CorrelationID)
	}
	if handlerCorrelation != fact.CorrelationID || handlerRequestID != fact.RequestID {
		t.Errorf("handler saw correlation %q/request %q, want the guard values %q/%q", handlerCorrelation, handlerRequestID, fact.CorrelationID, fact.RequestID)
	}
	if fact.Level != LevelSession || fact.Kind != KindQuery || fact.ObjectType != "user" {
		t.Errorf("classification missing from access fact: %+v", fact)
	}
}

func TestGuardFailsClosedWhenSinkRejects(t *testing.T) {
	sink := &fakeSink{failAll: true}
	declaration := testDeclarations[0]
	handlerRan := false
	server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, func(ctx context.Context, input *testInput) (*testOutput, error) {
		handlerRan = true
		return &testOutput{Body: "leaked"}, nil
	})
	response := doGet(t, server, declaration.Path, sessionCookie)
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the access record fails", response.StatusCode)
	}
	if handlerRan {
		t.Fatal("handler must not run when the access record cannot be persisted")
	}
}

func TestGuardSeparatesHandlerDenialFromRequestFailure(t *testing.T) {
	declaration := testDeclarations[0]
	t.Run("handler 4xx records denial", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, func(ctx context.Context, input *testInput) (*testOutput, error) {
			return nil, huma.Error403Forbidden("需要管理员权限")
		})
		response := doGet(t, server, declaration.Path, sessionCookie)
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", response.StatusCode)
		}
		facts := sink.recorded()
		if len(facts) != 2 {
			t.Fatalf("recorded %d facts, want access + denial", len(facts))
		}
		denial := facts[1]
		if denial.Outcome != OutcomeDenied || denial.Status != http.StatusForbidden {
			t.Errorf("denial fact = %+v", denial)
		}
		if denial.CorrelationID != facts[0].CorrelationID {
			t.Errorf("denial must share the request correlation")
		}
	})
	t.Run("handler 5xx records request failure, not denial", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, func(ctx context.Context, input *testInput) (*testOutput, error) {
			return nil, huma.Error500InternalServerError("暂时不可用")
		})
		response := doGet(t, server, declaration.Path, sessionCookie)
		response.Body.Close()
		if response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", response.StatusCode)
		}
		facts := sink.recorded()
		if len(facts) != 2 || facts[1].Outcome != OutcomeRequestFailure || facts[1].Status != http.StatusInternalServerError {
			t.Fatalf("request failure facts = %+v, want access + request_failure", facts)
		}
	})
}

func TestFlowOperationsRequireValidFlowBeforeHandler(t *testing.T) {
	declaration := testDeclarations[3] // readAuthenticationFlow, LevelFlow

	t.Run("absent flow rejected with 401 before handler", func(t *testing.T) {
		sink := &fakeSink{}
		handlerRan := false
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, func(ctx context.Context, input *testInput) (*testOutput, error) {
			handlerRan = true
			return &testOutput{}, nil
		})
		response := doGet(t, server, declaration.Path)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", response.StatusCode)
		}
		if handlerRan {
			t.Fatal("handler must not run without a flow credential")
		}
		if facts := sink.recorded(); len(facts) != 0 {
			t.Fatalf("rejected flows must not emit business audit facts, got %+v", facts)
		}
	})

	t.Run("expired flow rejected with 401 before handler", func(t *testing.T) {
		sink := &fakeSink{}
		handlerRan := false
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, func(ctx context.Context, input *testInput) (*testOutput, error) {
			handlerRan = true
			return &testOutput{}, nil
		})
		response := doGet(t, server, declaration.Path, expiredFlowCook)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", response.StatusCode)
		}
		if handlerRan {
			t.Fatal("handler must not run for an expired flow")
		}
	})

	t.Run("flow resolution failure answers 503", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, okHandler(t))
		response := doGet(t, server, declaration.Path, brokenFlowCook)
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", response.StatusCode)
		}
	})

	t.Run("valid flow resumes stored correlation", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, func(ctx context.Context, input *testInput) (*testOutput, error) {
			meta, ok := execution.FromContext(ctx)
			if !ok {
				return nil, errors.New("missing correlation metadata")
			}
			if meta.CorrelationID != "login-flow-correlation" {
				t.Errorf("flow operation must resume the stored correlation, got %q", meta.CorrelationID)
			}
			if meta.Actor.Kind != execution.PrincipalUser || meta.Actor.ID != 42 {
				t.Errorf("flow actor = %+v, want the flow's bound user 42", meta.Actor)
			}
			if meta.Session.ID != 0 || meta.Session.AuthRevision != 0 {
				t.Errorf("flow identity must not carry a session reference, got %+v", meta.Session)
			}
			return &testOutput{Body: "ok"}, nil
		})
		response := doGet(t, server, declaration.Path, loginFlowCook)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.StatusCode)
		}
		facts := sink.recorded()
		if len(facts) != 1 || facts[0].Outcome != OutcomeGranted || facts[0].ActorUserID != 42 || facts[0].CorrelationID != "login-flow-correlation" {
			t.Fatalf("flow access facts = %+v, want one granted record for the flow user", facts)
		}
	})

	t.Run("user-less flow rejected with 401", func(t *testing.T) {
		sink := &fakeSink{}
		handlerRan := false
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, func(ctx context.Context, input *testInput) (*testOutput, error) {
			handlerRan = true
			return &testOutput{}, nil
		})
		response := doGet(t, server, declaration.Path, "__Host-quoin-flow=userless-flow")
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 for a flow without a bound user", response.StatusCode)
		}
		if handlerRan {
			t.Fatal("handler must not run for a user-less flow")
		}
		if facts := sink.recorded(); len(facts) != 0 {
			t.Fatalf("unattributable flows must not emit facts, got %+v", facts)
		}
	})
}

func TestFlowOrAdminAcceptsAdminSessionAndTypedFlowsOnly(t *testing.T) {
	declaration := testDeclarations[5] // readAuthDelivery, LevelFlowOrAdmin

	t.Run("admin session admitted with fresh correlation", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, func(ctx context.Context, input *testInput) (*testOutput, error) {
			meta, _ := execution.FromContext(ctx)
			if meta.CorrelationID == "login-flow-correlation" || meta.CorrelationID == "" {
				t.Errorf("session path must keep the request correlation, got %q", meta.CorrelationID)
			}
			return &testOutput{Body: "ok"}, nil
		})
		response := doGet(t, server, declaration.Path, sessionCookie)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.StatusCode)
		}
	})

	t.Run("non-admin session rejected with recorded denial", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, okHandler(t))
		response := doGet(t, server, declaration.Path, operatorCookie)
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", response.StatusCode)
		}
		if facts := sink.recorded(); len(facts) != 1 || facts[0].Outcome != OutcomeDenied {
			t.Fatalf("facts = %+v, want one denial", sink.recorded())
		}
	})

	t.Run("initialization flow admitted without session", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, func(ctx context.Context, input *testInput) (*testOutput, error) {
			meta, _ := execution.FromContext(ctx)
			if meta.CorrelationID != "init-flow-correlation" {
				t.Errorf("flow path must resume the stored correlation, got %q", meta.CorrelationID)
			}
			return &testOutput{Body: "ok"}, nil
		})
		response := doGet(t, server, declaration.Path, initFlowCookie)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.StatusCode)
		}
		facts := sink.recorded()
		if len(facts) != 1 || facts[0].Outcome != OutcomeGranted || facts[0].ActorUserID != 33 {
			t.Fatalf("flow-or-admin facts = %+v, want one granted record for the flow user 33", facts)
		}
	})

	t.Run("login flow type rejected with 403", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, okHandler(t))
		response := doGet(t, server, declaration.Path, loginFlowCook)
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 for a login flow on delivery management", response.StatusCode)
		}
		facts := sink.recorded()
		if len(facts) != 1 || facts[0].Outcome != OutcomeDenied || facts[0].ActorUserID != 42 || facts[0].Status != http.StatusForbidden {
			t.Fatalf("wrong-type flow facts = %+v, want one denial for the claimed flow user 42", facts)
		}
	})

	t.Run("neither credential rejected with 401", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, okHandler(t))
		response := doGet(t, server, declaration.Path)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", response.StatusCode)
		}
	})
}

func TestPublicOperationProceedsWithoutAudit(t *testing.T) {
	sink := &fakeSink{}
	declaration := testDeclarations[4] // startAuthentication, LevelPublic
	server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, func(ctx context.Context, input *testInput) (*testOutput, error) {
		meta, ok := execution.FromContext(ctx)
		if !ok {
			return nil, errors.New("missing correlation metadata")
		}
		if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
			t.Errorf("pre-identity actor = %+v, want system 0", meta.Actor)
		}
		return &testOutput{Body: "ok"}, nil
	})
	request, _ := http.NewRequest(http.MethodPost, server.URL+declaration.Path, nil)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if facts := sink.recorded(); len(facts) != 0 {
		t.Fatalf("public surface must stay on bounded logging, got %+v", facts)
	}
}

func TestRawWrapFailsClosedAndStreamsUnbuffered(t *testing.T) {
	declaration := Declaration{ID: "streamTaskEvents", Method: http.MethodGet, Path: "/api/v1/tasks/events", Level: LevelSession, Kind: KindStream}
	registry := newTestRegistry(t, declaration)
	newServer := func(sink Sink) *httptest.Server {
		wrapped, err := mustAdmission(t, registry, sink).Wrap(declaration.ID, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if _, ok := execution.FromContext(request.Context()); !ok {
				http.Error(writer, "missing correlation metadata", http.StatusInternalServerError)
				return
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = writer.Write([]byte("data: first\n\n"))
		}))
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(wrapped)
		t.Cleanup(server.Close)
		return server
	}

	// Sink failure rejects the request before the handler streams anything.
	failing := &fakeSink{failAll: true}
	failingServer := newServer(failing)
	failingRequest, _ := http.NewRequest(http.MethodGet, failingServer.URL+declaration.Path, nil)
	failingRequest.Header.Set("Cookie", sessionCookie)
	response, err := failingServer.Client().Do(failingRequest)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the access record fails", response.StatusCode)
	}

	sink := &fakeSink{}
	streaming := newServer(sink)
	request, _ := http.NewRequest(http.MethodGet, streaming.URL+declaration.Path, nil)
	request.Header.Set("Cookie", sessionCookie)
	response, err = streaming.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	buffer := make([]byte, 16)
	read, _ := response.Body.Read(buffer)
	if string(buffer[:read]) != "data: first\n\n" {
		t.Errorf("streamed payload = %q", string(buffer[:read]))
	}
	if facts := sink.recorded(); len(facts) != 1 || facts[0].Outcome != OutcomeGranted {
		t.Errorf("stream access facts = %+v, want exactly one granted record", sink.recorded())
	}
}

func TestRawWrapRecordsDenialAndRejectsUndeclaredRoute(t *testing.T) {
	declaration := Declaration{ID: "downloadBackup", Method: http.MethodGet, Path: "/api/v1/backups/{backupId}/download", Level: LevelAdmin, Kind: KindSensitiveRead, ObjectType: "backup"}
	registry := newTestRegistry(t, declaration)
	sink := &fakeSink{}
	wrapped, err := mustAdmission(t, registry, sink).Wrap(declaration.ID, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "需要管理员权限", http.StatusForbidden)
	}))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(wrapped)
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/backups/3/download", nil)
	request.Header.Set("Cookie", operatorCookie)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.StatusCode)
	}
	facts := sink.recorded()
	if len(facts) != 1 || facts[0].Outcome != OutcomeDenied || facts[0].Status != http.StatusForbidden || facts[0].Kind != KindSensitiveRead {
		t.Errorf("denial facts = %+v, want one sensitive-read denial", facts)
	}
	if _, err := mustAdmission(t, newTestRegistry(t), &fakeSink{}).Wrap("undeclaredRoute", http.NotFoundHandler()); err == nil {
		t.Fatal("raw routes without declarations must fail wiring")
	}
}

func TestUnknownOperationFailsClosedAtRuntime(t *testing.T) {
	registry, err := NewAccessRegistry()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	config := huma.DefaultConfig("test", "1.0.0")
	config.OpenAPIPath, config.DocsPath, config.SchemasPath = "", "", ""
	api := humago.New(mux, config)
	api.UseMiddleware(mustAdmission(t, registry, &fakeSink{}).HumaMiddleware())
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/unregistered", OperationID: "unregisteredOperation"}, func(ctx context.Context, input *struct{}) (*struct{}, error) {
		return &struct{}{}, nil
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/api/v1/unregistered")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for an operation without a declaration", response.StatusCode)
	}
}

func TestAssertRawSurfaceRequiresEveryDeclaredRawRouteWrapped(t *testing.T) {
	rawDeclarations := []Declaration{
		{ID: "streamTaskEvents", Method: http.MethodGet, Path: "/api/v1/tasks/events", Level: LevelSession, Kind: KindStream, Raw: true},
		{ID: "downloadBackup", Method: http.MethodGet, Path: "/api/v1/backups/{backupId}/download", Level: LevelAdmin, Kind: KindSensitiveRead, ObjectType: "backup", Raw: true},
	}
	registry := newTestRegistry(t, rawDeclarations...)
	sink := &fakeSink{}
	admission := mustAdmission(t, registry, sink)

	// A forgotten wrapper must fail the construction assertion by name.
	err := admission.AssertRawSurface()
	if err == nil || !strings.Contains(err.Error(), "streamTaskEvents") || !strings.Contains(err.Error(), "downloadBackup") {
		t.Fatalf("unwrapped raw routes must fail the assertion, got %v", err)
	}

	wrap := func(id string) {
		t.Helper()
		wrapped, err := admission.Wrap(id, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(wrapped)
		t.Cleanup(server.Close)
	}
	wrap("streamTaskEvents")
	if err := admission.AssertRawSurface(); err == nil || !strings.Contains(err.Error(), "downloadBackup") {
		t.Fatalf("partially wrapped raw surface must fail, got %v", err)
	}

	// A double wrap of the same declaration is a wiring mistake.
	wrap("downloadBackup")
	if _, err := admission.Wrap("downloadBackup", http.NotFoundHandler()); err == nil {
		t.Fatal("double wrapping a raw route must fail wiring")
	}
	if err := admission.AssertRawSurface(); err != nil {
		t.Fatalf("fully wrapped raw surface must pass, got %v", err)
	}
}

func TestAssertRawSurfaceIgnoresHumaOperations(t *testing.T) {
	registry := newTestRegistry(t)
	admission := mustAdmission(t, registry, &fakeSink{})
	if err := admission.AssertRawSurface(); err != nil {
		t.Fatalf("a surface without raw routes needs no wrappers, got %v", err)
	}
}

func TestFlowCorrelatedAdminContinuesStoredCorrelation(t *testing.T) {
	declaration := Declaration{
		ID: "completeContactChange", Method: http.MethodPost, Path: "/api/v1/auth/contact-change/complete",
		Level: LevelAdmin, Kind: KindCommand, ObjectType: "user", FlowCorrelated: true,
	}

	t.Run("admin session keeps identity while resuming the flow correlation", func(t *testing.T) {
		sink := &fakeSink{}
		server := newTestAPI(t, mustAdmission(t, newTestRegistry(t, declaration), sink), declaration, func(ctx context.Context, input *testInput) (*testOutput, error) {
			meta, ok := execution.FromContext(ctx)
			if !ok {
				return nil, errors.New("missing correlation metadata")
			}
			if meta.CorrelationID != "login-flow-correlation" {
				t.Errorf("complete command must continue the stored flow correlation, got %q", meta.CorrelationID)
			}
			if meta.Actor.ID != 7 || meta.Actor.Kind != execution.PrincipalUser {
				t.Errorf("actor = %+v, want the admin session user 7", meta.Actor)
			}
			if meta.Session.ID != 101 || meta.Session.AuthRevision != 3 {
				t.Errorf("session reference = %+v, want the admin session 101 at revision 3", meta.Session)
			}
			return &testOutput{Body: "ok"}, nil
		})
		request, _ := http.NewRequest(http.MethodPost, server.URL+declaration.Path, nil)
		request.Header.Set("Cookie", sessionCookie+"; "+loginFlowCook)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.StatusCode)
		}
		facts := sink.recorded()
		if len(facts) != 1 || facts[0].Outcome != OutcomeGranted || facts[0].ActorUserID != 7 || facts[0].CorrelationID != "login-flow-correlation" {
			t.Fatalf("facts = %+v, want one granted record under the flow correlation", facts)
		}
	})

	t.Run("missing or invalid flow credential rejects before the handler", func(t *testing.T) {
		for _, cookies := range []string{sessionCookie, sessionCookie + "; __Host-quoin-flow=expired-flow"} {
			sink := &fakeSink{}
			handlerRan := false
			server := newTestAPI(t, mustAdmission(t, newTestRegistry(t, declaration), sink), declaration, func(ctx context.Context, input *testInput) (*testOutput, error) {
				handlerRan = true
				return &testOutput{}, nil
			})
			request, _ := http.NewRequest(http.MethodPost, server.URL+declaration.Path, nil)
			request.Header.Set("Cookie", cookies)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%q: status = %d, want 401", cookies, response.StatusCode)
			}
			if handlerRan {
				t.Fatalf("%q: handler must not run without the starting flow", cookies)
			}
			facts := sink.recorded()
			if len(facts) != 1 || facts[0].Outcome != OutcomeDenied || facts[0].ActorUserID != 7 {
				t.Fatalf("%q: facts = %+v, want one denial for the admin", cookies, facts)
			}
		}
	})
}
