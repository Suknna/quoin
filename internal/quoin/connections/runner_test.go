package connections_test

// Automatic audit and lifecycle association coverage (ADR-0006 phase 5):
// every active connection mutation leaves its durable trace — the automatic
// audit row in the same transaction, the command ledger for the replayable
// create/rotate commands — and nothing is recorded for a failed closed
// execution. The tests read the real audit_events and client_commands tables.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

func countAudit(t *testing.T, db *sql.DB, action, outcome string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action=? AND outcome=?`, action, outcome).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countCommands(t *testing.T, db *sql.DB, commandType string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM client_commands WHERE command_type=?`, commandType).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCreateRecordsLedgerAndAutomaticAudit(t *testing.T) {
	service, database, _ := newService(t)
	correlation := nextCorrelation()
	ctx := adminContext(t, correlation)
	created, err := service.Create(ctx, thanosInput(""), 1, "cmd-audit-create")
	if err != nil {
		t.Fatal(err)
	}
	// The ledger row persists the durable replayable outcome.
	if got := countCommands(t, database, "connection.create"); got != 1 {
		t.Fatalf("create must persist exactly one ledger row, got %d", got)
	}
	var principalType string
	var principalID int64
	var outcome, resultObject, ledgerCorrelation string
	if err := database.QueryRow(`SELECT principal_type,principal_id,outcome,result_object_type,COALESCE(correlation_id,'') FROM client_commands WHERE command_type='connection.create'`).Scan(&principalType, &principalID, &outcome, &resultObject, &ledgerCorrelation); err != nil {
		t.Fatal(err)
	}
	if principalType != "user" || principalID != 1 || outcome != "committed" || resultObject != "connection" {
		t.Fatalf("ledger row wrong: %s/%d %s %s", principalType, principalID, outcome, resultObject)
	}
	if ledgerCorrelation != correlation {
		t.Fatalf("ledger must persist the business correlation %q, got %q", correlation, ledgerCorrelation)
	}
	// The automatic audit row is the same-transaction fact; business code
	// never wrote it by hand.
	if got := countAudit(t, database, "connection.create", audit.OutcomeSuccess); got != 1 {
		t.Fatalf("create must record exactly one success audit row, got %d", got)
	}
	var actorType string
	var eventCorrelation string
	var domainRef string
	if err := database.QueryRow(`SELECT actor_type,correlation_id,domain_ref_type FROM audit_events WHERE action='connection.create'`).Scan(&actorType, &eventCorrelation, &domainRef); err != nil {
		t.Fatal(err)
	}
	if actorType != "user" || eventCorrelation != correlation || domainRef != "connection" {
		t.Fatalf("audit row wrong: actor=%s correlation=%q domain=%s", actorType, eventCorrelation, domainRef)
	}
	// The non-secret summary is the replay payload; the secret never enters.
	var payload string
	if err := database.QueryRow(`SELECT result_payload_json FROM client_commands WHERE command_type='connection.create'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(`"id":%d`, created.ID); !containsString(payload, want) {
		t.Fatalf("replay payload must carry the summary projection: %s", payload)
	}
	for _, secret := range []string{"secret-password", "bearerToken", "password"} {
		if containsString(payload, secret) {
			t.Fatalf("replay payload must not carry secret material: %s", payload)
		}
	}
}

func TestCreateReplayKeepsOriginalCorrelationWithoutNewRows(t *testing.T) {
	service, database, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	input := thanosInput("replay-correlation-secret")
	input.Name = "replay-correlation"
	first, err := service.Create(ctx, input, 1, "cmd-replay-correlation")
	if err != nil {
		t.Fatal(err)
	}
	// A retry on the same operation carries a fresh correlation but the same
	// command id: the stored outcome and the ORIGINAL correlation are
	// authoritative and no new durable rows appear.
	retry := adminContext(t, nextCorrelation())
	replayed, err := service.Create(retry, input, 1, "cmd-replay-correlation")
	if err != nil {
		t.Fatalf("replay must succeed: %v", err)
	}
	if replayed.ID != first.ID || replayed.RowVersion != first.RowVersion {
		t.Fatalf("replay must return the original summary: %+v vs %+v", replayed, first)
	}
	if got := countAudit(t, database, "connection.create", audit.OutcomeSuccess); got != 1 {
		t.Fatalf("replay must not produce new audit rows, got %d", got)
	}
	if got := countCommands(t, database, "connection.create"); got != 1 {
		t.Fatalf("replay must not produce new ledger rows, got %d", got)
	}
	var ledgerCorrelation string
	if err := database.QueryRow(`SELECT COALESCE(correlation_id,'') FROM client_commands WHERE command_type='connection.create'`).Scan(&ledgerCorrelation); err != nil {
		t.Fatal(err)
	}
	meta, _ := execution.FromContext(ctx)
	if ledgerCorrelation != meta.CorrelationID {
		t.Fatalf("replay must keep the original correlation %q, got %q", meta.CorrelationID, ledgerCorrelation)
	}
}

func TestCommandReusedWithDifferentRequestRecordsNothingNew(t *testing.T) {
	service, database, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	input := thanosInput("")
	input.Name = "reused-thanos"
	if _, err := service.Create(ctx, input, 1, "cmd-reused"); err != nil {
		t.Fatal(err)
	}
	other := thanosInput("")
	other.Name = "other-thanos"
	if _, err := service.Create(ctx, other, 1, "cmd-reused"); !errors.Is(err, execution.ErrCommandReused) {
		t.Fatalf("same command id with a different request must be rejected, got %v", err)
	}
	if got := countCommands(t, database, "connection.create"); got != 1 {
		t.Fatalf("reuse must not add ledger rows, got %d", got)
	}
	if got := countAudit(t, database, "connection.create", audit.OutcomeSuccess); got != 1 {
		t.Fatalf("reuse must not add audit rows, got %d", got)
	}
}

func TestMissingMetadataFailsClosedWithoutAnyTrace(t *testing.T) {
	service, database, _ := newService(t)
	// No execution metadata on the context: the command fails closed with no
	// connection row, no ledger row and no audit row — never an anonymous
	// "assume system" fallback.
	if _, err := service.Create(context.Background(), thanosInput(""), 1, "cmd-no-metadata"); err == nil {
		t.Fatal("create without execution metadata must fail closed")
	}
	var connectionsCount, commandsCount, auditCount int
	if err := database.QueryRow(`SELECT (SELECT COUNT(*) FROM connections),(SELECT COUNT(*) FROM client_commands),(SELECT COUNT(*) FROM audit_events)`).Scan(&connectionsCount, &commandsCount, &auditCount); err != nil {
		t.Fatal(err)
	}
	if connectionsCount != 0 || commandsCount != 0 || auditCount != 0 {
		t.Fatalf("failed-closed execution must leave no trace: connections=%d commands=%d audit=%d", connectionsCount, commandsCount, auditCount)
	}
}

func TestCallerPrincipalMismatchFailsClosed(t *testing.T) {
	service, database, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	if _, err := service.Create(ctx, thanosInput(""), 42, "cmd-wrong-actor"); err == nil {
		t.Fatal("a caller principal differing from the context actor must fail")
	}
	if got := countAudit(t, database, "connection.create", audit.OutcomeSuccess); got != 0 {
		t.Fatalf("mismatched attribution must not record success, got %d", got)
	}
}

func TestRevokedSessionFailsClosedWithoutDurableRecord(t *testing.T) {
	service, database, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	created, err := service.Create(ctx, thanosInput(""), 1, "cmd-revoked-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE sessions SET revoked_at=? WHERE id=1`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Disable(ctx, created.Name, created.RowVersion); !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("a revoked session must fail closed with the actor-changed sentinel, got %v", err)
	}
	// The clean rollback leaves no audit record for the denied execution.
	if got := countAudit(t, database, "connection.disable", audit.OutcomeSuccess); got != 0 {
		t.Fatalf("denied disable must not be audited as success, got %d", got)
	}
	if got := countAudit(t, database, "connection.disable", audit.OutcomeRejected); got != 0 {
		t.Fatalf("an authorization failure is not a deterministic rejection, got %d", got)
	}
}

func TestEnableRecordsAutomaticAuditWithoutLedger(t *testing.T) {
	service, database, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	created, err := service.Create(ctx, thanosInput(""), 1, "cmd-enable-create")
	if err != nil {
		t.Fatal(err)
	}
	// A deterministic rejection IS recorded — as a rejected audit, without a
	// ledger row (the non-replayable Execute path) and without state change.
	if _, err := service.Enable(ctx, created.Name, created.RowVersion, 0, 1); !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("enable without a passed probe must be rejected, got %v", err)
	}
	if got := countAudit(t, database, "connection.enable", audit.OutcomeRejected); got != 1 {
		t.Fatalf("deterministic enable rejection must be recorded, got %d", got)
	}
	if got := countCommands(t, database, "connection.enable"); got != 0 {
		t.Fatalf("enable owns no client command key, got %d ledger rows", got)
	}
	var enabled int
	if err := database.QueryRow(`SELECT enabled FROM connections WHERE id=?`, created.ID).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 {
		t.Fatal("rejected enable must not change the connection")
	}

	probe := passedMetricsProbe(t, service, database, created, "boot-audit-enable", 1)
	enabledSummary, err := service.Enable(ctx, created.Name, created.RowVersion, probe, 1)
	if err != nil || !enabledSummary.Enabled {
		t.Fatalf("enable after exact probe: %v %+v", err, enabledSummary)
	}
	if got := countAudit(t, database, "connection.enable", audit.OutcomeSuccess); got != 1 {
		t.Fatalf("successful enable must be audited exactly once, got %d", got)
	}
}

func TestStaleRowVersionCarriesCurrentVersion(t *testing.T) {
	service, database, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	created, err := service.Create(ctx, thanosInput(""), 1, "cmd-stale-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enable(ctx, created.Name, created.RowVersion-1, 0, 1); err == nil {
		t.Fatal("stale row version must conflict")
	}
	// The conflict rejection keeps the authoritative current row version so
	// the HTTP conflict payload can offer a refresh hint.
	var current int64
	if err := database.QueryRow(`SELECT row_version FROM connections WHERE id=?`, created.ID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enable(ctx, created.Name, created.RowVersion-1, 0, 1); err != nil {
		var version *connections.RowVersionError
		if !errors.As(err, &version) {
			t.Fatalf("stale enable must surface the row version error, got %v", err)
		}
		if version.Current != current || version.ID != created.ID {
			t.Fatalf("conflict must carry current=%d id=%d, got %+v", current, created.ID, version)
		}
	}
}

func TestProbeStartPersistsCorrelationAndInitiator(t *testing.T) {
	service, database, _ := newService(t)
	correlation := nextCorrelation()
	ctx := adminContext(t, correlation)
	created, err := service.Create(ctx, thanosInput(""), 1, "cmd-probe-correlation")
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := service.StartProbe(ctx, created.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The attempt creation persists the operation association atomically
	// (attempt.CreateOn): correlation, original initiator.
	var storedCorrelation, initiatorType string
	var initiatorID int64
	if err := database.QueryRow(`SELECT COALESCE(operation_correlation_id,''),COALESCE(initiator_type,''),COALESCE(initiator_id,0) FROM execution_attempts WHERE id=?`, attemptID).Scan(&storedCorrelation, &initiatorType, &initiatorID); err != nil {
		t.Fatal(err)
	}
	if storedCorrelation != correlation || initiatorType != "user" || initiatorID != 1 {
		t.Fatalf("attempt association wrong: correlation=%q initiator=%s/%d", storedCorrelation, initiatorType, initiatorID)
	}
	if got := countAudit(t, database, "connection.probe.start", audit.OutcomeSuccess); got != 1 {
		t.Fatalf("probe start must be audited exactly once, got %d", got)
	}
	var domainRef string
	if err := database.QueryRow(`SELECT domain_ref_type FROM audit_events WHERE action='connection.probe.start'`).Scan(&domainRef); err != nil {
		t.Fatal(err)
	}
	if domainRef != "connection_probe_attempt" {
		t.Fatalf("probe start audit must locate the attempt, got %s", domainRef)
	}
}

func TestProbeResultCommitSystemAuditPreservesTaskAssociation(t *testing.T) {
	service, database, _ := newService(t)
	correlation := nextCorrelation()
	ctx := adminContext(t, correlation)
	created, err := service.Create(ctx, thanosInput(""), 1, "cmd-probe-system-audit")
	if err != nil {
		t.Fatal(err)
	}
	if err := registerPlinthSlot(database); err != nil {
		t.Fatal(err)
	}
	attemptID, err := service.StartProbe(ctx, created.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The runtime-driven lifecycle arrives without user metadata: the system
	// task scope is explicit, and the attempt's user correlation stays
	// untouched (the persisted association is immutable).
	if _, _, _, ok, err := service.BindQueuedToStream(context.Background(), attemptID, "boot-system-audit", 9, 5*time.Minute); err != nil || !ok {
		t.Fatalf("bind: %v ok=%v", err, ok)
	}
	if err := service.AcceptProbe(context.Background(), attemptID, "boot-system-audit", 9); err != nil {
		t.Fatal(err)
	}
	child := &connections.TypedChild{Thanos: &connections.ThanosProbeChild{Query: "vector(1)", ResponseType: "vector", SampleCount: 1, SampleValue: "1", DetailJSON: `{"kind":"thanos"}`}}
	result := connections.TypedProbeResult{Outcome: "passed", ResultDigest: fmt.Sprintf("%064x", attemptID), StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z"}
	if err := service.CommitProbeResult(context.Background(), attemptID, "boot-system-audit", 9, result, child); err != nil {
		t.Fatal(err)
	}
	var actorType string
	var actorID int64
	if err := database.QueryRow(`SELECT actor_type,actor_id FROM audit_events WHERE action='connection.probe.result_commit'`).Scan(&actorType, &actorID); err != nil {
		t.Fatal(err)
	}
	if actorType != "system" || actorID != 0 {
		t.Fatalf("result commit must carry the explicit system identity, got %s/%d", actorType, actorID)
	}
	var storedCorrelation, initiatorType string
	var initiatorID int64
	if err := database.QueryRow(`SELECT COALESCE(operation_correlation_id,''),COALESCE(initiator_type,''),COALESCE(initiator_id,0) FROM execution_attempts WHERE id=?`, attemptID).Scan(&storedCorrelation, &initiatorType, &initiatorID); err != nil {
		t.Fatal(err)
	}
	if storedCorrelation != correlation || initiatorType != "user" || initiatorID != 1 {
		t.Fatalf("system closure must preserve the attempt association, got %q %s/%d", storedCorrelation, initiatorType, initiatorID)
	}
}

func TestDiscoveryRequiresMetadataBeforeNetworkAndAuditsOutcome(t *testing.T) {
	service, database, _ := newService(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"fixture-chat-model"}]}`))
	}))
	defer server.Close()

	// Missing metadata fails closed BEFORE any external network call.
	if _, err := service.DiscoverProviderModels(context.Background(), server.URL, "sk-discovery-secret"); err == nil {
		t.Fatal("discovery without execution metadata must fail closed")
	}
	if requests != 0 {
		t.Fatalf("unauthorized discovery must not reach the network, got %d requests", requests)
	}

	ctx := adminContext(t, nextCorrelation())
	view, err := service.DiscoverProviderModels(ctx, server.URL, "sk-discovery-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !view.Available || len(view.Models) != 1 || view.Models[0].ID != "fixture-chat-model" {
		t.Fatalf("discovery projection wrong: %+v", view)
	}
	if requests != 1 {
		t.Fatalf("discovery must call upstream exactly once, got %d", requests)
	}
	// The discovery fact is audited without a ledger row and without the key.
	if got := countAudit(t, database, "connection.model_discovery", audit.OutcomeSuccess); got != 1 {
		t.Fatalf("discovery must be audited, got %d", got)
	}
	if got := countCommands(t, database, "connection.model_discovery"); got != 0 {
		t.Fatalf("discovery carries no client command key, got %d", got)
	}
	var detail string
	if err := database.QueryRow(`SELECT detail_json FROM audit_events WHERE action='connection.model_discovery'`).Scan(&detail); err == nil && containsString(detail, "sk-discovery-secret") {
		t.Fatalf("audit must not carry the API key: %s", detail)
	}
	// An unreachable upstream is a durable but honest negative finding.
	unavailable, err := service.DiscoverProviderModels(ctx, "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	if unavailable.Available || unavailable.Detail == "" {
		t.Fatalf("unavailable upstream must report a stable hint: %+v", unavailable)
	}
}

func TestRotateRecordsAutomaticAuditReplacingManualRow(t *testing.T) {
	service, database, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	created, err := service.Create(ctx, thanosInput("rotate-audit-secret"), 1, "cmd-rotate-audit-create")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := service.Rotate(ctx, created.Name, created.RowVersion, thanosInput("rotate-audit-secret-2"), 1, "cmd-rotate-audit")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.RevalidationRequired != true {
		t.Fatalf("rotation must require revalidation: %+v", rotated)
	}
	// Exactly one audit row per command, written by the runner in the same
	// transaction — the old manual rotate audit insert is gone.
	if got := countAudit(t, database, "connection.rotate", audit.OutcomeSuccess); got != 1 {
		t.Fatalf("rotate must be audited exactly once, got %d", got)
	}
	if got := countCommands(t, database, "connection.rotate"); got != 1 {
		t.Fatalf("rotate must persist exactly one ledger row, got %d", got)
	}
	var payload string
	if err := database.QueryRow(`SELECT result_payload_json FROM client_commands WHERE command_type='connection.rotate'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var summary connections.Summary
	if err := json.Unmarshal([]byte(payload), &summary); err != nil {
		t.Fatalf("rotate replay payload must decode to the summary: %v", err)
	}
	if summary.CurrentRevisionID != rotated.CurrentRevisionID {
		t.Fatalf("replay payload must freeze the rotated summary: %+v vs %+v", summary, rotated)
	}
}

// containsString is the test-side substring check (the production helper is
// unexported in the connections package).
func containsString(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
