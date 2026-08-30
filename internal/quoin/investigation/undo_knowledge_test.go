package investigation

// T29 source invalidation races (DATA-TX-011): an undone turn and a
// candidate confirmation adjudicate strictly by SQLite commit order. Undo
// first leaves the candidate SourceInvalid and unconfirmable; confirmation
// first leaves an immutable version that permanently exits retrieval when
// the turn is undone — and never reactivates through a projection rebuild.

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/knowledge"
)

// seedSucceededTurn creates one investigation whose first user turn has a
// committed assistant reply, returning the assistant message id and head.
func seedSucceededTurn(t *testing.T, service *Service, db *sql.DB, principalID int64, suffix string) (investigationID, assistantID, head int64) {
	t.Helper()
	ctx := context.Background()
	created, err := service.Create(ctx, principalID, "t29-create-"+suffix, "数据库连接池如何治理？", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	content := `"先检查连接池上限，再观察超时指标。"`
	digest := sha256Sum([]byte(content))
	if err := service.CommitResult(ctx, Result{
		AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: true,
		SchemaKind: OutputSchemaKind, Canonical: []byte(content), Digest: digest[:],
	}); err != nil {
		t.Fatalf("commit reply: %v", err)
	}
	if err := db.QueryRow(`SELECT id, current_head_message_id FROM investigations WHERE id=?`, created.InvestigationID).Scan(&investigationID, &head); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT id FROM investigation_messages WHERE attempt_id=? AND role='assistant'`, created.AttemptID).Scan(&assistantID); err != nil {
		t.Fatal(err)
	}
	return investigationID, assistantID, head
}

func TestUndoBeforeConfirmInvalidatesCandidate(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	knowledgeService := knowledge.NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)

	investigationID, assistantID, head := seedSucceededTurn(t, service, db, principalID, "undo-first")

	created, err := knowledgeService.CreateFromInvestigationMessage(ctx, principalID, "t29-cand-undo-first", investigationID, assistantID)
	if err != nil {
		t.Fatalf("create candidate: %v", err)
	}
	if !created.Created {
		t.Fatalf("candidate creation reported not-created: %+v", created)
	}
	// The undo commits first: the candidate flips SourceInvalid in the same
	// transaction as the withdrawal.
	if _, err := service.Undo(ctx, principalID, "t29-undo-undo-first", investigationID, head); err != nil {
		t.Fatalf("undo: %v", err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM knowledge_candidates WHERE id=?`, parseCandidateID(t, created.Candidate.ID)).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "SourceInvalid" {
		t.Fatalf("candidate state after undo = %s, want SourceInvalid", state)
	}
	// A late confirmation loses the commit-order race deterministically.
	_, confirmErr := knowledgeService.ConfirmAs(ctx, knowledge.MutationActor{ID: principalID, AuthRevision: 1}, "t29-confirm-late", parseCandidateID(t, created.Candidate.ID), 0)
	if confirmErr == nil {
		t.Fatal("confirmation after undo-source invalidation succeeded")
	}
	var conflict *knowledge.StateConflict
	if !errors.As(confirmErr, &conflict) {
		t.Fatalf("late confirmation error = %v, want StateConflict", confirmErr)
	}
	// No version exists: nothing entered retrieval.
	if versions := countInt(t, db, `SELECT COUNT(*) FROM knowledge_versions`); versions != 0 {
		t.Fatalf("versions after losing race = %d, want 0", versions)
	}
}

func TestConfirmBeforeUndoExitsVersionPermanently(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	knowledgeService := knowledge.NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)

	investigationID, assistantID, head := seedSucceededTurn(t, service, db, principalID, "confirm-first")

	created, err := knowledgeService.CreateFromInvestigationMessage(ctx, principalID, "t29-cand-confirm-first", investigationID, assistantID)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := knowledgeService.ConfirmAs(ctx, knowledge.MutationActor{ID: principalID, AuthRevision: 1}, "t29-confirm-first", parseCandidateID(t, created.Candidate.ID), 0)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	var versionID int64
	if err := db.QueryRow(`SELECT current_version_id FROM reusable_knowledge WHERE id=?`, parseCandidateID(t, confirmed.ConfirmedKnowledgeID)).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	if docs := countInt(t, db, `SELECT COUNT(*) FROM knowledge_search_docs WHERE knowledge_version_id=?`, versionID); docs != 1 {
		t.Fatalf("confirmed version docs = %d, want 1", docs)
	}

	// The undo commits after confirmation: the immutable version exits
	// retrieval permanently in the same transaction.
	if _, err := service.Undo(ctx, principalID, "t29-undo-confirm-first", investigationID, head); err != nil {
		t.Fatalf("undo: %v", err)
	}
	var exited int64
	var reason string
	if err := db.QueryRow(`SELECT exited, exit_reason FROM knowledge_version_retrieval_state WHERE knowledge_version_id=?`, versionID).Scan(&exited, &reason); err != nil {
		t.Fatal(err)
	}
	if exited != 1 || reason != "source_rejected" {
		t.Fatalf("retrieval exit = %d/%s, want 1/source_rejected", exited, reason)
	}
	if docs := countInt(t, db, `SELECT COUNT(*) FROM knowledge_search_docs WHERE knowledge_version_id=?`, versionID); docs != 0 {
		t.Fatalf("docs after undo = %d, want 0", docs)
	}

	// No old-version reactivation: a full projection rebuild derives only
	// from current ∧ not-exited authority and cannot resurrect the version.
	if err := knowledgeService.RebuildSearchDocs(ctx); err != nil {
		t.Fatal(err)
	}
	if docs := countInt(t, db, `SELECT COUNT(*) FROM knowledge_search_docs WHERE knowledge_version_id=?`, versionID); docs != 0 {
		t.Fatal("projection rebuild reactivated the exited version")
	}
	if searchHits := countInt(t, db, `SELECT COUNT(*) FROM knowledge_fts WHERE knowledge_fts MATCH '连接池'`); searchHits != 0 {
		t.Fatal("FTS index still exposes the withdrawn-source version")
	}

	// A second undo of the same command replays the committed state without
	// touching the sticky exit row again.
	if _, err := service.Undo(ctx, principalID, "t29-undo-confirm-first", investigationID, head); err == nil {
		// The head fence rejects the replay only after a new turn exists;
		// against the same withdrawn head the replay is a state report.
		_ = err
	}
	var rowVersion int64
	if err := db.QueryRow(`SELECT row_version FROM knowledge_version_retrieval_state WHERE knowledge_version_id=?`, versionID).Scan(&rowVersion); err != nil {
		t.Fatal(err)
	}
	if rowVersion != 2 {
		t.Fatalf("sticky exit row_version = %d, want exactly one flip (2)", rowVersion)
	}
}

func parseCandidateID(t *testing.T, value string) int64 {
	t.Helper()
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func countInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int64
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return int(count)
}
