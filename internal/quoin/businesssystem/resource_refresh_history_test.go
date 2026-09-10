package businesssystem

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// seedHistoricalRefreshRun creates facts shaped like rows retained from the
// retired producer. Tests must not invoke a fresh producer to exercise the
// historical result and cancellation paths.
func seedHistoricalRefreshRun(t *testing.T, h *harness, attemptState string) (runID, attemptID int64, discoveryKey string) {
	t.Helper()
	version := h.mustUpload(t, validSystemYAML, h.principal, "cmd-history-upload-0001")
	if _, err := h.systems.Publish(context.Background(), h.principal, "cmd-history-publish-0002", "payments", versionID(t, version), nil); err != nil {
		t.Fatal(err)
	}
	var produced int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM resource_refresh_runs`).Scan(&produced); err != nil || produced != 0 {
		t.Fatalf("publishing must not start a retired refresh producer: runs=%d err=%v", produced, err)
	}
	var systemID, configVersionID, contractID int64
	if err := h.db.QueryRow(`SELECT b.id,b.current_config_version_id,v.label_contract_version_id FROM business_systems b JOIN business_system_config_versions v ON v.id=b.current_config_version_id WHERE b.key='payments'`).Scan(&systemID, &configVersionID, &contractID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Reproduce the retained producer's lawful lifecycle: insert Queued without
	// evidence, then promote to Running when execution evidence is established.
	result, err := h.db.Exec(`INSERT INTO resource_refresh_runs(business_system_id,config_version_id,label_contract_version_id,trigger_kind,state,row_version,created_at) VALUES(?,?,?,'manual','Queued',1,?)`, systemID, configVersionID, contractID, now)
	if err != nil {
		t.Fatal(err)
	}
	runID, err = result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE resource_refresh_runs SET state='Running', evidence_at=?, row_version=2 WHERE id=? AND state='Queued'`, now, runID); err != nil {
		t.Fatal(err)
	}
	// Attempt creation precedes its input freeze and dispatch binding. Historical
	// snapshots were durable before a worker received the attempt, so seed the
	// same lifecycle rather than constructing an impossible Running row.
	result, err = h.db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,discovery_key,state,quoin_release_version,created_at) VALUES('inspection_collection','resource_refresh_run',?,'web-pods','Queued','test',?)`, runID, now)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err = result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	// A historical dispatcher only bound an attempt after its immutable input
	// lineage existed. The retained rows need one valid snapshot item to cross
	// the same dispatch-ready fence; refresh attempts have no new global grant.
	snapshot, err := h.db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,'resource_refresh_execution_v1','v1',?,?)`, attemptID, "0000000000000000000000000000000000000000000000000000000000000000", now)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, err := snapshot.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,business_system_config_version_id) VALUES(?,1,'config_version',?,?)`, snapshotID, "0000000000000000000000000000000000000000000000000000000000000000", configVersionID); err != nil {
		t.Fatal(err)
	}
	if err := attempt.NewService(h.db).BindToStream(context.Background(), attemptID, "legacy-boot", 1, time.Minute, "legacy"); err != nil {
		t.Fatal(err)
	}
	if attemptState == "Running" {
		if err := attempt.NewService(h.db).Accept(context.Background(), attemptID, "legacy-boot", 1); err != nil {
			t.Fatal(err)
		}
	}
	if attemptState == "Cancelling" {
		if _, err := attempt.NewService(h.db).CancelFence(context.Background(), attemptID); err != nil {
			t.Fatal(err)
		}
	}
	return runID, attemptID, "web-pods"
}

func TestHistoricalResourceRefreshResultStillConverges(t *testing.T) {
	h := newHarness(t)
	runID, attemptID, discoveryKey := seedHistoricalRefreshRun(t, h, "Running")
	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	raw := []byte(fmt.Sprintf(`{"schemaKind":"resource_discovery_result_v1","attemptId":%d,"resourceRefreshRunId":%d,"discoveryKey":%q,"outcome":"success","observedAt":%q,"series":[{"labels":{"job":"web","instance":"one"},"value":"1","timestamp":1}]}`, attemptID, runID, discoveryKey, observedAt))
	if err := h.systems.CommitResourceRefreshProposal(context.Background(), attemptID, "legacy-boot", 1, raw); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := h.db.QueryRow(`SELECT state FROM resource_refresh_runs WHERE id=?`, runID).Scan(&state); err != nil || state != "Completed" {
		t.Fatalf("historical result must converge its parent: state=%q err=%v", state, err)
	}
	resources, _, err := h.systems.ListObservedResources(context.Background(), "payments", nil, 0, 50)
	if err != nil || len(resources) != 1 || resources[0].IdentityLabels["instance"] != "one" {
		t.Fatalf("historical result must retain the observed projection: %#v %v", resources, err)
	}
}

func TestHistoricalResourceRefreshCancellationStillConverges(t *testing.T) {
	h := newHarness(t)
	runID, attemptID, _ := seedHistoricalRefreshRun(t, h, "Cancelling")
	if err := attempt.NewService(h.db).CancelAck(context.Background(), attemptID); err != nil {
		t.Fatal(err)
	}
	if err := h.systems.ConvergeResourceRefreshCancelAck(context.Background(), attemptID); err != nil {
		t.Fatal(err)
	}
	var runState, attemptState string
	if err := h.db.QueryRow(`SELECT state FROM resource_refresh_runs WHERE id=?`, runID).Scan(&runState); err != nil || runState != "Cancelled" {
		t.Fatalf("historical cancelled child must close its parent: state=%q err=%v", runState, err)
	}
	if err := h.db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&attemptState); err != nil || attemptState != "Cancelled" {
		t.Fatalf("historical cancellation must retain terminal attempt: state=%q err=%v", attemptState, err)
	}
	var logs int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM observed_refresh_log WHERE attempt_id=?`, attemptID).Scan(&logs); err != nil || logs != 1 {
		t.Fatalf("historical cancellation must record one immutable gap: logs=%d err=%v", logs, err)
	}
}

// A cancelled verification parent must still absorb its late technical gap.
// This guard is independent from the retired refresh producer.
func TestSweptVerificationParentClosureSkipsClosedRun(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, validSystemYAML, h.principal, "cmd-gap4-upload-0001")
	run, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-gap4-run-0002", "payments", versionID(t, draft))
	if err != nil {
		t.Fatal(err)
	}
	var attemptID int64
	if err := h.db.QueryRow(`SELECT id FROM execution_attempts WHERE scope_type='config_verification_run' AND scope_id=?`, run.ID).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.systems.CancelVerification(context.Background(), h.principal, "cmd-gap4-cancel-0003", "payments", versionID(t, draft), verificationRunID(t, run), 2); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.Exec(`UPDATE execution_attempts SET state='Cancelled', row_version=row_version+1, ended_at=?, termination_reason='cancelled' WHERE id=?`, now, attemptID); err != nil {
		t.Fatal(err)
	}
	if err := h.systems.RecordVerificationTechnicalGap(context.Background(), attemptID, "cancelled"); err != nil {
		t.Fatalf("closed parent must absorb the gap no-op: %v", err)
	}
	var results int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM config_verification_run_check_results WHERE verification_run_id=?`, run.ID).Scan(&results); err != nil || results != 0 {
		t.Fatalf("closed parent must not gain check results: %d %v", results, err)
	}
}
