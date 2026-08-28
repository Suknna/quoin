package inspection

// run_check browser journey domain tests: admission freezing, dispatch
// fences, the shared journey result closure, and run convergence into the
// report analysis.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	_ "github.com/Suknna/quoin/internal/quoin/bootstrap"
	quoinconfig "github.com/Suknna/quoin/internal/quoin/config"
)

func journeyInspectionResult(attemptID, operationID int64) []byte {
	_, catalogVersion, catalogDigest, err := quoinconfig.JourneyCatalog()
	if err != nil {
		panic(err)
	}
	catalog := map[string]any{"digest": catalogDigest, "version": catalogVersion}
	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	body, _ := json.Marshal(map[string]any{
		"schemaKind": "browser_journey_result_v1", "attemptId": attemptID, "operationId": operationID,
		"outcome": "success",
		"probeResults": []map[string]any{
			{"phase": "admission", "result": "Authenticated", "journeyId": "authentication.url-prefix.v1", "journeyVersion": 1, "catalog": catalog, "reasonCode": nil, "observedAt": observedAt},
			{"phase": "completion", "result": "Authenticated", "journeyId": "authentication.url-prefix.v1", "journeyVersion": 1, "catalog": catalog, "reasonCode": nil, "observedAt": observedAt},
		},
		"evidence":        []map[string]any{{"kind": "structured", "primary": true, "observedAt": observedAt, "content": map[string]any{"statusText": "SYSTEM OK"}, "artifactId": nil}},
		"traceArtifactId": nil, "traceIntegrity": nil, "gapCode": nil, "originalGapCode": nil, "terminalReason": nil, "errorDetail": nil,
	})
	return body
}

func TestRunCheckJourneyResultClosure(t *testing.T) {
	h := newTestHarness(t)
	h.publishMixedPlan(t)
	h.seedBrowserIdentity(t, true)
	h.seedModelProvider(t)
	ctx := context.Background()
	detail, err := h.service.CreateInspectionRun(ctx, h.principal, "cmd-1", "payments", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	// The ready free identity left the browser child bare; admission creates
	// its operation and freezes the operation-bound input.
	admitted, err := h.service.AdmitNextJourneyChild(ctx)
	if err != nil || !admitted {
		t.Fatalf("admission should admit the browser child: admitted=%v err=%v", admitted, err)
	}
	var browserAttempt, operationID int64
	if err := h.db.QueryRow(`SELECT a.id,o.id FROM execution_attempts a JOIN browser_operations o ON o.owner_attempt_id=a.id WHERE a.scope_id=? AND a.check_key='status-page'`, detail.RunID).Scan(&browserAttempt, &operationID); err != nil {
		t.Fatal(err)
	}
	var kind string
	if err := h.db.QueryRow(`SELECT schema_kind FROM attempt_input_snapshots WHERE attempt_id=?`, browserAttempt).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "inspection_collection_v1" {
		t.Fatalf("admission should freeze inspection_collection_v1, got %s", kind)
	}
	// Walk the durable dispatch fences exactly like the runtime loop.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id='boot-j1',lintel_connection_epoch=1,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Running',started_at=?,row_version=row_version+1 WHERE id=? AND state='Starting'`, now, operationID); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.BindToSlot(ctx, browserAttempt, "lintel", "boot-j1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, browserAttempt, "boot-j1", 1); err != nil {
		t.Fatal(err)
	}
	// Settle the PromQL child too so the run can converge all-ok.
	promqlAttempt := h.promqlAttemptID(t, detail.RunID)
	h.dispatchPromQL(t, promqlAttempt)
	if err := h.service.CommitPromQLProposal(ctx, promqlAttempt, "plinth-boot", 1, promqlSuccessProposal(promqlAttempt, detail.RunID, "success", "instant")); err != nil {
		t.Fatal(err)
	}
	if err := h.service.CommitJourneyProposal(ctx, browserAttempt, "boot-j1", 1, journeyInspectionResult(browserAttempt, operationID)); err != nil {
		t.Fatal(err)
	}
	var status string
	var evidenceID int64
	if err := h.db.QueryRow(`SELECT status,evidence_id FROM inspection_check_results WHERE run_id=? AND check_key='status-page'`, detail.RunID).Scan(&status, &evidenceID); err != nil {
		t.Fatal(err)
	}
	if status != "ok" || evidenceID < 1 {
		t.Fatalf("journey result should derive ok + evidence, got %s/%d", status, evidenceID)
	}
	var browserState, operationState string
	if err := h.db.QueryRow(`SELECT a.state,o.state FROM execution_attempts a JOIN browser_operations o ON o.owner_attempt_id=a.id WHERE a.id=?`, browserAttempt).Scan(&browserState, &operationState); err != nil {
		t.Fatal(err)
	}
	if browserState != "Succeeded" || operationState != "Succeeded" {
		t.Fatalf("journey closure = attempt %s / operation %s", browserState, operationState)
	}
	final, err := h.service.GetRun(ctx, "payments", detail.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "Completed" {
		t.Fatalf("all-ok run should converge Completed, got %s", final.State)
	}
	// Completed collection immediately owns its analysis attempt.
	var analysis int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM execution_attempts WHERE attempt_type='inspection_analysis' AND scope_type='run' AND scope_id=?`, detail.RunID).Scan(&analysis); err != nil {
		t.Fatal(err)
	}
	if analysis != 1 {
		t.Fatalf("convergence should create one analysis attempt, got %d", analysis)
	}
}
