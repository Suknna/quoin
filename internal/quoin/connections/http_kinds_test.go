package connections_test

// ADR-0014 behavior coverage for the registry-backed trusted HTTP connection
// kinds: a synthetic HTTP plugin kind flows through create → sealed
// credential → gateway material acquire exactly like the legacy metrics
// kinds, while unknown and revoked (plugin disabled) types fail closed on
// every outbound path. Behavior is asserted through the public service
// surface, never through private types.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/connections"
)

// fixtureConnectionKinds resolves the legacy metrics kinds (and any extra
// test plugin) through a real frozen plugin registry and its deployment view.
func fixtureConnectionKinds(t *testing.T, extra ...plugins.Plugin) *plugins.ConnectionKindView {
	t.Helper()
	registry := plugins.NewRegistry()
	base := []plugins.Plugin{
		{ID: "prometheus", Version: "1", DefaultEnabled: true, ConnectionKind: "prometheus",
			ConnectionTransport: plugins.ConnectionTransportHTTP,
			ConnectionAuthModes: []string{plugins.AuthModeNone, plugins.AuthModeBasic, plugins.AuthModeBearer}},
		{ID: "thanos", Version: "1", DefaultEnabled: true, ConnectionKind: "thanos",
			ConnectionTransport: plugins.ConnectionTransportHTTP,
			ConnectionAuthModes: []string{plugins.AuthModeNone, plugins.AuthModeBasic, plugins.AuthModeBearer}},
	}
	for _, plugin := range append(base, extra...) {
		if err := registry.Register(plugin); err != nil {
			t.Fatalf("register %s: %v", plugin.ID, err)
		}
	}
	return registry.ConnectionKindView()
}

// syntheticHTTPPlugin is the test-only trusted HTTP connection kind: a
// regular HTTP query platform that is neither prometheus nor thanos.
func syntheticHTTPPlugin(authModes ...string) plugins.Plugin {
	if len(authModes) == 0 {
		authModes = []string{plugins.AuthModeNone, plugins.AuthModeBasic, plugins.AuthModeBearer}
	}
	return plugins.Plugin{
		ID: "synthmetrics", Version: "1", DefaultEnabled: true, ConnectionKind: "synth-http",
		ConnectionTransport: plugins.ConnectionTransportHTTP, ConnectionAuthModes: authModes,
	}
}

// createSynthetic creates one connection of the given kind with a bearer
// secret, mirroring the legacy metrics create path.
func createSynthetic(t *testing.T, service *connections.Service, ctx context.Context, name, kind, authType string) connections.Summary {
	t.Helper()
	config, err := json.Marshal(map[string]any{"type": kind, "baseUrl": "https://synth.example", "authType": authType})
	if err != nil {
		t.Fatal(err)
	}
	var secret json.RawMessage
	if authType == "bearer" {
		secret, _ = json.Marshal(map[string]string{"type": kind, "bearerToken": "synth-token"})
	} else if authType == "basic" {
		config, _ = json.Marshal(map[string]any{"type": kind, "baseUrl": "https://synth.example", "authType": authType, "username": "synth-user"})
		secret, _ = json.Marshal(map[string]string{"type": kind, "username": "synth-user", "password": "synth-password"})
	}
	created, err := service.Create(ctx, connections.CreateInput{Name: name, Type: kind, NonSecretJSON: config, Secret: secret, SecretPresent: len(secret) > 0}, 1, "synth-create-"+name)
	if err != nil {
		t.Fatalf("create %s connection: %v", kind, err)
	}
	return created
}

// TestSyntheticHTTPKindMatchesLegacyMetricsGatewayPath proves the outbound
// credential path is kind-generic: a brand-new HTTP plugin kind creates,
// seals and acquires material exactly like prometheus/thanos, in every
// bounded auth mode.
func TestSyntheticHTTPKindMatchesLegacyMetricsGatewayPath(t *testing.T) {
	service, _, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	service.SetConnectionKinds(fixtureConnectionKinds(t, syntheticHTTPPlugin()))

	bearer := createSynthetic(t, service, ctx, "synth-bearer", "synth-http", "bearer")
	basic := createSynthetic(t, service, ctx, "synth-basic", "synth-http", "basic")
	none := createSynthetic(t, service, ctx, "synth-none", "synth-http", "none")

	payload, err := service.AcquireMetricsConnection(ctx, bearer.ID)
	if err != nil {
		t.Fatalf("bearer acquire: %v", err)
	}
	if payload.ConnectionType != "synth-http" ||
		payload.ConnectionRevisionID != bearer.CurrentRevisionID ||
		payload.CredentialGeneration != bearer.CurrentGenerationID {
		t.Fatalf("acquire bound the wrong pair: %+v", payload)
	}
	if payload.Metrics == nil || payload.Metrics.BearerToken != "synth-token" || payload.Metrics.Username != "" || payload.Metrics.Password != "" {
		t.Fatalf("bearer carrier = %+v", payload.Metrics)
	}
	var revision struct {
		Type     string `json:"type"`
		BaseURL  string `json:"baseUrl"`
		AuthType string `json:"authType"`
	}
	if err := json.Unmarshal(payload.RevisionConfigJSON, &revision); err != nil {
		t.Fatal(err)
	}
	if revision.Type != "synth-http" || revision.BaseURL != "https://synth.example" || revision.AuthType != "bearer" {
		t.Fatalf("synthetic revision projection = %+v", revision)
	}

	basicPayload, err := service.AcquireMetricsConnection(ctx, basic.ID)
	if err != nil {
		t.Fatalf("basic acquire: %v", err)
	}
	if basicPayload.Metrics == nil || basicPayload.Metrics.Username != "synth-user" || basicPayload.Metrics.Password != "synth-password" {
		t.Fatalf("basic carrier = %+v", basicPayload.Metrics)
	}

	nonePayload, err := service.AcquireMetricsConnection(ctx, none.ID)
	if err != nil {
		t.Fatalf("none acquire: %v", err)
	}
	if nonePayload.Metrics == nil || nonePayload.Metrics.Username != "" || nonePayload.Metrics.Password != "" || nonePayload.Metrics.BearerToken != "" {
		t.Fatalf("auth-none kind must deliver an empty carrier: %+v", nonePayload.Metrics)
	}
}

// TestUnknownAndRevokedConnectionTypesFailClosed proves the fail-closed
// boundary: a type no plugin declares cannot be created, and a kind whose
// plugin left the deployment enablement set is revoked for both new creates
// and existing rows' gateway material.
func TestUnknownAndRevokedConnectionTypesFailClosed(t *testing.T) {
	service, _, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	view := fixtureConnectionKinds(t, syntheticHTTPPlugin())
	service.SetConnectionKinds(view)

	// Unknown type: rejected before any state is written.
	config, _ := json.Marshal(map[string]any{"type": "splunk", "baseUrl": "https://splunk.example", "authType": "none"})
	if _, err := service.Create(ctx, connections.CreateInput{Name: "unknown-kind", Type: "splunk", NonSecretJSON: config}, 1, "unknown-kind-create"); !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("unknown type must be rejected, got %v", err)
	}

	// Auth mode outside the declaration is rejected even for a trusted kind.
	service.SetConnectionKinds(fixtureConnectionKinds(t, syntheticHTTPPlugin(plugins.AuthModeBasic)))
	restrictive := createSynthetic(t, service, ctx, "synth-basic-only", "synth-http", "basic")
	bearerConfig, _ := json.Marshal(map[string]any{"type": "synth-http", "baseUrl": "https://synth.example", "authType": "bearer"})
	if _, err := service.Create(ctx, connections.CreateInput{Name: "synth-undeclared-mode", Type: "synth-http", NonSecretJSON: bearerConfig}, 1, "synth-undeclared-mode"); !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("undeclared auth mode must be rejected, got %v", err)
	}

	// Revocation: deploying without the synthmetrics plugin closes the kind
	// for new creates AND for the existing row's gateway material.
	service.SetConnectionKinds(fixtureConnectionKinds(t))
	if _, err := service.Create(ctx, connections.CreateInput{Name: "synth-revoked", Type: "synth-http", NonSecretJSON: bearerConfig}, 1, "synth-revoked"); !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("revoked kind create must be rejected, got %v", err)
	}
	if _, err := service.AcquireMetricsConnection(ctx, restrictive.ID); !errors.Is(err, connections.ErrAcquireDenied) {
		t.Fatalf("revoked kind acquire must be denied, got %v", err)
	}

	// The legacy metrics kinds stay gateway-executable throughout.
	legacy := createSynthetic(t, service, ctx, "legacy-prometheus", "prometheus", "none")
	if _, err := service.AcquireMetricsConnection(ctx, legacy.ID); err != nil {
		t.Fatalf("legacy kind acquire must keep working: %v", err)
	}
}

// TestModelProviderStaysOffGatewayKindRegistry proves the credential split:
// model_provider never resolves as an HTTP kind (a plugin cannot even
// declare it — reserved), so its credentials cannot leak onto the gateway
// seam and remain Plinth-exclusive.
func TestModelProviderStaysOffGatewayKindRegistry(t *testing.T) {
	registry := plugins.NewRegistry()
	if err := registry.Register(plugins.Plugin{ID: "impostor", Version: "1", ConnectionKind: "model_provider",
		ConnectionTransport: plugins.ConnectionTransportHTTP, ConnectionAuthModes: []string{plugins.AuthModeBearer}}); err == nil {
		t.Fatal("a plugin must not declare the reserved model_provider kind")
	}
}
