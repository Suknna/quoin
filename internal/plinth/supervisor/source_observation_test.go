package supervisor

// Fake-upstream protocol tests for the source observation slice: the metrics
// Discoverer binding executes the descriptor-declared query over a real HTTP
// fake of the Prometheus-compatible API and derives identities exactly from
// the declared label set. The registry binding assertions pin the ADR-0004
// rule that a capability without a bound implementation must never be
// advertised.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/plugins/builtin"
)

func testSupervisorRegistry(t *testing.T) *plugins.Registry {
	t.Helper()
	// Assemble an isolated registry (the same shared builtin source the
	// production host uses, without the agent tool-executor binding) and
	// inject it into the Supervisor, keeping these tests decoupled from the
	// process-global lazy assembly.
	registry := builtin.Registry()
	for _, pluginID := range []string{plugins.PrometheusID, plugins.ThanosID} {
		descriptor, _ := registry.Descriptor(pluginID)
		bundle := plugins.ExecutionBundle{PluginID: pluginID, Location: plugins.LocationPlinthSupervisor}
		for _, capability := range descriptor.Capabilities {
			switch capability {
			case plugins.CapabilityDiscover:
				bundle.Capabilities = append(bundle.Capabilities, capability)
				bundle.Discoverer = &metricsDiscoverer{registry: registry}
			case plugins.CapabilityCollect:
				bundle.Capabilities = append(bundle.Capabilities, capability)
				bundle.Collector = &metricsCollector{}
			}
		}
		if err := registry.RegisterBundle(bundle); err != nil {
			t.Fatal(err)
		}
	}
	supervisor := &Supervisor{Registry: registry}
	registry = supervisor.pluginRegistry()
	bundle, bound := registry.Bundle(plugins.PrometheusID)
	if !bound || bundle.Discoverer == nil || bundle.Location != plugins.LocationPlinthSupervisor {
		t.Fatalf("prometheus bundle wrong: %#v bound=%v", bundle, bound)
	}
	bundle, bound = registry.Bundle(plugins.ThanosID)
	if !bound || bundle.Discoverer == nil {
		t.Fatalf("thanos bundle missing discoverer binding")
	}
	if _, bound := registry.Bundle(plugins.AlertmanagerID); bound {
		t.Fatal("alertmanager must not bind an execution it does not have")
	}
	return registry
}

func fakeMetricsUpstream(t *testing.T, body string, warnings []string, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || r.URL.Query().Get("query") != "up" {
			t.Errorf("upstream received %s?%s; want the frozen discovery query", r.URL.Path, r.URL.RawQuery)
		}
		if len(warnings) != 0 {
			payload, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"result": []any{}}, "warnings": warnings})
			_, _ = w.Write(payload)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func discoverCall(t *testing.T, registry *plugins.Registry, settings, username, password, bearerToken string) (*plugins.DiscoverResult, error) {
	t.Helper()
	bundle, bound := registry.Bundle(plugins.PrometheusID)
	if !bound {
		t.Fatal("prometheus bundle missing")
	}
	// The unified slot->ref->resolve construction: slot names are the refs,
	// resolution failures fail the pass, and nothing downgrades to anonymous.
	call, err := newMetricsCall(testPluginRegistry(), plugins.PrometheusID, json.RawMessage(settings), username, password, bearerToken)
	if err != nil {
		t.Fatal(err)
	}
	return bundle.Discoverer.Discover(context.Background(), call, plugins.DiscoverRequest{ObjectType: "target"})
}

func TestMetricsDiscovererProjectsIdentityAndFiltersNothingSilently(t *testing.T) {
	registry := testSupervisorRegistry(t)
	body := `{"status":"success","data":{"result":[
		{"metric":{"__name__":"up","job":"web","instance":"one:80","pod":"one-v1"}},
		{"metric":{"__name__":"up","job":"api","instance":"two:80"}}]}}`
	server := fakeMetricsUpstream(t, body, nil, http.StatusOK)
	settings := fmt.Sprintf(`{"type":"prometheus","baseUrl":%q,"authType":"basic","username":"metrics-user"}`, server.URL)
	result, err := discoverCall(t, registry, settings, "metrics-user", "metrics-pass", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Incomplete {
		t.Fatal("complete pass reported incomplete")
	}
	if len(result.Objects) != 2 {
		t.Fatalf("objects = %d, want 2", len(result.Objects))
	}
	if result.Objects[0].CanonicalIdentity != "job=web,instance=one:80" || result.Objects[0].DisplayName != "web/one:80" {
		t.Fatalf("identity projection wrong: %#v", result.Objects[0])
	}
}

func TestMetricsDiscovererErrorsOnMissingIdentityLabel(t *testing.T) {
	registry := testSupervisorRegistry(t)
	body := `{"status":"success","data":{"result":[{"metric":{"__name__":"up","job":"web"}}]}}`
	server := fakeMetricsUpstream(t, body, nil, http.StatusOK)
	settings := fmt.Sprintf(`{"type":"prometheus","baseUrl":%q,"authType":"none"}`, server.URL)
	if _, err := discoverCall(t, registry, settings, "", "", ""); err == nil || !strings.Contains(err.Error(), "identity label") {
		t.Fatalf("missing identity = %v, want an identity label error", err)
	}
}

func TestMetricsDiscovererReportsTruncationAndWarningsAsIncomplete(t *testing.T) {
	registry := testSupervisorRegistry(t)
	var series []string
	for i := 0; i < 20; i++ {
		series = append(series, fmt.Sprintf(`{"metric":{"job":"j","instance":"i-%02d"}}`, i))
	}
	body := `{"status":"success","data":{"result":[` + strings.Join(series, ",") + `]}}`
	server := fakeMetricsUpstream(t, body, nil, http.StatusOK)
	settings := fmt.Sprintf(`{"type":"prometheus","baseUrl":%q,"authType":"none"}`, server.URL)
	bundle, _ := registry.Bundle(plugins.PrometheusID)
	call, callErr := newMetricsCall(testPluginRegistry(), plugins.PrometheusID, json.RawMessage(settings), "", "", "")
	if callErr != nil {
		t.Fatal(callErr)
	}
	// The frozen budget caps the pass; truncation is an honest incompleteness
	// fact instead of a silently narrowed scope.
	result, err := bundle.Discoverer.Discover(context.Background(), call, plugins.DiscoverRequest{ObjectType: "target", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Objects) != 5 || !result.Incomplete {
		t.Fatalf("truncated pass = %d objects incomplete=%v", len(result.Objects), result.Incomplete)
	}
	// Upstream warnings mark the pass incomplete even with an empty result.
	warned := fakeMetricsUpstream(t, `{"status":"success","data":{"result":[]}}`, []string{"storage throttled"}, http.StatusOK)
	warnedSettings := fmt.Sprintf(`{"type":"prometheus","baseUrl":%q,"authType":"none"}`, warned.URL)
	bundle, _ = registry.Bundle(plugins.PrometheusID)
	warnedCall, warnedErr := newMetricsCall(testPluginRegistry(), plugins.PrometheusID, json.RawMessage(warnedSettings), "", "", "")
	if warnedErr != nil {
		t.Fatal(warnedErr)
	}
	result, err = bundle.Discoverer.Discover(context.Background(), warnedCall, plugins.DiscoverRequest{ObjectType: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Incomplete {
		t.Fatal("warned pass must report incompleteness")
	}
}

func TestSourceObservationInputValidatesAgainstDescriptorCatalog(t *testing.T) {
	registry := testSupervisorRegistry(t)
	descriptor, _ := registry.Descriptor(plugins.PrometheusID)
	object := descriptor.DiscoverObjects[0]
	input := sourceObservationInput{
		SchemaKind: sourceObservationExecutionSchemaKind, AttemptID: 7, ObservationRunID: 3,
		PluginID: descriptor.ID, ObjectType: object.ObjectType,
		Query: object.Query, IdentityLabels: object.IdentityLabels, Limit: object.Limit,
	}
	supervisor := &Supervisor{}
	dispatch := &runtimev1.DispatchAttempt{ScopeId: 3}
	if err := supervisor.validateSourceObservationInput(dispatch, 7, input); err != nil {
		t.Fatalf("frozen descriptor copy rejected: %v", err)
	}
	input.Query = "up{job=\"ghost\"}"
	if err := supervisor.validateSourceObservationInput(dispatch, 7, input); err == nil {
		t.Fatal("a drifted frozen query was accepted")
	}
}

// TestMetricsDiscovererAppliesResolvedCredentials pins the credential
// contract of the discovery pass through the shared slot->ref->resolve
// construction: resolved credentials are applied verbatim, and an enforcing
// upstream never sees an anonymous or empty-credential request succeed.
func TestMetricsDiscovererAppliesResolvedCredentials(t *testing.T) {
	registry := testSupervisorRegistry(t)
	var auth []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if ok {
			auth = append(auth, "basic:"+user)
		} else if bearer := r.Header.Get("Authorization"); bearer != "" {
			auth = append(auth, "bearer:"+bearer)
		} else {
			auth = append(auth, "anonymous")
		}
		if user != "metrics-user" || pass != "metrics-pass" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":"error","error":"unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"job":"web","instance":"one:80"}}]}}`))
	}))
	t.Cleanup(server.Close)
	settings := fmt.Sprintf(`{"type":"prometheus","baseUrl":%q,"authType":"basic","username":"metrics-user"}`, server.URL)
	bundle, _ := registry.Bundle(plugins.PrometheusID)
	call, callErr := newMetricsCall(testPluginRegistry(), plugins.PrometheusID, json.RawMessage(settings), "metrics-user", "metrics-pass", "")
	if callErr != nil {
		t.Fatal(callErr)
	}
	result, err := bundle.Discoverer.Discover(context.Background(), call, plugins.DiscoverRequest{ObjectType: "target"})
	if err != nil {
		t.Fatalf("resolved credentials were not applied to the discovery pass: %v (requests: %v)", err, auth)
	}
	if len(result.Objects) != 1 {
		t.Fatalf("authenticated discovery objects = %d, want 1", len(result.Objects))
	}
	for _, request := range auth {
		if request == "anonymous" {
			t.Fatalf("discovery fell back to an anonymous request: %v", auth)
		}
	}
}

// TestMetricsDiscovererFailsClosedOnUnresolvableSecret pins the no-anonymous-
// fallback rule: a secret ref that cannot be resolved must fail the pass with
// an error, never execute without credentials against an authed endpoint.
func TestMetricsDiscovererFailsClosedOnUnresolvableSecret(t *testing.T) {
	registry := testSupervisorRegistry(t)
	var sawUnauthorizedAccept bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); !ok {
			sawUnauthorizedAccept = true
			_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"job":"web","instance":"one:80"}}]}}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	settings := fmt.Sprintf(`{"type":"prometheus","baseUrl":%q,"authType":"basic","username":"metrics-user"}`, server.URL)
	bundle, _ := registry.Bundle(plugins.PrometheusID)
	// An empty resolved credential against an enforcing upstream must fail
	// the pass with a 401 — never succeed anonymously.
	call, callErr := newMetricsCall(testPluginRegistry(), plugins.PrometheusID, json.RawMessage(settings), "metrics-user", "", "")
	if callErr != nil {
		t.Fatal(callErr)
	}
	_, err := bundle.Discoverer.Discover(context.Background(), call, plugins.DiscoverRequest{ObjectType: "target"})
	if err == nil {
		t.Fatal("unresolvable secret silently executed the discovery pass")
	}
	if sawUnauthorizedAccept {
		t.Fatal("authed endpoint answered an anonymous discovery request")
	}
}
