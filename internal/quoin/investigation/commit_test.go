package investigation

// Result adjudication tests (DATA-INVEST-001/002, RUNTIME-TASK-008): the
// sealed assistant message and head move commit in one transaction, late
// results stay audit-only, withdrawn branches never re-enter the active
// head, and an identical replay rebuilds the original verdict.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCommitResultSealsAssistantMessage(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, principalID, "cmd-commit-1", "请回答", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	content := `"调查结论文本"`
	digest := sha256Sum([]byte(content))
	if err := service.CommitResult(ctx, Result{
		AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: true,
		SchemaKind: OutputSchemaKind, Canonical: []byte(content), Digest: digest[:],
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var assistantID, parentID int64
	var role, status, committed string
	var seq int64
	if err := db.QueryRow(`SELECT id,role,status,content,parent_message_id,seq FROM investigation_messages WHERE attempt_id=? AND role='assistant'`, created.AttemptID).
		Scan(&assistantID, &role, &status, &committed, &parentID, &seq); err != nil {
		t.Fatal(err)
	}
	if role != "assistant" || status != "active" || committed != "调查结论文本" || parentID != created.MessageID || seq != 2 {
		t.Fatalf("assistant row wrong: %s/%s/seq=%d parent=%d", role, status, seq, parentID)
	}
	var head int64
	if err := db.QueryRow(`SELECT current_head_message_id FROM investigations WHERE id=?`, created.InvestigationID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if head != assistantID {
		t.Fatalf("head=%d want assistant %d", head, assistantID)
	}
	var attemptState string
	if err := db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, created.AttemptID).Scan(&attemptState); err != nil {
		t.Fatal(err)
	}
	if attemptState != "Succeeded" {
		t.Fatalf("attempt state=%s want Succeeded", attemptState)
	}
	// The identical proposal replays the original verdict (RUNTIME-TASK-008).
	if err := service.CommitResult(ctx, Result{
		AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: true,
		SchemaKind: OutputSchemaKind, Canonical: []byte(content), Digest: digest[:],
	}); err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	// A divergent proposal stays a late result.
	if err := service.CommitResult(ctx, Result{
		AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: true,
		SchemaKind: OutputSchemaKind, Canonical: []byte(`"别的文本"`), Digest: sha256Sum([]byte(`"别的文本"`))[:],
	}); !errors.Is(err, ErrLateResult) {
		t.Fatalf("divergent replay err=%v want ErrLateResult", err)
	}
}

func TestCommitResultLatePaths(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, principalID, "cmd-late-1", "请回答", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	content := `"文本"`
	digest := sha256Sum([]byte(content))
	result := Result{AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: []byte(content), Digest: digest[:]}
	// A wrong boot/epoch binding never commits (audit-only).
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "other-boot", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: []byte(content), Digest: digest[:]}); !errors.Is(err, ErrLateResult) {
		t.Fatalf("wrong boot err=%v want ErrLateResult", err)
	}
	// A burned lease never commits (RUNTIME-TASK-008).
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE execution_attempts SET lease_until=?, row_version=row_version+1 WHERE id=?`, past, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitResult(ctx, result); !errors.Is(err, ErrLateResult) {
		t.Fatalf("burned lease err=%v want ErrLateResult", err)
	}
	// A withdrawn user message never re-enters the active branch
	// (DATA-INVEST-002).
	if _, err := db.Exec(`UPDATE execution_attempts SET lease_until=?, row_version=row_version+1 WHERE id=?`, time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano), created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE investigation_messages SET status='withdrawn' WHERE id=?`, created.MessageID); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitResult(ctx, result); !errors.Is(err, ErrLateResult) {
		t.Fatalf("withdrawn branch err=%v want ErrLateResult", err)
	}
	// The withdrawn message stays immutable (no resurrect path).
	if _, err := db.Exec(`UPDATE investigation_messages SET status='active' WHERE id=?`, created.MessageID); err == nil {
		t.Fatal("withdrawn -> active must be rejected by the frozen trigger")
	}
}

func TestCommitFailureAndReplay(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, principalID, "cmd-fail-1", "请回答", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	reason := "provider_unavailable"
	failure := Result{AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: false, Termination: reason}
	if err := service.CommitResult(ctx, failure); err != nil {
		t.Fatalf("failure commit: %v", err)
	}
	var attemptState, sealedReason string
	if err := db.QueryRow(`SELECT state, termination_reason FROM execution_attempts WHERE id=?`, created.AttemptID).Scan(&attemptState, &sealedReason); err != nil {
		t.Fatal(err)
	}
	if attemptState != "Failed" || sealedReason != reason {
		t.Fatalf("failed attempt: %s/%s", attemptState, sealedReason)
	}
	// No assistant message exists for a failed turn; the head stays on the
	// user message (retryable by a later ticket).
	var assistants int
	if err := db.QueryRow(`SELECT COUNT(*) FROM investigation_messages WHERE attempt_id=? AND role='assistant'`, created.AttemptID).Scan(&assistants); err != nil {
		t.Fatal(err)
	}
	if assistants != 0 {
		t.Fatalf("failed turn left %d assistant messages", assistants)
	}
	var head int64
	if err := db.QueryRow(`SELECT current_head_message_id FROM investigations WHERE id=?`, created.InvestigationID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if head != created.MessageID {
		t.Fatalf("head=%d want user message %d", head, created.MessageID)
	}
	// The identical failure replays its original verdict; a different
	// reason stays a late result.
	if err := service.CommitResult(ctx, failure); err != nil {
		t.Fatalf("identical failure replay: %v", err)
	}
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: false, Termination: "timeout"}); !errors.Is(err, ErrLateResult) {
		t.Fatalf("divergent failure err=%v want ErrLateResult", err)
	}
}

func TestCommitRejectsWrongSchemaKindAndDigest(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, principalID, "cmd-guard-1", "请回答", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	content := `"文本"`
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: true, SchemaKind: "initial_analysis_output_v1", Canonical: []byte(content), Digest: sha256Sum([]byte(content))[:]}); err == nil || !strings.Contains(err.Error(), "schema kind") {
		t.Fatalf("wrong schema err=%v", err)
	}
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: []byte(content), Digest: sha256Sum([]byte("other"))[:]}); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("wrong digest err=%v", err)
	}
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: []byte(`{"not":"a string"}`), Digest: sha256Sum([]byte(`{"not":"a string"}`))[:]}); err == nil || !strings.Contains(err.Error(), "JSON string") {
		t.Fatalf("wrong shape err=%v", err)
	}
}

var _ = sql.ErrNoRows
