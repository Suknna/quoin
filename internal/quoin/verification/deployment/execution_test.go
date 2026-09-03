package deployment_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/businesssystem"
	"github.com/Suknna/quoin/internal/quoin/verification/deployment"
)

// The product-executed items drive the real machinery: connection items get
// their probe started, config items get a deployment_acceptance run, and the
// reconciler maps both onto immutable item results (VERIFY-CONFIG-001,
// executor kind quoin).
func TestExecutionEnqueueAndReconcile(t *testing.T) {
	h := newHarness(t)
	h.wireArtifacts()
	h.seedReadyIdentity()
	h.seedThanosConnection()
	h.seedPublishedConfig()

	probeCalls := []string{}
	h.service.SetExecutionHooks(deployment.ExecutionHooks{
		StartConnectionProbe: func(ctx context.Context, connectionName string) error {
			probeCalls = append(probeCalls, connectionName)
			return nil
		},
		RunConfigVerification: func(ctx context.Context, principalID, systemID, versionID, contractVersionID, manifestItemID int64) error {
			systems := businesssystem.NewService(h.db)
			systems.UseClock(h.service.Now)
			_, err := systems.RunDeploymentAcceptanceVerification(ctx, principalID, systemID, versionID, contractVersionID, manifestItemID)
			return err
		},
	})

	invocation := h.start()
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_invocation_items WHERE invocation_id=? AND object_kind IN ('connection','config')`, invocation); got != 2 {
		t.Fatalf("connection+config items = %d, want 2", got)
	}
	if err := h.service.EnqueueExecution(context.Background(), invocation); err != nil {
		t.Fatalf("enqueue execution: %v", err)
	}
	if state := h.text(`SELECT state FROM config_verification_runs WHERE purpose='deployment_acceptance'`); state != "Passed" {
		t.Fatalf("zero-check DA run state = %s, want Passed (deterministic completion)", state)
	}
	// Re-enqueue must not duplicate work.
	if err := h.service.EnqueueExecution(context.Background(), invocation); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM config_verification_runs WHERE purpose='deployment_acceptance'`); got != 1 {
		t.Fatalf("DA runs = %d, want 1", got)
	}

	// A finished probe result matching the frozen binding feeds the item.
	h.seedPassedProbeResult()

	h.advance(30 * time.Minute)
	if err := h.service.ReconcileResults(context.Background(), invocation); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	configItem := h.queryInt(`SELECT id FROM verification_invocation_items WHERE invocation_id=? AND object_kind='config'`, invocation)
	connectionItem := h.queryInt(`SELECT id FROM verification_invocation_items WHERE invocation_id=? AND object_kind='connection'`, invocation)
	if outcome := h.text(`SELECT outcome FROM verification_item_results WHERE item_id=?`, configItem); outcome != "passed" {
		t.Fatalf("config item outcome = %s, want passed", outcome)
	}
	if producer := h.text(`SELECT producer_type FROM verification_item_results WHERE item_id=?`, configItem); producer != "quoin" {
		t.Fatalf("config item producer = %s", producer)
	}
	if outcome := h.text(`SELECT outcome FROM verification_item_results WHERE item_id=?`, connectionItem); outcome != "passed" {
		t.Fatalf("connection item outcome = %s, want passed", outcome)
	}
	if producer := h.text(`SELECT producer_type FROM verification_item_results WHERE item_id=?`, connectionItem); producer != "runtime" {
		t.Fatalf("connection item producer = %s", producer)
	}
	// Reconcile is idempotent.
	if err := h.service.ReconcileResults(context.Background(), invocation); err != nil {
		t.Fatalf("re-reconcile: %v", err)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_item_results WHERE item_id IN (?,?)`, configItem, connectionItem); got != 2 {
		t.Fatalf("results after replay = %d, want 2", got)
	}
}

// seedThanosConnection creates the enabled connection with its current
// revision and credential generation (the drift test's seed, reused).
func (h *harness) seedThanosConnection() {
	h.mustExec(`INSERT INTO connections(name,type,enabled,row_version,created_at) VALUES ('thanos-main','thanos',1,1,'2026-03-01T08:00:00Z')`)
	connectionID := h.queryInt(`SELECT id FROM connections WHERE name='thanos-main'`)
	h.mustExec(`INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_by,created_at) VALUES (?,1,'{"address":"https://thanos.test"}',?,'2026-03-01T08:00:00Z')`, connectionID, h.userID)
	revisionID := h.queryInt(`SELECT id FROM connection_revisions`)
	h.mustExec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_by,created_at)
		VALUES (?,1,1,1,X'000000000000000000000001',X'00000000000000000000000000000001',?,'2026-03-01T08:00:00Z')`, connectionID, h.userID)
	h.mustExec(`UPDATE connections SET current_revision_id=?,current_credential_generation_id=?,row_version=row_version+1 WHERE id=?`, revisionID, h.queryInt(`SELECT id FROM credential_generations`), connectionID)
}

// seedPassedProbeResult commits the attempt and its typed probe header
// atomically (the frozen Succeeded transition demands the domain result in
// the same transaction), matching the frozen connection binding.
func (h *harness) seedPassedProbeResult() {
	h.t.Helper()
	connectionID := h.queryInt(`SELECT id FROM connections WHERE name='thanos-main'`)
	var revisionID, generationID int64
	if err := h.db.QueryRow(`SELECT current_revision_id,current_credential_generation_id FROM connections WHERE id=?`, connectionID).Scan(&revisionID, &generationID); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.db.Exec(`BEGIN IMMEDIATE`); err != nil {
		h.t.Fatal(err)
	}
	h.mustExec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at)
		VALUES ('connection_probe','connection',?,'Queued','v1.2.3','2026-03-01T09:04:00Z')`, connectionID)
	attemptID := h.queryInt(`SELECT id FROM execution_attempts WHERE attempt_type='connection_probe'`)
	// A registered Plinth slot with a confirmed current credential is the
	// dispatch precondition.
	// Registration mirrors qruntime.Register: the confirmed credential is
	// created directly with the slot switching to registered in one step.
	h.mustExec(`INSERT INTO runtime_credentials(slot,generation,token_digest,confirmed_at,created_at)
		VALUES ('plinth',1,X'` + strings.Repeat("11", 32) + `','2026-03-01T08:01:00Z','2026-03-01T08:00:00Z')`)
	h.mustExec(`UPDATE runtime_slots SET state='registered',current_credential_id=(SELECT id FROM runtime_credentials WHERE slot='plinth'),row_version=2 WHERE slot='plinth'`)
	h.mustExec(`UPDATE runtime_credentials SET first_authenticated_at='2026-03-01T08:02:00Z',row_version=2 WHERE slot='plinth'`)
	h.mustExec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at)
		VALUES (?,'connection_probe_v1','r1','`+strings.Repeat("01", 32)+`','2026-03-01T09:04:30Z')`, attemptID)
	snapshotID := h.queryInt(`SELECT id FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID)
	h.mustExec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id)
		VALUES (?,1,'probe_input','`+strings.Repeat("02", 32)+`',?)`, snapshotID, revisionID)
	h.mustExec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at)
		VALUES (?,'thanos_probe',?,?,?,'2026-03-01T09:05:00Z')`, attemptID, connectionID, revisionID, generationID)
	h.mustExec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot-1',connection_epoch=1,lease_until='2026-03-01T09:20:00Z',runtime_release_version='v1.2.3',row_version=2 WHERE id=?`, attemptID)
	h.mustExec(`UPDATE execution_attempts SET state='Running',accepted_at='2026-03-01T09:05:00Z',row_version=3 WHERE id=?`, attemptID)
	h.mustExec(`INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at)
		VALUES (?,?,?,?,?,1,'thanos.probe.v1',1,?,'passed',?,'2026-03-01T09:05:00Z','2026-03-01T09:10:00Z','2026-03-01T09:10:00Z')`,
		attemptID, connectionID, "thanos", revisionID, generationID, deployment.ProbeContractDigest(), strings.Repeat("ab", 32))
	probeResultID := h.queryInt(`SELECT id FROM connection_probe_results WHERE attempt_id=?`, attemptID)
	h.mustExec(`INSERT INTO thanos_connection_probe_results(probe_result_id,query,response_type,sample_count,sample_value,detail_json)
		VALUES (?, 'vector(1)','vector',1,'1','{}')`, probeResultID)
	h.mustExec(`UPDATE execution_attempts SET state='Succeeded',ended_at='2026-03-01T09:10:00Z',row_version=4 WHERE id=?`, attemptID)
	if _, err := h.db.Exec(`COMMIT`); err != nil {
		h.t.Fatal(err)
	}
}

// seedPublishedConfig creates the active Label Contract and the current
// published config version with no checks (the deterministic DA completion).
func (h *harness) seedPublishedConfig() {
	h.t.Helper()
	h.mustExec(`INSERT INTO label_contracts(version,yaml_body,contract_json,digest,parser_version,schema_version,state,row_version,created_at)
		VALUES (1,'items: []','{"items":[]}','` + strings.Repeat("cd", 32) + `','p1','s1','draft',1,'2026-03-01T08:00:00Z')`)
	contractID := h.queryInt(`SELECT id FROM label_contracts`)
	// Activation is the atomic pointer INSERT; direct state updates are
	// forbidden by trigger.
	h.mustExec(`INSERT INTO label_contract_activations(contract_id,expected_target_row_version,expected_state_row_version,expected_current_contract_id,items_json,created_at)
		VALUES (?,1,1,NULL,'[]','2026-03-01T08:05:00Z')`, contractID)
	systemID := h.queryInt(`SELECT id FROM business_systems WHERE key='orders'`)
	catalogDigest := strings.Repeat("ab", 32)
	bodyDigest := strings.Repeat("ef", 32)
	h.mustExec(`INSERT INTO business_system_config_versions(business_system_id,version_seq,state,yaml_body,parser_version,schema_version,
		label_contract_version_id,journey_catalog_digest,journey_catalog_version,digest,created_by,created_at,
		system_key,display_name,enabled,timezone,resource_refresh_interval_seconds)
		VALUES (?,1,'draft','root: {}','p1','s1',?,?, 'v1',?,?,'2026-03-01T08:10:00Z',
		'orders','订单系统',1,'Asia/Shanghai',300)`, systemID, contractID, catalogDigest, bodyDigest, h.userID)
	versionID := h.queryInt(`SELECT id FROM business_system_config_versions`)
	h.mustExec(`UPDATE business_systems SET current_config_version_id=?,display_name='订单系统',enabled=1,timezone='Asia/Shanghai',resource_refresh_interval_seconds=300,row_version=row_version+1 WHERE id=?`, versionID, systemID)
}
