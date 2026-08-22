package businesssystem

// Config Verification Run lifecycle tests (T17, DATA-CONFIG-007): the
// zero-check deterministic completion inside the create command, the Queued
// hold for check-bearing drafts until the executor tickets, the active-run
// fence, the cancel fence with its row-version precondition and the run
// history projection.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const zeroCheckSystemYAML = `system_key: checks-free
display_name: 无检查系统
enabled: false
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
resource_discoveries: []
inspection_plans: []
`

func TestRunVerificationZeroCheckDraftPassesInCommand(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, zeroCheckSystemYAML, 1, "cmd-t17-zero-0001")
	detail, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t17-run-0001", "checks-free", versionID(t, draft))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if detail.State != "Passed" || detail.RowVersion != 3 || detail.EvidenceAt == nil || detail.ResultDetail != nil {
		t.Fatalf("zero-check run must complete Passed inside the command: %#v", detail)
	}
	if len(detail.CheckResults) != 0 {
		t.Fatalf("zero-check run carries no check results: %#v", detail.CheckResults)
	}
	var states []string
	var taskRows int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM task_change_log WHERE object_type='config_verification_run' AND object_id=? AND change_type='state_changed'`, verificationRunID(t, detail)).Scan(&taskRows); err != nil {
		t.Fatal(err)
	}
	rows, err := h.db.Query(`SELECT change_type FROM task_change_log WHERE object_type='config_verification_run' AND object_id=? ORDER BY id`, verificationRunID(t, detail))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var changeType string
		if err := rows.Scan(&changeType); err != nil {
			t.Fatal(err)
		}
		states = append(states, changeType)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(states) != 3 || states[0] != "created" || states[1] != "state_changed" || states[2] != "state_changed" {
		t.Fatalf("task log must carry created + 2 transitions: %v", states)
	}
	if taskRows != 2 {
		t.Fatalf("state_changed rows wrong: %d", taskRows)
	}
	// Command replay returns the stored run.
	replayed, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t17-run-0001", "checks-free", versionID(t, draft))
	if err != nil || replayed.ID != detail.ID {
		t.Fatalf("replay must return the original run: %#v %v", replayed, err)
	}
	// A second distinct command creates a second Passed run (the fence only
	// applies while a run is active).
	again, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t17-run-0002", "checks-free", versionID(t, draft))
	if err != nil || again.State != "Passed" || again.ID == detail.ID {
		t.Fatalf("second run after Passed must be allowed: %#v %v", again, err)
	}
}

func TestRunVerificationCheckBearingDraftStaysQueued(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, validSystemYAML, 1, "cmd-t17-checks-0001")
	detail, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t17-run-0003", "payments", versionID(t, draft))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if detail.State != "Queued" || detail.RowVersion != 1 || detail.EvidenceAt != nil {
		// The PromQL/browser executors arrive with their own tickets; until
		// then the run stays accepted-but-not-started (honest Queued).
		t.Fatalf("check-bearing run must stay Queued: %#v", detail)
	}
	// The active fence rejects a second run over the same draft.
	_, err = h.systems.RunVerification(context.Background(), h.principal, "cmd-t17-run-0004", "payments", versionID(t, draft))
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Code != "active_conflict" {
		t.Fatalf("active run must conflict, got %#v %v", conflict, err)
	}
}

func TestCancelVerificationFence(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, validSystemYAML, 1, "cmd-t17-cancel-0001")
	detail, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t17-run-0005", "payments", versionID(t, draft))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	id := verificationRunID(t, detail)
	// Stale expectedRowVersion conflicts.
	_, err = h.systems.CancelVerification(context.Background(), h.principal, "cmd-t17-cancel-0002", "payments", versionID(t, draft), id, 99)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Code != "row_version_conflict" {
		t.Fatalf("stale cancel must conflict: %#v %v", conflict, err)
	}
	cancelled, err := h.systems.CancelVerification(context.Background(), h.principal, "cmd-t17-cancel-0003", "payments", versionID(t, draft), id, 1)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.State != "Cancelled" || cancelled.RowVersion != 2 || cancelled.ResultDetail == nil {
		t.Fatalf("cancel projection wrong: %#v", cancelled)
	}
	// Cancel replay is idempotent; a second distinct cancel command hits the
	// terminal fence.
	if _, err := h.systems.CancelVerification(context.Background(), h.principal, "cmd-t17-cancel-0003", "payments", versionID(t, draft), id, 1); err != nil {
		t.Fatalf("cancel replay must be idempotent: %v", err)
	}
	_, err = h.systems.CancelVerification(context.Background(), h.principal, "cmd-t17-cancel-0004", "payments", versionID(t, draft), id, 2)
	if !errors.As(err, &conflict) || conflict.Code != "row_version_conflict" {
		t.Fatalf("terminal cancel must conflict: %#v %v", conflict, err)
	}
	// After the terminal run a fresh run may be created.
	fresh, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t17-run-0006", "payments", versionID(t, draft))
	if err != nil || fresh.State != "Queued" {
		t.Fatalf("run after cancel must be allowed: %#v %v", fresh, err)
	}
}

func TestRunVerificationRejectsPublishedVersion(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, validSystemYAML, 1, "cmd-t17-pub-0001")
	if _, err := h.systems.Publish(context.Background(), h.principal, "cmd-t17-pub-0002", "payments", versionID(t, draft), nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	_, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t17-run-0007", "payments", versionID(t, draft))
	var conflict *ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Detail, "未发布草稿") {
		t.Fatalf("published version must refuse verification: %#v %v", conflict, err)
	}
}

func TestListVerificationsHistory(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, validSystemYAML, 1, "cmd-t17-hist-0001")
	first, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t17-hist-0002", "payments", versionID(t, draft))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.systems.CancelVerification(context.Background(), h.principal, "cmd-t17-hist-0003", "payments", versionID(t, draft), verificationRunID(t, first), 1); err != nil {
		t.Fatal(err)
	}
	second, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t17-hist-0004", "payments", versionID(t, draft))
	if err != nil {
		t.Fatal(err)
	}
	items, more, err := h.systems.ListVerifications(context.Background(), "payments", versionID(t, draft), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if more || len(items) != 2 || items[0].ID != second.ID || items[1].ID != first.ID {
		t.Fatalf("history order wrong: %#v more=%v", items, more)
	}
	oneMore, more2, err := h.systems.ListVerifications(context.Background(), "payments", versionID(t, draft), items[0].CreatedAt+"\x00"+items[0].ID, 10)
	if err != nil || more2 || len(oneMore) != 1 || oneMore[0].ID != first.ID {
		t.Fatalf("cursor page wrong: %#v %v", oneMore, err)
	}
}

func TestListVerificationsUsesCreatedAtAndIDKeysetOrder(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, validSystemYAML, 1, "cmd-t17-page-0001")
	version := versionID(t, draft)
	// Give later IDs deliberately older timestamps. The existing service clock
	// is the proper test seam; config verification run origin is immutable.
	clockValues := []time.Time{
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 1, 0, time.UTC),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 1, 0, time.UTC),
	}
	clockIndex := 0
	h.systems.now = func() time.Time {
		value := clockValues[clockIndex]
		clockIndex++
		return value
	}
	runs := make([]VerificationRunDetail, 3)
	for index := range runs {
		detail, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t17-page-000"+strconv.Itoa(index+2), "payments", version)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.systems.CancelVerification(context.Background(), h.principal, "cmd-t17-page-cancel-000"+strconv.Itoa(index+2), "payments", version, verificationRunID(t, detail), 1); err != nil {
			t.Fatal(err)
		}
		runs[index] = detail
	}

	var received []string
	cursor := ""
	for page := 0; page < 3; page++ {
		items, more, err := h.systems.ListVerifications(context.Background(), "payments", version, cursor, 1)
		if err != nil || len(items) != 1 {
			t.Fatalf("page %d: items=%#v more=%v err=%v", page, items, more, err)
		}
		received = append(received, items[0].ID)
		cursor = items[0].CreatedAt + "\x00" + items[0].ID
		if more != (page < 2) {
			t.Fatalf("page %d more=%v", page, more)
		}
	}
	// Runs 0 and 2 share a timestamp, so the secondary ID order must decide
	// their order; run 1 is older despite its middle ID.
	want := []string{runs[2].ID, runs[0].ID, runs[1].ID}
	if strings.Join(received, ",") != strings.Join(want, ",") {
		t.Fatalf("created-at/id keyset pages=%v want=%v", received, want)
	}
}

func versionID(t *testing.T, detail ConfigVersionDetail) int64 {
	t.Helper()
	id, err := strconv.ParseInt(detail.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func verificationRunID(t *testing.T, detail VerificationRunDetail) int64 {
	t.Helper()
	id, err := strconv.ParseInt(detail.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
