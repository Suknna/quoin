package alerts

// Intake-issue acknowledgment coverage (ADR-0006): the admin acknowledgment
// runs through the shared runner — the automatic audit carries the verified
// administrator and the operation correlation, deterministic conflicts are
// recorded as rejected facts that change nothing, and every fail-closed
// identity case leaves no durable trace.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// seedOpenIntakeIssue opens one real identity_conflict issue by relaying a
// conflicting webhook, the same anomaly shape TestIdentityConflictRecords-
// IntakeIssue constructs.
func seedOpenIntakeIssue(t *testing.T, service *Service, ctx context.Context, sourceID, credentialID int64) int64 {
	t.Helper()
	labelsB := map[string]string{"alertname": "CPU", "instance": "b"}
	fingerprint := FingerprintOf(labelsB)
	committed := time.Now().UTC().Format(time.RFC3339Nano)
	database := service.db
	if _, err := database.ExecContext(ctx, `INSERT INTO alert_occurrences(source_id, fingerprint, starts_at, state, row_version, labels_canonical, labels_digest, first_seen_at, last_state_change_at) VALUES(?,?,?,?,1,?,?,?,?)`,
		sourceID, fingerprint, "2026-08-17T10:00:00Z", "Firing", `{"alertname":"CPU","instance":"a"}`, DigestLabels(`{"alertname":"CPU","instance":"a"}`), committed, committed); err != nil {
		t.Fatal(err)
	}
	conflicting := webhookBody("firing", labelsB, "2026-08-17T10:00:00Z", "")
	result, err := service.Deliver(ctx, "relay-ack-seed", sourceID, credentialID, 1, conflicting, time.Now().UTC())
	if err != nil || !result.Accepted {
		t.Fatalf("seed delivery err=%v result=%+v", err, result)
	}
	var issueID int64
	if err := database.QueryRowContext(ctx, `SELECT id FROM alert_intake_issues WHERE kind='identity_conflict' AND acknowledged_at IS NULL`).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	return issueID
}

func TestAcknowledgeIntakeIssueAuditsAndRecordsConflicts(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	db := database.SQL
	sourceID, credentialID := seedSource(t, service, context.Background(), "ack-am")
	issueID := seedOpenIntakeIssue(t, service, context.Background(), sourceID, credentialID)
	ctx := adminCommandContext(t, context.Background())

	var rowVersion int64
	if err := db.QueryRowContext(context.Background(), `SELECT row_version FROM alert_intake_issues WHERE id=?`, issueID).Scan(&rowVersion); err != nil {
		t.Fatal(err)
	}
	applied, err := service.AcknowledgeIntakeIssue(ctx, issueID, 1, rowVersion, "2026-09-15T00:00:00Z")
	if err != nil || !applied {
		t.Fatalf("acknowledge = (%v, %v)", applied, err)
	}
	row := scanAuditRow(t, db, `SELECT actor_type,actor_id,action,outcome,phase,correlation_id,domain_ref_type,domain_ref_id,initiator_type,initiator_id FROM audit_events WHERE action=?`, opAcknowledgeIntake)
	if row.actorType != "user" || row.actorID != 1 || row.action != opAcknowledgeIntake || row.outcome != "success" ||
		row.corrID != "alerts-"+t.Name() || row.refType != objectIntakeIssue || row.refID != issueID || row.initiator != "user" || row.initiatorID != 1 {
		t.Fatalf("acknowledge audit row = %+v", row)
	}
	var acknowledgedBy int64
	if err := db.QueryRowContext(context.Background(), `SELECT acknowledged_by FROM alert_intake_issues WHERE id=?`, issueID).Scan(&acknowledgedBy); err != nil || acknowledgedBy != 1 {
		t.Fatalf("acknowledged_by=%d err=%v", acknowledgedBy, err)
	}

	// The acknowledgment is sticky: a repeat (same or stale version) is a
	// recorded deterministic rejection that changes nothing.
	applied, err = service.AcknowledgeIntakeIssue(ctx, issueID, 1, rowVersion, "2026-09-15T00:00:01Z")
	if err != nil || applied {
		t.Fatalf("repeat acknowledge = (%v, %v), want (false, nil)", applied, err)
	}
	rejected := scanAuditRow(t, db, `SELECT actor_type,actor_id,action,outcome,phase,correlation_id,domain_ref_type,domain_ref_id,initiator_type,initiator_id FROM audit_events WHERE outcome='rejected' AND action=?`, opAcknowledgeIntake)
	if rejected.action != opAcknowledgeIntake || rejected.outcome != "rejected" || rejected.refID != issueID || rejected.actorID != 1 {
		t.Fatalf("repeat acknowledge audit row = %+v", rejected)
	}
	var acknowledgedAt string
	if err := db.QueryRowContext(context.Background(), `SELECT acknowledged_at FROM alert_intake_issues WHERE id=?`, issueID).Scan(&acknowledgedAt); err != nil || acknowledgedAt != "2026-09-15T00:00:00Z" {
		t.Fatalf("sticky acknowledgment drifted: %q err=%v", acknowledgedAt, err)
	}

	// A stale expectedRowVersion on an open issue is likewise a recorded
	// rejection, and the correct version still applies afterwards.
	secondIssue := seedOpenIntakeIssueStandalone(t, service, sourceID)
	applied, err = service.AcknowledgeIntakeIssue(ctx, secondIssue, 1, 99, "2026-09-15T00:00:02Z")
	if err != nil || applied {
		t.Fatalf("stale acknowledge = (%v, %v), want (false, nil)", applied, err)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events WHERE outcome='rejected' AND domain_ref_id=?`, secondIssue); got != 1 {
		t.Fatalf("stale acknowledge rejection audit rows = %d, want 1", got)
	}
	applied, err = service.AcknowledgeIntakeIssue(ctx, secondIssue, 1, 1, "2026-09-15T00:00:03Z")
	if err != nil || !applied {
		t.Fatalf("fresh acknowledge = (%v, %v)", applied, err)
	}
}

// seedOpenIntakeIssueStandalone opens a second identity_conflict issue under
// a fresh relay id so the per-issue assertions stay isolated.
func seedOpenIntakeIssueStandalone(t *testing.T, service *Service, sourceID int64) int64 {
	t.Helper()
	labelsB := map[string]string{"alertname": "CPU", "instance": "b"}
	fingerprint := FingerprintOf(labelsB)
	committed := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := service.db.ExecContext(context.Background(), `INSERT INTO alert_occurrences(source_id, fingerprint, starts_at, state, row_version, labels_canonical, labels_digest, first_seen_at, last_state_change_at) VALUES(?,?,?,?,1,?,?,?,?)`,
		sourceID, fingerprint, "2026-08-17T11:00:00Z", "Firing", `{"alertname":"CPU","instance":"a"}`, DigestLabels(`{"alertname":"CPU","instance":"a"}`), committed, committed); err != nil {
		t.Fatal(err)
	}
	conflicting := webhookBody("firing", labelsB, "2026-08-17T11:00:00Z", "")
	result, err := service.Deliver(context.Background(), "relay-ack-seed-2", sourceID, service.sourceCredentialID(t, sourceID), 1, conflicting, time.Now().UTC())
	if err != nil || !result.Accepted {
		t.Fatalf("seed delivery err=%v result=%+v", err, result)
	}
	var issueID int64
	if err := service.db.QueryRowContext(context.Background(), `SELECT id FROM alert_intake_issues WHERE kind='identity_conflict' AND acknowledged_at IS NULL ORDER BY id DESC LIMIT 1`).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	return issueID
}

// sourceCredentialID resolves one Active credential of the seeded source.
func (service *Service) sourceCredentialID(t *testing.T, sourceID int64) int64 {
	t.Helper()
	var credentialID int64
	if err := service.db.QueryRowContext(context.Background(), `SELECT id FROM alert_source_credentials WHERE source_id=? AND state='Active' ORDER BY id DESC LIMIT 1`, sourceID).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	return credentialID
}

// Fail closed: missing execution metadata, a mismatched actor parameter and a
// non-admin session reject the acknowledgment without any durable trace.
func TestAcknowledgeIntakeIssueFailClosed(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	db := database.SQL
	sourceID, credentialID := seedSource(t, service, context.Background(), "ackfail-am")
	issueID := seedOpenIntakeIssue(t, service, context.Background(), sourceID, credentialID)

	// Missing metadata (integration gap, never "assume system").
	if _, err := service.AcknowledgeIntakeIssue(context.Background(), issueID, 1, 1, "2026-09-15T00:00:00Z"); !errors.Is(err, execution.ErrMissingContext) {
		t.Fatalf("missing metadata must surface ErrMissingContext, got %v", err)
	}

	// The caller-supplied actor must match the verified metadata principal.
	ctx := adminCommandContext(t, context.Background())
	if _, err := service.AcknowledgeIntakeIssue(ctx, issueID, 2, 1, "2026-09-15T00:00:00Z"); err == nil {
		t.Fatal("mismatched actor parameter must be refused")
	}

	// A live non-admin session fails the in-transaction role re-check.
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
		CorrelationID: "alerts-ack-operator",
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 2},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-ack-operator"},
		Session:       execution.SessionRef{ID: 2, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcknowledgeIntakeIssue(operatorCtx, issueID, 2, 1, "2026-09-15T00:00:00Z"); err == nil {
		t.Fatal("operator acknowledge must fail the admin role re-check")
	}

	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events WHERE action=?`, opAcknowledgeIntake); got != 0 {
		t.Fatalf("acknowledge audit rows after fail-closed attempts = %d, want 0", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM alert_intake_issues WHERE acknowledged_at IS NOT NULL`); got != 0 {
		t.Fatalf("acknowledged issues after fail-closed acknowledgments = %d, want 0", got)
	}
}

// 凭据吊销后的业务拒绝按 ADR-0011 落入 credential_denied 接入问题：拒绝
// 不产生 delivery 行，但来源级问题必须可见、可聚合计数、可确认，确认后
// 再次发生会重新出现。
func TestCredentialDeniedRecordsIntakeIssue(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	db := database.SQL
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "denied-am")
	// 吊销凭据（触发 Q217 的提交顺序裁决：其后的投递确定性拒绝）。
	if _, _, err := service.RetireCredential(adminCommandContext(t, ctx), "denied-retire-1", "denied-am", credentialID, 1); err != nil {
		t.Fatal(err)
	}

	body := webhookBody("firing", map[string]string{"alertname": "CPU", "instance": "a"}, "2026-09-17T10:00:00Z", "")
	result, err := service.Deliver(ctx, "relay-denied-1", sourceID, credentialID, 1, body, time.Now().UTC())
	if err != nil || !result.Rejected {
		t.Fatalf("revoked credential delivery = (%+v, %v), want a deterministic rejection", result, err)
	}
	// 拒绝不创建 delivery 行（Q217）。
	if got := countRows(t, db, `SELECT COUNT(*) FROM alert_deliveries WHERE event_id='relay-denied-1'`); got != 0 {
		t.Fatalf("rejected delivery rows = %d, want 0", got)
	}
	// 但接入问题必须落账：来源级、无 delivery/item 闭合、未确认、首次计数 1。
	var issueID, count int64
	var kind, detail string
	if err := db.QueryRow(`SELECT id, kind, occurrence_count, detail_json FROM alert_intake_issues WHERE source_id=? AND acknowledged_at IS NULL`, sourceID).
		Scan(&issueID, &kind, &count, &detail); err != nil {
		t.Fatalf("credential_denied issue missing: %v", err)
	}
	if kind != "credential_denied" || count != 1 {
		t.Fatalf("issue=(%s,%d), want (credential_denied,1)", kind, count)
	}
	var deliveryRef, itemRef any
	if err := db.QueryRow(`SELECT delivery_id, delivery_item_id FROM alert_intake_issues WHERE id=?`, issueID).Scan(&deliveryRef, &itemRef); err != nil {
		t.Fatal(err)
	}
	if deliveryRef != nil || itemRef != nil {
		t.Fatalf("source-level issue must not link a delivery/item: %v/%v", deliveryRef, itemRef)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM alert_intake_issue_events WHERE issue_id=?`, issueID); got != 1 {
		t.Fatalf("issue events = %d, want 1", got)
	}

	// 同一被拒凭据的重复拒绝沿开放签名聚合计数。
	result, err = service.Deliver(ctx, "relay-denied-2", sourceID, credentialID, 1, body, time.Now().UTC())
	if err != nil || !result.Rejected {
		t.Fatalf("second rejected delivery = (%+v, %v)", result, err)
	}
	if err := db.QueryRow(`SELECT occurrence_count FROM alert_intake_issues WHERE id=?`, issueID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("occurrence_count=%d, want 2 after the second rejection", count)
	}

	// 确认不删除历史；再次拒绝重新出现新问题。
	var rowVersion int64
	if err := db.QueryRow(`SELECT row_version FROM alert_intake_issues WHERE id=?`, issueID).Scan(&rowVersion); err != nil {
		t.Fatal(err)
	}
	applied, err := service.AcknowledgeIntakeIssue(adminCommandContext(t, ctx), issueID, 1, rowVersion, "2026-09-17T11:00:00Z")
	if err != nil || !applied {
		t.Fatalf("acknowledge = (%v, %v)", applied, err)
	}
	result, err = service.Deliver(ctx, "relay-denied-3", sourceID, credentialID, 1, body, time.Now().UTC())
	if err != nil || !result.Rejected {
		t.Fatalf("third rejected delivery = (%+v, %v)", result, err)
	}
	var openID int64
	if err := db.QueryRow(`SELECT id FROM alert_intake_issues WHERE source_id=? AND kind='credential_denied' AND acknowledged_at IS NULL`, sourceID).Scan(&openID); err != nil {
		t.Fatalf("a fresh open issue must reappear after acknowledgement: %v", err)
	}
	if openID == issueID {
		t.Fatal("acknowledged issue must not reopen in place")
	}
}
