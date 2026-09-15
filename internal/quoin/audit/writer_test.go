package audit

// The writer tests execute against the applied contract schema
// (internal/gen/contracts/schema.sql), which already carries the coordinated
// correlation extension and audit_cleanup_batches, so the writer is verified
// against the DDL main applied rather than a parallel copy.

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "quoin.db")+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	return db
}

func validRecord() Record {
	return Record{
		ActorType:     ActorUser,
		ActorID:       7,
		Action:        "item.create",
		Outcome:       OutcomeSuccess,
		Phase:         PhaseExecute,
		DomainRefType: "quoin_item",
		DomainRefID:   3,
		CorrelationID: "corr-writer",
		RequestID:     "req-1",
		InitiatorType: ActorUser,
		InitiatorID:   7,
		Targets:       []RecordTarget{{Type: "quoin_item", ID: 3}},
	}
}

func TestWriterPersistsEventAndTargets(t *testing.T) {
	db := newTestDB(t)
	writer := NewWriter()
	record := validRecord()
	version := int64(5)
	record.Targets = []RecordTarget{{Type: "quoin_item", ID: 3, Version: &version}}

	id, err := writer.Write(context.Background(), db, record)
	if err != nil {
		t.Fatal(err)
	}
	var actorType, action, outcome, phase, correlation, request, initiatorType, refType, targetType string
	var actorID, initiatorID, refID, targetID int64
	var targetVersion sql.NullInt64
	if err := db.QueryRow(`
		SELECT e.actor_type,e.actor_id,e.action,e.outcome,e.phase,e.correlation_id,e.request_id,
		       e.initiator_type,e.initiator_id,e.domain_ref_type,e.domain_ref_id,
		       t.target_type,t.target_id,t.target_version
		FROM audit_events e JOIN audit_event_targets t ON t.audit_event_id=e.id WHERE e.id=?`, id).
		Scan(&actorType, &actorID, &action, &outcome, &phase, &correlation, &request,
			&initiatorType, &initiatorID, &refType, &refID, &targetType, &targetID, &targetVersion); err != nil {
		t.Fatal(err)
	}
	if actorType != ActorUser || actorID != 7 || action != "item.create" || outcome != OutcomeSuccess || phase != PhaseExecute {
		t.Fatalf("event row actor=%s/%d action=%s outcome=%s phase=%s", actorType, actorID, action, outcome, phase)
	}
	if correlation != "corr-writer" || request != "req-1" || initiatorType != ActorUser || initiatorID != 7 {
		t.Fatalf("correlation fields corr=%s req=%s initiator=%s/%d", correlation, request, initiatorType, initiatorID)
	}
	if refType != "quoin_item" || refID != 3 || targetID != 3 || targetVersion.Int64 != 5 {
		t.Fatalf("refs ref=%s/%d target=%d/%d", refType, refID, targetID, targetVersion.Int64)
	}

	// Empty optional fields persist as NULL, not empty strings.
	accessRecord := validRecord()
	accessRecord.Action = "report.download"
	accessRecord.Phase = PhaseAccess
	accessRecord.ClientCommandID = ""
	accessRecord.DomainRefType = ""
	accessRecord.DomainRefID = 0
	accessRecord.Targets = nil
	accessRecord.RequestID = "req-2"
	accessID, err := writer.Write(context.Background(), db, accessRecord)
	if err != nil {
		t.Fatal(err)
	}
	var phaseRead, commandID, refTypeRead sql.NullString
	if err := db.QueryRow(`SELECT phase,client_command_id,domain_ref_type FROM audit_events WHERE id=?`, accessID).
		Scan(&phaseRead, &commandID, &refTypeRead); err != nil {
		t.Fatal(err)
	}
	if phaseRead.String != PhaseAccess || commandID.Valid || refTypeRead.Valid {
		t.Fatalf("access record phase=%s command=%v ref=%v, want access phase with NULL command/ref", phaseRead.String, commandID, refTypeRead)
	}
}

func TestWriterRejectsInvalidRecordsWithoutInserting(t *testing.T) {
	db := newTestDB(t)
	writer := NewWriter()
	cases := []struct {
		name    string
		mutate  func(*Record)
		message string
	}{
		{"missing correlation", func(r *Record) { r.CorrelationID = "" }, "correlation id"},
		{"control character correlation", func(r *Record) { r.CorrelationID = "corr\n\tid" }, "correlation id"},
		{"invalid outcome", func(r *Record) { r.Outcome = "ok" }, "invalid outcome"},
		{"invalid phase", func(r *Record) { r.Phase = "middle" }, "invalid phase"},
		{"invalid actor type", func(r *Record) { r.ActorType = "robot" }, "invalid actor type"},
		{"user actor without id", func(r *Record) { r.ActorID = 0 }, "positive id"},
		{"system actor with id", func(r *Record) { r.ActorType = ActorSystem; r.ActorID = 9 }, "id 0"},
		{"initiator id without type", func(r *Record) { r.InitiatorType = ""; r.InitiatorID = 7 }, "initiator id requires initiator type"},
		{"missing action", func(r *Record) { r.Action = "" }, "action is required"},
		{"target without type", func(r *Record) { r.Targets = []RecordTarget{{ID: 3}} }, "target type"},
		{"negative target id", func(r *Record) { r.Targets = []RecordTarget{{Type: "quoin_item", ID: -1}} }, "target id"},
		{"zero target version", func(r *Record) {
			zero := int64(0)
			r.Targets = []RecordTarget{{Type: "quoin_item", ID: 3, Version: &zero}}
		}, "target version"},
	}
	for _, testCase := range cases {
		record := validRecord()
		testCase.mutate(&record)
		if _, err := writer.Write(context.Background(), db, record); err == nil || !strings.Contains(err.Error(), testCase.message) {
			t.Fatalf("%s: err=%v, want rejection mentioning %q", testCase.name, err, testCase.message)
		}
	}
	// Every rejection happened before any INSERT: no partial rows.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid records inserted %d rows", count)
	}
	if _, err := writer.Write(context.Background(), db, validRecord()); err != nil {
		t.Fatalf("valid record failed after rejections: %v", err)
	}
}

func TestWriterDefaultsPhaseToExecute(t *testing.T) {
	db := newTestDB(t)
	writer := NewWriter()
	record := validRecord()
	record.Phase = ""
	id, err := writer.Write(context.Background(), db, record)
	if err != nil {
		t.Fatal(err)
	}
	var phase string
	if err := db.QueryRow(`SELECT phase FROM audit_events WHERE id=?`, id).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if phase != PhaseExecute {
		t.Fatalf("phase=%q, want default %q", phase, PhaseExecute)
	}
}

func TestRecordCleanupBatchPersistsBatchAndAuditAtomically(t *testing.T) {
	db := newTestDB(t)
	writer := NewWriter()
	ctx := context.Background()

	record := func() Record {
		batchRecord := validRecord()
		batchRecord.Action = "audit.cleanup"
		batchRecord.InitiatorType = ActorSystem
		batchRecord.InitiatorID = 0
		return batchRecord
	}
	batch := CleanupBatch{
		CutoffAt:       "2026-03-15T00:00:00Z",
		UpperEventID:   4200,
		DeletedEvents:  3100,
		DeletedTargets: 2900,
		Final:          true,
		CreatedAt:      "2026-09-15T10:00:00Z",
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.RecordCleanupBatch(ctx, tx, record(), batch); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var cutoff string
	var upper, deletedEvents, deletedTargets, finalBatch int64
	var createdAt string
	if err := db.QueryRow(`SELECT cutoff_at,upper_event_id,deleted_events,deleted_targets,final,created_at FROM audit_cleanup_batches`).
		Scan(&cutoff, &upper, &deletedEvents, &deletedTargets, &finalBatch, &createdAt); err != nil {
		t.Fatal(err)
	}
	if cutoff != "2026-03-15T00:00:00Z" || upper != 4200 || deletedEvents != 3100 || deletedTargets != 2900 || finalBatch != 1 {
		t.Fatalf("batch row cutoff=%s upper=%d events=%d targets=%d final=%d", cutoff, upper, deletedEvents, deletedTargets, finalBatch)
	}
	if createdAt != "2026-09-15T10:00:00Z" {
		t.Fatalf("batch created=%s", createdAt)
	}
	var action, refType, correlation string
	var refID int64
	if err := db.QueryRow(`SELECT action,domain_ref_type,domain_ref_id,correlation_id FROM audit_events`).
		Scan(&action, &refType, &refID, &correlation); err != nil {
		t.Fatal(err)
	}
	if action != "audit.cleanup" || refType != "audit_cleanup_batch" || refID != 1 || correlation != "corr-writer" {
		t.Fatalf("audit action=%s ref=%s/%d corr=%s, want batch domain reference with correlation", action, refType, refID, correlation)
	}

	// Invalid records and batches must insert nothing.
	failureRecords := []func() Record{
		func() Record { broken := validRecord(); broken.CorrelationID = ""; return broken },
		func() Record { broken := validRecord(); broken.Outcome = "ok"; return broken },
	}
	batches := []CleanupBatch{
		{CutoffAt: ""},
		{CutoffAt: strings.Repeat("x", 65)},
		{CutoffAt: "2026-03-15T00:00:00Z", DeletedEvents: -1},
	}
	for _, broken := range failureRecords {
		failureTx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.RecordCleanupBatch(ctx, failureTx, broken(), CleanupBatch{CutoffAt: "2026-03-15T00:00:00Z"}); err == nil {
			t.Fatal("invalid cleanup record accepted")
		}
		if err := failureTx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	for _, brokenBatch := range batches {
		failureTx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.RecordCleanupBatch(ctx, failureTx, validRecord(), brokenBatch); err == nil {
			t.Fatal("invalid cleanup batch accepted")
		}
		if err := failureTx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_cleanup_batches`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("cleanup batches=%d, want only the valid one", count)
	}
}
