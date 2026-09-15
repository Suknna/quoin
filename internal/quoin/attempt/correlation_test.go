package attempt

// Correlation persistence tests over the real frozen schema: the
// attempt-creating transaction stores the execution context's correlation
// and original initiator, new operations fail closed without metadata, and
// legacy rows report absence explicitly instead of an error or a fabricated
// identity.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// seedCorrelationAttempt inserts one Queued initial-analysis attempt via the
// established seed chain (the frozen scope trigger requires the active
// analysis object) and returns its id. The row carries no correlation
// columns.
func seedCorrelationAttempt(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	attemptID, _ := seedAttempt(t, db)
	return attemptID
}

func correlationContext(t *testing.T) context.Context {
	t.Helper()
	meta := execution.Metadata{
		CorrelationID: "corr-" + strings.ToLower(strings.Repeat("a", 29)), // bounded opaque token
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Initiator:     execution.Principal{Kind: execution.PrincipalUser, ID: 42},
		Source:        execution.Source{Kind: execution.SourceTask, RequestID: "req-1"},
	}
	ctx, err := execution.WithMetadata(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func correlationTransaction(t *testing.T, ctx context.Context, db *sql.DB, write func(*execution.Tx) error) error {
	t.Helper()
	registry := execution.NewRegistry()
	op, err := registry.Register(execution.Operation{
		Name: "attempt.correlation.test", Class: execution.ClassWrite, ObjectType: "execution_attempt",
		Authorize: func(context.Context, *execution.Tx) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = execution.Execute(ctx, execution.NewRunner(db, registry, nil), op, func(tx *execution.Tx) (struct{}, error) {
		return struct{}{}, write(tx)
	}, func(struct{}) int64 { return 0 })
	return err
}

func persistCorrelationForTest(t *testing.T, ctx context.Context, db *sql.DB, attemptID int64) error {
	t.Helper()
	return correlationTransaction(t, ctx, db, func(tx *execution.Tx) error {
		return PersistCorrelationOn(ctx, tx, attemptID)
	})
}

func TestPersistCorrelationOnStoresContextMetadata(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	attemptID := seedCorrelationAttempt(t, db)

	if err := persistCorrelationForTest(t, correlationContext(t), db, attemptID); err != nil {
		t.Fatal(err)
	}
	correlation, present, err := LoadCorrelation(context.Background(), db, attemptID)
	if err != nil || !present {
		t.Fatalf("load correlation after persist: present=%v err=%v", present, err)
	}
	if correlation.OperationCorrelationID != "corr-"+strings.Repeat("a", 29) {
		t.Fatalf("operation correlation = %q", correlation.OperationCorrelationID)
	}
	// The initiator is stored exactly as the context declared it, separate
	// from the acting (system) principal of the persisting step.
	if correlation.InitiatorType != "user" || correlation.InitiatorID != 42 {
		t.Fatalf("initiator = %q/%d, want user/42", correlation.InitiatorType, correlation.InitiatorID)
	}
}

func TestPersistCorrelationOnFailsClosedWithoutMetadata(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	attemptID := seedCorrelationAttempt(t, db)

	err := persistCorrelationForTest(t, context.Background(), db, attemptID)
	if !errors.Is(err, execution.ErrMissingContext) {
		t.Fatalf("missing metadata err = %v, want execution.ErrMissingContext", err)
	}
	if _, present, loadErr := LoadCorrelation(context.Background(), db, attemptID); present || loadErr != nil {
		t.Fatalf("failed persist must leave the row untouched: present=%v err=%v", present, loadErr)
	}
}

func TestPersistCorrelationOnMissingAttemptFails(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	if err := persistCorrelationForTest(t, correlationContext(t), db, 424242); !errors.Is(err, ErrAttemptMissing) {
		t.Fatalf("missing attempt err = %v, want ErrAttemptMissing", err)
	}
}

func TestLoadCorrelationReportsLegacyAbsentExplicitly(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	attemptID := seedCorrelationAttempt(t, db)

	correlation, present, err := LoadCorrelation(context.Background(), db, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("legacy row without correlation columns must report absent, not a fabricated identity")
	}
	if correlation != (Correlation{}) {
		t.Fatalf("legacy correlation = %+v, want the zero value", correlation)
	}
}

func TestLoadCorrelationMissingRowIsError(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	if _, _, err := LoadCorrelation(context.Background(), db, 424242); !errors.Is(err, ErrAttemptMissing) {
		t.Fatalf("missing row err = %v, want ErrAttemptMissing", err)
	}
}

// alternativeCorrelationContext carries a different correlation identity for
// the immutability rejection case.
func alternativeCorrelationContext(t *testing.T) context.Context {
	t.Helper()
	meta := execution.Metadata{
		CorrelationID: "corr-" + strings.ToLower(strings.Repeat("c", 29)),
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Initiator:     execution.Principal{Kind: execution.PrincipalService, ID: 7},
		Source:        execution.Source{Kind: execution.SourceTask},
	}
	ctx, err := execution.WithMetadata(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestPersistCorrelationOnRepeatsAreIdempotent(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	attemptID := seedCorrelationAttempt(t, db)
	ctx := correlationContext(t)

	if err := persistCorrelationForTest(t, ctx, db, attemptID); err != nil {
		t.Fatal(err)
	}
	// A repeat with the same association must be a no-op: the persisted
	// association stays and the row version does not advance again.
	if err := persistCorrelationForTest(t, ctx, db, attemptID); err != nil {
		t.Fatalf("idempotent repeat failed: %v", err)
	}
	var version int64
	if err := db.QueryRow(`SELECT row_version FROM execution_attempts WHERE id=?`, attemptID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("row_version = %d after seed(1) + first persist + repeat, want 2 (repeat must not bump)", version)
	}
}

func TestPersistCorrelationOnRejectsDifferentAssociation(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	attemptID := seedCorrelationAttempt(t, db)
	first := correlationContext(t)
	if err := persistCorrelationForTest(t, first, db, attemptID); err != nil {
		t.Fatal(err)
	}
	err := persistCorrelationForTest(t, alternativeCorrelationContext(t), db, attemptID)
	if err == nil {
		t.Fatal("a different association must be rejected: the persisted correlation is immutable")
	}
	stored, present, loadErr := LoadCorrelation(context.Background(), db, attemptID)
	if loadErr != nil || !present {
		t.Fatalf("stored association unreadable: present=%v err=%v", present, loadErr)
	}
	if stored.OperationCorrelationID != "corr-"+strings.Repeat("a", 29) || stored.InitiatorType != "user" {
		t.Fatalf("stored association changed after rejection: %+v", stored)
	}
}

func TestCreateOnPersistsCorrelationWithTheInsert(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES('corr-target','prometheus',0,?)`,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	connectionID, _ := connection.LastInsertId()
	var attemptID int64
	ctx := correlationContext(t)
	if err := correlationTransaction(t, ctx, db, func(tx *execution.Tx) error {
		var err error
		attemptID, err = CreateOn(ctx, tx,
			`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
			 VALUES('connection_probe','connection',?,'Queued','test','probe-v1',?)`,
			connectionID, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stored, present, loadErr := LoadCorrelation(context.Background(), db, attemptID)
	if loadErr != nil || !present {
		t.Fatalf("CreateOn did not persist the association: present=%v err=%v", present, loadErr)
	}
	if stored.OperationCorrelationID != "corr-"+strings.Repeat("a", 29) || stored.InitiatorType != "user" || stored.InitiatorID != 42 {
		t.Fatalf("CreateOn stored the wrong association: %+v", stored)
	}
}

func TestCreateOnFailsClosedWithoutMetadataAndLeavesNoRow(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES('corr-target-2','prometheus',0,?)`,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	connectionID, _ := connection.LastInsertId()
	var attemptCountBefore int
	if err := db.QueryRow(`SELECT COUNT(*) FROM execution_attempts`).Scan(&attemptCountBefore); err != nil {
		t.Fatal(err)
	}
	err = correlationTransaction(t, correlationContext(t), db, func(tx *execution.Tx) error {
		_, err := CreateOn(context.Background(), tx,
			`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
			 VALUES('connection_probe','connection',?,'Queued','test','probe-v1',?)`,
			connectionID, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	})
	if !errors.Is(err, execution.ErrMissingContext) {
		t.Fatalf("CreateOn without metadata err = %v, want execution.ErrMissingContext", err)
	}
	var attemptCountAfter int
	if err := db.QueryRow(`SELECT COUNT(*) FROM execution_attempts`).Scan(&attemptCountAfter); err != nil {
		t.Fatal(err)
	}
	if attemptCountAfter != attemptCountBefore {
		t.Fatalf("failed creation left %d rows (before %d): the caller's transaction must discard the attempt", attemptCountAfter, attemptCountBefore)
	}
}
