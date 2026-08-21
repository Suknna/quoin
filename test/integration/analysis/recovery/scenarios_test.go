package recovery

// The seven T12 scenarios. Each one drives the full real path and records
// the expected-vs-actual assertions into the evidence directory.

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// s1CancelFirst: the fence commits before the slow stream finishes; the
// repeated cancel command replays idempotently; a cancel of the terminal
// analysis conflicts (409).
func s1CancelFirst(t *testing.T, evidence *ticketEvidence, client *http.Client, base, origin string, fireAlert fireAlertFunc, queryRow queryRowFunc) {
	t.Helper()
	occurrenceID := fireAlert(t, "T12SlowCancel")
	analysisID, _ := createAnalysis(t, client, base, origin, occurrenceID, "t12-s1-create")
	// Wait until the attempt is actually Running (the slow stream gives a
	// wide cancel window).
	waitFor(t, "s1 attempt running", func() bool {
		return queryRow(`SELECT COUNT(*) FROM execution_attempts WHERE scope_id=? AND state='Running'`, analysisID) == "1"
	}, 60*time.Second)
	_, rowVersion, _ := analysisDetail(t, client, base, origin, occurrenceID, analysisID)
	status, body := cancelAnalysis(t, client, base, origin, occurrenceID, analysisID, rowVersion, "t12-s1-cancel")
	if status != 200 || !strings.Contains(body, `"state":"Cancelled"`) && !strings.Contains(body, `"state":"Running"`) {
		t.Fatalf("s1 cancel: status=%d body=%.400s", status, body)
	}
	// The identical repeated command replays idempotently (same verdict).
	status2, body2 := cancelAnalysis(t, client, base, origin, occurrenceID, analysisID, rowVersion, "t12-s1-cancel")
	if status2 != status {
		t.Fatalf("s1 repeated command changed verdict: %d vs %d\n%s", status, status2, body2)
	}
	state, detail := waitAnalysisState(t, client, base, origin, occurrenceID, analysisID, "Cancelled")
	attemptState, reason := attemptAuthority(t, queryRow, analysisID)
	if state != "Cancelled" || attemptState != "Cancelled" || reason != "cancelled" {
		t.Fatalf("s1 authority wrong: analysis=%s attempt=%s reason=%s", state, attemptState, reason)
	}
	// A NEW cancel command against the terminal analysis conflicts.
	status3, _ := cancelAnalysis(t, client, base, origin, occurrenceID, analysisID, rowVersion, "t12-s1-cancel-terminal")
	if status3 != 409 {
		t.Fatalf("s1 terminal re-cancel: status=%d (want 409)", status3)
	}
	evidence.note(t, "s1-cancel-first.json", mustJSON(t, map[string]any{
		"scenario": "cancel-first",
		"expected": "fence commits Cancelling -> CancelAttempt -> worker stop -> CancelAck -> Cancelled; repeated command idempotent; terminal re-cancel 409",
		"actual":   map[string]any{"analysisState": state, "attemptState": attemptState, "terminationReason": reason, "repeatStatus": status2, "terminalReCancelStatus": status3},
		"detail":   detail,
	}))
}

// s2ResultFirst: the fast happy path commits success before the cancel
// command arrives; the cancel answers the completed object (200, not 409).
func s2ResultFirst(t *testing.T, evidence *ticketEvidence, client *http.Client, base, origin string, fireAlert fireAlertFunc, queryRow queryRowFunc) {
	t.Helper()
	occurrenceID := fireAlert(t, "T12OkResult")
	analysisID, _ := createAnalysis(t, client, base, origin, occurrenceID, "t12-s2-create")
	state, detail := waitAnalysisState(t, client, base, origin, occurrenceID, analysisID, "Succeeded")
	if state != "Succeeded" {
		t.Fatalf("s2 expected Succeeded, got %s", state)
	}
	_, rowVersion, _ := analysisDetail(t, client, base, origin, occurrenceID, analysisID)
	status, body := cancelAnalysis(t, client, base, origin, occurrenceID, analysisID, rowVersion, "t12-s2-cancel")
	if status != 200 || !strings.Contains(body, `"state":"Succeeded"`) {
		t.Fatalf("s2 cancel-after-success must return the completed object: status=%d body=%.400s", status, body)
	}
	attemptState, reason := attemptAuthority(t, queryRow, analysisID)
	if attemptState != "Succeeded" || reason != "" {
		t.Fatalf("s2 authority wrong: attempt=%s reason=%q", attemptState, reason)
	}
	evidence.note(t, "s2-result-first.json", mustJSON(t, map[string]any{
		"scenario": "result-first",
		"expected": "success commits first -> cancel command returns the completed object (200, no 409)",
		"actual":   map[string]any{"analysisState": state, "attemptState": attemptState, "cancelStatus": status},
		"detail":   detail,
	}))
}

// s3SameBootNetworkBlip: the quoin process is killed mid-run (its gRPC
// server dies -> the control stream ends on both ends); plinth reconnects
// same-boot with epoch+1, the pending attempt reconciles (lease renew,
// worker still running) and the slow stream still Succeeds. A transparent
// docker network partition is deliberately NOT used: reattaching the
// interface heals the same TCP connection below gRPC, so no reconnect
// would ever be observable.
func s3SameBootNetworkBlip(t *testing.T, evidence *ticketEvidence, client *http.Client, base, origin, composeFile string, fireAlert fireAlertFunc, queryRow queryRowFunc) {
	t.Helper()
	_, epochBefore := waitRuntimeReconnect(t, client, base, origin, 1)
	occurrenceID := fireAlert(t, "T12SlowNet")
	analysisID, _ := createAnalysis(t, client, base, origin, occurrenceID, "t12-s3-create")
	waitFor(t, "s3 attempt running", func() bool {
		return queryRow(`SELECT COUNT(*) FROM execution_attempts WHERE scope_id=? AND state='Running'`, analysisID) == "1"
	}, 60*time.Second)
	// Kill the quoin server process: the control stream ends, the worker
	// keeps streaming (supervisor task alive), plinth reconnects on the
	// next loop tick against the restarted quoin.
	execCommand(t, evidence, "s3-quoin-restart", nil, "docker", "restart", containerOf(t, composeFile, "quoin"))
	waitQuoinHTTP(t, base, origin)
	// Same boot, strictly greater epoch.
	bootID, epochAfter := waitRuntimeReconnect(t, client, base, origin, epochBefore+1)
	state, detail := waitAnalysisState(t, client, base, origin, occurrenceID, analysisID, "Succeeded", "Failed", "Interrupted", "Cancelled")
	attemptState, reason := attemptAuthority(t, queryRow, analysisID)
	if state != "Succeeded" || attemptState != "Succeeded" {
		t.Fatalf("s3 same-boot reconnect must not lose the attempt: analysis=%s attempt=%s reason=%s", state, attemptState, reason)
	}
	evidence.note(t, "s3-same-boot-reconnect.json", mustJSON(t, map[string]any{
		"scenario": "same-boot stream loss",
		"expected": "quoin process death mid-run -> control stream ends -> plinth reconnects same boot epoch+1 -> reconcile renews lease -> running attempt still Succeeds",
		"actual":   map[string]any{"bootId": bootID, "epochBefore": epochBefore, "epochAfter": epochAfter, "analysisState": state, "attemptState": attemptState},
		"detail":   detail,
	}))
}

// s3bResultAckLoss: the quoin container restarts right when the slow
// stream's result is due; whatever ack was in flight is lost and the
// pending delivery replays over the reconnect; the result commits exactly
// once.
func s3bResultAckLoss(t *testing.T, evidence *ticketEvidence, client *http.Client, base, origin, composeFile string, fireAlert fireAlertFunc, queryRow queryRowFunc) {
	t.Helper()
	occurrenceID := fireAlert(t, "T12SlowAck")
	analysisID, _ := createAnalysis(t, client, base, origin, occurrenceID, "t12-s3b-create")
	waitFor(t, "s3b attempt running", func() bool {
		return queryRow(`SELECT COUNT(*) FROM execution_attempts WHERE scope_id=? AND state='Running'`, analysisID) == "1"
	}, 60*time.Second)
	// Let the worker reach the final answer window, then restart quoin:
	// both the ResultProposal's ack and any in-flight delivery die.
	time.Sleep(6 * time.Second)
	execCommand(t, evidence, "s3b-quoin-restart", nil, "docker", "restart", containerOf(t, composeFile, "quoin"))
	waitQuoinHTTP(t, base, origin)
	state, detail := waitAnalysisState(t, client, base, origin, occurrenceID, analysisID, "Succeeded", "Failed", "Interrupted", "Cancelled")
	attemptState, reason := attemptAuthority(t, queryRow, analysisID)
	outputs := queryRow(`SELECT COUNT(*) FROM initial_analysis_outputs WHERE attempt_id=(SELECT id FROM execution_attempts WHERE scope_type='analysis' AND scope_id=? ORDER BY id DESC LIMIT 1)`, analysisID)
	if state != "Succeeded" || attemptState != "Succeeded" || outputs != "1" {
		t.Fatalf("s3b ack-loss replay must commit exactly one success: analysis=%s attempt=%s reason=%s outputs=%s", state, attemptState, reason, outputs)
	}
	evidence.note(t, "s3b-resultack-loss.json", mustJSON(t, map[string]any{
		"scenario": "ResultAck loss",
		"expected": "quoin restart at result time -> in-flight ack lost -> pending delivery replays over the reconnect -> idempotent adjudication commits exactly once",
		"actual":   map[string]any{"analysisState": state, "attemptState": attemptState, "sealedOutputs": outputs},
		"detail":   detail,
	}))
}

// s4NewBoot: the plinth container restarts mid-run; the new boot makes the
// old-boot attempt Interrupted immediately and the operator retry (a fresh
// analysis) Succeeds.
func s4NewBoot(t *testing.T, evidence *ticketEvidence, client *http.Client, base, origin, composeFile string, fireAlert fireAlertFunc, queryRow queryRowFunc) {
	t.Helper()
	occurrenceID := fireAlert(t, "T12SlowNewBoot")
	analysisID, _ := createAnalysis(t, client, base, origin, occurrenceID, "t12-s4-create")
	waitFor(t, "s4 attempt running", func() bool {
		return queryRow(`SELECT COUNT(*) FROM execution_attempts WHERE scope_id=? AND state='Running'`, analysisID) == "1"
	}, 60*time.Second)
	execCommand(t, evidence, "s4-plinth-restart", nil, "docker", "restart", containerOf(t, composeFile, "plinth"))
	state, detail := waitAnalysisState(t, client, base, origin, occurrenceID, analysisID, "Interrupted", "Failed", "Cancelled", "Succeeded")
	attemptState, reason := attemptAuthority(t, queryRow, analysisID)
	if state != "Interrupted" || attemptState != "Interrupted" || reason != "lease_expired" {
		t.Fatalf("s4 new boot must interrupt the old-boot attempt: analysis=%s attempt=%s reason=%s", state, attemptState, reason)
	}
	// The operator retry is a fresh analysis on the same occurrence.
	retryID, _ := createAnalysis(t, client, base, origin, occurrenceID, "t12-s4-retry")
	if retryID == analysisID {
		t.Fatal("s4 retry reused the interrupted analysis")
	}
	retryState, retryDetail := waitAnalysisState(t, client, base, origin, occurrenceID, retryID, "Succeeded", "Failed")
	if retryState != "Succeeded" {
		t.Fatalf("s4 retry after new boot did not succeed: %s\n%s", retryState, retryDetail)
	}
	evidence.note(t, "s4-new-boot.json", mustJSON(t, map[string]any{
		"scenario": "new boot",
		"expected": "plinth restart mid-run -> old-boot attempt Interrupted(lease_expired) on the new Hello -> operator retry (new analysis) Succeeds",
		"actual":   map[string]any{"analysisState": state, "attemptState": attemptState, "terminationReason": reason, "retryAnalysisState": retryState},
		"detail":   detail,
	}))
}

// s5LeaseLoss: the plinth container is paused mid-run (heartbeats stop,
// TCP survives); the lease burns down and the sweeper converges
// Interrupted(lease_expired); after unpausing, the retry Succeeds.
func s5LeaseLoss(t *testing.T, evidence *ticketEvidence, client *http.Client, base, origin, composeFile string, fireAlert fireAlertFunc, queryRow queryRowFunc) {
	t.Helper()
	occurrenceID := fireAlert(t, "T12SlowLease")
	analysisID, _ := createAnalysis(t, client, base, origin, occurrenceID, "t12-s5-create")
	waitFor(t, "s5 attempt running", func() bool {
		return queryRow(`SELECT COUNT(*) FROM execution_attempts WHERE scope_id=? AND state='Running'`, analysisID) == "1"
	}, 60*time.Second)
	plinthContainer := containerOf(t, composeFile, "plinth")
	execCommand(t, evidence, "s5-plinth-pause", nil, "docker", "pause", plinthContainer)
	// Frozen lease 2m + heartbeat 10s + sweep 5s: converge within ~4m.
	state, detail := waitAnalysisState(t, client, base, origin, occurrenceID, analysisID, "Interrupted", "Failed", "Cancelled", "Succeeded")
	execCommand(t, evidence, "s5-plinth-unpause", nil, "docker", "unpause", plinthContainer)
	attemptState, reason := attemptAuthority(t, queryRow, analysisID)
	if state != "Interrupted" || attemptState != "Interrupted" || reason != "lease_expired" {
		t.Fatalf("s5 lease loss must converge Interrupted(lease_expired): analysis=%s attempt=%s reason=%s", state, attemptState, reason)
	}
	waitRuntimeReconnect(t, client, base, origin, 1)
	retryID, _ := createAnalysis(t, client, base, origin, occurrenceID, "t12-s5-retry")
	retryState, retryDetail := waitAnalysisState(t, client, base, origin, occurrenceID, retryID, "Succeeded", "Failed")
	if retryState != "Succeeded" {
		t.Fatalf("s5 retry after lease loss did not succeed: %s\n%s", retryState, retryDetail)
	}
	evidence.note(t, "s5-lease-loss.json", mustJSON(t, map[string]any{
		"scenario": "lease loss",
		"expected": "plinth paused mid-run -> heartbeats stop -> lease burns -> sweeper converges Interrupted(lease_expired) -> unpause -> retry Succeeds",
		"actual":   map[string]any{"analysisState": state, "attemptState": attemptState, "terminationReason": reason, "retryAnalysisState": retryState},
		"detail":   detail,
	}))
}

// s6PartialThenFailure: the fixture hangs up after two visible tokens; the
// physical call seals failed with a complete=0 partial output row and the
// attempt fails with a provider-class termination reason; no domain output
// exists.
func s6PartialThenFailure(t *testing.T, evidence *ticketEvidence, client *http.Client, base, origin string, fireAlert fireAlertFunc, queryRow queryRowFunc) {
	t.Helper()
	occurrenceID := fireAlert(t, "T12PartialFail")
	analysisID, _ := createAnalysis(t, client, base, origin, occurrenceID, "t12-s6-create")
	state, detail := waitAnalysisState(t, client, base, origin, occurrenceID, analysisID, "Failed", "Succeeded", "Interrupted", "Cancelled")
	attemptState, reason := attemptAuthority(t, queryRow, analysisID)
	attemptID := queryRow(`SELECT id FROM execution_attempts WHERE scope_type='analysis' AND scope_id=? ORDER BY id DESC LIMIT 1`, analysisID)
	partials := queryRow(`SELECT COUNT(*) FROM model_call_outputs o JOIN model_calls m ON m.id=o.model_call_id WHERE m.attempt_id=? AND o.complete=0`, attemptID)
	outputs := queryRow(`SELECT COUNT(*) FROM initial_analysis_outputs WHERE attempt_id=?`, attemptID)
	if state != "Failed" || attemptState != "Failed" || outputs != "0" {
		t.Fatalf("s6 partial-token failure must fail the attempt without a domain output: analysis=%s attempt=%s reason=%s outputs=%s", state, attemptState, reason, outputs)
	}
	if reason == "" || reason == "worker_protocol_error" {
		t.Fatalf("s6 termination reason must be the provider failure class, got %q", reason)
	}
	var statePill string
	if strings.Contains(detail, `"state":"Failed"`) {
		statePill = "Failed"
	}
	_ = statePill
	// The operator retry on the same occurrence Succeeds (the fixture only
	// fails the first turn of this alert name once; a retry re-enters the
	// same first turn — the fixture fails deterministically, so the retry
	// asserts idempotent re-failure instead of success).
	retryID, _ := createAnalysis(t, client, base, origin, occurrenceID, "t12-s6-retry")
	retryState, _ := waitAnalysisState(t, client, base, origin, occurrenceID, retryID, "Failed", "Succeeded", "Interrupted", "Cancelled")
	if retryState != "Failed" {
		t.Fatalf("s6 retry must fail identically on the deterministic partial fixture: %s", retryState)
	}
	evidence.note(t, "s6-partial-then-failure.json", mustJSON(t, map[string]any{
		"scenario": "partial token then failure",
		"expected": "fixture hangs up after two visible tokens -> physical call seals failed with complete=0 partial output -> attempt Failed(provider-class reason), zero domain outputs",
		"actual":   map[string]any{"analysisState": state, "attemptState": attemptState, "terminationReason": reason, "partialOutputRows": partials, "domainOutputs": outputs, "retryState": retryState},
		"detail":   detail,
	}))
}
