package app

// Regression coverage for the source observation slice in its ADR-0011 local
// execution shape: observation_run children bind, execute (metrics_discover
// internal tool) and commit through the observation authority inside Quoin,
// a dead caller context never kills a claimed execution, cancellation fences
// converge locally without a runtime round trip, and the Plinth reconnect
// reconciliation neither dispatches nor disturbs locally-bound children.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/inspection"
	"github.com/Suknna/quoin/internal/quoin/observation"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	_ "modernc.org/sqlite"
)

// observationRecoveryInput mirrors the frozen source_observation_execution_v1
// canonical shape (field order included: json.Marshal of a struct is order
// sensitive and the sealed snapshot digest must match the observation
// package's deterministic rebuild byte for byte).
type observationRecoveryInput struct {
	SchemaKind       string   `json:"schemaKind"`
	AttemptID        int64    `json:"attemptId"`
	ObservationRunID int64    `json:"observationRunId"`
	PluginID         string   `json:"pluginId"`
	ObjectType       string   `json:"objectType"`
	Query            string   `json:"query"`
	IdentityLabels   []string `json:"identityLabels"`
	Limit            int      `json:"limit"`
	GrantID          int64    `json:"grantId"`
}

// stubLocalDiscoverTool replaces the internal tool lookup for one test: the
// discover stub returns one complete pass over a single target.
func stubLocalDiscoverTool(t *testing.T) {
	t.Helper()
	previous := localMetricsToolEntry
	localMetricsToolEntry = func(name string) (plugins.ToolEntry, bool) {
		if name != "metrics_discover" {
			return plugins.ToolEntry{}, false
		}
		return plugins.ToolEntry{Timeout: time.Second, Invoke: func(ctx context.Context, exec plugins.ToolExecution) (json.RawMessage, error) {
			if exec.Conn.Type != "prometheus" || exec.Conn.ID != 1 {
				return nil, fmt.Errorf("discover stub got connection %d/%s", exec.Conn.ID, exec.Conn.Type)
			}
			return json.Marshal(plugins.DiscoverResult{Objects: []plugins.DiscoveredObject{{
				ObjectType: "target", CanonicalIdentity: "job=api,instance=127.0.0.1:9090", DisplayName: "api/127.0.0.1:9090",
			}}})
		}}, true
	}
	t.Cleanup(func() { localMetricsToolEntry = previous })
}

// TestLocalObservationExecutionRunsThroughObservationAuthorityAndSurvivesDeadRequestContext
// drives one full local execution: the Queued child binds under the local
// identity, the frozen input rebuilds through the observation authority, the
// internal discovery tool executes, and the typed result commits with the run
// converging — all on a caller context that died before the scan started.
func TestLocalObservationExecutionRunsThroughObservationAuthorityAndSurvivesDeadRequestContext(t *testing.T) {
	stubLocalDiscoverTool(t)
	db, service, sent, attemptID := newSourceObservationRecoveryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service.runLocalExecutionPass(ctx)

	var state, runState string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	if state != "Succeeded" {
		t.Fatalf("local observation execution left attempt %d in %s, want Succeeded", attemptID, state)
	}
	mustQuery(t, db, `SELECT r.state FROM observation_runs r JOIN execution_attempts a ON a.scope_id=r.id WHERE a.id=?`, &runState, attemptID)
	if runState != "Completed" {
		t.Fatalf("local observation execution left the run in %s, want Completed", runState)
	}
	var identityKey string
	mustQuery(t, db, `SELECT identity_key FROM observed_source_objects WHERE connection_id=1 AND object_type='target'`, &identityKey)
	if identityKey != "instance=127.0.0.1:9090\x1fjob=api" {
		t.Fatalf("observed identity key %q, want the projected source identity", identityKey)
	}
	var boot string
	mustQuery(t, db, `SELECT boot_id FROM execution_attempts WHERE id=?`, &boot, attemptID)
	if boot != localExecutionBootID {
		t.Fatalf("local execution binding boot %q, want %q", boot, localExecutionBootID)
	}
	if frames := sent(); len(frames) != 0 {
		t.Fatalf("local execution sent %d control frames, want none", len(frames))
	}
}

// TestLocalObservationCancellationConvergesWithoutRuntimeFrames proves the
// Cancelling fence of a locally executed child converges in-process: the
// attempt reaches Cancelled, the object records its honest cancelled gap and
// the run closes with warnings — no CancelAttempt/CancelAck frame involved.
func TestLocalObservationCancellationConvergesWithoutRuntimeFrames(t *testing.T) {
	db, service, sent, attemptID := newSourceObservationRecoveryFixture(t)
	// Claim the child locally first: the cancellation fence of a locally
	// executed child must converge without a runtime round trip.
	if err := service.bindLocalAttempt(context.Background(), service.Observations.Attempts(), attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Observations.Attempts().CancelFence(context.Background(), attemptID); err != nil {
		t.Fatal(err)
	}
	if err := service.dispatchCancelRouted(context.Background(), attemptID); err != nil {
		t.Fatal(err)
	}
	var attemptState, runState string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &attemptState, attemptID)
	mustQuery(t, db, `SELECT r.state FROM observation_runs r JOIN execution_attempts a ON a.scope_id=r.id WHERE a.id=?`, &runState, attemptID)
	if attemptState != "Cancelled" {
		t.Fatalf("local cancellation left attempt %d in %s, want Cancelled", attemptID, attemptState)
	}
	if runState != "CompletedWithWarnings" {
		t.Fatalf("local cancellation left the run in %s, want CompletedWithWarnings with an honest cancelled gap", runState)
	}
	var gapReason string
	mustQuery(t, db, `SELECT gap_reason FROM observation_run_objects WHERE attempt_id=?`, &gapReason, attemptID)
	if gapReason != "cancelled" {
		t.Fatalf("object gap reason is %q, want cancelled", gapReason)
	}
	if frames := sent(); len(frames) != 0 {
		t.Fatalf("local cancellation sent %d control frames, want none", len(frames))
	}
}

// TestReconnectSkipsLocallyBoundObservationChildren proves the Plinth
// reconnect reconciliation ignores locally-bound children (they are owned by
// the local executor and the lease sweeper) and converges a legacy
// plinth-bound Assigned child as loss instead of dispatching it.
func TestReconnectSkipsLocallyBoundObservationChildren(t *testing.T) {
	db, service, sent, attemptID := newSourceObservationRecoveryFixture(t)
	localBoot := localExecutionBootID
	epoch := int64(1)
	// A locally bound Running child must not be interrupted by a Plinth Hello.
	if err := service.bindLocalAttempt(context.Background(), service.Observations.Attempts(), attemptID); err != nil {
		t.Fatal(err)
	}
	service.alignReconcileReport(context.Background(), "plinth-boot", []attempt.View{{
		ID: attemptID, AttemptType: "inspection_collection", ScopeType: "observation_run", ScopeID: 1,
		State: "Running", BootID: &localBoot, ConnectionEpoch: &epoch,
	}}, nil)
	var state string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	if state != "Running" {
		t.Fatalf("reconcile disturbed a locally bound child (state %s), want Running", state)
	}
	// A legacy plinth-bound Assigned child the runtime never reported converges
	// as loss: nothing can execute it through the frame protocol anymore.
	legacy := attempt.View{
		ID: attemptID, AttemptType: "inspection_collection", ScopeType: "observation_run", ScopeID: 1,
		State: "Assigned", BootID: ptr("plinth-boot"), ConnectionEpoch: ptrInt64(1),
	}
	service.alignReconcileReport(context.Background(), "plinth-boot", []attempt.View{legacy}, nil)
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	if state != "Interrupted" {
		t.Fatalf("legacy unreported Assigned child is %s, want Interrupted loss convergence", state)
	}
	var runState string
	mustQuery(t, db, `SELECT r.state FROM observation_runs r JOIN execution_attempts a ON a.scope_id=r.id WHERE a.id=?`, &runState, attemptID)
	if runState != "CompletedWithWarnings" {
		t.Fatalf("legacy child left the run in %s, want CompletedWithWarnings with an honest interrupted gap", runState)
	}
	if frames := sent(); len(frames) != 0 {
		t.Fatalf("reconcile sent %d control frames, want none", len(frames))
	}
}

func ptr(value string) *string { return &value }

func ptrInt64(value int64) *int64 { return &value }

// newSourceObservationRecoveryFixture seeds one Running observation run with
// one Queued discovery child (frozen snapshot, source grant, persisted
// correlation) over the frozen schema, and wires only the authorities the
// local execution path needs. It returns the runtime service, the outbound
// frame collector and the seeded attempt id.
func newSourceObservationRecoveryFixture(t *testing.T) (*sql.DB, *RuntimeService, func() []*runtimev1.ControlEnvelope, int64) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/observation-recovery.db?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	d64 := fmt.Sprintf("%064x", 1)
	// The enabled-connection chain the source grant closure trigger verifies:
	// root key binding → connection → revision → credential generation →
	// passed metrics probe → explicit enable qualification → enable.
	mustExec(t, db, `INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now)
	mustExec(t, db, `INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'x',?,?)`, now, now)
	mustExec(t, db, `INSERT INTO connections(id,name,type,enabled,row_version,revalidation_required,created_at) VALUES(1,'main-prometheus','prometheus',0,1,0,?)`, now)
	mustExec(t, db, `INSERT INTO connection_revisions(id,connection_id,revision_seq,config_json,created_at) VALUES(1,1,1,'{}',?)`, now)
	mustExec(t, db, `INSERT INTO credential_generations(id,connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(1,1,1,1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now)
	mustExec(t, db, `UPDATE connections SET current_revision_id=1,current_credential_generation_id=1,row_version=2 WHERE id=1`)
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES(900,'connection_probe','connection',1,'Queued','q',?)`, now)
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(900,900,'connection_probe_v1','v1',?,?)`, d64, now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(900,1,'connection_revision',?,1)`, d64)
	mustExec(t, db, `INSERT INTO attempt_connection_grants(id,attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(901,900,'prometheus_probe',1,1,1,?)`, now)
	mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='probe-boot',connection_epoch=1,lease_until=?,runtime_release_version='p',row_version=row_version+1 WHERE id=900`, now)
	mustExec(t, db, `UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=900`, now, now)
	mustExec(t, db, `INSERT INTO connection_probe_results(id,attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(1,900,1,'prometheus',1,1,1,'fixture',1,?,'passed',?,?,?,?)`, d64, d64, now, now, now)
	mustExec(t, db, `INSERT INTO thanos_connection_probe_results(probe_result_id,query,response_type,sample_count,sample_value,detail_json) VALUES(1,'vector(1)','vector',1,'1','{}')`)
	mustExec(t, db, `UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=900`, now)
	mustExec(t, db, `INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(1,3,1,1,?)`, now)
	mustExec(t, db, `UPDATE connections SET enabled=1,row_version=3 WHERE id=1`)
	// The run and its unclaimed object row precede the child attempt, exactly
	// like the admission path: the frozen scope trigger demands it.
	mustExec(t, db, `INSERT INTO observation_runs(id,connection_id,plugin_id,trigger_kind,state,row_version,created_at) VALUES(1,1,'prometheus','enablement','Running',1,?)`, now)
	mustExec(t, db, `INSERT INTO observation_run_objects(observation_run_id,object_type,status,gap_reason,created_at) VALUES(1,'target','gap','runtime_unavailable',?)`, now)
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,discovery_key,state,quoin_release_version,operation_correlation_id,initiator_type,initiator_id,created_at)
		VALUES(1,'inspection_collection','observation_run',1,'target','Queued','q','corr-observation-recovery','system',0,?)`, now)
	mustExec(t, db, `UPDATE observation_run_objects SET attempt_id=1 WHERE observation_run_id=1 AND object_type='target'`)
	mustExec(t, db, `INSERT INTO attempt_connection_grants(id,attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(1,1,'config_thanos_query',1,1,1,?)`, now)
	// The sealed snapshot digest equals the observation authority's
	// deterministic rebuild of the frozen input.
	input := observationRecoveryInput{
		SchemaKind: "source_observation_execution_v1", AttemptID: 1, ObservationRunID: 1,
		PluginID: "prometheus", ObjectType: "target", Query: "up",
		IdentityLabels: []string{"job", "instance"}, Limit: 500, GrantID: 1,
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(1,1,'source_observation_execution_v1','v1',?,?)`, hex.EncodeToString(digest[:]), now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(1,1,'connection_revision',?,1)`, d64)

	registry := plugins.NewRegistry()
	if err := registry.Register(plugins.Plugin{
		ID: "prometheus", Version: "1", DisplayName: "Prometheus", Description: "metrics source",
		ConnectionKind: "prometheus", DefaultEnabled: true,
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
	observations := observation.NewService(db, registry, enabled)
	if err := observations.SetReader(fixtureReadOnlyPool(t, db)); err != nil {
		t.Fatal(err)
	}
	analyses := analysis.NewService(db)
	if err := analyses.SetReader(fixtureReadOnlyPool(t, db)); err != nil {
		t.Fatal(err)
	}
	inspections := inspection.NewService(db)
	if err := inspections.SetReader(fixtureReadOnlyPool(t, db)); err != nil {
		t.Fatal(err)
	}
	conns := connections.NewService(db, func() ([]byte, error) { return make([]byte, 32), nil })
	if err := conns.SetReader(fixtureReadOnlyPool(t, db)); err != nil {
		t.Fatal(err)
	}
	slots := qruntime.NewService()
	var sent []*runtimev1.ControlEnvelope
	service := &RuntimeService{
		Slots: slots, Analyses: analyses, Inspections: inspections, Observations: observations, Connections: conns, writer: db,
		sendEnvelopeForTest: func(_ string, envelope *runtimev1.ControlEnvelope) error {
			sent = append(sent, envelope)
			return nil
		},
	}
	return db, service, func() []*runtimev1.ControlEnvelope { return sent }, 1
}
