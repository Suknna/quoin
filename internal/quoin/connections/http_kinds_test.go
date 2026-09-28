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
	"fmt"
	"strings"
	"testing"
	"time"

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

// probingSyntheticPlugin declares the same synthetic kind with a bounded
// read-only probe contract: one GET /health expected to answer 200.
func probingSyntheticPlugin() plugins.Plugin {
	plugin := syntheticHTTPPlugin()
	plugin.ConnectionProbePath = "/health"
	return plugin
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

// TestSyntheticHTTPKindProbeLifecycleEnablesConnection walks the full probe
// closure for a plugin-declared HTTP kind: create → StartProbe (freezing the
// current revision+generation pair under the <kind>_probe purpose) → bind →
// accept → CommitProbeResult over the shared HTTP typed child → Enable with
// the immutable qualification. The bearer secret must appear nowhere in the
// non-secret projections, the probe record or the attempt tables.
func TestSyntheticHTTPKindProbeLifecycleEnablesConnection(t *testing.T) {
	service, database, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	service.SetConnectionKinds(fixtureConnectionKinds(t, probingSyntheticPlugin()))
	created := createSynthetic(t, service, ctx, "synth-probe", "synth-http", "bearer")

	attemptID, err := service.StartProbe(ctx, created.Name)
	if err != nil {
		t.Fatalf("synthetic kind probe start: %v", err)
	}
	var purpose string
	if err := database.QueryRow(`SELECT purpose FROM attempt_connection_grants WHERE attempt_id=?`, attemptID).Scan(&purpose); err != nil {
		t.Fatal(err)
	}
	if purpose != "synth-http_probe" {
		t.Fatalf("probe grant purpose %q, want synth-http_probe", purpose)
	}
	var actionSet string
	if err := database.QueryRow(`SELECT action_set_id FROM connection_probe_results WHERE 1=0`).Scan(&actionSet); err == nil {
		t.Fatal("no probe result may exist before the commit")
	}

	if _, _, _, ok, err := service.BindQueuedToStream(context.Background(), attemptID, "boot-synth", 7, 5*time.Minute); err != nil || !ok {
		t.Fatalf("bind: %v ok=%v", err, ok)
	}
	if err := service.AcceptProbe(context.Background(), attemptID, "boot-synth", 7); err != nil {
		t.Fatalf("accept: %v", err)
	}
	child := &connections.TypedChild{HTTP: &connections.HTTPProbeChild{
		RequestMethod: "GET", RequestPath: "/health", ExpectedStatus: 200, ObservedStatus: 200,
		DetailJSON: `{"method":"GET","path":"/health","expectedStatus":200,"observedStatus":200}`,
	}}
	result := connections.TypedProbeResult{Outcome: "passed", ResultDigest: fmt.Sprintf("%064x", attemptID), StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z"}
	if err := service.CommitProbeResult(context.Background(), attemptID, "boot-synth", 7, result, child); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var storedActionSet string
	var observedStatus int
	if err := database.QueryRow(`SELECT r.action_set_id, h.observed_status FROM connection_probe_results r JOIN http_connection_probe_results h ON h.probe_result_id=r.id WHERE r.attempt_id=?`, attemptID).Scan(&storedActionSet, &observedStatus); err != nil {
		t.Fatalf("typed child closure: %v", err)
	}
	if storedActionSet != connections.HTTPProbeActionSetID || observedStatus != 200 {
		t.Fatalf("probe result actionSet=%s observed=%d, want %s/200", storedActionSet, observedStatus, connections.HTTPProbeActionSetID)
	}

	enabled, err := service.Enable(ctx, created.Name, created.RowVersion, 0, 1)
	if !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("enable without an explicit passed probe must be rejected, got %v", err)
	}
	var probeResultID int64
	if err := database.QueryRow(`SELECT id FROM connection_probe_results WHERE attempt_id=?`, attemptID).Scan(&probeResultID); err != nil {
		t.Fatal(err)
	}
	enabled, err = service.Enable(ctx, created.Name, created.RowVersion, probeResultID, 1)
	if err != nil {
		t.Fatalf("enable with the passed probe: %v", err)
	}
	if !enabled.Enabled {
		t.Fatal("connection must be enabled after the passed probe")
	}

	// Credential material never leaks into the non-secret surfaces: the
	// bearer token exists only inside the encrypted generation envelope.
	var configJSON, detailJSON string
	if err := database.QueryRow(`SELECT config_json FROM connection_revisions WHERE id=?`, created.CurrentRevisionID).Scan(&configJSON); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT detail_json FROM http_connection_probe_results WHERE probe_result_id=?`, probeResultID).Scan(&detailJSON); err != nil {
		t.Fatal(err)
	}
	for _, surface := range map[string]string{"revision config": configJSON, "probe detail": detailJSON} {
		if strings.Contains(surface, "synth-token") {
			t.Fatalf("bearer token leaked into %s: %s", surface, "<redacted>")
		}
	}
}

// TestSyntheticHTTPKindChildDivergenceFailsClosed proves the commit-side
// registry re-check: a result whose request shape diverges from the frozen
// plugin declaration is rejected, never sealed.
func TestSyntheticHTTPKindChildDivergenceFailsClosed(t *testing.T) {
	service, _, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	service.SetConnectionKinds(fixtureConnectionKinds(t, probingSyntheticPlugin()))
	created := createSynthetic(t, service, ctx, "synth-diverge", "synth-http", "none")
	attemptID, err := service.StartProbe(ctx, created.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := service.BindQueuedToStream(context.Background(), attemptID, "boot-diverge", 3, 5*time.Minute); err != nil || !ok {
		t.Fatalf("bind: %v ok=%v", err, ok)
	}
	if err := service.AcceptProbe(context.Background(), attemptID, "boot-diverge", 3); err != nil {
		t.Fatal(err)
	}
	child := &connections.TypedChild{HTTP: &connections.HTTPProbeChild{
		RequestMethod: "GET", RequestPath: "/elsewhere", ExpectedStatus: 200, ObservedStatus: 200,
		DetailJSON: `{"method":"GET","path":"/elsewhere","expectedStatus":200,"observedStatus":200}`,
	}}
	result := connections.TypedProbeResult{Outcome: "passed", ResultDigest: fmt.Sprintf("%064x", attemptID), StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z"}
	if err := service.CommitProbeResult(context.Background(), attemptID, "boot-diverge", 3, result, child); err == nil {
		t.Fatal("a diverging request shape must fail closed")
	}
}

// TestSyntheticHTTPKindWithoutProbeContractFailsClosed proves the ADR-0014
// closure rule: a kind whose plugin declares no probe contract can never
// start a probe attempt (it would strand outside every terminal write path).
func TestSyntheticHTTPKindWithoutProbeContractFailsClosed(t *testing.T) {
	service, _, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	service.SetConnectionKinds(fixtureConnectionKinds(t, syntheticHTTPPlugin()))
	created := createSynthetic(t, service, ctx, "synth-noprobe", "synth-http", "none")
	if _, err := service.StartProbe(ctx, created.Name); !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("probe start without a declared contract must be rejected, got %v", err)
	}
}

// TestRevokedSyntheticHTTPKindProbeFailsClosed proves the revocation
// boundary on the probe path: once the owning plugin leaves the deployment
// enablement set, its kinds cannot start probes anymore.
func TestRevokedSyntheticHTTPKindProbeFailsClosed(t *testing.T) {
	service, _, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	service.SetConnectionKinds(fixtureConnectionKinds(t, probingSyntheticPlugin()))
	created := createSynthetic(t, service, ctx, "synth-revoked-probe", "synth-http", "none")
	service.SetConnectionKinds(fixtureConnectionKinds(t))
	if _, err := service.StartProbe(ctx, created.Name); !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("revoked kind probe start must be rejected, got %v", err)
	}
}

// TestSyntheticHTTPKindProbeCancelAndInterruptCloseTyped proves the full
// fence coverage for registry kinds: a user cancellation and a lease-driven
// interruption both seal the shared HTTP typed child (frozen request shape,
// observed 0 — no manufactured success) and converge the attempt terminal
// state under the same closure rules as the core kinds.
func TestSyntheticHTTPKindProbeCancelAndInterruptCloseTyped(t *testing.T) {
	service, database, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	service.SetConnectionKinds(fixtureConnectionKinds(t, probingSyntheticPlugin()))

	// User cancellation: Running probe → Cancelling fence → Cancelled.
	created := createSynthetic(t, service, ctx, "synth-cancel", "synth-http", "none")
	attemptID, err := service.StartProbe(ctx, created.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := service.BindQueuedToStream(context.Background(), attemptID, "boot-cancel", 5, 5*time.Minute); err != nil || !ok {
		t.Fatalf("bind: %v ok=%v", err, ok)
	}
	if err := service.AcceptProbe(context.Background(), attemptID, "boot-cancel", 5); err != nil {
		t.Fatal(err)
	}
	var rowVersion int64
	if err := database.QueryRow(`SELECT row_version FROM execution_attempts WHERE id=?`, attemptID).Scan(&rowVersion); err != nil {
		t.Fatal(err)
	}
	if err := service.CancelProbe(adminContext(t, nextCorrelation()), attemptID, rowVersion); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := service.RecordCancelAck(context.Background(), attemptID); err != nil {
		t.Fatalf("cancel ack: %v", err)
	}
	var state string
	var observedStatus int
	if err := database.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "Cancelled" {
		t.Fatalf("cancelled probe state %s, want Cancelled", state)
	}
	if err := database.QueryRow(`SELECT h.observed_status FROM connection_probe_results r JOIN http_connection_probe_results h ON h.probe_result_id=r.id WHERE r.attempt_id=?`, attemptID).Scan(&observedStatus); err != nil {
		t.Fatalf("cancelled typed child: %v", err)
	}
	if observedStatus != 0 {
		t.Fatalf("cancelled probe recorded observed status %d, want 0", observedStatus)
	}

	// Runtime-driven interruption converges the same way.
	interruptedConn := createSynthetic(t, service, ctx, "synth-interrupt", "synth-http", "none")
	interruptAttempt, err := service.StartProbe(ctx, interruptedConn.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := service.BindQueuedToStream(context.Background(), interruptAttempt, "boot-interrupt", 6, 5*time.Minute); err != nil || !ok {
		t.Fatalf("bind: %v ok=%v", err, ok)
	}
	if err := service.AcceptProbe(context.Background(), interruptAttempt, "boot-interrupt", 6); err != nil {
		t.Fatal(err)
	}
	if err := service.InterruptProbe(context.Background(), interruptAttempt, "lease_expired"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if err := database.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, interruptAttempt).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "Interrupted" {
		t.Fatalf("interrupted probe state %s, want Interrupted", state)
	}
	var actionSet string
	if err := database.QueryRow(`SELECT r.action_set_id FROM connection_probe_results r WHERE r.attempt_id=?`, interruptAttempt).Scan(&actionSet); err != nil {
		t.Fatal(err)
	}
	if actionSet != connections.HTTPProbeActionSetID {
		t.Fatalf("interrupted probe action set %q, want %q", actionSet, connections.HTTPProbeActionSetID)
	}
}
