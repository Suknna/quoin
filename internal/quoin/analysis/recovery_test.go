package analysis

// T12 recovery-closure tests: interruption converges the analysis in the
// same transaction, the Cancelling fence exception ends as Cancelled, and
// interruption frees the occurrence for a fresh analysis (the operator
// retry path, DATA-ANALYSIS-001/002).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// runAccepted creates one analysis and drives its attempt to Running on a
// live binding.
func runAccepted(t *testing.T, service *Service, db *sql.DB) (analysisID, attemptID int64) {
	t.Helper()
	ctx := context.Background()
	seedProviderChain(t, db)
	occurrenceID := seedOccurrence(t, db)
	created, err := service.Create(ctx, occurrenceID, 1, "cmd-recovery-create")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, created.AttemptID, "boot-a", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(ctx, created.AttemptID, "boot-a", 1); err != nil {
		t.Fatal(err)
	}
	return created.AnalysisID, created.AttemptID
}

func TestCommitInterruptionClosesAttemptAndAnalysis(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	analysisID, attemptID := runAccepted(t, service, db)

	if err := service.CommitInterruption(ctx, attemptID, "lease_expired"); err != nil {
		t.Fatal(err)
	}
	detail, err := service.Get(ctx, analysisID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "Interrupted" {
		t.Fatalf("analysis state=%q, want Interrupted", detail.State)
	}
	attempt, err := service.Attempts().Get(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.State != "Interrupted" || attempt.TerminationReason == nil || *attempt.TerminationReason != "lease_expired" {
		t.Fatalf("attempt=%+v", attempt)
	}
	// Interruption is idempotent and never rewrites the terminal result.
	if err := service.CommitInterruption(ctx, attemptID, "replaced"); err != nil {
		t.Fatal(err)
	}
	attempt, _ = service.Attempts().Get(ctx, attemptID)
	if attempt.TerminationReason == nil || *attempt.TerminationReason != "lease_expired" {
		t.Fatalf("re-interruption rewrote the reason: %+v", attempt)
	}
	// The interrupted occurrence accepts a fresh analysis (operator retry).
	occurrenceID := seedLookupOccurrence(t, db, analysisID)
	fresh, err := service.Create(ctx, occurrenceID, 1, "cmd-recovery-retry")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.AnalysisID == analysisID {
		t.Fatal("retry after interruption reused the terminal analysis")
	}
}

// seedLookupOccurrence resolves the occurrence backing one analysis.
func seedLookupOccurrence(t *testing.T, db *sql.DB, analysisID int64) int64 {
	t.Helper()
	var occurrenceID int64
	if err := db.QueryRow(`SELECT occurrence_id FROM initial_analyses WHERE id=?`, analysisID).Scan(&occurrenceID); err != nil {
		t.Fatal(err)
	}
	return occurrenceID
}

func TestCommitInterruptionConvergesCancellingToCancelled(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	analysisID, attemptID := runAccepted(t, service, db)

	// The operator fence committed first; the loss event arrives while the
	// runtime stop is in flight (RUNTIME-TASK-006 fence exception).
	if _, err := service.Cancel(ctx, analysisID, 1, 2, "cmd-recovery-cancel"); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitInterruption(ctx, attemptID, "lease_expired"); err != nil {
		t.Fatal(err)
	}
	detail, err := service.Get(ctx, analysisID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "Cancelled" {
		t.Fatalf("analysis state=%q, want Cancelled", detail.State)
	}
	attempt, _ := service.Attempts().Get(ctx, attemptID)
	if attempt.State != "Cancelled" {
		t.Fatalf("attempt state=%q, want Cancelled", attempt.State)
	}
	// The late CancelAck is idempotent and does not strand the analysis.
	if err := service.CancelAck(ctx, attemptID); err != nil {
		t.Fatal(err)
	}
	detail, _ = service.Get(ctx, analysisID)
	if detail.State != "Cancelled" {
		t.Fatalf("late cancel ack changed state: %q", detail.State)
	}
}

func TestFailureReplayIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	analysisID, attemptID := runAccepted(t, service, db)

	first := service.CommitResult(ctx, Result{
		AttemptID: attemptID, BootID: "boot-a", Epoch: 1, Succeeded: false,
		Termination: "provider_unavailable",
	})
	if first != nil {
		t.Fatalf("failure commit: %v", first)
	}
	// The identical failure retry (lost ResultAck) replays as the original
	// verdict; a divergent one stays a late result.
	if err := service.CommitResult(ctx, Result{AttemptID: attemptID, BootID: "boot-a", Epoch: 1, Succeeded: false, Termination: "provider_unavailable"}); err != nil {
		t.Fatalf("identical failure replay: %v", err)
	}
	if err := service.CommitResult(ctx, Result{AttemptID: attemptID, BootID: "boot-a", Epoch: 1, Succeeded: false, Termination: "invalid_response"}); !errors.Is(err, ErrLateResult) {
		t.Fatalf("divergent failure replay: %v", err)
	}
	detail, _ := service.Get(ctx, analysisID)
	if detail.State != "Failed" {
		t.Fatalf("analysis state=%q, want Failed", detail.State)
	}
}

var _ *sql.DB

func TestExpiredLeaseRejectsResult(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	analysisID, attemptID := runAccepted(t, service, db)
	sealAgentCall(t, db, attemptID, "fixture-chat-1")
	// Burn the lease without converging the row (the sweeper window).
	if _, err := db.Exec(`UPDATE execution_attempts SET lease_until=?,row_version=row_version+1 WHERE id=?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), attemptID); err != nil {
		t.Fatal(err)
	}
	content := []byte(`"过期租约下的结果"`)
	digest := sha256.Sum256(content)
	if err := service.CommitResult(ctx, Result{AttemptID: attemptID, BootID: "boot-a", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: content, Digest: digest[:]}); !errors.Is(err, ErrLateResult) {
		t.Fatalf("expired-lease result accepted: %v", err)
	}
	if err := service.CommitResult(ctx, Result{AttemptID: attemptID, BootID: "boot-a", Epoch: 1, Succeeded: false, Termination: "provider_unavailable"}); !errors.Is(err, ErrLateResult) {
		t.Fatalf("expired-lease failure accepted: %v", err)
	}
	detail, _ := service.Get(ctx, analysisID)
	if detail.State != "Running" {
		t.Fatalf("expired-lease results must not move the analysis: %s", detail.State)
	}
}

func TestSealRepairsQueuedAnalysisCrashWindow(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	// Simulate the accept crash window with the real sequence: create a
	// fresh analysis, accept the attempt row and skip the analysis UPDATE
	// (the two transactions of AcceptAttempt dying between them — the
	// analysis never left Queued).
	ctx := context.Background()
	created, err := service.Create(ctx, seedOccurrence(t, db), 1, "cmd-crash-window-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, created.AttemptID, "boot-b", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().Accept(ctx, created.AttemptID, "boot-b", 1); err != nil {
		t.Fatal(err)
	}
	sealAgentCall(t, db, created.AttemptID, "fixture-chat-1")
	content := []byte(`"崩溃窗口下的封存"`)
	digest := sha256.Sum256(content)
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-b", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: content, Digest: digest[:]}); err != nil {
		t.Fatal(err)
	}
	detail, _ := service.Get(ctx, created.AnalysisID)
	if detail.State != "Succeeded" || detail.Output == nil {
		t.Fatalf("crash-window seal stranded the analysis: %+v", detail)
	}
	// The same repair applies to the interruption closure.
	second, err := service.Create(ctx, seedOccurrence(t, db), 1, "cmd-crash-window-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, second.AttemptID, "boot-b", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().Accept(ctx, second.AttemptID, "boot-b", 1); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitInterruption(ctx, second.AttemptID, "lease_expired"); err != nil {
		t.Fatal(err)
	}
	detail2, _ := service.Get(ctx, second.AnalysisID)
	if detail2.State != "Interrupted" {
		t.Fatalf("crash-window interruption stranded the analysis: %s", detail2.State)
	}
}
