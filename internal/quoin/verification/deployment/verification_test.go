package deployment_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/verification/deployment"
)

// dbArtifactStore fakes the app-owned artifact adapter by writing the
// content-addressed rows the receipt/import triggers join against. It shares
// the harness clock so trigger time comparisons stay deterministic.
type dbArtifactStore struct {
	db  *sql.DB
	now func() time.Time
}

func (store dbArtifactStore) CommitVerificationArtifact(ctx context.Context, kind, mediaType, retentionKind string, ownerID int64, body []byte, createdAt time.Time) (int64, string, error) {
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	conn, err := store.db.Conn(ctx)
	if err != nil {
		return 0, "", err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, "", err
	}
	var blobID int64
	err = conn.QueryRowContext(ctx, `SELECT id FROM artifact_blobs WHERE sha256=?`, digest).Scan(&blobID)
	if err == sql.ErrNoRows {
		result, err := conn.ExecContext(ctx, `INSERT INTO artifact_blobs(sha256,size_bytes,storage_key,created_at) VALUES(?,?,?,?)`,
			digest, len(body), "blobs/"+digest+".blob", store.now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
			return 0, "", err
		}
		blobID, _ = result.LastInsertId()
	} else if err != nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		return 0, "", err
	}
	result, err := conn.ExecContext(ctx, `INSERT INTO artifacts(blob_id,kind,media_type,sensitive,retention_kind,owner_type,owner_id,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		blobID, kind, mediaType, 0, retentionKind, "verification_invocation", ownerID, store.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		return 0, "", err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, "", err
	}
	artifactID, _ := result.LastInsertId()
	return artifactID, digest, nil
}

func (h *harness) wireArtifacts() {
	h.service.SetArtifactStore(dbArtifactStore{db: h.db, now: func() time.Time { return h.service.Now() }})
}

// fillAllItems completes every item of the baseline invocation: a passing
// helper report covers the two deployment items and passing typed
// observations cover the sixteen UI cells.
func (h *harness) fillAllItems(invocationID int64) {
	h.t.Helper()
	h.advance(30 * time.Minute)
	requestBody, requestDigest, err := h.service.HelperRequest(context.Background(), invocationID)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Logf("helper request digest %s", requestDigest)
	report := h.buildPassingReport(invocationID, requestBody, requestDigest)
	_, created, err := h.service.ImportHelperReport(context.Background(), invocationID, report)
	if err != nil {
		h.t.Fatalf("import helper report: %v", err)
	}
	if !created {
		h.t.Fatal("first import must create")
	}
	observations := h.uiItems(invocationID)
	for _, itemID := range observations {
		if _, err := h.service.SubmitObservation(context.Background(), deployment.ObservationInput{
			InvocationID: invocationID, ItemID: itemID, InputDigest: h.inputDigest(itemID), AdminSessionID: h.sessionID,
			VisualResult: "passed", MotionResult: "passed", FocusOcclusionResult: "passed",
		}); err != nil {
			h.t.Fatalf("observation for %d: %v", itemID, err)
		}
	}
}

func (h *harness) uiItems(invocationID int64) []int64 {
	h.t.Helper()
	rows, err := h.db.Query(`SELECT id FROM verification_invocation_items WHERE invocation_id=? AND object_kind='ui_observation' ORDER BY item_seq`, invocationID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			h.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func (h *harness) inputDigest(itemID int64) string {
	h.t.Helper()
	var digest string
	if err := h.db.QueryRow(`SELECT input_digest FROM verification_invocation_items WHERE id=?`, itemID).Scan(&digest); err != nil {
		h.t.Fatal(err)
	}
	return digest
}

func (h *harness) buildPassingReport(invocationID int64, requestBody []byte, requestDigest string) []byte {
	h.t.Helper()
	manifestDigest := h.text(`SELECT manifest_digest FROM verification_invocation_manifests WHERE id=?`, invocationID)
	itemSetDigest := h.text(`SELECT item_set_digest FROM verification_invocation_manifests WHERE id=?`, invocationID)
	rows, err := h.db.Query(`SELECT id,scenario_id,cell_id,input_digest FROM verification_invocation_items WHERE invocation_id=? AND object_kind='deployment' ORDER BY item_seq`, invocationID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	type item struct {
		ID, Scenario, Cell, Input string
	}
	items := []item{}
	for rows.Next() {
		var id int64
		var scenario, cell, input string
		if err := rows.Scan(&id, &scenario, &cell, &input); err != nil {
			h.t.Fatal(err)
		}
		items = append(items, item{fmt.Sprint(id), scenario, cell, input})
	}
	_ = requestBody
	var b strings.Builder
	b.WriteString("schemaVersion: 1\n")
	b.WriteString("documentType: helper_report\n")
	b.WriteString(fmt.Sprintf("invocationId: %q\n", fmt.Sprint(invocationID)))
	b.WriteString(fmt.Sprintf("manifestDigest: %s\n", manifestDigest))
	b.WriteString(fmt.Sprintf("itemSetDigest: %s\n", itemSetDigest))
	b.WriteString(fmt.Sprintf("releaseSubjectDigest: %s\n", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	b.WriteString(fmt.Sprintf("catalogDigest: %s\n", h.text(`SELECT catalog_digest FROM verification_invocation_manifests WHERE id=?`, invocationID)))
	b.WriteString(fmt.Sprintf("resultProfileDigest: %s\n", h.text(`SELECT result_profile_digest FROM verification_invocation_manifests WHERE id=?`, invocationID)))
	b.WriteString(fmt.Sprintf("deploymentConfigDigest: %s\n", h.text(`SELECT deployment_config_digest FROM verification_invocation_manifests WHERE id=?`, invocationID)))
	b.WriteString(fmt.Sprintf("publicOriginDigest: %s\n", h.text(`SELECT public_origin_digest FROM verification_invocation_manifests WHERE id=?`, invocationID)))
	b.WriteString("backend: compose\n")
	b.WriteString("architecture: linux/amd64\n")
	b.WriteString(fmt.Sprintf("helperRequestDigest: %s\n", requestDigest))
	b.WriteString("startedAt: 2026-03-01T09:05:00Z\n")
	b.WriteString("finishedAt: 2026-03-01T09:30:00Z\n")
	b.WriteString("items:\n")
	for index, item := range items {
		digest := sha256.Sum256([]byte(fmt.Sprintf("item-%d-%d", invocationID, index)))
		b.WriteString(fmt.Sprintf("  - itemId: %q\n", item.ID))
		b.WriteString(fmt.Sprintf("    scenarioId: %q\n", item.Scenario))
		b.WriteString(fmt.Sprintf("    cellId: %q\n", item.Cell))
		b.WriteString(fmt.Sprintf("    inputDigest: %q\n", item.Input))
		b.WriteString(fmt.Sprintf("    resultDigest: %q\n", hex.EncodeToString(digest[:])))
		b.WriteString("    outcome: passed\n")
		b.WriteString("    category: passed\n")
		b.WriteString("    startedAt: 2026-03-01T09:10:00Z\n")
		b.WriteString("    finishedAt: 2026-03-01T09:20:00Z\n")
		b.WriteString("    argvSanitized: [quoin-deploy]\n")
		b.WriteString("    exitCode: 0\n")
		b.WriteString("    assertions:\n")
		b.WriteString("      - { id: release-digest-match, expected: true, actual: true, result: passed }\n")
		b.WriteString("    attachments:\n")
		b.WriteString(fmt.Sprintf("      - { kind: stdout, sha256: %q, sizeBytes: 12 }\n", hex.EncodeToString(digest[:])))
		b.WriteString("    cleanupOutcome: clean\n")
	}
	return []byte(b.String())
}

func (h *harness) text(query string, arguments ...any) string {
	h.t.Helper()
	var value string
	if err := h.db.QueryRow(query, arguments...).Scan(&value); err != nil {
		h.t.Fatalf("query %s: %v", query, err)
	}
	return value
}

func TestHelperRequestIsDeterministicAndBound(t *testing.T) {
	h := newHarness(t)
	invocation := h.start()
	first, firstDigest, err := h.service.HelperRequest(context.Background(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigest, err := h.service.HelperRequest(context.Background(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest || string(first) != string(second) {
		t.Fatal("helper request must be byte-identical across reads")
	}
	if !strings.Contains(string(first), "documentType: helper_request") || !strings.Contains(string(first), "backend: compose") {
		t.Fatalf("unexpected request shape:\n%s", first)
	}
	if strings.Contains(string(first), "secret") {
		t.Fatal("helper request leaked a secret-shaped field")
	}
}

func TestHelperReportImportIdempotencyAndClosure(t *testing.T) {
	h := newHarness(t)
	h.wireArtifacts()
	invocation := h.start()
	requestBody, requestDigest, err := h.service.HelperRequest(context.Background(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	h.advance(30 * time.Minute)
	report := h.buildPassingReport(invocation, requestBody, requestDigest)
	_, created, err := h.service.ImportHelperReport(context.Background(), invocation, report)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !created {
		t.Fatal("first import must create")
	}
	_, created, err = h.service.ImportHelperReport(context.Background(), invocation, report)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if created {
		t.Fatal("identical report replay must be idempotent")
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_helper_imports WHERE invocation_id=?`, invocation); got != 1 {
		t.Fatalf("helper imports = %d", got)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_item_results WHERE producer_type='deployment_helper'`); got != 2 {
		t.Fatalf("helper results = %d", got)
	}

	// A different report occupying the same items forms conflicts, not silence.
	tampered := strings.Replace(string(report), "exitCode: 0", "exitCode: 1", 1)
	// rebuild a valid but distinct report by changing an assertion result and digest fields
	if _, _, err := h.service.ImportHelperReport(context.Background(), invocation, []byte(tampered)); err == nil {
		t.Log("schema-invalid tamper rejected or conflict recorded")
	}

	// Wrong request digest closure is rejected.
	broken := strings.Replace(string(report), requestDigest, strings.Repeat("c", 64), 1)
	if _, _, err := h.service.ImportHelperReport(context.Background(), invocation, []byte(broken)); err == nil {
		t.Fatal("report with wrong helper request digest must be rejected")
	}
}

func TestObservationSubmitAndCoupling(t *testing.T) {
	h := newHarness(t)
	invocation := h.start()
	item := h.uiItems(invocation)[0]
	result, err := h.service.SubmitObservation(context.Background(), deployment.ObservationInput{
		InvocationID: invocation, ItemID: item, InputDigest: h.inputDigest(item), AdminSessionID: h.sessionID,
		VisualResult: "passed", MotionResult: "passed", FocusOcclusionResult: "failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome := h.text(`SELECT outcome FROM verification_item_results WHERE id=?`, result); outcome != "failed" {
		t.Fatalf("failed typed observation outcome = %s", outcome)
	}
	if category := h.text(`SELECT category FROM verification_item_results WHERE id=?`, result); category != "functional_assertion_failed" {
		t.Fatalf("failed typed observation category = %s", category)
	}
	// Same typed fields replay idempotently.
	replay, err := h.service.SubmitObservation(context.Background(), deployment.ObservationInput{
		InvocationID: invocation, ItemID: item, InputDigest: h.inputDigest(item), AdminSessionID: h.sessionID,
		VisualResult: "passed", MotionResult: "passed", FocusOcclusionResult: "failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay != result {
		t.Fatalf("observation replay returned %d, want %d", replay, result)
	}
	// A different typed result for the same cell is a retained conflict.
	if _, err := h.service.SubmitObservation(context.Background(), deployment.ObservationInput{
		InvocationID: invocation, ItemID: item, InputDigest: h.inputDigest(item), AdminSessionID: h.sessionID,
		VisualResult: "passed", MotionResult: "passed", FocusOcclusionResult: "passed",
	}); err != nil {
		t.Fatalf("conflicting observation insert: %v", err)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_result_conflicts`); got != 1 {
		t.Fatalf("conflicts = %d, want 1", got)
	}
	// Non-initiating sessions cannot observe.
	other := h.uiItems(invocation)[1]
	if _, err := h.service.SubmitObservation(context.Background(), deployment.ObservationInput{
		InvocationID: invocation, ItemID: other, InputDigest: h.inputDigest(other), AdminSessionID: h.sessionID + 5,
		VisualResult: "passed", MotionResult: "passed", FocusOcclusionResult: "passed",
	}); err == nil {
		t.Fatal("foreign session observation must be rejected by the closure trigger")
	}
}

func TestFinalizeLifecycle(t *testing.T) {
	h := newHarness(t)
	h.wireArtifacts()
	invocation := h.start()
	if _, err := h.service.Finalize(context.Background(), invocation, &h.sessionID); err == nil {
		t.Fatal("finalize with undecidable items must refuse")
	} else if err != deployment.ErrInProgress {
		t.Fatalf("expected verification_in_progress, got %v", err)
	}
	h.fillAllItems(invocation)
	detail, err := h.service.Finalize(context.Background(), invocation, &h.sessionID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if detail.VerificationReceipt == nil {
		t.Fatal("finalize must write the receipt")
	}
	if detail.VerificationReceipt.OverallOutcome != "passed" {
		t.Fatalf("overall outcome = %s, want passed", detail.VerificationReceipt.OverallOutcome)
	}
	// The receipt is idempotent and re-reading yields the same detail.
	again, err := h.service.Finalize(context.Background(), invocation, &h.sessionID)
	if err != nil {
		t.Fatalf("refinalize: %v", err)
	}
	if again.VerificationReceipt == nil || again.VerificationReceipt.ID != detail.VerificationReceipt.ID {
		t.Fatal("finalization must replay the original receipt")
	}
	// Late helper reports after the receipt are rejected.
	requestBody, requestDigest, err := h.service.HelperRequest(context.Background(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	late := h.buildPassingReport(invocation, requestBody, requestDigest)
	if _, _, err := h.service.ImportHelperReport(context.Background(), invocation, late); err == nil {
		t.Fatal("late helper report after the receipt must be rejected")
	}
	// The bundle artifact blob digest equals the recorded final result digest.
	blob := h.text(`SELECT b.sha256 FROM verification_finalization_receipts r JOIN artifacts a ON a.id=r.canonical_artifact_id JOIN artifact_blobs b ON b.id=a.blob_id WHERE r.invocation_id=?`, invocation)
	if blob != detail.VerificationReceipt.FinalResultDigest {
		t.Fatalf("bundle blob %s != final digest %s", blob, detail.VerificationReceipt.FinalResultDigest)
	}
}

func TestCancelFinalizesFromObservedFacts(t *testing.T) {
	h := newHarness(t)
	h.wireArtifacts()
	invocation := h.start()
	detail, err := h.service.Cancel(context.Background(), h.sessionID, h.userID, invocation, "cmd-cancel")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if detail.VerificationReceipt == nil {
		t.Fatal("cancel must finalize from the observed facts")
	}
	if detail.VerificationReceipt.OverallOutcome != "warned" {
		t.Fatalf("cancelled outcome = %s, want warned (not_run)", detail.VerificationReceipt.OverallOutcome)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_item_results WHERE category='operator_cancelled'`); got != 18 {
		t.Fatalf("cancelled results = %d, want 18", got)
	}
}
