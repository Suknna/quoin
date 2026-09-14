package businesssystem

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// seedHistoricalRefreshRun creates facts shaped like rows retained from the
// retired producer with pure test-only SQL. The retired producer is gone
// (ADR0004); the historical result and cancellation convergence paths must be
// exercised against lawful retained rows instead of a resurrected producer.
func seedHistoricalRefreshRun(t *testing.T, h *harness, attemptState string) (runID, attemptID int64, discoveryKey string) {
	t.Helper()
	version := h.mustUpload(t, validSystemYAML, h.principal, "cmd-history-upload-0001")
	h.publishFixture(t, "payments", versionID(t, version))
	var produced int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM resource_refresh_runs`).Scan(&produced); err != nil || produced != 0 {
		t.Fatalf("fixture publish must not start a retired refresh producer: runs=%d err=%v", produced, err)
	}
	var systemID, configVersionID int64
	if err := h.db.QueryRow(`SELECT b.id,b.current_config_version_id FROM business_systems b WHERE b.key='payments'`).Scan(&systemID, &configVersionID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Reproduce the retained producer's lawful lifecycle: insert Queued without
	// evidence, then promote to Running when execution evidence is established.
	result, err := h.db.Exec(`INSERT INTO resource_refresh_runs(business_system_id,config_version_id,trigger_kind,state,row_version,created_at) VALUES(?,?,'manual','Queued',1,?)`, systemID, configVersionID, now)
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
	snapshot, err := h.db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,'resource_discovery_execution_v1','v1',?,?)`, attemptID, "0000000000000000000000000000000000000000000000000000000000000000", now)
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

func resourceRefreshRunID(t *testing.T, run ResourceRefreshRunDetail) int64 {
	t.Helper()
	id, err := strconv.ParseInt(run.ID, 10, 64)
	if err != nil {
		t.Fatalf("parse resource refresh run ID %q: %v", run.ID, err)
	}
	return id
}

// TestHistoricalRefreshRunsAndObservedResourcesRemainReadable proves the
// retained history reads still serve facts the retired producer left behind:
// a Completed run and its observed-resource projection seeded with pure SQL
// stay readable without any retired write API.
func TestHistoricalRefreshRunsAndObservedResourcesRemainReadable(t *testing.T) {
	h := newHarness(t)
	version := h.mustUpload(t, validSystemYAML, h.principal, "cmd-history-read-upload-0001")
	h.publishFixture(t, "payments", versionID(t, version))
	var systemID, configVersionID int64
	if err := h.db.QueryRow(`SELECT id,current_config_version_id FROM business_systems WHERE key='payments'`).Scan(&systemID, &configVersionID); err != nil {
		t.Fatal(err)
	}
	const observedAt = "2026-09-11T10:00:00Z"
	// Reproduce the retired producer's lawful lifecycle: Queued insert without
	// evidence, evidence-backed Running, then the terminal Completed state.
	result, err := h.db.Exec(`INSERT INTO resource_refresh_runs(business_system_id,config_version_id,trigger_kind,state,row_version,created_at) VALUES(?,?,'manual','Queued',1,?)`, systemID, configVersionID, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE resource_refresh_runs SET state='Running',evidence_at=?,row_version=2 WHERE id=? AND state='Queued'`, observedAt, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE resource_refresh_runs SET state='Completed',row_version=3 WHERE id=? AND state='Running'`, runID); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("historical-identity")))
	resource, err := h.db.Exec(`INSERT INTO observed_resources(business_system_id,discovery_key,identity_key,identity_digest,labels_json,observed_at,current,last_successful_refresh_at,created_at) VALUES(?, 'web-pods', 'instance=one'||char(31)||'job=web', ?, ?, ?, 1, ?, ?)`, systemID, digest, `{"instance":"one","job":"web","pod":"one-v1"}`, observedAt, observedAt, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	resourceID, err := resource.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO observed_resource_identity_labels(observed_resource_id,name,value) VALUES(?, 'instance', 'one'), (?, 'job', 'web')`, resourceID, resourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO observed_refresh_log(resource_refresh_run_id,attempt_id,business_system_id,discovery_key,started_at,completed_at,complete) VALUES(?, NULL, ?, 'web-pods', ?, ?, 1)`, runID, systemID, observedAt, observedAt); err != nil {
		t.Fatal(err)
	}

	run, err := h.systems.GetResourceRefresh(context.Background(), "payments", runID)
	if err != nil || run.State != "Completed" || run.EvidenceAt == nil || run.ConfigVersionID != strconv.FormatInt(configVersionID, 10) {
		t.Fatalf("historical run must stay readable: %#v %v", run, err)
	}
	resources, _, err := h.systems.ListObservedResources(context.Background(), "payments", nil, 0, 50)
	if err != nil || len(resources) != 1 || !resources[0].Current || resources[0].IdentityLabels["instance"] != "one" || resources[0].IdentityLabels["job"] != "web" {
		t.Fatalf("historical observed projection must stay listable: %#v %v", resources, err)
	}
	detail, err := h.systems.GetObservedResource(context.Background(), "payments", resourceID)
	if err != nil || detail.Labels["pod"] != "one-v1" || detail.LastSuccessfulRefreshAt == nil {
		t.Fatalf("historical observed resource must stay fetchable: %#v %v", detail, err)
	}
}

// TestHistoricalResourceRefreshCancellationStillConverges keeps the retained
// maintenance drain behavior alive: a cancelled historical child must close
// its parent run exactly once, without re-executing anything.
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
