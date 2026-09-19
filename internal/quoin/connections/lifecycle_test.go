package connections_test

// Runtime-driven probe lifecycle scope contracts (ADR-0006): every
// supervisor transition (bind, accept, cancel ack, interrupt, result
// commit) that runs after the originating request ended restores the
// attempt's PERSISTED correlation and original initiator instead of
// starting a fresh root; unrelated inherited metadata is rejected instead
// of passed through; the model-call ledger shares the same contracts.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/connections"
	providerledger "github.com/Suknna/quoin/internal/quoin/connections/modelprovider"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// lifecycleAuditRow reads the newest audit row of one action with its
// correlation and identity fields.
func lifecycleAuditRow(t *testing.T, db *sql.DB, action string) (correlation, actorType string, actorID, initiatorID int64, initiatorType string) {
	t.Helper()
	if err := db.QueryRow(`SELECT correlation_id,actor_type,actor_id,COALESCE(initiator_id,0),COALESCE(initiator_type,'') FROM audit_events WHERE action=? ORDER BY id DESC LIMIT 1`, action).
		Scan(&correlation, &actorType, &actorID, &initiatorID, &initiatorType); err != nil {
		t.Fatal(err)
	}
	return correlation, actorType, actorID, initiatorID, initiatorType
}

func wantLifecycleAudit(t *testing.T, db *sql.DB, action, correlation string) {
	t.Helper()
	gotCorrelation, actorType, actorID, initiatorID, initiatorType := lifecycleAuditRow(t, db, action)
	if gotCorrelation != correlation || actorType != "system" || actorID != 0 || initiatorID != 1 || initiatorType != "user" {
		t.Fatalf("%s audit = correlation %q actor %s/%d initiator %s/%d, want %q system/0 user/1", action, gotCorrelation, actorType, actorID, initiatorType, initiatorID, correlation)
	}
}

// runningProbeFixture drives one probe from creation (admin correlation X)
// to Running via the real production paths, with unwired (post-request)
// contexts for every supervisor step. It returns the attempt id and the
// creating correlation. The probe targets a chat-only model provider so the
// attempt's first grant (model_probe_chat) can carry real model-call ledger
// rows; the lifecycle steps themselves are connection-type agnostic.
func runningProbeFixture(t *testing.T, name string) (*connections.Service, *sql.DB, int64, string) {
	t.Helper()
	service, database, _ := newService(t)
	correlation := nextCorrelation()
	ctx := adminContext(t, correlation)
	config, _ := json.Marshal(map[string]any{"type": "model_provider", "baseUrl": "https://api.example.com", "chatModelId": "lifecycle-chat", "contextBudgetTokens": 1024, "maxOutputTokens": 256})
	secret, _ := json.Marshal(map[string]string{"type": "model_provider", "apiKey": "lifecycle-api-key"})
	created, err := service.Create(ctx, connections.CreateInput{Name: name, Type: connections.TypeModelProvider, NonSecretJSON: config, Secret: secret, SecretPresent: true}, 1, "cmd-"+name)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := service.StartProbe(ctx, created.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := service.BindQueuedToStream(context.Background(), attemptID, "boot-"+name, 5, 5*time.Minute); err != nil || !ok {
		t.Fatalf("bind: %v ok=%v", err, ok)
	}
	if err := service.AcceptProbe(context.Background(), attemptID, "boot-"+name, 5); err != nil {
		t.Fatal(err)
	}
	// A Running model-provider probe owns its real qualification facts: the
	// schema's typed-child trigger closes every terminal probe result over
	// one succeeded and one cancelled chat call, so the supervisor's ledger
	// rows exist before any cancel/interrupt/result step. The unwired ledger
	// calls restore the attempt's persisted scope (production contract).
	grantID, err := firstGrantID(database, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	digest := func(n int64) string { return fmt.Sprintf("%064x", n) }
	reader := service.Reader()
	qualified, err := providerledger.Begin(context.Background(), database, reader, attemptID, grantID, 1, 0, "chat", "lifecycle-chat", digest(2), digest(3), digest(4), digest(5), 1024, 256, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := providerledger.WriteInputLineage(context.Background(), database, reader, qualified, "chat", digest(2), digest(3), attemptID); err != nil {
		t.Fatal(err)
	}
	if err := providerledger.Complete(context.Background(), database, reader, attemptID, qualified, providerledger.Completion{
		Outcome: "succeeded", ProviderRequestID: "req-lifecycle-qual-" + name, InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
		FinishReason: "stop", ResponseJSON: `{"assistantText":"ok","finishReason":"stop","tool_calls":[]}`, ResponseDigest: digest(6), ResponseComplete: true,
	}); err != nil {
		t.Fatal(err)
	}
	cancelled, err := providerledger.Begin(context.Background(), database, reader, attemptID, grantID, 2, 0, "chat", "lifecycle-chat", digest(2), digest(3), digest(4), digest(5), 1024, 256, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := providerledger.WriteInputLineage(context.Background(), database, reader, cancelled, "chat", digest(2), digest(3), attemptID); err != nil {
		t.Fatal(err)
	}
	if err := providerledger.Complete(context.Background(), database, reader, attemptID, cancelled, providerledger.Completion{Outcome: "cancelled", FailureReason: "cancelled", ProviderRequestID: "req-lifecycle-cancel-" + name}); err != nil {
		t.Fatal(err)
	}
	meta, _ := execution.FromContext(ctx)
	return service, database, attemptID, meta.CorrelationID
}

func TestProbeBindAndAcceptRestorePersistedCorrelationAfterRequestEnds(t *testing.T) {
	_, database, attemptID, original := runningProbeFixture(t, "lifecycle-bind-accept")
	// The bind and the accept ran unwired: their audit rows carry the
	// attempt's persisted correlation and the original user initiator with
	// the system task actor — never a fresh root.
	wantLifecycleAudit(t, database, "connection.probe.bind", original)
	wantLifecycleAudit(t, database, "connection.probe.accept", original)
	// The persisted attempt association itself is untouched.
	var stored string
	if err := database.QueryRow(`SELECT COALESCE(operation_correlation_id,'') FROM execution_attempts WHERE id=?`, attemptID).Scan(&stored); err != nil || stored != original {
		t.Fatalf("attempt association drifted: %q err=%v, want %q", stored, err, original)
	}
}

func TestProbeCancelAckAndInterruptRestorePersistedCorrelation(t *testing.T) {
	service, database, attemptID, original := runningProbeFixture(t, "lifecycle-cancel-ack")
	// The user cancellation carries its own operation correlation; the
	// runtime cancel ACK after the request ended restores the attempt's
	// original association.
	var rowVersion int64
	if err := database.QueryRow(`SELECT row_version FROM execution_attempts WHERE id=?`, attemptID).Scan(&rowVersion); err != nil {
		t.Fatal(err)
	}
	if err := service.CancelProbe(adminContext(t, nextCorrelation()), attemptID, rowVersion); err != nil {
		t.Fatal(err)
	}
	if err := service.RecordCancelAck(context.Background(), attemptID); err != nil {
		t.Fatal(err)
	}
	wantLifecycleAudit(t, database, "connection.probe.cancel_ack", original)

	// Interrupt over a fresh running probe restores the same association.
	interrupted, database2, attempt2, original2 := runningProbeFixture(t, "lifecycle-interrupt")
	if err := interrupted.InterruptProbe(context.Background(), attempt2, "lease_expired"); err != nil {
		t.Fatal(err)
	}
	wantLifecycleAudit(t, database2, "connection.probe.interrupt", original2)
}

func TestProbeLifecycleRejectsUnrelatedInheritedMetadata(t *testing.T) {
	service, database, attemptID, _ := runningProbeFixture(t, "lifecycle-unrelated")
	var eventsBefore int
	if err := database.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='connection.probe.accept'`).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}

	// A wired scope with an unrelated correlation (e.g. a long-lived stream
	// scope) must not launder its identity onto this attempt.
	unrelated, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "corr-unrelated-stream",
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceTask},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptProbe(unrelated, attemptID, "boot-lifecycle-unrelated", 5); err == nil {
		t.Fatal("accept with unrelated inherited correlation must be rejected")
	}

	// A user actor inherited from a finished request is equally rejected.
	if err := service.AcceptProbe(adminContext(t, nextCorrelation()), attemptID, "boot-lifecycle-unrelated", 5); err == nil {
		t.Fatal("accept with an inherited user actor must be rejected")
	}

	// Neither rejection leaves a durable record or a state change.
	var eventsAfter int
	if err := database.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='connection.probe.accept'`).Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if eventsAfter != eventsBefore {
		t.Fatalf("rejected accepts must not be audited, before=%d after=%d", eventsBefore, eventsAfter)
	}
	var state string
	if err := database.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "Running" {
		t.Fatalf("rejected accepts must leave the attempt Running, got %s", state)
	}
}

func TestProbeLifecycleWiredMatchingCorrelationPassesThrough(t *testing.T) {
	service, database, _, _ := runningProbeFixture(t, "lifecycle-wired")
	// A second probe, accepted by a WIRED system scope whose correlation
	// equals the attempt's persisted association: the legitimate pass-through.
	// The wired task scope acts on the originating administrator's behalf, so
	// it preserves the original initiator (execution.Delegate semantics).
	correlation := nextCorrelation()
	ctx := adminContext(t, correlation)
	config, _ := json.Marshal(map[string]any{"type": "thanos", "baseUrl": "https://thanos.example.com", "authType": "none"})
	created, err := service.Create(ctx, connections.CreateInput{Name: "lifecycle-wired-2", Type: connections.TypeThanos, NonSecretJSON: config}, 1, "cmd-lifecycle-wired-2")
	if err != nil {
		t.Fatal(err)
	}
	attempt2, err := service.StartProbe(ctx, created.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := service.BindQueuedToStream(context.Background(), attempt2, "boot-lifecycle-wired-2", 7, 5*time.Minute); err != nil || !ok {
		t.Fatalf("bind: %v ok=%v", err, ok)
	}
	wired, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Initiator:     execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source:        execution.Source{Kind: execution.SourceTask},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptProbe(wired, attempt2, "boot-lifecycle-wired-2", 7); err != nil {
		t.Fatalf("wired matching scope must pass through: %v", err)
	}
	wantLifecycleAudit(t, database, "connection.probe.accept", correlation)
}

func TestModelCallLedgerRestoresPersistedScopeAndAudits(t *testing.T) {
	service, database, attemptID, original := runningProbeFixture(t, "lifecycle-modelcall")
	// The supervisor's model-call lifecycle runs unwired after the request
	// ended: the ledger mutations restore the attempt's persisted scope and
	// audit under it. Call seq 3 continues after the fixture's qualification
	// calls (1 succeeded, 2 cancelled).
	grantID, err := firstGrantID(database, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	callID, err := providerledger.Begin(context.Background(), database, service.Reader(), attemptID, grantID, 3, 0, "chat", "lifecycle-chat", fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3), fmt.Sprintf("%064x", 4), fmt.Sprintf("%064x", 5), 1024, 256, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := providerledger.WriteInputLineage(context.Background(), database, service.Reader(), callID, "chat", fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3), attemptID); err != nil {
		t.Fatal(err)
	}
	if err := providerledger.Complete(context.Background(), database, service.Reader(), attemptID, callID, providerledger.Completion{
		Outcome: "succeeded", ProviderRequestID: "req-lifecycle", InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
		FinishReason: "stop", ResponseJSON: `{"assistantText":"ok","finishReason":"stop","tool_calls":[]}`, ResponseDigest: fmt.Sprintf("%064x", 6), ResponseComplete: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"connection.model_call.begin", "connection.model_call.input_lineage", "connection.model_call.complete"} {
		wantLifecycleAudit(t, database, action, original)
	}
}

func TestModelCallLedgerRejectsUnrelatedInheritedMetadataAndReplays(t *testing.T) {
	service, database, attemptID, _ := runningProbeFixture(t, "lifecycle-ledger-deny")
	grantID, err := firstGrantID(database, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	var callsBefore int
	if err := database.QueryRow(`SELECT COUNT(*) FROM model_calls WHERE attempt_id=?`, attemptID).Scan(&callsBefore); err != nil {
		t.Fatal(err)
	}
	var beginAudits int
	if err := database.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='connection.model_call.begin'`).Scan(&beginAudits); err != nil {
		t.Fatal(err)
	}

	// Unrelated inherited system correlation: rejected without trace.
	unrelated, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "corr-unrelated-ledger",
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceTask},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := providerledger.Begin(unrelated, database, service.Reader(), attemptID, grantID, 9, 0, "chat", "denied", fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3), fmt.Sprintf("%064x", 4), fmt.Sprintf("%064x", 5), 1, 1, 0, 0); err == nil {
		t.Fatal("ledger begin with unrelated inherited correlation must be rejected")
	}
	// Inherited user actor: equally rejected.
	if _, err := providerledger.Begin(adminContext(t, nextCorrelation()), database, service.Reader(), attemptID, grantID, 9, 0, "chat", "denied", fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3), fmt.Sprintf("%064x", 4), fmt.Sprintf("%064x", 5), 1, 1, 0, 0); err == nil {
		t.Fatal("ledger begin with an inherited user actor must be rejected")
	}
	var callsAfter int
	if err := database.QueryRow(`SELECT COUNT(*) FROM model_calls WHERE attempt_id=?`, attemptID).Scan(&callsAfter); err != nil {
		t.Fatal(err)
	}
	if callsAfter != callsBefore {
		t.Fatalf("rejected ledger begins must not insert rows, before=%d after=%d", callsBefore, callsAfter)
	}

	// Natural idempotence: a replayed Complete over the sealed call fails
	// closed and records nothing new. Call seq 4 continues after the fixture's
	// qualification calls; the budgets mirror the frozen revision contract.
	callID, err := providerledger.Begin(context.Background(), database, service.Reader(), attemptID, grantID, 4, 0, "chat", "lifecycle-chat", fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3), fmt.Sprintf("%064x", 4), fmt.Sprintf("%064x", 5), 1024, 256, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	completion := providerledger.Completion{Outcome: "failed", FailureReason: "invalid_response", ProviderRequestID: "req-replay"}
	if err := providerledger.Complete(context.Background(), database, service.Reader(), attemptID, callID, completion); err != nil {
		t.Fatal(err)
	}
	var completeAudits int
	if err := database.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='connection.model_call.complete'`).Scan(&completeAudits); err != nil {
		t.Fatal(err)
	}
	if err := providerledger.Complete(context.Background(), database, service.Reader(), attemptID, callID, completion); !errors.Is(err, providerledger.ErrLedgerDenied) {
		t.Fatalf("replayed complete must be denied, got %v", err)
	}
	var completeAuditsAfter int
	if err := database.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='connection.model_call.complete'`).Scan(&completeAuditsAfter); err != nil {
		t.Fatal(err)
	}
	if completeAuditsAfter != completeAudits {
		t.Fatalf("replayed complete must record nothing, before=%d after=%d", completeAudits, completeAuditsAfter)
	}
}

func firstGrantID(db *sql.DB, attemptID int64) (int64, error) {
	var grantID int64
	err := db.QueryRow(`SELECT id FROM attempt_connection_grants WHERE attempt_id=? ORDER BY id LIMIT 1`, attemptID).Scan(&grantID)
	return grantID, err
}
