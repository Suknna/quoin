package businesssystem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func assignRunningAttempt(t *testing.T, h *harness, attemptID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='test-boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=?`, now, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=?`, now, now, attemptID); err != nil {
		t.Fatal(err)
	}
}

func TestStartResourceRefreshFreezesPublishedDiscoveries(t *testing.T) {
	h := newHarness(t)
	version := h.mustUpload(t, validSystemYAML, 1, "cmd-refresh-upload-0001")
	if _, err := h.systems.Publish(context.Background(), 1, "cmd-refresh-publish-0002", "payments", versionID(t, version), nil); err != nil {
		t.Fatal(err)
	}
	detail, err := h.systems.StartResourceRefresh(context.Background(), 1, "cmd-refresh-start-0003", "payments")
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "Running" || detail.ConfigVersionID != version.ID || detail.EvidenceAt == nil {
		t.Fatalf("unexpected refresh detail: %#v", detail)
	}
	var attempts, grants int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM execution_attempts WHERE scope_type='resource_refresh_run' AND scope_id=? AND attempt_type='inspection_collection'`, detail.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM attempt_connection_grants WHERE attempt_id IN (SELECT id FROM execution_attempts WHERE scope_type='resource_refresh_run' AND scope_id=?) AND purpose='config_thanos_query'`, detail.ID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if attempts == 0 || attempts != grants {
		t.Fatalf("each discovery must own one frozen supervisor attempt and grant: attempts=%d grants=%d", attempts, grants)
	}
	replayed, err := h.systems.StartResourceRefresh(context.Background(), 1, "cmd-refresh-start-0003", "payments")
	if err != nil || replayed.ID != detail.ID {
		t.Fatalf("same command must replay its authoritative Run: %#v %v", replayed, err)
	}
}

func TestSchedulerCreatesOneDueRefreshForEnabledPublishedSystem(t *testing.T) {
	h := newHarness(t)
	yaml := strings.Replace(validSystemYAML, "enabled: false", "enabled: true", 1)
	version := h.mustUpload(t, yaml, 1, "cmd-schedule-upload-0001")
	if _, err := h.systems.Publish(context.Background(), 1, "cmd-schedule-publish-0002", "payments", versionID(t, version), nil); err != nil {
		t.Fatal(err)
	}
	ids, err := h.systems.StartDueResourceRefreshes(context.Background())
	if err != nil || len(ids) != 1 {
		t.Fatalf("first due tick must create exactly one Run: %#v %v", ids, err)
	}
	firstID := ids[0]
	ids, err = h.systems.StartDueResourceRefreshes(context.Background())
	if err != nil || len(ids) != 0 {
		t.Fatalf("active Run must suppress duplicate schedule: %#v %v", ids, err)
	}
	// A terminal Run becomes due again only after its published interval.
	if _, err := h.db.Exec(`UPDATE execution_attempts SET state='Cancelled', row_version=row_version+1, ended_at=?, termination_reason='cancelled' WHERE scope_type='resource_refresh_run' AND scope_id=?`, time.Now().UTC().Format(time.RFC3339Nano), firstID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE resource_refresh_runs SET state='Cancelled', result_detail='{}', row_version=row_version+1 WHERE id=?`, firstID); err != nil {
		t.Fatal(err)
	}
	h.systems.now = func() time.Time { return time.Now().UTC().Add(6 * time.Minute) }
	ids, err = h.systems.StartDueResourceRefreshes(context.Background())
	if err != nil || len(ids) != 1 {
		t.Fatalf("elapsed refresh interval must create next schedule Run: %#v %v", ids, err)
	}
	var kind, scheduled string
	if err := h.db.QueryRow(`SELECT trigger_kind,scheduled_for FROM resource_refresh_runs WHERE id=?`, firstID).Scan(&kind, &scheduled); err != nil || kind != "schedule" || scheduled == "" {
		t.Fatalf("scheduled run must persist its trigger boundary: %q %q %v", kind, scheduled, err)
	}
}

func TestRecordResourceRefreshTechnicalGapClosesFencedRun(t *testing.T) {
	h := newHarness(t)
	version := h.mustUpload(t, validSystemYAML, 1, "cmd-gap-upload-0001")
	if _, err := h.systems.Publish(context.Background(), 1, "cmd-gap-publish-0002", "payments", versionID(t, version), nil); err != nil {
		t.Fatal(err)
	}
	run, err := h.systems.StartResourceRefresh(context.Background(), 1, "cmd-gap-start-0003", "payments")
	if err != nil {
		t.Fatal(err)
	}
	var attemptID int64
	if err := h.db.QueryRow(`SELECT id FROM execution_attempts WHERE scope_type='resource_refresh_run' AND scope_id=?`, run.ID).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE execution_attempts SET state='Cancelled', row_version=row_version+1, ended_at=?, termination_reason='cancelled' WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), attemptID); err != nil {
		t.Fatal(err)
	}
	if err := h.systems.RecordResourceRefreshTechnicalGap(context.Background(), attemptID, "cancelled"); err != nil {
		t.Fatal(err)
	}
	var state string
	var logs int
	if err := h.db.QueryRow(`SELECT state FROM resource_refresh_runs WHERE id=?`, run.ID).Scan(&state); err != nil || state != "Cancelled" {
		t.Fatalf("fenced run must converge: %q %v", state, err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM observed_refresh_log WHERE attempt_id=?`, attemptID).Scan(&logs); err != nil || logs != 1 {
		t.Fatalf("technical gap must be logged once: %d %v", logs, err)
	}
}

func TestResourceRefreshSuccessKeepsTerminalResultDetailNull(t *testing.T) {
	h := newHarness(t)
	version := h.mustUpload(t, validSystemYAML, 1, "cmd-success-upload-0001")
	if _, err := h.systems.Publish(context.Background(), 1, "cmd-success-publish-0002", "payments", versionID(t, version), nil); err != nil {
		t.Fatal(err)
	}
	run, err := h.systems.StartResourceRefresh(context.Background(), 1, "cmd-success-start-0003", "payments")
	if err != nil {
		t.Fatal(err)
	}
	var attemptID int64
	var key string
	if err := h.db.QueryRow(`SELECT id,discovery_key FROM execution_attempts WHERE scope_type='resource_refresh_run' AND scope_id=?`, run.ID).Scan(&attemptID, &key); err != nil {
		t.Fatal(err)
	}
	assignRunningAttempt(t, h, attemptID)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	raw := []byte(fmt.Sprintf(`{"schemaKind":"resource_discovery_result_v1","attemptId":%d,"resourceRefreshRunId":%s,"discoveryKey":%q,"outcome":"success","observedAt":%q,"series":[{"labels":{"job":"web","instance":"one"},"value":"1","timestamp":1}]}`, attemptID, run.ID, key, now))
	if err := h.systems.CommitResourceRefreshProposal(context.Background(), attemptID, "test-boot", 1, raw); err != nil {
		t.Fatal(err)
	}
	var state string
	var result sql.NullString
	if err := h.db.QueryRow(`SELECT state,result_detail FROM resource_refresh_runs WHERE id=?`, run.ID).Scan(&state, &result); err != nil || state != "Completed" || result.Valid {
		t.Fatalf("successful refresh must close without result_detail: %q %#v %v", state, result, err)
	}
	current := true
	items, next, err := h.systems.ListObservedResources(context.Background(), "payments", &current, 0, 50)
	if err != nil || next != 0 || len(items) != 1 {
		t.Fatalf("list current resources: %#v %v %v", items, next, err)
	}
	if items[0].DiscoveryKey != "web-pods" || items[0].IdentityLabels["job"] != "web" || items[0].IdentityLabels["instance"] != "one" || !items[0].Current || items[0].Stale {
		t.Fatalf("resource projection wrong: %#v", items[0])
	}
}

func TestStartResourceRefreshConflictsWithActiveRun(t *testing.T) {
	h := newHarness(t)
	version := h.mustUpload(t, validSystemYAML, 1, "cmd-conflict-upload-0001")
	if _, err := h.systems.Publish(context.Background(), 1, "cmd-conflict-publish-0002", "payments", versionID(t, version), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.systems.StartResourceRefresh(context.Background(), 1, "cmd-conflict-start-0003", "payments"); err != nil {
		t.Fatal(err)
	}
	_, err := h.systems.StartResourceRefresh(context.Background(), 1, "cmd-conflict-start-0004", "payments")
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Code != "active_conflict" {
		t.Fatalf("second command must surface the frozen active conflict, got %#v", err)
	}
}

func TestResourceRefreshPartialGapClosesWithWarnings(t *testing.T) {
	h := newHarness(t)
	version := h.mustUpload(t, validSystemYAML, 1, "cmd-gap2-upload-0001")
	if _, err := h.systems.Publish(context.Background(), 1, "cmd-gap2-publish-0002", "payments", versionID(t, version), nil); err != nil {
		t.Fatal(err)
	}
	run, err := h.systems.StartResourceRefresh(context.Background(), 1, "cmd-gap2-start-0003", "payments")
	if err != nil {
		t.Fatal(err)
	}
	var attemptID int64
	var key string
	if err := h.db.QueryRow(`SELECT id,discovery_key FROM execution_attempts WHERE scope_type='resource_refresh_run' AND scope_id=?`, run.ID).Scan(&attemptID, &key); err != nil {
		t.Fatal(err)
	}
	assignRunningAttempt(t, h, attemptID)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	raw := []byte(fmt.Sprintf(`{"schemaKind":"resource_discovery_result_v1","attemptId":%d,"resourceRefreshRunId":%s,"discoveryKey":%q,"outcome":"gap","observedAt":%q,"gapReason":"partial_response","warnings":["up 查询部分 series 被截断"]}`, attemptID, run.ID, key, now))
	if err := h.systems.CommitResourceRefreshProposal(context.Background(), attemptID, "test-boot", 1, raw); err != nil {
		t.Fatal(err)
	}
	var state string
	var result sql.NullString
	if err := h.db.QueryRow(`SELECT state,result_detail FROM resource_refresh_runs WHERE id=?`, run.ID).Scan(&state, &result); err != nil || state != "CompletedWithWarnings" || result.Valid {
		t.Fatalf("partial refresh must close CompletedWithWarnings without result_detail: %q %#v %v", state, result, err)
	}
}

func TestRecordResourceRefreshTechnicalGapInterruptedConverges(t *testing.T) {
	h := newHarness(t)
	version := h.mustUpload(t, validSystemYAML, 1, "cmd-gap3-upload-0001")
	if _, err := h.systems.Publish(context.Background(), 1, "cmd-gap3-publish-0002", "payments", versionID(t, version), nil); err != nil {
		t.Fatal(err)
	}
	run, err := h.systems.StartResourceRefresh(context.Background(), 1, "cmd-gap3-start-0003", "payments")
	if err != nil {
		t.Fatal(err)
	}
	var attemptID int64
	if err := h.db.QueryRow(`SELECT id FROM execution_attempts WHERE scope_type='resource_refresh_run' AND scope_id=?`, run.ID).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	assignRunningAttempt(t, h, attemptID)
	// The lease sweeper fences the running child first; the closure routes by scope.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.Exec(`UPDATE execution_attempts SET state='Interrupted', row_version=row_version+1, ended_at=?, termination_reason='lease_expired' WHERE id=?`, now, attemptID); err != nil {
		t.Fatal(err)
	}
	if err := h.systems.RecordResourceRefreshTechnicalGap(context.Background(), attemptID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := h.db.QueryRow(`SELECT state FROM resource_refresh_runs WHERE id=?`, run.ID).Scan(&state); err != nil || state != "Interrupted" {
		t.Fatalf("interrupted child must converge its parent: %q %v", state, err)
	}
	var logs int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM observed_refresh_log WHERE attempt_id=?`, attemptID).Scan(&logs); err != nil || logs != 1 {
		t.Fatalf("interrupted gap must be logged once: %d %v", logs, err)
	}
}

func TestSweptVerificationParentClosureSkipsClosedRun(t *testing.T) {
	h := newHarness(t)
	// A cancelled parent must absorb the late technical gap silently: the
	// closure row is skipped instead of tripping the running-only trigger.
	draft := h.mustUpload(t, validSystemYAML, 1, "cmd-gap4-upload-0001")
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
