package connections_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	revealAttempt, err := service.StartProbe(ctx, prometheus.Name, nil, nil)
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
	attemptID, err := service.StartProbe(ctx, created.Name, nil, nil)
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
	attemptID, err := service.StartProbe(ctx, created.Name, nil, nil)
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
