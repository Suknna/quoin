package connections_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/connections"
)

func TestPrometheusAndThanosHaveIndependentCredentialsAndEnabledState(t *testing.T) {
	service, db, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	createMetrics := func(name, typ, authType string, secret map[string]string) connections.Summary {
		t.Helper()
		config, err := json.Marshal(map[string]any{
			"type": typ, "baseUrl": "https://metrics.example", "authType": authType,
		})
		if err != nil {
			t.Fatal(err)
		}
		if authType == "basic" {
			var projection map[string]any
			if err := json.Unmarshal(config, &projection); err != nil {
				t.Fatal(err)
			}
			projection["username"] = secret["username"]
			config, _ = json.Marshal(projection)
		}
		var raw json.RawMessage
		if len(secret) > 0 {
			secret["type"] = typ
			raw, _ = json.Marshal(secret)
		}
		created, err := service.Create(ctx, connections.CreateInput{Name: name, Type: typ, NonSecretJSON: config, Secret: raw, SecretPresent: len(raw) > 0}, 1, "metrics-create-"+name)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}

	prometheus := createMetrics("prometheus", connections.TypePrometheus, "bearer", map[string]string{"bearerToken": "prom-token"})
	thanos := createMetrics("thanos", connections.TypeThanos, "basic", map[string]string{"username": "thanos-user", "password": "thanos-password"})
	if prometheus.CurrentGenerationID == thanos.CurrentGenerationID || prometheus.CurrentRevisionID == thanos.CurrentRevisionID {
		t.Fatalf("metrics connections must own independent revision and credential generation: %+v %+v", prometheus, thanos)
	}
	// The bearer token decrypts only through the actual audited grant
	// fulfillment path, and the Prometheus carrier stays distinct from the
	// Thanos alias.
	revealAttempt, err := service.StartProbe(ctx, prometheus.Name)
	if err != nil {
		t.Fatal(err)
	}
	_, grantID, _, ok, err := service.BindQueuedToStream(context.Background(), revealAttempt, "boot-prometheus-reveal", 1, 5*time.Minute)
	if err != nil || !ok {
		t.Fatalf("bind prometheus reveal probe: %v ok=%v", err, ok)
	}
	payload, err := service.FulfillGrant(context.Background(), grantID, revealAttempt, "boot-prometheus-reveal", 1)
	if err != nil || payload.Metrics == nil || payload.Metrics.BearerToken != "prom-token" || payload.Thanos != nil {
		t.Fatalf("prometheus credential carrier = %+v, err=%v", payload, err)
	}
	// Close the reveal attempt so the qualification probe below can run
	// (one active probe per connection).
	if err := service.AcceptProbe(context.Background(), revealAttempt, "boot-prometheus-reveal", 1); err != nil {
		t.Fatal(err)
	}
	if err := service.InterruptProbe(context.Background(), revealAttempt, "lease_expired"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enable(ctx, prometheus.Name, prometheus.RowVersion, 0, 1); !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("Prometheus enable without probe must be rejected, got %v", err)
	}
	prometheusProbe := passedMetricsProbe(t, service, db, prometheus, "boot-prometheus", 1)
	if _, err := service.Enable(ctx, prometheus.Name, prometheus.RowVersion, prometheusProbe, 1); err != nil {
		t.Fatal(err)
	}
	thanosProbe := passedMetricsProbe(t, service, db, thanos, "boot-thanos", 2)
	if _, err := service.Enable(ctx, thanos.Name, thanos.RowVersion, thanosProbe, 1); err != nil {
		t.Fatalf("distinct metrics connection enable must independently qualify: %v", err)
	}
}

func TestPrometheusProbeCreatesExactGrant(t *testing.T) {
	service, db, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	config, _ := json.Marshal(map[string]any{
		"type": connections.TypePrometheus, "baseUrl": "https://metrics.example", "authType": "none",
	})
	created, err := service.Create(ctx, connections.CreateInput{Name: "prometheus-probe", Type: connections.TypePrometheus, NonSecretJSON: config}, 1, "prometheus-probe-create")
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := service.StartProbe(ctx, created.Name)
	if err != nil {
		t.Fatalf("start Prometheus probe: %v", err)
	}
	var purpose string
	var grantConnectionID, revisionID, generationID int64
	if err := db.QueryRowContext(ctx, `SELECT purpose,connection_id,connection_revision_id,credential_generation_id FROM attempt_connection_grants WHERE attempt_id=?`, attemptID).Scan(&purpose, &grantConnectionID, &revisionID, &generationID); err != nil {
		t.Fatal(err)
	}
	if purpose != "prometheus_probe" || grantConnectionID != created.ID || revisionID != created.CurrentRevisionID || generationID != created.CurrentGenerationID {
		t.Fatalf("probe grant did not freeze the exact Prometheus binding: purpose=%q connection=%d revision=%d generation=%d", purpose, grantConnectionID, revisionID, generationID)
	}
}

func TestInterruptPrometheusProbeClosesTypedResult(t *testing.T) {
	service, db, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	config, _ := json.Marshal(map[string]any{"type": connections.TypePrometheus, "baseUrl": "https://metrics.example", "authType": "none"})
	created, err := service.Create(ctx, connections.CreateInput{Name: "prometheus-interrupted", Type: connections.TypePrometheus, NonSecretJSON: config}, 1, "prometheus-interrupt-create")
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := service.StartProbe(ctx, created.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := service.BindQueuedToStream(context.Background(), attemptID, "boot-interrupted", 1, 5*time.Minute); err != nil || !ok {
		t.Fatalf("bind: %v ok=%v", err, ok)
	}
	if err := service.AcceptProbe(context.Background(), attemptID, "boot-interrupted", 1); err != nil {
		t.Fatal(err)
	}
	if err := service.InterruptProbe(context.Background(), attemptID, "lease_expired"); err != nil {
		t.Fatal(err)
	}
	var state, outcome, connectionType string
	if err := db.QueryRowContext(ctx, `SELECT a.state,p.outcome,p.connection_type FROM execution_attempts a JOIN connection_probe_results p ON p.attempt_id=a.id WHERE a.id=?`, attemptID).Scan(&state, &outcome, &connectionType); err != nil {
		t.Fatal(err)
	}
	if state != "Interrupted" || outcome != "interrupted" || connectionType != connections.TypePrometheus {
		t.Fatalf("interrupted Prometheus closure=%s/%s/%s", state, outcome, connectionType)
	}
	// An interrupt pass over an already-terminal attempt converges silently.
	if err := service.InterruptProbe(context.Background(), attemptID, "lease_expired"); err != nil {
		t.Fatalf("repeated interrupt must converge, got %v", err)
	}
}

func TestMetricsAuthSecretValidationRejectsSmuggledCredential(t *testing.T) {
	service, _, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	config, _ := json.Marshal(map[string]any{"type": connections.TypePrometheus, "baseUrl": "https://metrics.example", "authType": "bearer", "bearerToken": "leak"})
	if _, err := service.Create(ctx, connections.CreateInput{Name: "bad-prometheus", Type: connections.TypePrometheus, NonSecretJSON: config}, 1, fmt.Sprintf("metrics-bad-%d", seq.Next())); !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("secret in non-secret projection must be rejected, got %v", err)
	}
}

// TestAcquireMetricsConnectionServesProbeBeforeEnable reproduces the live
// acceptance failure (2026-09-21): the connection probe executes through the
// Stele gateway, whose material acquire once rejected disabled connections —
// but Create persists enabled=0 and Enable requires a passed probe result, so
// no metrics connection could ever qualify. The acquire seam must therefore
// serve freshly created (and rotated, revalidation-pending) connections;
// enabled-gating for model-visible queries stays at grant authorization.
func TestAcquireMetricsConnectionServesProbeBeforeEnable(t *testing.T) {
	service, _, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	config, _ := json.Marshal(map[string]any{"type": connections.TypePrometheus, "baseUrl": "http://prometheus.quoin-lab.svc.cluster.local:9090", "authType": "none"})
	created, err := service.Create(ctx, connections.CreateInput{Name: "mall-shop-prometheus", Type: connections.TypePrometheus, NonSecretJSON: config}, 1, "acquire-probe-create")
	if err != nil {
		t.Fatal(err)
	}
	if created.Enabled {
		t.Fatal("Create must persist the connection disabled (probe-then-enable lifecycle)")
	}
	payload, err := service.AcquireMetricsConnection(ctx, created.ID)
	if err != nil {
		t.Fatalf("material acquire for a not-yet-enabled connection must serve the probe: %v", err)
	}
	if payload.ConnectionType != connections.TypePrometheus ||
		payload.ConnectionRevisionID != created.CurrentRevisionID ||
		payload.CredentialGeneration != created.CurrentGenerationID {
		t.Fatalf("acquire payload binds the wrong pair: %+v", payload)
	}
	var revision struct {
		BaseURL  string `json:"baseUrl"`
		AuthType string `json:"authType"`
	}
	if err := json.Unmarshal(payload.RevisionConfigJSON, &revision); err != nil {
		t.Fatal(err)
	}
	if revision.BaseURL != "http://prometheus.quoin-lab.svc.cluster.local:9090" || revision.AuthType != "none" {
		t.Fatalf("revision projection = %+v", revision)
	}
	if payload.Metrics == nil || payload.Metrics.Username != "" || payload.Metrics.Password != "" || payload.Metrics.BearerToken != "" {
		t.Fatalf("auth-none connection must deliver an empty credential carrier: %+v", payload.Metrics)
	}
	// Deterministic denials keep their closed semantics.
	if _, err := service.AcquireMetricsConnection(ctx, created.ID+999); !errors.Is(err, connections.ErrAcquireDenied) {
		t.Fatalf("unknown connection acquire must be denied, got %v", err)
	}
	providerConfig, _ := json.Marshal(map[string]any{"type": connections.TypeModelProvider, "baseUrl": "https://api.example.com", "chatModelId": "chat", "embeddingModelId": "embed"})
	providerSecret, _ := json.Marshal(map[string]string{"type": connections.TypeModelProvider, "apiKey": "sk-test"})
	provider, err := service.Create(ctx, connections.CreateInput{Name: "main-openai", Type: connections.TypeModelProvider, NonSecretJSON: providerConfig, Secret: providerSecret, SecretPresent: true}, 1, "acquire-provider-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcquireMetricsConnection(ctx, provider.ID); !errors.Is(err, connections.ErrAcquireDenied) {
		t.Fatalf("model_provider material must stay off the gateway seam, got %v", err)
	}
}

// TestValidateMetricsExecutionGrantGuardsFrozenPair covers the P1 execution
// guard: after the acquire seam opened for probe-before-enable, a frozen
// Queued collection (observation discovery / inspection collection) must NOT
// execute once an admin disables the connection or a rotation replaces the
// frozen pair — no credential acquisition, no platform call. The guard re
// checks enabled/revalidation/revision/generation/root binding inside the
// runner transaction; the probe path deliberately stays off this guard.
func TestValidateMetricsExecutionGrantGuardsFrozenPair(t *testing.T) {
	service, db, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	config, _ := json.Marshal(map[string]any{"type": connections.TypePrometheus, "baseUrl": "https://metrics.example", "authType": "none"})
	created, err := service.Create(ctx, connections.CreateInput{Name: "guard-prometheus", Type: connections.TypePrometheus, NonSecretJSON: config}, 1, "guard-create")
	if err != nil {
		t.Fatal(err)
	}
	probeID := passedMetricsProbe(t, service, db, created, "boot-guard", 1)
	enabled, err := service.Enable(ctx, created.Name, created.RowVersion, probeID, 1)
	if err != nil || !enabled.Enabled {
		t.Fatalf("enable qualified connection: %v %+v", err, enabled)
	}
	seedSeq := 0
	seedCollectionGrant := func(revisionID, generationID int64) int64 {
		t.Helper()
		seedSeq++
		planKey := fmt.Sprintf("guard-plan-%d", seedSeq)
		now := "2026-09-21T00:00:00Z"
		if _, err := db.Exec(`INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_json,scope_kind,cron,timezone,row_version,created_by,created_at,updated_at)
			VALUES(?, 'guard plan',1,?, 'prometheus','promql_instant','1','{"expression":"up"}','{"kind":"integration"}','integration',NULL,'UTC',1,1,?,?)`, planKey, enabled.ID, now, now); err != nil {
			t.Fatal(err)
		}
		var planID int64
		if err := db.QueryRow(`SELECT id FROM inspection_plans WHERE plan_key=?`, planKey).Scan(&planID); err != nil {
			t.Fatal(err)
		}
		run, err := db.Exec(`INSERT INTO inspection_runs(plan_key,plan_id,connection_id,plugin_id,template_id,template_version,frozen_params_json,frozen_scope_json,trigger_kind,state,row_version,created_at)
			VALUES(?,?,?, 'prometheus','promql_instant','1','{"expression":"up"}','{"kind":"integration"}','manual','Queued',1,?)`, planKey, planID, enabled.ID, now)
		if err != nil {
			t.Fatal(err)
		}
		runID, _ := run.LastInsertId()
		// Runs are born Queued (frozen trigger); advance to Running with the
		// evidence timestamp the state machine requires before children attach.
		if _, err := db.Exec(`UPDATE inspection_runs SET state='Running',evidence_at=?,row_version=2 WHERE id=?`, now, runID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO inspection_run_checks(run_id,check_key,display_name,plugin_id,template_id,template_version,params_json,created_at)
			VALUES(?, 'up', 'up check', 'prometheus','promql_instant','1','{"expression":"up"}',?)`, runID, now); err != nil {
			t.Fatal(err)
		}
		attempt, err := db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,check_key,state,quoin_release_version,operation_correlation_id,initiator_type,initiator_id,created_at)
			VALUES('inspection_collection','run_check',?,'up','Queued','test','corr-guard-fixture','user',1,?)`, runID, now)
		if err != nil {
			t.Fatal(err)
		}
		attemptID, _ := attempt.LastInsertId()
		if _, err := db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at)
			VALUES(?, 'config_thanos_query', ?, ?, ?, ?)`, attemptID, enabled.ID, revisionID, generationID, now); err != nil {
			t.Fatal(err)
		}
		return attemptID
	}
	// Enabled connection, current pair: the frozen collection may execute.
	frozen := seedCollectionGrant(enabled.CurrentRevisionID, enabled.CurrentGenerationID)
	if err := service.ValidateMetricsExecutionGrant(context.Background(), frozen); err != nil {
		t.Fatalf("enabled current pair must validate: %v", err)
	}
	// Admin disable: the still-Queued frozen collection must be refused.
	disabled, err := service.Disable(ctx, created.Name, enabled.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled || disabled.RowVersion == 0 {
		t.Fatalf("disable returned %+v", disabled)
	}
	err = service.ValidateMetricsExecutionGrant(context.Background(), frozen)
	if !errors.Is(err, connections.ErrGrantDenied) {
		t.Fatalf("disabled connection must deny the frozen grant, got %v", err)
	}
	if !strings.Contains(err.Error(), "disabled or pending revalidation") {
		t.Fatalf("denial must name the failing condition, got %v", err)
	}
	// Rotation replaces the pair: the old frozen grant stays refused even after
	// the rotated connection is requalified and re-enabled.
	requalified := passedMetricsProbe(t, service, db, disabled, "boot-reenable", 2)
	reenabled, err := service.Enable(ctx, created.Name, disabled.RowVersion, requalified, 1)
	if err != nil {
		t.Fatalf("re-enable after fresh probe: %v", err)
	}
	rotatedConfig, _ := json.Marshal(map[string]any{"type": connections.TypePrometheus, "baseUrl": "https://metrics-2.example", "authType": "none"})
	rotated, err := service.Rotate(ctx, created.Name, reenabled.RowVersion, connections.CreateInput{
		Name: created.Name, Type: connections.TypePrometheus, NonSecretJSON: rotatedConfig,
	}, 1, "guard-rotate")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	err = service.ValidateMetricsExecutionGrant(context.Background(), frozen)
	if !errors.Is(err, connections.ErrGrantDenied) {
		t.Fatalf("pre-rotation frozen grant must stay denied after rotation, got %v", err)
	}
	// A grant frozen on the rotated pair validates once the connection is
	// requalified and enabled again (the recovery path stays open).
	fresh := passedMetricsProbe(t, service, db, rotated, "boot-after-rotation", 3)
	final, err := service.Enable(ctx, created.Name, rotated.RowVersion, fresh, 1)
	if err != nil {
		t.Fatalf("enable rotated connection: %v", err)
	}
	// Re-enabling clears the state gate, but must not revive the old pair.
	err = service.ValidateMetricsExecutionGrant(context.Background(), frozen)
	if !errors.Is(err, connections.ErrGrantDenied) || !strings.Contains(err.Error(), "frozen revision/generation pair no longer current") {
		t.Fatalf("old grant must remain denied after requalification with the pair mismatch reason, got %v", err)
	}
	refrozen := seedCollectionGrant(final.CurrentRevisionID, final.CurrentGenerationID)
	if err := service.ValidateMetricsExecutionGrant(context.Background(), refrozen); err != nil {
		t.Fatalf("fresh frozen pair on the enabled rotated connection must validate: %v", err)
	}
}
