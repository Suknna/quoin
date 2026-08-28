package businesssystem

// Config Verification Journey lifecycle tests (T23, DATA-CONFIG-007,
// DATA-BROWSER-003/006): the browser-check admission shapes, the frozen
// inspection_collection_v1 snapshot, the single browser_journey_results
// commit entry and its replay/conflict adjudication, and the no-retry
// convergence after a business gap.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/browser"
	quoinconfig "github.com/Suknna/quoin/internal/quoin/config"
)

const browserCheckSystemYAML = `system_key: payments
display_name: 支付系统
enabled: true
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
resource_discoveries: []
inspection_plans:
  - key: browser-plan
    display_name: 浏览器计划
    checks:
      - key: status-page
        display_name: 状态页
        analysis_question: 状态页是否正常?
        kind: browser
        journey_id: page.status-marker.v1
        journey_params:
          path: /status
`

// seedBrowserIdentity inserts one identity revision and (optionally) a
// published profile generation, mirroring the frozen projections the browser
// service owns.
func (h *harness) seedBrowserIdentity(t *testing.T, systemKey string, withProfile bool) (identityID, revisionID, profileID int64) {
	t.Helper()
	browsers := browser.NewService(h.db)
	_, catalogVersion, catalogDigest, err := quoinconfig.JourneyCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var existing struct {
		identity, revision, profile sql.NullInt64
	}
	if err := h.db.QueryRow(`SELECT i.id,i.current_revision_id,i.current_profile_generation_id FROM browser_identities i JOIN business_systems s ON s.id=i.business_system_id WHERE s.key=?`, systemKey).Scan(&existing.identity, &existing.revision, &existing.profile); err == nil && existing.identity.Valid && existing.profile.Valid == withProfile {
		return existing.identity.Int64, existing.revision.Int64, existing.profile.Int64
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.Exec(`INSERT OR IGNORE INTO sessions(id,user_id,session_token_digest,client_label,auth_revision_at_issue,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(91,1,?, 'test',1,?,?,?,'2030-01-01T00:00:00Z')`, make([]byte, 32), now, now, now); err != nil {
		t.Fatal(err)
	}
	identity, _, err := browsers.Configure(context.Background(), 1, browser.ConfigureInput{
		SystemKey: systemKey, Name: "状态身份", StartURL: "http://fixture.internal/login",
		Probe:           browser.ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: json.RawMessage(`{"authenticatedUrlPrefix":"http://fixture.internal/authenticated"}`)},
		ClientCommandID: "cmd-t23-identity-" + systemKey + fmt.Sprint(time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("configure identity: %v", err)
	}
	identityID, revisionID = identity.ID, identity.Revision.ID
	if !withProfile {
		return identityID, revisionID, 0
	}
	operation, err := browsers.StartManualLogin(context.Background(), systemKey, 1, 91, identity.RowVersion, "cmd-t23-login-"+fmt.Sprint(time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("start manual login: %v", err)
	}
	operationID := operation.ID
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id='boot-pub',lintel_connection_epoch=1,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Running',started_at=?,row_version=row_version+1 WHERE id=? AND state='Starting'`, now, operationID); err != nil {
		t.Fatal(err)
	}
	request, err := browsers.PreparePublish(context.Background(), systemKey, operationID, 1, operation.RowVersion+2, "cmd-t23-publish-"+fmt.Sprint(time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("prepare publish: %v", err)
	}
	digest := sha256.Sum256([]byte("profile-manifest-test"))
	if err := browsers.HandlePublishResult(context.Background(), browser.PublishResult{
		OperationID: operationID, CommandID: request.CommandID, Generation: request.NewGeneration,
		ChromiumRevision: "rev-test", ManifestDigest: digest[:], Accepted: true, BootID: "boot-pub", Epoch: 1,
		Probe: browser.ProbeResult{Phase: "publish", Result: "Authenticated", JourneyID: "authentication.url-prefix.v1", JourneyVersion: 1, CatalogDigest: catalogDigest, CatalogVersion: catalogVersion, ObservedAt: now},
	}); err != nil {
		t.Fatalf("publish profile: %v", err)
	}
	if err := h.db.QueryRow(`SELECT id FROM browser_profile_generations WHERE identity_id=? ORDER BY id DESC LIMIT 1`, identityID).Scan(&profileID); err != nil {
		t.Fatal(err)
	}
	// The real Stop fence releases the identity lock after publish; mirror its
	// committed confirmation so the identity is bookable again.
	if _, err := h.db.Exec(`UPDATE browser_operations SET stop_confirmed_at=?,stop_confirmation_basis='stop_ack',row_version=row_version+1 WHERE id=? AND stop_confirmed_at IS NULL`, now, operationID); err != nil {
		t.Fatal(err)
	}
	return identityID, revisionID, profileID
}

func journeyBrowserRun(t *testing.T, h *harness, commandID string, withProfile bool) (VerificationRunDetail, int64, int64) {
	t.Helper()
	draft := h.mustUpload(t, browserCheckSystemYAML, 1, "cmd-t23-upload-"+commandID)
	h.seedBrowserIdentity(t, "payments", withProfile)
	detail, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t23-run-"+commandID, "payments", versionID(t, draft))
	if err != nil {
		t.Fatalf("run browser draft: %v", err)
	}
	var attemptID, operationID int64
	var operationCount int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM browser_operations o JOIN execution_attempts a ON a.id=o.owner_attempt_id WHERE a.scope_id=?`, verificationRunID(t, detail)).Scan(&operationCount); err != nil {
		t.Fatal(err)
	}
	if operationCount > 0 {
		if err := h.db.QueryRow(`SELECT a.id,o.id FROM execution_attempts a JOIN browser_operations o ON o.owner_attempt_id=a.id WHERE a.scope_id=? AND a.check_key='status-page'`, verificationRunID(t, detail)).Scan(&attemptID, &operationID); err != nil {
			t.Fatal(err)
		}
	}
	return detail, attemptID, operationID
}

// startJourneyOperation walks the durable dispatch fences exactly like the
// runtime loop: Starting fence with boot/epoch, accepted Start, then the
// lintel-bound Running child.
func startJourneyOperation(t *testing.T, h *harness, attemptID, operationID int64) (boot string, epoch uint64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id='boot-j1',lintel_connection_epoch=1,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Running',started_at=?,row_version=row_version+1 WHERE id=? AND state='Starting'`, now, operationID); err != nil {
		t.Fatal(err)
	}
	attempts := h.systems.VerificationAttempts()
	if err := attempts.BindToSlot(context.Background(), attemptID, "lintel", "boot-j1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := attempts.Accept(context.Background(), attemptID, "boot-j1", 1); err != nil {
		t.Fatal(err)
	}
	return "boot-j1", 1
}

func journeyResultJSON(attemptID, operationID int64, mutate func(map[string]any)) []byte {
	_, catalogVersion, catalogDigest, err := quoinconfig.JourneyCatalog()
	if err != nil {
		panic(err)
	}
	catalog := map[string]any{"digest": catalogDigest, "version": catalogVersion}
	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	body := map[string]any{
		"schemaKind": "browser_journey_result_v1", "attemptId": attemptID, "operationId": operationID,
		"outcome": "success",
		"probeResults": []map[string]any{
			{"phase": "admission", "result": "Authenticated", "journeyId": "authentication.url-prefix.v1", "journeyVersion": 1, "catalog": catalog, "reasonCode": nil, "observedAt": observedAt},
			{"phase": "completion", "result": "Authenticated", "journeyId": "authentication.url-prefix.v1", "journeyVersion": 1, "catalog": catalog, "reasonCode": nil, "observedAt": observedAt},
		},
		"evidence":        []map[string]any{{"kind": "structured", "primary": true, "observedAt": time.Now().UTC().Format(time.RFC3339Nano), "content": map[string]any{"statusText": "SYSTEM OK"}, "artifactId": nil}},
		"traceArtifactId": nil, "traceIntegrity": nil, "gapCode": nil, "originalGapCode": nil, "terminalReason": nil, "errorDetail": nil,
	}
	if mutate != nil {
		mutate(body)
	}
	return mustMarshal(body)
}

func mustMarshal(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func TestRunVerificationBrowserCheckHappyJourneyPasses(t *testing.T) {
	h := newHarness(t)
	detail, attemptID, operationID := journeyBrowserRun(t, h, "0001", true)
	if detail.State != "Running" {
		t.Fatalf("browser run must stay Running while the journey executes: %#v", detail)
	}
	var frozenKind string
	if err := h.db.QueryRow(`SELECT schema_kind FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&frozenKind); err != nil || frozenKind != "inspection_collection_v1" {
		t.Fatalf("browser child must freeze inspection_collection_v1: %q %v", frozenKind, err)
	}
	var input struct {
		OperationID *int64 `json:"operationId"`
		Journey     struct {
			ID      string          `json:"id"`
			Params  json.RawMessage `json:"params"`
			Catalog struct {
				Digest string `json:"digest"`
			} `json:"catalog"`
		} `json:"journey"`
	}
	var snapshotDigest string
	if err := h.db.QueryRow(`SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&snapshotDigest); err != nil {
		t.Fatal(err)
	}
	// Rebuild through the dispatch authority: identical bytes or no dispatch.
	attempts := h.systems.VerificationAttempts()
	inputBytes, err := attempts.DispatchInputFor(context.Background(), attemptID)
	if err != nil {
		t.Fatalf("journey dispatch input rebuild: %v", err)
	}
	if err := json.Unmarshal(inputBytes.CanonicalJSON, &input); err != nil {
		t.Fatal(err)
	}
	if input.Journey.ID != "page.status-marker.v1" || string(input.Journey.Params) != `{"path":"/status"}` || input.OperationID == nil || *input.OperationID != operationID {
		t.Fatalf("frozen journey binding wrong: %s", inputBytes.CanonicalJSON)
	}
	sum := sha256.Sum256(inputBytes.CanonicalJSON)
	if fmt.Sprintf("%x", sum) != snapshotDigest {
		t.Fatalf("rebuilt input digest mismatch")
	}
	boot, epoch := startJourneyOperation(t, h, attemptID, operationID)
	if err := h.systems.CommitJourneyProposal(context.Background(), attemptID, boot, epoch, journeyResultJSON(attemptID, operationID, nil)); err != nil {
		t.Fatalf("commit journey success: %v", err)
	}
	var checkStatus string
	var evidenceID int64
	var ledgerOutcome string
	var attemptState, operationState string
	if err := h.db.QueryRow(`SELECT status,evidence_id FROM config_verification_run_check_results WHERE attempt_id=?`, attemptID).Scan(&checkStatus, &evidenceID); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT outcome FROM browser_journey_results WHERE attempt_id=?`, attemptID).Scan(&ledgerOutcome); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&attemptState); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT state FROM browser_operations WHERE id=?`, operationID).Scan(&operationState); err != nil {
		t.Fatal(err)
	}
	if checkStatus != "ok" || evidenceID < 1 || ledgerOutcome != "success" || attemptState != "Succeeded" || operationState != "Succeeded" {
		t.Fatalf("journey success must derive ok+Evidence+closed operation/attempt: status=%s evidence=%d ledger=%s attempt=%s op=%s", checkStatus, evidenceID, ledgerOutcome, attemptState, operationState)
	}
	var evidenceTarget string
	var params string
	if err := h.db.QueryRow(`SELECT target_type,params_json FROM evidence WHERE id=?`, evidenceID).Scan(&evidenceTarget, &params); err != nil {
		t.Fatal(err)
	}
	if evidenceTarget != "config_verification_run" || !json.Valid([]byte(params)) {
		t.Fatalf("journey evidence closure wrong: %s %s", evidenceTarget, params)
	}
	var probeCount int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM browser_probe_results WHERE operation_id=?`, operationID).Scan(&probeCount); err != nil {
		t.Fatal(err)
	}
	if probeCount != 2 {
		t.Fatalf("admission+completion probes must persist: %d", probeCount)
	}
	var runState string
	if err := h.db.QueryRow(`SELECT state FROM config_verification_runs WHERE id=?`, verificationRunID(t, detail)).Scan(&runState); err != nil {
		t.Fatal(err)
	}
	if runState != "Passed" {
		t.Fatalf("single browser check ok must Pass the run: %s", runState)
	}
}

func TestRunVerificationBrowserCheckJourneyGapClosesWithoutRetry(t *testing.T) {
	h := newHarness(t)
	detail, attemptID, operationID := journeyBrowserRun(t, h, "0002", true)
	boot, epoch := startJourneyOperation(t, h, attemptID, operationID)
	// The mandatory trace artifact lands through the runtime upload ledger;
	// seed the committed artifact + upload row the operation will reference.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	blob, err := h.db.Exec(`INSERT INTO artifact_blobs(sha256,size_bytes,storage_key,created_at) VALUES(?,7,?,?)`, fmt.Sprintf("%064x", time.Now().UnixNano()), fmt.Sprintf("gap-trace-%d", time.Now().UnixNano()), now)
	if err != nil {
		t.Fatal(err)
	}
	blobID, _ := blob.LastInsertId()
	art, err := h.db.Exec(`INSERT INTO artifacts(blob_id,kind,media_type,sensitive,retention_kind,owner_type,owner_id,expires_at,body_expired,created_at) VALUES(?,'trace','application/x-ndjson',1,'generated','browser_operation',?,?,0,?)`, blobID, operationID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	artifactID, _ := art.LastInsertId()
	trace := artifactID
	if err := h.systems.CommitJourneyProposal(context.Background(), attemptID, boot, epoch, journeyResultJSON(attemptID, operationID, func(body map[string]any) {
		body["outcome"] = "gap"
		body["evidence"] = []any{}
		body["gapCode"] = "journey_failed"
		body["terminalReason"] = "journey_failed"
		body["errorDetail"] = "step 2 (wait_for_selector) failed: selector [data-quoin-status] was not present before deadline"
		body["traceArtifactId"] = trace
		body["traceIntegrity"] = "complete"
	})); err != nil {
		t.Fatalf("commit journey gap: %v", err)
	}
	var checkStatus, gapReason string
	var evidenceID sql.NullInt64
	if err := h.db.QueryRow(`SELECT status,evidence_id,gap_reason FROM config_verification_run_check_results WHERE attempt_id=?`, attemptID).Scan(&checkStatus, &evidenceID, &gapReason); err != nil {
		t.Fatal(err)
	}
	if checkStatus != "gap" || gapReason != "journey_failed" || evidenceID.Valid {
		t.Fatalf("journey gap must carry gap code without Evidence: %s %s %v", checkStatus, gapReason, evidenceID.Valid)
	}
	var operationState, operationReason string
	var operationTrace sql.NullInt64
	if err := h.db.QueryRow(`SELECT state,terminal_reason,trace_artifact_id FROM browser_operations WHERE id=?`, operationID).Scan(&operationState, &operationReason, &operationTrace); err != nil {
		t.Fatal(err)
	}
	if operationState != "Failed" || operationReason != "journey_failed" || !operationTrace.Valid || operationTrace.Int64 != trace {
		t.Fatalf("failed journey must retain its mandatory trace: %s %s %v", operationState, operationReason, operationTrace.Valid)
	}
	var runState string
	if err := h.db.QueryRow(`SELECT state FROM config_verification_runs WHERE id=?`, verificationRunID(t, detail)).Scan(&runState); err != nil {
		t.Fatal(err)
	}
	if runState != "Failed" {
		t.Fatalf("journey gap must fail the run: %s", runState)
	}
	// No whole-run retry: a later dispatch scan finds nothing to re-execute.
	ids, err := h.systems.QueuedVerificationAttempts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("promql queue must stay empty: %v", ids)
	}
	var operations, children int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM browser_operations WHERE kind='journey'`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM execution_attempts WHERE scope_type='config_verification_run'`).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if operations != 1 || children != 1 {
		t.Fatalf("a failed journey must not auto-retry: operations=%d children=%d", operations, children)
	}
}

func TestRunVerificationBrowserCheckIdentityBusySettlesLocally(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, browserCheckSystemYAML, 1, "cmd-t23-upload-busy")
	identityID, revisionID, profileID := h.seedBrowserIdentity(t, "payments", true)
	// Occupy the identity with a never-stopped internal probe operation.
	if _, err := h.db.Exec(`INSERT INTO browser_operations(identity_id,identity_revision_id,profile_generation_id,owner_attempt_id,kind,state,journey_catalog_digest,journey_catalog_version,journey_id,journey_version,probe_phase,requested_at) VALUES(?, ?, ?, NULL, 'authentication_probe','Queued','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','v1','authentication.url-prefix.v1',1,'revision_change',?)`, identityID, revisionID, profileID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	detail, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t23-run-busy", "payments", versionID(t, draft))
	if err != nil {
		t.Fatalf("busy identity run: %v", err)
	}
	var status, gap string
	var digest []byte
	var attemptState string
	if err := h.db.QueryRow(`SELECT status,gap_reason,result_digest FROM config_verification_run_check_results WHERE verification_run_id=? AND check_key='status-page'`, verificationRunID(t, detail)).Scan(&status, &gap, &digest); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT a.state FROM execution_attempts a JOIN config_verification_run_check_results r ON r.attempt_id=a.id WHERE r.verification_run_id=?`, verificationRunID(t, detail)).Scan(&attemptState); err != nil {
		t.Fatal(err)
	}
	if status != "gap" || gap != "identity_busy" || len(digest) != 32 || attemptState != "Succeeded" {
		t.Fatalf("busy identity must settle identity_busy with a digest-closed child: %s %s %d %s", status, gap, len(digest), attemptState)
	}
	var operations int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM browser_operations WHERE kind='journey'`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if operations != 0 {
		t.Fatalf("identity_busy must not create a journey operation: %d", operations)
	}
	if detail.State != "Failed" {
		t.Fatalf("identity_busy gap must fail the run: %s", detail.State)
	}
}

func TestRunVerificationBrowserCheckWithoutProfileSettlesUnauthenticated(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, browserCheckSystemYAML, 1, "cmd-t23-upload-noprofile")
	h.seedBrowserIdentity(t, "payments", false)
	detail, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t23-run-noprofile", "payments", versionID(t, draft))
	if err != nil {
		t.Fatalf("profile-less run: %v", err)
	}
	var status, gap, attemptState string
	if err := h.db.QueryRow(`SELECT status,gap_reason FROM config_verification_run_check_results WHERE verification_run_id=? AND check_key='status-page'`, verificationRunID(t, detail)).Scan(&status, &gap); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT a.state FROM execution_attempts a JOIN config_verification_run_check_results r ON r.attempt_id=a.id WHERE r.verification_run_id=?`, verificationRunID(t, detail)).Scan(&attemptState); err != nil {
		t.Fatal(err)
	}
	if status != "gap" || gap != "authentication_required" || attemptState != "Failed" || detail.State != "Failed" {
		t.Fatalf("no profile must close the final check and Run as authentication_required: %s %s %s run=%s", status, gap, attemptState, detail.State)
	}
}

func TestCommitJourneyProposalReplayAndConflicts(t *testing.T) {
	h := newHarness(t)
	_, attemptID, operationID := journeyBrowserRun(t, h, "0003", true)
	boot, epoch := startJourneyOperation(t, h, attemptID, operationID)
	success := journeyResultJSON(attemptID, operationID, nil)
	if err := h.systems.CommitJourneyProposal(context.Background(), attemptID, boot, epoch, success); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// The runtime's Stop fence releases the identity for the second run.
	if _, err := h.db.Exec(`UPDATE browser_operations SET stop_confirmed_at=?,stop_confirmation_basis='stop_ack',row_version=row_version+1 WHERE id=? AND stop_confirmed_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), operationID); err != nil {
		t.Fatal(err)
	}
	// Same digest replays.
	if err := h.systems.CommitJourneyProposal(context.Background(), attemptID, boot, epoch, success); err != nil {
		t.Fatalf("replay must be idempotent: %v", err)
	}
	// A second, different journey on the same operation loses to the ledger
	// unique key; a mutated payload on a fresh child is rejected by the
	// catalog output schema revalidation.
	_, attempt2, operation2 := journeyBrowserRun(t, h, "0004", true)
	boot2, epoch2 := startJourneyOperation(t, h, attempt2, operation2)
	if err := h.systems.CommitJourneyProposal(context.Background(), attempt2, boot2, epoch2, journeyResultJSON(attempt2, operation2, func(body map[string]any) {
		body["evidence"] = []map[string]any{{"kind": "structured", "primary": true, "observedAt": time.Now().UTC().Format(time.RFC3339Nano), "content": map[string]any{"unexpected": true}, "artifactId": nil}}
	})); err == nil {
		t.Fatalf("typed output violating the catalog output schema must be rejected")
	}
	if err := h.systems.CommitJourneyProposal(context.Background(), attempt2, boot2, epoch2, journeyResultJSON(attempt2, operation2, func(body map[string]any) {
		body["probeResults"] = []any{}
	})); err == nil {
		t.Fatalf("success without authenticated probes must be rejected")
	}
	h2 := newHarness(t)
	_, attempt3, operation3 := journeyBrowserRun(t, h2, "0005", true)
	boot3, epoch3 := startJourneyOperation(t, h2, attempt3, operation3)
	if err := h2.systems.CommitJourneyProposal(context.Background(), attempt3, boot3, epoch3, journeyResultJSON(attempt3, operation3, func(body map[string]any) {
		probes := body["probeResults"].([]map[string]any)
		probes[1]["journeyVersion"] = 2
	})); err == nil {
		t.Fatalf("a result whose probe differs from the frozen identity revision must be rejected")
	}
}

func TestCommitJourneyProposalRejectsStaleBoot(t *testing.T) {
	h := newHarness(t)
	_, attemptID, operationID := journeyBrowserRun(t, h, "0005", true)
	startJourneyOperation(t, h, attemptID, operationID)
	if err := h.systems.CommitJourneyProposal(context.Background(), attemptID, "boot-other", 1, journeyResultJSON(attemptID, operationID, nil)); err == nil {
		t.Fatalf("a result from another boot must be rejected")
	}
	if err := h.systems.CommitJourneyProposal(context.Background(), attemptID, "boot-j1", 9, journeyResultJSON(attemptID, operationID, nil)); err != nil {
		t.Fatalf("same-boot successor epoch is a legal delivery: %v", err)
	}
}
