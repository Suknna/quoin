package inspection

// T26 domain contracts: a re-analysis is a new immutable report Attempt over
// the original evidence, while a recollection is a new Run bound to the exact
// prior Run configuration. Cancellation fences collection and analysis work
// without rewriting either historical Run or Evidence.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/attempt"
)

func completeRunWithFirstReport(t *testing.T, h *testHarness) (RunDetail, []int64, []int64) {
	t.Helper()
	h.publishMixedPlan(t)
	h.seedBrowserIdentity(t, false)
	h.seedModelProvider(t)
	store, err := artifact.NewStore(h.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.service.SetArtifactWriter(store.MaterializeEvidenceTransaction)
	ctx := context.Background()
	run, err := h.service.CreateInspectionRun(ctx, h.principal, "create-run-0001", "payments", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	promqlID := h.promqlAttemptID(t, run.RunID)
	h.dispatchPromQL(t, promqlID)
	if err := h.service.CommitPromQLProposal(ctx, promqlID, "plinth-boot", 1, promqlSuccessProposal(promqlID, run.RunID, "success", "instant")); err != nil {
		t.Fatal(err)
	}
	analysisID := h.analysisAttemptID(t, run.RunID)
	if err := h.attempts.BindToSlot(ctx, analysisID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, analysisID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	callID := h.seedSucceededModelCall(t, analysisID, promptDigest)
	evidenceIDs := evidenceIDsForRun(t, h, run.RunID)
	artifactIDs := artifactIDsForAttempt(t, h, analysisID)
	if err := h.service.CommitReportProposal(ctx, analysisID, "plinth-boot", 1, reportProposalBody(analysisID, run.RunID, callID, "first report", evidenceIDs, artifactIDs, promptDigest)); err != nil {
		t.Fatal(err)
	}
	return run, evidenceIDs, artifactIDs
}

func evidenceIDsForRun(t *testing.T, h *testHarness, runID int64) []int64 {
	t.Helper()
	rows, err := h.db.Query(`SELECT evidence_id FROM inspection_check_results WHERE run_id=? AND evidence_id IS NOT NULL ORDER BY check_key`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func artifactIDsForAttempt(t *testing.T, h *testHarness, attemptID int64) []int64 {
	t.Helper()
	rows, err := h.db.Query(`SELECT g.artifact_id FROM attempt_artifact_grants g
		JOIN attempt_input_snapshots s ON s.id=g.source_id
		JOIN attempt_input_items i ON i.snapshot_id=s.id AND i.artifact_id=g.artifact_id
		WHERE g.attempt_id=? AND g.source_kind='input_snapshot' ORDER BY i.item_seq`, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestReanalyzeCreatesNextImmutableReportVersionFromExistingEvidence(t *testing.T) {
	h := newTestHarness(t)
	run, evidenceIDs, artifactIDs := completeRunWithFirstReport(t, h)
	ctx := context.Background()

	next, err := h.service.ReanalyzeRun(ctx, h.principal, "reanalyze-run-0001", "payments", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if next.Type != "inspection_analysis" || next.State != "Queued" || next.ID == "" || next.RowVersion != 1 {
		t.Fatalf("reanalyze response = %+v", next)
	}
	var version int64
	if err := h.db.QueryRow(`SELECT inspection_report_version FROM attempt_input_snapshots WHERE attempt_id=?`, next.AttemptID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("reanalyze report version = %d, want 2", version)
	}
	if got := evidenceIDsForRun(t, h, run.RunID); len(got) != len(evidenceIDs) || got[0] != evidenceIDs[0] {
		t.Fatalf("reanalyze must not alter collected evidence: got %v want %v", got, evidenceIDs)
	}
	if got := artifactIDsForAttempt(t, h, next.AttemptID); len(got) != len(artifactIDs) || got[0] != artifactIDs[0] {
		t.Fatalf("reanalyze must grant the original artifact set: got %v want %v", got, artifactIDs)
	}

	replayed, err := h.service.ReanalyzeRun(ctx, h.principal, "reanalyze-run-0001", "payments", run.RunID)
	if err != nil || replayed.AttemptID != next.AttemptID {
		t.Fatalf("same command must replay its Attempt: %+v err=%v", replayed, err)
	}
	if _, err = h.service.ReanalyzeRun(ctx, h.principal, "reanalyze-run-0002", "payments", run.RunID); err == nil {
		t.Fatal("a second active re-analysis must be rejected")
	} else {
		var rejection *RejectionError
		if !errors.As(err, &rejection) || rejection.Code != "active_conflict" {
			t.Fatalf("second active re-analysis error = %v", err)
		}
	}
}

func TestRerunCreatesIndependentRunWithImmutableLineage(t *testing.T) {
	h := newTestHarness(t)
	h.publishMixedPlan(t)
	h.seedBrowserIdentity(t, false)
	ctx := context.Background()
	original, err := h.service.CreateInspectionRun(ctx, h.principal, "create-run-0001", "payments", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.service.CancelRun(ctx, h.principal, "cancel-run-0001", "payments", original.RunID, original.RowVersion); err != nil {
		t.Fatal(err)
	}
	// Recollection must retain its source snapshot even after a later publish
	// advances the system's current configuration pointer.
	var sourceVersion int64
	if err := h.db.QueryRow(`SELECT config_version_id FROM inspection_runs WHERE id=?`, original.RunID).Scan(&sourceVersion); err != nil {
		t.Fatal(err)
	}
	if current := h.publishMixedPlan(t); current == sourceVersion {
		t.Fatalf("second publish reused source config version %d", current)
	}
	if _, err := h.db.Exec(`
		INSERT INTO inspection_runs(business_system_id, plan_key, config_version_id, label_contract_version_id, trigger_kind, scheduled_for, state, rerun_of_id, created_at)
		SELECT business_system_id, plan_key, config_version_id, label_contract_version_id, 'schedule', ?, 'Queued', id, ?
		FROM inspection_runs WHERE id=?`, "2026-08-28T10:00:00Z", "2026-08-28T09:59:00Z", original.RunID); err == nil {
		t.Fatal("scheduled rerun bypassed the manual-only lineage closure")
	}

	rerun, err := h.service.RerunInspection(ctx, h.principal, "rerun-run-0001", "payments", original.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if rerun.RunID == original.RunID || rerun.State != "Running" || rerun.TriggerKind != "manual" {
		t.Fatalf("rerun detail = %+v", rerun)
	}
	var rerunVersion, rerunOf int64
	if err := h.db.QueryRow(`SELECT config_version_id FROM inspection_runs WHERE id=?`, original.RunID).Scan(&sourceVersion); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT config_version_id,rerun_of_id FROM inspection_runs WHERE id=?`, rerun.RunID).Scan(&rerunVersion, &rerunOf); err != nil {
		t.Fatal(err)
	}
	if rerunVersion != sourceVersion || rerunOf != original.RunID {
		t.Fatalf("rerun lineage = version %d rerun_of %d; want version %d rerun_of %d", rerunVersion, rerunOf, sourceVersion, original.RunID)
	}
	var checks int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM execution_attempts WHERE scope_type='run_check' AND scope_id=?`, rerun.RunID).Scan(&checks); err != nil {
		t.Fatal(err)
	}
	if checks != 2 {
		t.Fatalf("rerun checks = %d, want 2", checks)
	}
	replayed, err := h.service.RerunInspection(ctx, h.principal, "rerun-run-0001", "payments", original.RunID)
	if err != nil || replayed.RunID != rerun.RunID {
		t.Fatalf("same rerun command must replay its Run: %+v err=%v", replayed, err)
	}
}

func TestCancelRunFencesRunningAnalysisWithoutRewritingTheRun(t *testing.T) {
	h := newTestHarness(t)
	run, _, _ := completeRunWithFirstReport(t, h)
	ctx := context.Background()
	retry, err := h.service.ReanalyzeRun(ctx, h.principal, "reanalyze-run-0001", "payments", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.BindToSlot(ctx, retry.AttemptID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, retry.AttemptID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	current, err := h.service.GetRun(ctx, "payments", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := h.service.CancelRunWithDispatch(ctx, h.principal, "cancel-analysis-0001", "payments", run.RunID, current.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.DispatchAttemptIDs) != 1 || outcome.DispatchAttemptIDs[0] != retry.AttemptID {
		t.Fatalf("cancel dispatch IDs = %v, want [%d]", outcome.DispatchAttemptIDs, retry.AttemptID)
	}
	var attemptState, runState string
	if err := h.db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, retry.AttemptID).Scan(&attemptState); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT state FROM inspection_runs WHERE id=?`, run.RunID).Scan(&runState); err != nil {
		t.Fatal(err)
	}
	if attemptState != "Cancelling" || runState != "CompletedWithGaps" {
		t.Fatalf("analysis cancel must keep immutable collection result: attempt=%s run=%s", attemptState, runState)
	}
}

func TestRerunRejectsSkippedOverlapSourceBeforeSQLiteClosure(t *testing.T) {
	h := newTestHarness(t)
	h.publishMixedPlan(t)
	ctx := context.Background()
	result, err := h.db.Exec(`
		INSERT INTO inspection_runs(
			business_system_id,plan_key,config_version_id,label_contract_version_id,
			trigger_kind,scheduled_for,state,created_at
		)
		SELECT b.id,'mixed-plan',b.current_config_version_id,v.label_contract_version_id,
			'schedule','2026-01-01T00:00:00Z','SkippedOverlap','2026-01-01T00:00:00Z'
		FROM business_systems b
		JOIN business_system_config_versions v ON v.id=b.current_config_version_id
		WHERE b.key='payments'`)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RerunInspection(ctx, h.principal, "rerun-skipped-overlap-0001", "payments", sourceID); err == nil {
		t.Fatal("SkippedOverlap source must be rejected before the SQLite closure")
	} else {
		var rejection *RejectionError
		if !errors.As(err, &rejection) || rejection.Code != "state_conflict" {
			t.Fatalf("SkippedOverlap rerun error = %v, want state_conflict", err)
		}
	}
}

func TestCancelFirstRejectsLateReportProposal(t *testing.T) {
	h := newTestHarness(t)
	run, evidenceIDs, artifactIDs := completeRunWithFirstReport(t, h)
	ctx := context.Background()
	retry, err := h.service.ReanalyzeRun(ctx, h.principal, "reanalyze-late-result-0001", "payments", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.BindToSlot(ctx, retry.AttemptID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, retry.AttemptID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	callID := h.seedSucceededModelCall(t, retry.AttemptID, promptDigest)
	current, err := h.service.GetRun(ctx, "payments", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CancelRunWithDispatch(ctx, h.principal, "cancel-late-result-0001", "payments", run.RunID, current.RowVersion); err != nil {
		t.Fatal(err)
	}
	if err := h.service.CommitReportProposal(ctx, retry.AttemptID, "plinth-boot", 1, reportProposalBody(retry.AttemptID, run.RunID, callID, "late report", evidenceIDs, artifactIDs, promptDigest)); !errors.Is(err, attempt.ErrLateResult) {
		t.Fatalf("late proposal after cancellation = %v, want attempt.ErrLateResult", err)
	}
	var reports int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_reports WHERE run_id=?`, run.RunID).Scan(&reports); err != nil {
		t.Fatal(err)
	}
	if reports != 1 {
		t.Fatalf("late report created version %d after cancellation", reports)
	}
}

func TestRunDetailProjectsLatestFailedAnalysisForRecovery(t *testing.T) {
	h := newTestHarness(t)
	run, _, _ := completeRunWithFirstReport(t, h)
	ctx := context.Background()
	retry, err := h.service.ReanalyzeRun(ctx, h.principal, "reanalyze-failed-detail-0001", "payments", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.BindToSlot(ctx, retry.AttemptID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, retry.AttemptID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.CommitResult(ctx, retry.AttemptID, "plinth-boot", 1, false, "provider_unavailable"); err != nil {
		t.Fatal(err)
	}
	detail, err := h.service.GetRun(ctx, "payments", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.AnalysisActive || detail.LatestAnalysis == nil || detail.LatestAnalysis.ID != retry.ID || detail.LatestAnalysis.State != "Failed" || detail.LatestAnalysis.TerminationReason == nil || *detail.LatestAnalysis.TerminationReason != "provider_unavailable" {
		t.Fatalf("latest analysis projection=%+v, active=%v", detail.LatestAnalysis, detail.AnalysisActive)
	}
}

func TestCancelRunDispatchesAssignedAnalysisBecauseRuntimeMayAlreadyHaveIt(t *testing.T) {
	h := newTestHarness(t)
	run, _, _ := completeRunWithFirstReport(t, h)
	ctx := context.Background()
	retry, err := h.service.ReanalyzeRun(ctx, h.principal, "reanalyze-assigned-0001", "payments", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.BindToSlot(ctx, retry.AttemptID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	current, err := h.service.GetRun(ctx, "payments", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := h.service.CancelRunWithDispatch(ctx, h.principal, "cancel-assigned-0001", "payments", run.RunID, current.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.DispatchAttemptIDs) != 1 || outcome.DispatchAttemptIDs[0] != retry.AttemptID {
		t.Fatalf("assigned cancellation must dispatch CancelAttempt, got %v", outcome.DispatchAttemptIDs)
	}
	var state string
	if err := h.db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, retry.AttemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "Cancelling" {
		t.Fatalf("assigned attempt state=%s, want Cancelling", state)
	}
}

func TestCancelRunReportsCompletedWinnerWhenNoChildRemains(t *testing.T) {
	h := newTestHarness(t)
	run, _, _ := completeRunWithFirstReport(t, h)
	current, err := h.service.GetRun(context.Background(), "payments", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := h.service.CancelRunWithDispatch(context.Background(), h.principal, "cancel-winner-0001", "payments", run.RunID, current.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Detail.RunID != run.RunID || outcome.Detail.State != current.State || len(outcome.DispatchAttemptIDs) != 0 {
		t.Fatalf("result winner must be returned unchanged: %+v", outcome)
	}
}
