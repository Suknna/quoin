package app

// Regression coverage for the source observation recovery slice (mall-shop
// incident 2026-09-16): the control-stream envelope adjudicators must route
// observation_run children to the observation authority — never silently drop
// them through the config verification aggregate — and must survive a request
// context that died with the transport, because the durable accept is a
// bounded adjudication of one envelope, not work owned by the stream. The
// reconnect redispatch must rebuild the frozen source_observation_execution_v1
// input through the observation rebuilder so an Assigned child the runtime
// never accepted recovers through the normal product path.

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

func TestSourceObservationAcceptRoutesToObservationAuthorityAndSurvivesDeadRequestContext(t *testing.T) {
	db, service, _, attemptID := newSourceObservationRecoveryFixture(t)
	// The transport died between frame delivery and adjudication (stream
	// replacement under single-connection SQLite contention): the durable
	// accept must still commit on its detached, bounded scope.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	envelope := &runtimev1.ControlEnvelope{BootId: "plinth-boot", ConnectionEpoch: 1}
	accept := &runtimev1.AttemptAccept{AttemptId: attemptID}
	service.handleAttemptAcceptRouted(ctx, envelope, accept)
	var state string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	if state != "Running" {
		t.Fatalf("observation accept on a dead request context left attempt %d in %s, want Running", attemptID, state)
	}
}

func TestSourceObservationCancelAckConvergesRunThroughObservationAuthority(t *testing.T) {
	db, service, _, attemptID := newSourceObservationRecoveryFixture(t)
	// Simulate the stuck cancellation fence of the incident: the runtime
	// confirmed the stop, the CancelAck arrives, the run must converge.
	mustExec(t, db, `UPDATE execution_attempts SET state='Cancelling',row_version=row_version+1 WHERE id=?`, attemptID)
	service.handleCancelAckRouted(context.Background(), "plinth", &runtimev1.CancelAck{AttemptId: attemptID})
	var attemptState, runState string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &attemptState, attemptID)
	mustQuery(t, db, `SELECT r.state FROM observation_runs r JOIN execution_attempts a ON a.scope_id=r.id WHERE a.id=?`, &runState, attemptID)
	if attemptState != "Cancelled" {
		t.Fatalf("observation cancel ack left attempt %d in %s, want Cancelled", attemptID, attemptState)
	}
	if runState != "CompletedWithWarnings" {
		t.Fatalf("observation cancel ack left the run in %s, want CompletedWithWarnings with an honest cancelled gap", runState)
	}
	var gapReason string
	mustQuery(t, db, `SELECT gap_reason FROM observation_run_objects WHERE attempt_id=?`, &gapReason, attemptID)
	if gapReason != "cancelled" {
		t.Fatalf("object gap reason is %q, want cancelled", gapReason)
	}
}

func TestReconnectRedispatchRebuildsObservationInputThroughObservationAuthority(t *testing.T) {
	_, service, sent, attemptID := newSourceObservationRecoveryFixture(t)
	boot := "plinth-boot"
	epoch := int64(1)
	view := attempt.View{ID: attemptID, AttemptType: "inspection_collection", ScopeType: "observation_run", ScopeID: 1, State: "Assigned", BootID: &boot, ConnectionEpoch: &epoch}
	// Drive the real reconnect alignment: the runtime did not report the
	// Assigned child, so the reconciler must re-dispatch it (RUNTIME-TASK-005).
	service.alignReconcileReport(context.Background(), "plinth-boot", []attempt.View{view}, nil)
	frames := sent()
	if len(frames) != 1 {
		t.Fatalf("redispatch sent %d frames, want 1", len(frames))
	}
	dispatch := frames[0].GetDispatchAttempt()
	if dispatch == nil {
		t.Fatalf("redispatch sent %T, want DispatchAttempt", frames[0].Msg)
	}
	if dispatch.GetScopeType() != runtimev1.ScopeType_SCOPE_TYPE_OBSERVATION_RUN {
		t.Fatalf("redispatch scope type %s, want SCOPE_TYPE_OBSERVATION_RUN", dispatch.GetScopeType())
	}
	if got := dispatch.GetInput().GetSchemaKind(); got != "source_observation_execution_v1" {
		t.Fatalf("redispatch schema kind %q, want source_observation_execution_v1", got)
	}
	if dispatch.GetAttemptId() != attemptID {
		t.Fatalf("redispatch attempt id %d, want %d", dispatch.GetAttemptId(), attemptID)
	}
}

// newSourceObservationRecoveryFixture seeds one Running observation run with
// one Assigned discovery child (frozen snapshot, source grant, persisted
// correlation) over the frozen schema, and wires only the authorities the
// envelope adjudicators need. It returns the runtime service, the outbound
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
	lease := time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339Nano)
	d64 := fmt.Sprintf("%064x", 1)
	// The enabled-connection chain the source grant closure trigger verifies:
	// root key binding → connection → revision → credential generation →
	// passed metrics probe → explicit enable qualification → enable.
	mustExec(t, db, `INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now)
	mustExec(t, db, `INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'x',?,?)`, now, now)
	// The plinth slot is registered with a confirmed credential: the attempt
	// binding trigger refuses dispatch against an unregistered slot.
	mustExec(t, db, `INSERT INTO runtime_slots(slot,state,row_version,created_at) VALUES('plinth','unregistered',1,?)`, now)
	credentialResult, err := db.Exec(`INSERT INTO runtime_credentials(slot,generation,token_digest,created_at,confirmed_at,row_version) VALUES('plinth',1,?,?,?,1)`, make([]byte, 32), now, now)
	if err != nil {
		t.Fatal(err)
	}
	credentialID, _ := credentialResult.LastInsertId()
	mustExec(t, db, `UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=2 WHERE slot='plinth'`, credentialID)
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
	mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='plinth-boot',connection_epoch=1,lease_until=?,runtime_release_version='p',row_version=row_version+1 WHERE id=1`, lease)

	registry := plugins.NewRegistry()
	if err := registry.RegisterDescriptor(plugins.Descriptor{
		ID: "prometheus", Version: "1", DisplayName: "Prometheus", Description: "metrics source",
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
	observations := observation.NewService(db, registry, enabled)
	if err := observations.SetReader(fixtureReadOnlyPool(t, db)); err != nil {
		t.Fatal(err)
	}
	analyses := analysis.NewService(db)
	if err := analyses.SetReader(fixtureReadOnlyPool(t, db)); err != nil {
		t.Fatal(err)
	}
	// The cancel-ack adjudicator reads the attempt scope through the
	// inspection service's reader before routing to the observation authority.
	inspections := inspection.NewService(db)
	if err := inspections.SetReader(fixtureReadOnlyPool(t, db)); err != nil {
		t.Fatal(err)
	}
	conns := connections.NewService(db, func() ([]byte, error) { return make([]byte, 32), nil })
	if err := conns.SetReader(fixtureReadOnlyPool(t, db)); err != nil {
		t.Fatal(err)
	}
	slots := qruntime.NewService(db)
	if err := slots.SetReader(fixtureReadOnlyPool(t, db)); err != nil {
		t.Fatal(err)
	}
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
