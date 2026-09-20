package app

// Dispatch correlation propagation tests: the stored execution_attempts
// association is the single correlation authority every dispatch path echoes
// verbatim; a legacy row without correlation dispatches with an empty id, and
// a missing attempt row fails the dispatch instead of fabricating an identity
// (ADR-0006). The probe DispatchAttempt frame is retired with local
// execution (ADR-0011); the helper under test is the same one the remaining
// agent dispatches use.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/execution"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	_ "modernc.org/sqlite"
)

var testCorrelationID = "corr-" + strings.Repeat("b", 29)

// seedProbeAttemptWithCorrelation inserts one enabled connection plus its
// Queued connection_probe attempt; when correlate is true the attempt row
// receives the persisted association through the attempt helper.
func seedProbeAttemptWithCorrelation(t *testing.T, db *sql.DB, correlate bool) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES(?,?,0,?)`,
		"probe-target-"+now, connections.TypePrometheus, now)
	if err != nil {
		t.Fatal(err)
	}
	connectionID, _ := connection.LastInsertId()
	attemptRow, err := db.Exec(
		`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		 VALUES('connection_probe','connection',?,'Queued','test','probe-v1',?)`, connectionID, now)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, _ := attemptRow.LastInsertId()
	if !correlate {
		return attemptID
	}
	meta := execution.Metadata{
		CorrelationID: testCorrelationID,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Initiator:     execution.Principal{Kind: execution.PrincipalUser, ID: 42},
		Source:        execution.Source{Kind: execution.SourceTask, RequestID: "req-1"},
	}
	ctx, err := execution.WithMetadata(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	op, err := runner.Register(execution.Operation{Name: "attempt.correlation.fixture", Class: execution.ClassWrite, ObjectType: "execution_attempt", Authorize: func(context.Context, *execution.Tx) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execution.Execute(ctx, runner, op, func(tx *execution.Tx) (int64, error) {
		return attemptID, attempt.PersistCorrelationOn(ctx, tx, attemptID)
	}, func(id int64) int64 { return id }); err != nil {
		t.Fatal(err)
	}
	return attemptID
}

func newDispatchTestService(t *testing.T) (*RuntimeService, *[]*runtimev1.ControlEnvelope, *sql.DB) {
	t.Helper()
	path := t.TempDir() + "/test.db"
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	connections.ProbeContractSource = func() string { return "contract_version: 1" }
	var captured []*runtimev1.ControlEnvelope
	reader, err := execution.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	connectionService := connections.NewService(db, nil)
	connectionService.SetReader(reader)
	service := NewRuntimeControl(qruntime.NewService(), "test", connectionService, db)
	service.sendEnvelopeForTest = func(slot string, envelope *runtimev1.ControlEnvelope) error {
		captured = append(captured, envelope)
		return nil
	}
	return service, &captured, db
}

func TestDispatchCorrelationEchoesStoredAssociation(t *testing.T) {
	service, _, db := newDispatchTestService(t)
	correlated := seedProbeAttemptWithCorrelation(t, db, true)
	legacy := seedProbeAttemptWithCorrelation(t, db, false)

	echoed, err := dispatchOperationCorrelation(context.Background(), service.Connections.Reader(), correlated)
	if err != nil {
		t.Fatal(err)
	}
	if echoed != testCorrelationID {
		t.Fatalf("correlated dispatch operation_correlation_id = %q, want the stored association", echoed)
	}
	echoed, err = dispatchOperationCorrelation(context.Background(), service.Connections.Reader(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	if echoed != "" {
		t.Fatalf("legacy dispatch operation_correlation_id = %q, want empty", echoed)
	}
}

func TestDispatchCorrelationFailsWhenAttemptRowMissing(t *testing.T) {
	service, _, _ := newDispatchTestService(t)
	if _, err := dispatchOperationCorrelation(context.Background(), service.Connections.Reader(), 424242); err == nil {
		t.Fatal("correlation lookup of an unknown attempt must fail instead of dispatching without its identity")
	}
}
