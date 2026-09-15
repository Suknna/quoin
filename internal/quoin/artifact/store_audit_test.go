package artifact

// Audit-contract tests (ADR-0006): the runtime upload ledger mutations run
// through the store's execution runner, so the default audit records them
// without any business audit call, deterministic rejections are recorded and
// commit nothing, and the download access fact refuses without trusted
// execution metadata (sensitive bytes can never leave uncorrelated).

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// auditEventRow is one persisted audit_events row as the tests read it.
type auditEventRow struct {
	ActorType     string
	ActorID       int64
	Action        string
	Outcome       string
	Phase         string
	CorrelationID string
	RequestID     string
	InitiatorType string
	DomainRefType string
	DomainRefID   int64
}

func fetchAuditEvents(t *testing.T, db *sql.DB, action string) []auditEventRow {
	t.Helper()
	rows, err := db.Query(`
		SELECT actor_type, actor_id, action, outcome, phase, COALESCE(correlation_id,''),
		       COALESCE(request_id,''), COALESCE(initiator_type,''), COALESCE(domain_ref_type,''), COALESCE(domain_ref_id,0)
		FROM audit_events WHERE action=? ORDER BY id`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []auditEventRow
	for rows.Next() {
		var event auditEventRow
		if err := rows.Scan(&event.ActorType, &event.ActorID, &event.Action, &event.Outcome, &event.Phase,
			&event.CorrelationID, &event.RequestID, &event.InitiatorType, &event.DomainRefType, &event.DomainRefID); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func auditTargetExists(t *testing.T, db *sql.DB, action, targetType string, targetID int64) bool {
	t.Helper()
	var count int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM audit_event_targets t JOIN audit_events e ON e.id=t.audit_event_id
		WHERE e.action=? AND t.target_type=? AND t.target_id=?`, action, targetType, targetID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count > 0
}

// TestUploadLedgerMutationsAreAuditedByDefault proves the Execute conversion:
// a begin and a commit each persist exactly one success event under the
// documented system task root, the commit pins the created artifact, a
// committed replay is itself audited, and nothing enters the client-command
// ledger (the runtime data plane never receives a human command id).
func TestUploadLedgerMutationsAreAuditedByDefault(t *testing.T) {
	db, store := newTestStore(t)
	attemptID, toolCallID := seedToolOwner(t, db)
	header := interruptedUploadHeader(attemptID, toolCallID, "audited body")
	file, replayID, err := store.BeginUpload(context.Background(), header)
	if err != nil || replayID != 0 {
		t.Fatalf("begin replay=%d err=%v", replayID, err)
	}
	if _, err := file.WriteString("audited body"); err != nil {
		t.Fatal(err)
	}
	artifactID, err := store.CommitUpload(context.Background(), header, file)
	if err != nil {
		t.Fatal(err)
	}

	begins := fetchAuditEvents(t, db, operationUploadBegin)
	if len(begins) != 1 {
		t.Fatalf("begin events=%d want 1", len(begins))
	}
	begin := begins[0]
	if begin.ActorType != "system" || begin.ActorID != 0 || begin.Outcome != audit.OutcomeSuccess || begin.Phase != audit.PhaseExecute {
		t.Fatalf("begin actor=%s/%d outcome=%s phase=%s", begin.ActorType, begin.ActorID, begin.Outcome, begin.Phase)
	}
	if begin.CorrelationID == "" {
		t.Fatal("fresh begin recorded without correlation")
	}
	commits := fetchAuditEvents(t, db, operationUploadCommit)
	if len(commits) != 1 {
		t.Fatalf("commit events=%d want 1", len(commits))
	}
	commit := commits[0]
	if commit.Outcome != audit.OutcomeSuccess || commit.DomainRefType != objectTypeArtifact || commit.DomainRefID != artifactID {
		t.Fatalf("commit outcome=%s ref=%s/%d want artifact/%d", commit.Outcome, commit.DomainRefType, commit.DomainRefID, artifactID)
	}
	if !auditTargetExists(t, db, operationUploadCommit, objectTypeArtifact, artifactID) {
		t.Fatal("commit audit carries no artifact target")
	}

	// A committed replay answers the stored id and is recorded as its own
	// begin fact, with the replayed artifact as the object.
	if _, replayID, err := store.BeginUpload(context.Background(), header); err != nil || replayID != artifactID {
		t.Fatalf("replay id=%d err=%v", replayID, err)
	}
	begins = fetchAuditEvents(t, db, operationUploadBegin)
	if len(begins) != 2 || begins[1].Outcome != audit.OutcomeSuccess || begins[1].DomainRefID != artifactID {
		t.Fatalf("replay begin events=%+v", begins)
	}
	var commands int
	if err := db.QueryRow(`SELECT COUNT(*) FROM client_commands`).Scan(&commands); err != nil {
		t.Fatal(err)
	}
	if commands != 0 {
		t.Fatalf("client_commands rows=%d, runtime uploads must not mint command ids", commands)
	}
}

// TestUploadRejectionIsRecordedWithoutArtifact proves the runner's rejection
// classification: a deterministic upload rejection persists exactly one
// rejected audit event, rolls the business stage back (no artifact row) and
// leaves the original ledger row untouched.
func TestUploadRejectionIsRecordedWithoutArtifact(t *testing.T) {
	db, store := newTestStore(t)
	ctx := context.Background()
	attemptID, toolCallID := seedToolOwner(t, db)
	header := interruptedUploadHeader(attemptID, toolCallID, "first-body")
	file, _, err := store.BeginUpload(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	_ = os.RemoveAll(store.StagingPath(header.UploadID))
	store.AbortUpload(header.UploadID)

	different := interruptedUploadHeader(attemptID, toolCallID, "second-body")
	_, _, err = store.BeginUpload(ctx, different)
	var rejection *Rejection
	if !errors.As(err, &rejection) || rejection.Reason != RejectMetadataMismatch {
		t.Fatalf("err=%v", err)
	}
	begins := fetchAuditEvents(t, db, operationUploadBegin)
	if len(begins) != 2 || begins[1].Outcome != audit.OutcomeRejected {
		t.Fatalf("begin events=%+v want a rejected retry event", begins)
	}
	if begins[1].ActorType != "system" || begins[1].CorrelationID == "" {
		t.Fatalf("rejected begin actor=%s/%d correlation=%q", begins[1].ActorType, begins[1].ActorID, begins[1].CorrelationID)
	}
	var artifacts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM artifacts`).Scan(&artifacts); err != nil || artifacts != 0 {
		t.Fatalf("artifacts=%d err=%v", artifacts, err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM runtime_artifact_uploads WHERE upload_id=?`, header.UploadID).Scan(&state); err != nil || state != "uploading" {
		t.Fatalf("ledger state=%q err=%v", state, err)
	}
}

// TestUploadRejectsUserContext proves the upload authorization fence: a
// caller-provided user scope can never drive the runtime data plane, and the
// refusal leaves no ledger row and no audit event (an authorization failure
// is not a business rejection).
func TestUploadRejectsUserContext(t *testing.T) {
	db, store := newTestStore(t)
	attemptID, toolCallID := seedToolOwner(t, db)
	header := interruptedUploadHeader(attemptID, toolCallID, "user context body")
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "user-scope",
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 7},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BeginUpload(ctx, header); err == nil {
		t.Fatal("user context unexpectedly began a runtime upload")
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runtime_artifact_uploads`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("ledger rows=%d err=%v", rows, err)
	}
	if events := fetchAuditEvents(t, db, operationUploadBegin); len(events) != 0 {
		t.Fatalf("authorization refusal recorded audit events: %+v", events)
	}
}

// TestRecordDownloadAuditRequiresTrustedCorrelation proves the sensitive
// download fence: the access fact is written through the shared audit writer
// with phase=access and the request's trusted metadata, and an uncorrelated
// context is refused without any row — so a caller that audits before bytes
// fails closed instead of releasing content without correlation.
func TestRecordDownloadAuditRequiresTrustedCorrelation(t *testing.T) {
	db, store := newTestStore(t)
	// No execution metadata: refused, nothing recorded.
	if err := store.RecordDownloadAudit(context.Background(), "user", 7, 42); !errors.Is(err, execution.ErrMissingContext) {
		t.Fatalf("uncorrelated audit err=%v", err)
	}
	if events := fetchAuditEvents(t, db, actionArtifactDownload); len(events) != 0 {
		t.Fatalf("refused audit persisted rows: %+v", events)
	}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "download-correlation",
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 7},
		Initiator:     execution.Principal{Kind: execution.PrincipalUser, ID: 7},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "request-9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDownloadAudit(ctx, "user", 7, 42); err != nil {
		t.Fatal(err)
	}
	events := fetchAuditEvents(t, db, actionArtifactDownload)
	if len(events) != 1 {
		t.Fatalf("download events=%d want 1", len(events))
	}
	event := events[0]
	if event.ActorType != "user" || event.ActorID != 7 || event.Outcome != audit.OutcomeSuccess || event.Phase != audit.PhaseAccess {
		t.Fatalf("download event=%+v", event)
	}
	if event.CorrelationID != "download-correlation" || event.RequestID != "request-9" || event.InitiatorType != "user" {
		t.Fatalf("download correlation=%q request=%q initiator=%s", event.CorrelationID, event.RequestID, event.InitiatorType)
	}
	if event.DomainRefType != objectTypeArtifact || event.DomainRefID != 42 {
		t.Fatalf("download ref=%s/%d", event.DomainRefType, event.DomainRefID)
	}
	if !auditTargetExists(t, db, actionArtifactDownload, objectTypeArtifact, 42) {
		t.Fatal("download audit carries no artifact target")
	}
	// An actor type outside the frozen vocabulary fails closed as well.
	if err := store.RecordDownloadAudit(ctx, "operator", 7, 42); err == nil {
		t.Fatal("invalid actor type unexpectedly recorded")
	}
}

// TestIdleGCCollectRecordsNoAudit proves the idle-pass rule: a GC pass with
// no overdue bodies and no orphan candidates is ErrNoTransition in both
// stages — the runner records no audit rows for scheduler idleness (idleness
// is not a business event) and no state changes.
func TestIdleGCCollectRecordsNoAudit(t *testing.T) {
	db, store := newTestStore(t)
	attemptID, toolCallID := seedToolOwner(t, db)
	// One live, unexpired artifact: no expiry candidates, no orphans.
	uploadText(t, store, context.Background(), attemptID, toolCallID, "live body")
	if err := store.RunGarbageCollection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if events := fetchAuditEvents(t, db, operationGCCollect); len(events) != 0 {
		t.Fatalf("idle GC recorded audit events: %+v", events)
	}
	var expired int
	if err := db.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE body_expired=1`).Scan(&expired); err != nil || expired != 0 {
		t.Fatalf("idle GC expired rows: count=%d err=%v", expired, err)
	}
}

// Physical collection records an intent before unlink and a separate result;
// the following idle pass must not add any of those lifecycle facts.
func TestGCCollectAuditsOnlyRealWork(t *testing.T) {
	db, store := newTestStore(t)
	store.now = func() time.Time { return time.Now().UTC().Add(-91 * 24 * time.Hour) }
	attemptID, toolCallID := seedToolOwner(t, db)
	_, shaHex := uploadText(t, store, context.Background(), attemptID, toolCallID, "expired body")
	store.now = func() time.Time { return time.Now().UTC() }
	if err := store.RunGarbageCollection(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := fetchAuditEvents(t, db, operationGCCollect)
	if len(events) != 1 {
		t.Fatalf("active pass events=%+v want one expiry mutation", events)
	}
	begins := fetchAuditEvents(t, db, operationGCIntent)
	completions := fetchAuditEvents(t, db, operationGCComplete)
	if len(begins) != 1 || len(completions) != 1 || begins[0].CorrelationID != completions[0].CorrelationID || completions[0].Outcome != audit.OutcomeSuccess {
		t.Fatalf("physical collection facts: begins=%+v completions=%+v", begins, completions)
	}
	for _, event := range events {
		if event.Outcome != audit.OutcomeSuccess {
			t.Fatalf("active pass event=%+v want success", event)
		}
	}
	var expired int
	if err := db.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE body_expired=1`).Scan(&expired); err != nil || expired != 1 {
		t.Fatalf("expired rows=%d err=%v", expired, err)
	}
	if _, err := os.Stat(filepath.Join(store.dir, "blobs", shaHex+".blob")); !os.IsNotExist(err) {
		t.Fatalf("collected blob stat err=%v, want not exist", err)
	}
	// The next pass is fully idle and records nothing further.
	if err := store.RunGarbageCollection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if events := fetchAuditEvents(t, db, operationGCCollect); len(events) != 1 {
		t.Fatalf("idle pass events=%+v want no new rows", events)
	}
	if len(fetchAuditEvents(t, db, operationGCIntent)) != 1 || len(fetchAuditEvents(t, db, operationGCComplete)) != 1 {
		t.Fatal("idle pass added physical collection facts")
	}
}

// TestAuditFailureRollsBackUploadBegin proves the runner's atomicity on the
// audit leg: when the audit INSERT cannot persist, the whole transaction —
// the business ledger row included — rolls back and the caller sees the
// failure. No forged success, no orphaned ledger row.
func TestAuditFailureRollsBackUploadBegin(t *testing.T) {
	db, store := newTestStore(t)
	attemptID, toolCallID := seedToolOwner(t, db)
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF; DROP TABLE audit_events`); err != nil {
		t.Fatal(err)
	}
	header := interruptedUploadHeader(attemptID, toolCallID, "audited body")
	file, replayID, err := store.BeginUpload(context.Background(), header)
	if err == nil {
		t.Fatal("audit failure must fail the upload begin")
	}
	if replayID != 0 || file != nil {
		t.Fatalf("failed begin replay=%d file=%v", replayID, file)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runtime_artifact_uploads WHERE upload_id=?`, header.UploadID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("ledger rows=%d err=%v — the audit failure must roll back the business stage", rows, err)
	}
}

// TestUploadRejectLifecycleIsAuditedWithAttemptCorrelation proves the
// failure-path closure: a commit failure marks the ledger row rejected
// through the audited artifact.upload.reject runner mutation, detached from
// the caller's context, and the reject event preserves the dispatch
// attempt's frozen operation correlation when the header row binds one.
func TestUploadRejectLifecycleIsAuditedWithAttemptCorrelation(t *testing.T) {
	db, store := newTestStore(t)
	attemptID, toolCallID := seedToolOwner(t, db)
	if _, err := db.Exec(`UPDATE execution_attempts SET operation_correlation_id='attempt-op-correlation', row_version=row_version+1 WHERE id=?`, attemptID); err != nil {
		t.Fatal(err)
	}
	header := interruptedUploadHeader(attemptID, toolCallID, "reject lifecycle body")
	file, _, err := store.BeginUpload(context.Background(), header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("short"); err != nil {
		t.Fatal(err)
	}
	// Size mismatch deterministically fails the commit and drives the
	// rejection lifecycle.
	if _, err := store.CommitUpload(context.Background(), header, file); err == nil {
		t.Fatal("size mismatch must fail the commit")
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM runtime_artifact_uploads WHERE upload_id=?`, header.UploadID).Scan(&state); err != nil || state != "rejected" {
		t.Fatalf("failed upload lifecycle state=%q err=%v, want rejected", state, err)
	}
	rejects := fetchAuditEvents(t, db, operationUploadReject)
	if len(rejects) != 1 || rejects[0].Outcome != audit.OutcomeSuccess {
		t.Fatalf("reject events=%+v want exactly one success", rejects)
	}
	if rejects[0].ActorType != "system" || rejects[0].CorrelationID != "attempt-op-correlation" {
		t.Fatalf("reject event=%+v want the attempt's frozen operation correlation", rejects[0])
	}
	if _, err := os.Stat(store.StagingPath(header.UploadID)); !os.IsNotExist(err) {
		t.Fatalf("staging file survived the rejection: err=%v", err)
	}
}

// TestUploadRejectTerminalRowRecordsNothing proves the missed-transition
// fence: a rejection against a row that already left the uploading state is
// ErrNoTransition — the runner records no audit and never rewrites history.
func TestUploadRejectTerminalRowRecordsNothing(t *testing.T) {
	db, store := newTestStore(t)
	attemptID, toolCallID := seedToolOwner(t, db)
	artifactID, _ := uploadText(t, store, context.Background(), attemptID, toolCallID, "committed body")
	// The upload committed: its row is terminal. A late failure-path reject
	// (e.g. an unknown-outcome retry losing a race) must be a no-op.
	store.rejectUpload(context.Background(), interruptedUploadHeader(attemptID, toolCallID, "committed body").UploadID, RejectInternal)
	var state string
	if err := db.QueryRow(`SELECT state FROM runtime_artifact_uploads WHERE artifact_id=?`, artifactID).Scan(&state); err != nil || state != "committed" {
		t.Fatalf("terminal row state=%q err=%v, want committed untouched", state, err)
	}
	if events := fetchAuditEvents(t, db, operationUploadReject); len(events) != 0 {
		t.Fatalf("terminal-row reject recorded audit events: %+v", events)
	}
}
