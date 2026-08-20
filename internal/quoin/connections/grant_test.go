package connections_test

// T07 race/fence tests: grant fulfillment fences (boot/epoch/attempt/
// terminal), cancellation commit order, secret-input command replay and
// queued-dispatch binding. All assertions run against the real SQLite
// schema with the real AEAD envelope.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/connections"
)

func grantFixture(t *testing.T) (*connections.Service, *sql.DB, int64, int64, string, uint64) {
	t.Helper()
	service, database, _ := newService(t)
	ctx := context.Background()
	input := thanosInput("grant-secret-password")
	input.Name = "grant-thanos"
	summary, err := service.Create(ctx, input, 1, "cmd-grant-create")
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := service.StartProbe(ctx, summary.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Register a confirmed plinth slot (the dispatch fence requires it),
	// then bind through the real stream-attach path.
	if err := registerPlinthSlot(database); err != nil {
		t.Fatal(err)
	}
	_, grantID, _, ok, err := service.BindQueuedToStream(ctx, attemptID, "boot-grant", 7, 5*time.Minute)
	if err != nil || !ok {
		t.Fatalf("fixture bind failed: %v %v", err, ok)
	}
	return service, database, attemptID, grantID, "boot-grant", 7
}

func TestGrantFulfillmentFences(t *testing.T) {
	service, _, attemptID, grantID, boot, epoch := grantFixture(t)
	ctx := context.Background()

	// Correct binding decrypts the typed secret.
	payload, err := service.FulfillGrant(ctx, grantID, attemptID, boot, epoch)
	if err != nil {
		t.Fatalf("correct binding must fulfill: %v", err)
	}
	if payload.Thanos == nil || payload.Thanos.Password != "grant-secret-password" {
		t.Fatalf("thanos secret not decrypted: %+v", payload.Thanos)
	}
	if payload.ConnectionType != connections.TypeThanos || payload.RevisionConfigJSON == nil {
		t.Fatalf("payload projection incomplete: %+v", payload)
	}

	// Wrong boot / wrong epoch / wrong attempt are all denied.
	if _, err := service.FulfillGrant(ctx, grantID, attemptID, "other-boot", epoch); !errors.Is(err, connections.ErrGrantDenied) {
		t.Fatalf("wrong boot must be denied, got %v", err)
	}
	if _, err := service.FulfillGrant(ctx, grantID, attemptID, boot, epoch+1); !errors.Is(err, connections.ErrGrantDenied) {
		t.Fatalf("wrong epoch must be denied, got %v", err)
	}
	if _, err := service.FulfillGrant(ctx, grantID, attemptID+999, boot, epoch); !errors.Is(err, connections.ErrGrantDenied) {
		t.Fatalf("foreign attempt must be denied, got %v", err)
	}
}

func TestGrantDeniedAfterTerminal(t *testing.T) {
	service, _, attemptID, grantID, boot, epoch := grantFixture(t)
	ctx := context.Background()
	// Close through the real terminal path, then confirm the reveal cannot
	// be replayed after closure.
	if err := service.AcceptProbe(ctx, attemptID, boot, epoch); err != nil {
		t.Fatal(err)
	}
	if err := commitPassedThanos(service, attemptID, boot, epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := service.FulfillGrant(ctx, grantID, attemptID, boot, epoch); !errors.Is(err, connections.ErrGrantDenied) {
		t.Fatalf("terminal attempt must deny the grant, got %v", err)
	}
}

func commitPassedThanos(service *connections.Service, attemptID int64, boot string, epoch uint64) error {
	detail := []byte(`{"kind":"thanos","query":"vector(1)","responseType":"vector","sampleCount":1,"sampleValue":"1"}`)
	result := connections.TypedProbeResult{Outcome: "passed", Detail: detail, ResultDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z"}
	child := &connections.TypedChild{Thanos: &connections.ThanosProbeChild{Query: "vector(1)", ResponseType: "vector", SampleCount: 1, SampleValue: "1", DetailJSON: `{"kind":"thanos"}`}}
	return service.CommitProbeResult(context.Background(), attemptID, boot, epoch, result, child)
}

func TestProbeCancelCommitOrder(t *testing.T) {
	service, database, attemptID, grantID, boot, epoch := grantFixture(t)
	ctx := context.Background()
	if err := service.AcceptProbe(ctx, attemptID, boot, epoch); err != nil {
		t.Fatal(err)
	}
	var rowVersion int64
	if err := database.QueryRow(`SELECT row_version FROM execution_attempts WHERE id=?`, attemptID).Scan(&rowVersion); err != nil {
		t.Fatal(err)
	}
	_ = database
	// Cancellation fence commits first (cancelled result + Cancelling).
	if err := service.CancelProbe(ctx, attemptID, rowVersion); err != nil {
		t.Fatal(err)
	}
	// A late result proposal after the fence is rejected
	// (RUNTIME-CANCEL-002).
	detail := []byte(`{"kind":"thanos","query":"vector(1)","responseType":"vector","sampleCount":1,"sampleValue":"1"}`)
	late := connections.TypedProbeResult{Outcome: "passed", Detail: detail, ResultDigest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z"}
	child := &connections.TypedChild{Thanos: &connections.ThanosProbeChild{Query: "vector(1)", ResponseType: "vector", SampleCount: 1, SampleValue: "1", DetailJSON: "{}"}}
	if err := service.CommitProbeResult(ctx, attemptID, boot, epoch, late, child); err == nil {
		t.Fatal("late result after cancellation fence must be rejected")
	}
	// CancelAck finalizes to Cancelled with the cancelled reason.
	if err := service.RecordCancelAck(ctx, attemptID); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	if err := database.QueryRow(`SELECT state,termination_reason FROM execution_attempts WHERE id=?`, attemptID).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "Cancelled" || reason != "cancelled" {
		t.Fatalf("cancel ack must finalize, got %s/%s", state, reason)
	}
	_ = grantID
}

func TestCreateCommandReplay(t *testing.T) {
	service, _, _, _ := grantFixtureLight(t)
	ctx := context.Background()
	input := thanosInput("first-secret-value")
	input.Name = "replay-thanos"
	first, err := service.Create(ctx, input, 1, "cmd-replay-1")
	if err != nil {
		t.Fatal(err)
	}
	// A retry with the SAME command id but a DIFFERENT secret value replays
	// the original result without comparing the secret
	// (x-quoin-secret-input-idempotency).
	retryInput := thanosInput("second-secret-value")
	retryInput.Name = "replay-thanos"
	replayed, err := service.Create(ctx, retryInput, 1, "cmd-replay-1")
	if err != nil {
		t.Fatalf("replay must succeed: %v", err)
	}
	if replayed.ID != first.ID || replayed.RowVersion != first.RowVersion {
		t.Fatalf("replay must return the original summary, got %+v vs %+v", replayed, first)
	}
	// A different command id still conflicts on the taken name.
	if _, err := service.Create(ctx, retryInput, 1, "cmd-replay-2"); !errors.Is(err, connections.ErrNameTaken) {
		t.Fatalf("new command with taken name must conflict, got %v", err)
	}
}

func registerPlinthSlot(database *sql.DB) error {
	if _, err := database.Exec(`INSERT OR IGNORE INTO runtime_slots(slot,state,row_version,created_at) VALUES('plinth','unregistered',1,?)`, "2026-01-01T00:00:00Z"); err != nil {
		return err
	}
	cred, err := database.Exec(`INSERT INTO runtime_credentials(slot,generation,token_digest,confirmed_at,created_at) VALUES('plinth',1,?,?,?)`, make([]byte, 32), "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	if err != nil {
		return err
	}
	credID, err := cred.LastInsertId()
	if err != nil {
		return err
	}
	_, err = database.Exec(`UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=row_version+1 WHERE slot='plinth'`, credID)
	return err
}

func grantFixtureLight(t *testing.T) (*connections.Service, *sql.DB, int64, int64) {
	t.Helper()
	service, database, _ := newService(t)
	return service, database, 0, 0
}

func TestQueuedDispatchBindsOnConnect(t *testing.T) {
	service, database, _, _ := grantFixtureLight(t)
	ctx := context.Background()
	input := thanosInput("queued-secret")
	input.Name = "queued-thanos"
	summary, err := service.Create(ctx, input, 1, "cmd-queued-create")
	if err != nil {
		t.Fatal(err)
	}
	// No live stream: the attempt stays Queued.
	attemptID, err := service.StartProbe(ctx, summary.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var state string
	var runtimeSlot sql.NullString
	if err := database.QueryRow(`SELECT state,runtime_slot FROM execution_attempts WHERE id=?`, attemptID).Scan(&state, &runtimeSlot); err != nil {
		t.Fatal(err)
	}
	if state != "Queued" || runtimeSlot.Valid {
		t.Fatalf("expected unbound Queued attempt, got %s/%v", state, runtimeSlot)
	}
	// Register the slot so the dispatch fence accepts the bind.
	if err := registerPlinthSlot(database); err != nil {
		t.Fatal(err)
	}
	// The stream attach path binds and returns the dispatch tuple.
	bound, grantID, snapshot, ok, err := service.BindQueuedToStream(ctx, attemptID, "boot-late", 3, 5*time.Minute)
	if err != nil || !ok {
		t.Fatalf("queued attempt must bind: %v %v", err, ok)
	}
	if bound.ID != summary.ID || grantID == 0 || len(snapshot) == 0 {
		t.Fatalf("dispatch tuple incomplete: %+v %d %d", bound, grantID, len(snapshot))
	}
	if err := database.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "Assigned" {
		t.Fatalf("bound attempt must be Assigned, got %s", state)
	}
	// A second binder loses the race harmlessly.
	if _, _, _, ok, err := service.BindQueuedToStream(ctx, attemptID, "boot-late", 3, 5*time.Minute); err != nil || ok {
		t.Fatalf("second bind must be a no-op, got %v %v", err, ok)
	}
}

func TestListRendersConfig(t *testing.T) {
	service, _, _, _ := grantFixtureLight(t)
	ctx := context.Background()
	input := thanosInput("")
	input.Name = "list-thanos"
	if _, err := service.Create(ctx, input, 1, "cmd-list-create"); err != nil {
		t.Fatal(err)
	}
	summaries, more, err := service.List(ctx, "", 10)
	if err != nil {
		t.Fatalf("list must scan the typed config projection: %v", err)
	}
	if more || len(summaries) != 1 {
		t.Fatalf("unexpected page: %d items, more=%v", len(summaries), more)
	}
	if summaries[0].Name != "list-thanos" || len(summaries[0].Config) == 0 {
		t.Fatalf("summary projection wrong: %+v", summaries[0])
	}
}

func TestRotateSwitchesPairAndLateResultClosesOldPair(t *testing.T) {
	service, database, _ := newService(t)
	ctx := context.Background()
	input := thanosInput("original-secret-value")
	input.Name = "rotate-thanos"
	summary, err := service.Create(ctx, input, 1, "cmd-rotate-create")
	if err != nil {
		t.Fatal(err)
	}
	if err := registerPlinthSlot(database); err != nil {
		t.Fatal(err)
	}
	attemptID, err := service.StartProbe(ctx, summary.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, grantID, _, ok, err := service.BindQueuedToStream(ctx, attemptID, "boot-rotate", 1, 5*time.Minute)
	if err != nil || !ok {
		t.Fatalf("bind: %v %v", err, ok)
	}
	if err := service.AcceptProbe(ctx, attemptID, "boot-rotate", 1); err != nil {
		t.Fatal(err)
	}
	// Rotate while the probe is in flight: the current pair switches.
	rotated, err := service.Rotate(ctx, summary.Name, summary.RowVersion, thanosInput("next-secret-value"), 1, "cmd-rotate-1")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.CurrentGenerationID == summary.CurrentGenerationID || rotated.CurrentRevisionID == summary.CurrentRevisionID {
		t.Fatalf("rotate must switch the current pair: %+v vs %+v", rotated, summary)
	}
	if rotated.RevalidationRequired != true {
		t.Fatalf("rotate must require revalidation: %+v", rotated)
	}
	// Command replay without secret comparison.
	replayed, err := service.Rotate(ctx, summary.Name, rotated.RowVersion, thanosInput("replayed-other-secret"), 1, "cmd-rotate-1")
	if err != nil {
		t.Fatalf("rotate replay: %v", err)
	}
	if replayed.RowVersion != rotated.RowVersion {
		t.Fatalf("rotate replay must return the original summary, got %+v vs %+v", replayed, rotated)
	}
	// The in-flight probe's late result closes over the OLD frozen pair,
	// not the rotated current (commit-order race).
	detail := []byte(`{"kind":"thanos","query":"vector(1)","responseType":"vector","sampleCount":1,"sampleValue":"1"}`)
	result := connections.TypedProbeResult{Outcome: "passed", Detail: detail, ResultDigest: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z"}
	child := &connections.TypedChild{Thanos: &connections.ThanosProbeChild{Query: "vector(1)", ResponseType: "vector", SampleCount: 1, SampleValue: "1", DetailJSON: `{"kind":"thanos"}`}}
	if err := service.CommitProbeResult(ctx, attemptID, "boot-rotate", 1, result, child); err != nil {
		t.Fatalf("late result must close over the frozen pair: %v", err)
	}
	var closedRevision, closedGeneration int64
	if err := database.QueryRow(`SELECT connection_revision_id,credential_generation_id FROM connection_probe_results WHERE attempt_id=?`, attemptID).Scan(&closedRevision, &closedGeneration); err != nil {
		t.Fatal(err)
	}
	if closedRevision != summary.CurrentRevisionID || closedGeneration != summary.CurrentGenerationID {
		t.Fatalf("late result closed the wrong pair: got (%d,%d) want (%d,%d)", closedRevision, closedGeneration, summary.CurrentRevisionID, summary.CurrentGenerationID)
	}
	_ = grantID
}
