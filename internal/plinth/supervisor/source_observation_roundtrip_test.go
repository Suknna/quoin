package supervisor

// Cross-wire roundtrip: the canonical proposal bytes this supervisor seals
// are exactly what the Quoin control plane (observation.CommitProposal)
// adjudicates. The partial/warnings case is the review regression: a gap
// proposal must arrive with empty objects so the Run converges instead of
// being rejected into a permanent Running state.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/observation"
	_ "modernc.org/sqlite"
)

// supervisorAdminContext is the trusted-entry execution metadata the
// connection commands re-verify in-transaction (admin user 1, session 1).
func supervisorAdminContext(t *testing.T) context.Context {
	t.Helper()
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "corr-supervisor-roundtrip",
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-supervisor-roundtrip"},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func roundtripHarness(t *testing.T) (*observation.Service, *sql.DB, func(commandID string) observation.SourceObservationRun) {
	t.Helper()
	directory := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+directory+"/roundtrip.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'x',1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,randomblob(32),1,'roundtrip-test',?,?,?,?)`, now, now, "2036-09-15T00:00:00Z", "2036-09-22T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now); err != nil {
		t.Fatal(err)
	}
	// An isolated but identical discovery catalog keeps the roundtrip focused
	// on the wire shape rather than the shared catalog's tool-schema churn.
	registry := plugins.NewRegistry()
	if err := registry.RegisterDescriptor(plugins.Descriptor{
		ID: "prometheus", Version: "1", DisplayName: "Prometheus", Description: "metrics source",
		DefaultEnabled: true,
		Capabilities:   []plugins.Capability{plugins.CapabilityDiscover},
		ConnectionKind: "prometheus",
		DiscoverObjects: []plugins.DiscoverObject{
			{ObjectType: "target", IdentityLabels: []string{"job", "instance"}, Query: "up", Limit: 500},
		},
	}); err != nil {
		t.Fatal(err)
	}
	enabled, err := registry.ResolveEnabled(nil)
	if err != nil {
		t.Fatal(err)
	}
	service := observation.NewService(db, registry, enabled)
	// The composition read seam (mirrors observation.newTestService): a real
	// query_only reader over the same fixture file, opened through the
	// execution.OpenReadOnly factory and validated by SetReader's probe — a
	// second writable handle is never an acceptable reader. Without it every
	// read fails closed (context canceled). One pool, same lifetime as the
	// writer: the reader closes in t.Cleanup alongside db, and the runners
	// never close an injected pool themselves.
	reader, err := execution.OpenReadOnly(directory + "/roundtrip.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	if err := service.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	// The real probe→enable path so the frozen source grant resolves. The
	// connections service shares the same read-only pool: StartProbe resolves
	// the connection summary through the same fail-closed read seam.
	connService := connections.NewService(db, func() ([]byte, error) { return make([]byte, 32), nil })
	if err := connService.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	connections.ProbeContractSource = func() string { return string(gencontracts.ConnectionProbesYAML) }
	configJSON, _ := json.Marshal(map[string]any{"type": "prometheus", "baseUrl": "https://metrics.test", "authType": "none"})
	summary, err := connService.Create(supervisorAdminContext(t), connections.CreateInput{Name: "roundtrip-prometheus", Type: "prometheus", NonSecretJSON: configJSON}, 1, "roundtrip-create")
	if err != nil {
		t.Fatal(err)
	}
	probeID, err := connService.StartProbe(supervisorAdminContext(t), summary.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := connService.BindQueuedToStream(context.Background(), probeID, "roundtrip-boot", 1, 5*time.Minute); err != nil || !ok {
		t.Fatalf("bind probe: %v ok=%v", err, ok)
	}
	if err := connService.AcceptProbe(context.Background(), probeID, "roundtrip-boot", 1); err != nil {
		t.Fatal(err)
	}
	if err := connService.CommitProbeResult(context.Background(), probeID, "roundtrip-boot", 1, connections.TypedProbeResult{Outcome: "passed", ResultDigest: fmt.Sprintf("%064x", probeID), StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z"}, &connections.TypedChild{Thanos: &connections.ThanosProbeChild{Query: "vector(1)", ResponseType: "vector", SampleCount: 1, SampleValue: "1", DetailJSON: `{"kind":"prometheus"}`}}); err != nil {
		t.Fatal(err)
	}
	var qualified int64
	if err := db.QueryRow(`SELECT id FROM connection_probe_results WHERE attempt_id=?`, probeID).Scan(&qualified); err != nil {
		t.Fatal(err)
	}
	if _, err := connService.Enable(supervisorAdminContext(t), summary.Name, summary.RowVersion, qualified, 1); err != nil {
		t.Fatal(err)
	}
	return service, db, func(commandID string) observation.SourceObservationRun {
		run, err := service.StartRun(supervisorAdminContext(t), 1, commandID, "roundtrip-prometheus", "manual", nil)
		if err != nil {
			t.Fatalf("start run: %v", err)
		}
		queued, err := service.QueuedObservationAttempts(context.Background())
		if err != nil || len(queued) != 1 {
			t.Fatalf("queued children = %v, %v", queued, err)
		}
		attempts := service.Attempts()
		if err := attempts.BindToStream(context.Background(), queued[0], "roundtrip-boot", 1, time.Minute, "test"); err != nil {
			t.Fatalf("bind child: %v", err)
		}
		if err := attempts.Accept(context.Background(), queued[0], "roundtrip-boot", 1); err != nil {
			t.Fatalf("accept child: %v", err)
		}
		return run
	}
}

func TestSourceObservationProposalRoundtripGapConverges(t *testing.T) {
	service, db, startRun := roundtripHarness(t)
	run := startRun("roundtrip-gap-0001")
	runID := parseRoundtripID(t, run.ID)
	// Seal the proposal exactly as proposeSourceObservation does for an
	// incomplete pass carrying partial facts: the marshal authority clears
	// the objects and the control plane must accept and converge.
	input := sourceObservationInput{ObservationRunID: runID, ObjectType: "target"}
	attemptID := roundtripAttemptID(t, db, runID)
	canonical, err := marshalSourceObservationProposal(attemptID, input, "gap", []observedTarget{
		{Identity: map[string]string{"job": "web", "instance": "one:80"}},
	}, []string{"storage throttled"}, "partial_response")
	if err != nil {
		t.Fatal(err)
	}
	var shape struct {
		Objects []json.RawMessage `json:"objects"`
	}
	if err := json.Unmarshal(canonical, &shape); err != nil {
		t.Fatal(err)
	}
	if len(shape.Objects) != 0 {
		t.Fatalf("gap proposal carried %d objects; only success may propose objects", len(shape.Objects))
	}
	if err := service.CommitProposal(context.Background(), attemptID, "roundtrip-boot", 1, canonical); err != nil {
		t.Fatalf("gap proposal rejected by the control plane: %v", err)
	}
	// The Run converged instead of wedging in Running, the child kept the
	// honest gap reason, and no partial fact touched the identity projection.
	if state := roundtripRunState(t, db, runID); state != "CompletedWithWarnings" {
		t.Fatalf("run state = %s, want CompletedWithWarnings", state)
	}
	object := roundtripObject(t, db, runID)
	if object.Status != "gap" || object.GapReason == nil || *object.GapReason != "partial_response" {
		t.Fatalf("gap object wrong: %#v", object)
	}
	var projected int
	if err := db.QueryRow(`SELECT COUNT(*) FROM observed_source_objects`).Scan(&projected); err != nil {
		t.Fatal(err)
	}
	if projected != 0 {
		t.Fatalf("gap pass projected %d resources", projected)
	}
}

func TestSourceObservationProposalRoundtripSuccessProjects(t *testing.T) {
	service, db, startRun := roundtripHarness(t)
	run := startRun("roundtrip-success-0001")
	runID := parseRoundtripID(t, run.ID)
	input := sourceObservationInput{ObservationRunID: runID, ObjectType: "target"}
	attemptID := roundtripAttemptID(t, db, runID)
	canonical, err := marshalSourceObservationProposal(attemptID, input, "success", []observedTarget{
		{
			Identity:    map[string]string{"job": "web", "instance": "one:80"},
			Labels:      map[string]string{"job": "web", "instance": "one:80"},
			DisplayName: "web/one:80",
		},
	}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.CommitProposal(context.Background(), attemptID, "roundtrip-boot", 1, canonical); err != nil {
		t.Fatalf("success proposal rejected by the control plane: %v", err)
	}
	if state := roundtripRunState(t, db, runID); state != "Completed" {
		t.Fatalf("success run state = %s", state)
	}
	object := roundtripObject(t, db, runID)
	if object.Status != "ok" || object.Evidence == nil {
		t.Fatalf("success run wrong: %#v", object)
	}
	var labelsJSON string
	if err := db.QueryRow(`SELECT labels_json FROM observed_source_objects`).Scan(&labelsJSON); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(labelsJSON, `"instance":"one:80"`) {
		t.Fatalf("identity projection wrong: %s", labelsJSON)
	}
}

// roundtripRunState reads the converged run state directly (the HTTP read
// surface is retired from observation.Service).
func roundtripRunState(t *testing.T, db *sql.DB, runID int64) string {
	t.Helper()
	var state string
	if err := db.QueryRow(`SELECT state FROM observation_runs WHERE id=?`, runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// roundtripObject asserts the run's single child object row.
func roundtripObject(t *testing.T, db *sql.DB, runID int64) struct {
	Status    string
	GapReason *string
	Evidence  *int64
	Attempt   *int64
} {
	t.Helper()
	var item struct {
		Status    string
		GapReason *string
		Evidence  *int64
		Attempt   *int64
	}
	if err := db.QueryRow(`SELECT status,gap_reason,evidence_id,attempt_id FROM observation_run_objects WHERE observation_run_id=?`, runID).Scan(&item.Status, &item.GapReason, &item.Evidence, &item.Attempt); err != nil {
		t.Fatal(err)
	}
	return item
}

func parseRoundtripID(t *testing.T, raw string) int64 {
	t.Helper()
	var id int64
	if _, err := fmt.Sscan(raw, &id); err != nil {
		t.Fatalf("parse locator %q: %v", raw, err)
	}
	return id
}

func roundtripAttemptID(t *testing.T, db *sql.DB, runID int64) int64 {
	t.Helper()
	object := roundtripObject(t, db, runID)
	if object.Attempt == nil {
		t.Fatal("run has no bound child attempt")
	}
	return *object.Attempt
}
