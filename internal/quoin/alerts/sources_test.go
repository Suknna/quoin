package alerts

// Alert-source management command coverage (ADR-0006): the runner-executed
// client commands produce their audit facts automatically with a real
// administrator session proof, replay idempotently from the durable ledger,
// roll the business stage back on failure, record deterministic rejections,
// and keep every secret out of the ledger and the audit trail.

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// auditRow is the whitelisted projection of one audit event used by the
// assertions below.
type auditRow struct {
	actorType   string
	actorID     int64
	action      string
	outcome     string
	phase       string
	corrID      string
	refType     string
	refID       int64
	initiator   string
	initiatorID int64
}

func scanAuditRow(t *testing.T, db *sql.DB, query string, args ...any) auditRow {
	t.Helper()
	var row auditRow
	err := db.QueryRow(query, args...).Scan(&row.actorType, &row.actorID, &row.action, &row.outcome, &row.phase, &row.corrID, &row.refType, &row.refID, &row.initiator, &row.initiatorID)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestCreateSourceAuditsAutomaticallyAndReplays(t *testing.T) {
	service, database, done := newTestService(t)
	defer done()
	db := database.SQL
	ctx := adminCommandContext(t, context.Background())

	digest := make([]byte, 32)
	digest[0] = 7
	result, replayed, err := service.CreateSource(ctx, "create-audit-0001", "checkout-am", "alertmanager", digest)
	if err != nil || replayed {
		t.Fatalf("create = (%+v, %v, %v)", result, replayed, err)
	}
	if result.SourceID == 0 || result.CredentialID == 0 || result.SourceKey != "checkout-am" {
		t.Fatalf("create result = %+v", result)
	}
	// The success audit is produced without any business audit call.
	row := scanAuditRow(t, db, `SELECT actor_type,actor_id,action,outcome,phase,correlation_id,domain_ref_type,domain_ref_id,initiator_type,initiator_id FROM audit_events`)
	if row.actorType != "user" || row.actorID != 1 || row.action != opCreateSource || row.outcome != "success" ||
		row.phase != "execute" || row.corrID != "alerts-"+t.Name() || row.refType != objectSource || row.refID != result.SourceID ||
		row.initiator != "user" || row.initiatorID != 1 {
		t.Fatalf("audit row = %+v", row)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_event_targets WHERE target_type=? AND target_id=?`, objectSource, result.SourceID); got != 1 {
		t.Fatalf("audit target rows = %d, want 1", got)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM alert_source_credentials WHERE id=?`, result.CredentialID).Scan(&state); err != nil || state != "Active" {
		t.Fatalf("first credential state=%q err=%v", state, err)
	}

	// Idempotent replay: the same client command id and the same request
	// digest return the stored result and the original correlation without a
	// new business success audit or a second source row.
	replayResult, replayed, err := service.CreateSource(ctx, "create-audit-0001", "checkout-am", "alertmanager", digest)
	if err != nil || !replayed {
		t.Fatalf("replay = (%+v, %v, %v)", replayResult, replayed, err)
	}
	if replayResult != result {
		t.Fatalf("replayed result = %+v, want %+v", replayResult, result)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events`); got != 1 {
		t.Fatalf("audit rows after replay = %d, want 1 (no duplicate success)", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM alert_sources`); got != 1 {
		t.Fatalf("sources after replay = %d, want 1", got)
	}
	// The ledger outcome stays committed and its payload carries no secret.
	var outcome, payload string
	if err := db.QueryRow(`SELECT outcome,COALESCE(result_payload_json,'') FROM client_commands WHERE client_command_id='create-audit-0001'`).Scan(&outcome, &payload); err != nil {
		t.Fatal(err)
	}
	if outcome != "committed" {
		t.Fatalf("ledger outcome = %q", outcome)
	}
	if want := `"sourceKey":"checkout-am"`; !contains(payload, want) {
		t.Fatalf("ledger payload %q missing %q", payload, want)
	}

	// Command-key reuse with a different request: deterministic conflict with
	// no new durable trace.
	if _, _, err := service.CreateSource(ctx, "create-audit-0001", "other-am", "alertmanager", digest); !errors.Is(err, execution.ErrCommandReused) {
		t.Fatalf("reused command id must surface ErrCommandReused, got %v", err)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events`); got != 1 {
		t.Fatalf("audit rows after reuse conflict = %d, want 1", got)
	}

	// Duplicate source key under a fresh command id: the UNIQUE violation
	// rolls everything back and surfaces verbatim (the HTTP layer maps it to
	// the conflict problem); no ledger or audit trace remains.
	if _, _, err := service.CreateSource(ctx, "create-audit-0002", "checkout-am", "alertmanager", digest); err == nil {
		t.Fatal("duplicate source key must be rejected")
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events`); got != 1 {
		t.Fatalf("audit rows after duplicate rejection = %d, want 1 (clean rollback leaves no trace)", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM client_commands`); got != 1 {
		t.Fatalf("ledger rows after duplicate rejection = %d, want 1", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// A mid-transaction failure must leave no business row, no ledger row and no
// audit row: the audit/rollback contract of the runner.
func TestCreateSourceRollsBackOnMidTransactionFailure(t *testing.T) {
	service, database, done := newTestService(t)
	defer done()
	db := database.SQL
	ctx := adminCommandContext(t, context.Background())

	// A digest that violates the frozen CHECK (length = 32) fails the
	// credential INSERT after the source INSERT succeeded inside the same
	// runner transaction.
	shortDigest := make([]byte, 31)
	if _, _, err := service.CreateSource(ctx, "create-rollback-0001", "broken-am", "alertmanager", shortDigest); err == nil {
		t.Fatal("short digest must fail the credential insert")
	}
	for name, query := range map[string]string{
		"alert_sources":            `SELECT COUNT(*) FROM alert_sources`,
		"alert_source_credentials": `SELECT COUNT(*) FROM alert_source_credentials`,
		"client_commands":          `SELECT COUNT(*) FROM client_commands`,
		"audit_events":             `SELECT COUNT(*) FROM audit_events`,
	} {
		if got := countRows(t, db, query); got != 0 {
			t.Fatalf("%s rows after rolled-back create = %d, want 0", name, got)
		}
	}
}

// Lifecycle: rotate, retire (including the recorded stale-version rejection
// and its rejection replay) and disable each commit their own fact in the
// same transaction as the domain change.
func TestSourceLifecycleRecordsAuditFactsAndRejections(t *testing.T) {
	service, database, done := newTestService(t)
	defer done()
	db := database.SQL
	ctx := adminCommandContext(t, context.Background())

	created, _, err := service.CreateSource(ctx, "lifecycle-create-0001", "cycle-am", "alertmanager", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	rotated, _, err := service.RotateCredential(ctx, "lifecycle-rotate-0001", "cycle-am", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if rotated.CredentialID == created.CredentialID {
		t.Fatal("rotation must mint a new credential generation")
	}
	if row := scanAuditRow(t, db, `SELECT actor_type,actor_id,action,outcome,phase,correlation_id,domain_ref_type,domain_ref_id,initiator_type,initiator_id FROM audit_events WHERE action=?`, opRotateCredential); row.refType != objectCredential || row.refID != rotated.CredentialID || row.outcome != "success" {
		t.Fatalf("rotate audit row = %+v", row)
	}
	// Only the digest — never the raw bearer — reaches the database.
	if got := countRows(t, db, `SELECT COUNT(*) FROM alert_source_credentials WHERE id=? AND length(digest)=32`, rotated.CredentialID); got != 1 {
		t.Fatal("rotated credential must store a 32-byte digest only")
	}

	// Stale row version: deterministic rejection recorded as a rejected fact
	// with its ledger row; credential untouched.
	stale, _, err := service.RetireCredential(ctx, "lifecycle-retire-stale", "cycle-am", rotated.CredentialID, 99)
	if err == nil || stale.ID != "" {
		t.Fatalf("stale retire = (%+v, %v), want a rejection", stale, err)
	}
	rejected := scanAuditRow(t, db, `SELECT actor_type,actor_id,action,outcome,phase,correlation_id,domain_ref_type,domain_ref_id,initiator_type,initiator_id FROM audit_events WHERE outcome='rejected'`)
	if rejected.action != opRetireCredential || rejected.refID != rotated.CredentialID {
		t.Fatalf("rejected audit row = %+v", rejected)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM client_commands WHERE client_command_id='lifecycle-retire-stale' AND outcome='rejected_known'`); got != 1 {
		t.Fatalf("rejected ledger rows = %d, want 1", got)
	}
	if state := countRows(t, db, `SELECT COUNT(*) FROM alert_source_credentials WHERE id=? AND state='Active'`, rotated.CredentialID); state != 1 {
		t.Fatal("rejected retire must not change the credential state")
	}
	// Replaying the rejected command returns the same stored rejection
	// without a new audit row.
	if _, _, replayErr := service.RetireCredential(ctx, "lifecycle-retire-stale", "cycle-am", rotated.CredentialID, 99); replayErr == nil {
		t.Fatal("replayed rejection must stay rejected")
	} else {
		var sameRejection *execution.Rejection
		if !errors.As(replayErr, &sameRejection) || sameRejection.Code != CodeRowVersionConflict {
			t.Fatalf("replayed rejection = %v, want row_version_conflict", replayErr)
		}
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events`); got != 3 {
		t.Fatalf("audit rows after rejection replay = %d, want 3", got)
	}

	summary, replayed, err := service.RetireCredential(ctx, "lifecycle-retire-0001", "cycle-am", rotated.CredentialID, 1)
	if err != nil || replayed {
		t.Fatalf("retire = (%+v, %v, %v)", summary, replayed, err)
	}
	if summary.ID == "" || summary.State != "Retired" || summary.RetiredAt == nil {
		t.Fatalf("retired summary = %+v", summary)
	}
	// The durable replay returns the identical summary projection.
	replaySummary, replayed, err := service.RetireCredential(ctx, "lifecycle-retire-0001", "cycle-am", rotated.CredentialID, 1)
	if err != nil || !replayed {
		t.Fatalf("retire replay = (%+v, %v, %v), want a replayed %+v", replaySummary, replayed, err, summary)
	}
	if !reflect.DeepEqual(replaySummary, summary) {
		t.Fatalf("replayed summary = %+v, want %+v", replaySummary, summary)
	}

	detail, err := service.GetSource(ctx, "cycle-am")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.SetSourceEnabled(ctx, "lifecycle-disable-stale", "cycle-am", false, detail.RowVersion+3); err == nil {
		t.Fatal("stale disable must be rejected")
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events WHERE action=? AND outcome='rejected'`, opSetSourceEnabled); got != 1 {
		t.Fatalf("stale disable rejection audit rows = %d, want 1", got)
	}
	disabled, replayed, err := service.SetSourceEnabled(ctx, "lifecycle-disable-0001", "cycle-am", false, detail.RowVersion)
	if err != nil || replayed {
		t.Fatalf("disable = (%+v, %v, %v)", disabled, replayed, err)
	}
	if disabled.Enabled || disabled.DisabledAt == nil {
		t.Fatalf("disabled source = %+v", disabled.SourceSummary)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events WHERE action=? AND outcome='success'`, opSetSourceEnabled); got != 1 {
		t.Fatalf("disable success audit rows = %d, want 1", got)
	}
}

// The reveal audit commits inside the runner transaction before the caller
// releases the raw bearer; a missing credential is a recorded rejection and
// releases nothing.
func TestRecordRevealAuditBeforeRelease(t *testing.T) {
	service, database, done := newTestService(t)
	defer done()
	db := database.SQL
	ctx := adminCommandContext(t, context.Background())

	_, credentialID := seedSource(t, service, context.Background(), "reveal-am")
	if err := service.RecordRevealAudit(ctx, credentialID); err != nil {
		t.Fatal(err)
	}
	row := scanAuditRow(t, db, `SELECT actor_type,actor_id,action,outcome,phase,correlation_id,domain_ref_type,domain_ref_id,initiator_type,initiator_id FROM audit_events WHERE action=?`, opRevealCredential)
	if row.actorID != 1 || row.outcome != "success" || row.refType != objectCredential || row.refID != credentialID || row.corrID != "alerts-"+t.Name() {
		t.Fatalf("reveal audit row = %+v", row)
	}

	if err := service.RecordRevealAudit(ctx, credentialID+999); err == nil {
		t.Fatal("unknown credential must be rejected")
	}
	var outcome string
	if err := db.QueryRow(`SELECT outcome FROM audit_events WHERE action=? AND domain_ref_id=?`, opRevealCredential, credentialID+999).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != "rejected" {
		t.Fatalf("unknown-credential reveal audit outcome = %q", outcome)
	}
}

// Fail closed: missing execution metadata, a non-admin principal and a
// revoked session reject the commands with no durable trace.
func TestSourceCommandsFailClosed(t *testing.T) {
	service, database, done := newTestService(t)
	defer done()
	db := database.SQL

	// Missing metadata (integration gap, never "assume system").
	if _, _, err := service.CreateSource(context.Background(), "create-ghost-0001", "ghost-am", "alertmanager", make([]byte, 32)); !errors.Is(err, execution.ErrMissingContext) {
		t.Fatalf("missing metadata must fail with ErrMissingContext, got %v", err)
	}

	// Non-admin principal with its own live session.
	now := "2026-09-14T00:00:00Z"
	idle, absolute := "2036-09-14T00:00:00Z", "2036-09-21T00:00:00Z"
	for _, statement := range []string{
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(2,'op','Op','operator',1,1,'fixture',1,'` + now + `','` + now + `')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(2,2,randomblob(32),1,'fixture','` + now + `','` + now + `','` + idle + `','` + absolute + `')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	operatorCtx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "alerts-operator",
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 2},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-operator"},
		Session:       execution.SessionRef{ID: 2, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.CreateSource(operatorCtx, "create-operator-0001", "ghost-am", "alertmanager", make([]byte, 32)); !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("operator session must surface ErrActorChanged, got %v", err)
	}

	// Session revoked after admission (revocation is terminal in the schema).
	if _, err := db.Exec(`UPDATE sessions SET revoked_at='2026-09-14T01:00:00Z' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	revokedCtx := adminCommandContext(t, context.Background())
	if _, _, err := service.CreateSource(revokedCtx, "create-revoked-0001", "ghost-am", "alertmanager", make([]byte, 32)); !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("revoked session must surface ErrActorChanged, got %v", err)
	}

	// No durable trace from any fail-closed path.
	for name, query := range map[string]string{
		"audit_events":    `SELECT COUNT(*) FROM audit_events`,
		"client_commands": `SELECT COUNT(*) FROM client_commands`,
		"alert_sources":   `SELECT COUNT(*) FROM alert_sources`,
	} {
		if got := countRows(t, db, query); got != 0 {
			t.Fatalf("%s rows after fail-closed commands = %d, want 0", name, got)
		}
	}
}
