package alerts

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// newTestService builds a fresh bootstrap database + alert service in a temp
// dir, returning teardown and the generated root key path.

// webhookBody builds an Alertmanager payload whose fingerprint is the real
// FingerprintOf(labels) — the only payload shape the intake accepts as
// identity-clean (Q215).
func webhookBody(status string, labels map[string]string, startsAt string, extra string) []byte {
	fingerprint := fmt.Sprintf("%x", binary.BigEndian.Uint64(FingerprintOf(labels)))
	return []byte(fmt.Sprintf(`{"status":"firing","alerts":[{"status":%q,"labels":%s,"startsAt":%q,"fingerprint":%q%s}],"truncatedAlerts":0}`, status, mustJSON(labels), startsAt, fingerprint, extra))
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestDeliverFiringCreatesOccurrenceAndObservation(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()

	sourceID, credentialID := seedSource(t, service, ctx, "prod-am")

	body := webhookBody("firing", map[string]string{"alertname": "CPU", "instance": "db-1"}, "2026-08-17T10:00:00Z", `,"annotations":{"summary":"high"}`)
	received := time.Date(2026, 8, 17, 10, 5, 0, 0, time.UTC)
	result, err := service.Deliver(ctx, "relay-1", sourceID, credentialID, 1, body, received)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.Processed != 1 {
		t.Fatalf("result=%+v", result)
	}
	var occurrenceCount int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_occurrences WHERE source_id=? AND state='Firing'`, sourceID).Scan(&occurrenceCount); err != nil || occurrenceCount != 1 {
		t.Fatalf("occurrence count=%d err=%v", occurrenceCount, err)
	}
	var observationCount int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_observations`).Scan(&observationCount); err != nil || observationCount != 1 {
		t.Fatalf("observation count=%d err=%v", observationCount, err)
	}
	var deliveryCount int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_deliveries`).Scan(&deliveryCount); err != nil || deliveryCount != 1 {
		t.Fatalf("delivery count=%d err=%v", deliveryCount, err)
	}
}

func TestDeliverRelayReplayIsIdempotent(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "replay-am")

	body := webhookBody("firing", map[string]string{"alertname": "CPU"}, "2026-08-17T10:00:00Z", "")
	received := time.Now().UTC()
	first, err := service.Deliver(ctx, "relay-same", sourceID, credentialID, 1, body, received)
	if err != nil || !first.Accepted {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := service.Deliver(ctx, "relay-same", sourceID, credentialID, 1, body, received)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Accepted || second.DeliveryID != first.DeliveryID {
		t.Fatalf("replay must return the original delivery id: first=%+v second=%+v", first, second)
	}
	var deliveryCount int
	_ = database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_deliveries`).Scan(&deliveryCount)
	if deliveryCount != 1 {
		t.Fatalf("replay duplicated the delivery: count=%d", deliveryCount)
	}
	var observationCount int
	_ = database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_observations`).Scan(&observationCount)
	if observationCount != 1 {
		t.Fatalf("replay duplicated observations: count=%d", observationCount)
	}
}

func TestResolvedClosesOccurrenceWithoutReopen(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "cycle-am")

	firing := webhookBody("firing", map[string]string{"alertname": "CPU"}, "2026-08-17T10:00:00Z", "")
	resolved := webhookBody("resolved", map[string]string{"alertname": "CPU"}, "2026-08-17T10:00:00Z", `,"endsAt":"2026-08-17T10:30:00Z"`)
	lateFiring := webhookBody("firing", map[string]string{"alertname": "CPU"}, "2026-08-17T10:00:00Z", "")

	received := time.Now().UTC()
	if result, err := service.Deliver(ctx, "relay-1", sourceID, credentialID, 1, firing, received); err != nil || !result.Accepted {
		t.Fatalf("firing err=%v result=%+v", err, result)
	}
	if result, err := service.Deliver(ctx, "relay-2", sourceID, credentialID, 1, resolved, received); err != nil || !result.Accepted {
		t.Fatalf("resolved err=%v result=%+v", err, result)
	}
	var state string
	if err := database.SQL.QueryRowContext(ctx, `SELECT state FROM alert_occurrences`).Scan(&state); err != nil || state != "Resolved" {
		t.Fatalf("state=%s err=%v", state, err)
	}
	// Late firing must not reopen.
	if result, err := service.Deliver(ctx, "relay-3", sourceID, credentialID, 1, lateFiring, received); err != nil || !result.Accepted {
		t.Fatalf("late err=%v result=%+v", err, result)
	}
	if err := database.SQL.QueryRowContext(ctx, `SELECT state FROM alert_occurrences`).Scan(&state); err != nil || state != "Resolved" {
		t.Fatalf("late firing reopened occurrence: state=%s", state)
	}
	var effectCount int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_observations WHERE effect='late_firing_after_resolved'`).Scan(&effectCount); err != nil || effectCount != 1 {
		t.Fatalf("late observation missing: count=%d err=%v", effectCount, err)
	}
}

func TestRejectedWebhookNeverTouchesOccurrences(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "reject-am")

	result, err := service.Deliver(ctx, "relay-bad", sourceID, credentialID, 1, []byte(`not json`), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Rejected {
		t.Fatalf("expected rejected, got %+v", result)
	}
	var occurrenceCount int
	_ = database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_occurrences`).Scan(&occurrenceCount)
	if occurrenceCount != 0 {
		t.Fatalf("rejected delivery must not create occurrences: %d", occurrenceCount)
	}
	var rejectedCount int
	_ = database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_deliveries WHERE status='rejected'`).Scan(&rejectedCount)
	if rejectedCount != 1 {
		t.Fatalf("rejected delivery not recorded: %d", rejectedCount)
	}
}

func TestIdentityConflictRecordsIntakeIssue(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "conflict-am")

	// Construct the anomaly honestly: a stored occurrence whose label set
	// diverged from the fingerprint identity (the exact condition
	// DATA-ALERT-004 detects). The incoming delivery carries labels B whose
	// own fingerprint equals the stored fingerprint, so the payload
	// cross-check passes and only the stored-snapshot comparison fires.
	labelsB := map[string]string{"alertname": "CPU", "instance": "b"}
	canonicalB, err := CanonicalLabels(labelsB)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := FingerprintOf(labelsB)
	committed := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.SQL.ExecContext(ctx, `INSERT INTO alert_occurrences(source_id, fingerprint, starts_at, state, row_version, labels_canonical, labels_digest, first_seen_at, last_state_change_at) VALUES(?,?,?,?,1,?,?,?,?)`,
		sourceID, fingerprint, "2026-08-17T10:00:00Z", "Firing", `{"alertname":"CPU","instance":"a"}`, DigestLabels(`{"alertname":"CPU","instance":"a"}`), committed, committed); err != nil {
		t.Fatal(err)
	}

	conflicting := webhookBody("firing", labelsB, "2026-08-17T10:00:00Z", "")
	received := time.Now().UTC()
	// Same triple (source+fingerprint+startsAt) but different labels than the
	// stored snapshot: identity_conflict must be recorded, the stored
	// snapshot must stay untouched.
	if result, err := service.Deliver(ctx, "relay-2", sourceID, credentialID, 1, conflicting, received); err != nil || !result.Accepted {
		t.Fatalf("conflict err=%v result=%+v", err, result)
	}
	var issueCount int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_intake_issues WHERE kind='identity_conflict'`).Scan(&issueCount); err != nil || issueCount != 1 {
		t.Fatalf("identity conflict issue not recorded: %d err=%v", issueCount, err)
	}
	var storedLabels string
	_ = database.SQL.QueryRowContext(ctx, `SELECT labels_canonical FROM alert_occurrences`).Scan(&storedLabels)
	if storedLabels != `{"alertname":"CPU","instance":"a"}` {
		t.Fatalf("conflicting labels must never overwrite the snapshot: %s", storedLabels)
	}
	_ = canonicalB
}

func TestResolvedFirstCreatesClosedOccurrence(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "resolved-first-am")

	// A resolved-only delivery (Alertmanager fires only status:"resolved") must
	// create the occurrence already closed with a resolved_first observation
	// (DATA-ALERT-006); the schema CHECK rejects observed_state='resolved'
	// with any effect other than resolved/resolved_first.
	body := webhookBody("resolved", map[string]string{"alertname": "CPU"}, "2026-08-17T10:00:00Z", `,"endsAt":"2026-08-17T10:30:00Z"`)
	received := time.Now().UTC()
	result, err := service.Deliver(ctx, "relay-resolved-first", sourceID, credentialID, 1, body, received)
	if err != nil || !result.Accepted || result.Processed != 1 {
		t.Fatalf("resolved-first err=%v result=%+v", err, result)
	}
	var state, resolvedAt string
	if err := database.SQL.QueryRowContext(ctx, `SELECT state, resolved_at FROM alert_occurrences`).Scan(&state, &resolvedAt); err != nil || state != "Resolved" || resolvedAt == "" {
		t.Fatalf("state=%s resolvedAt=%q err=%v", state, resolvedAt, err)
	}
	var effect string
	if err := database.SQL.QueryRowContext(ctx, `SELECT effect FROM alert_observations`).Scan(&effect); err != nil || effect != "resolved_first" {
		t.Fatalf("effect=%s err=%v", effect, err)
	}
	// The closed occurrence must not appear in the Firing snapshot.
	snapshot, err := service.AlertSnapshot(ctx)
	if err != nil || len(snapshot.Items) != 0 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}

func TestIntakeIssueRepeatAdvancesAggregate(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "repeat-am")

	// Two deliveries with the same identity_conflict signature must advance
	// the open aggregate: last_event_id set, occurrence_count+1, row_version+1
	// (trg_alert_intake_issues_repeat_update rejects a bare UPDATE).
	labelsB := map[string]string{"alertname": "CPU", "instance": "b"}
	fingerprint := FingerprintOf(labelsB)
	committed := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.SQL.ExecContext(ctx, `INSERT INTO alert_occurrences(source_id, fingerprint, starts_at, state, row_version, labels_canonical, labels_digest, first_seen_at, last_state_change_at) VALUES(?,?,?,?,1,?,?,?,?)`,
		sourceID, fingerprint, "2026-08-17T10:00:00Z", "Firing", `{"alertname":"CPU","instance":"a"}`, DigestLabels(`{"alertname":"CPU","instance":"a"}`), committed, committed); err != nil {
		t.Fatal(err)
	}
	conflicting := webhookBody("firing", labelsB, "2026-08-17T10:00:00Z", "")
	received := time.Now().UTC()
	for _, relay := range []string{"relay-r1", "relay-r2"} {
		result, err := service.Deliver(ctx, relay, sourceID, credentialID, 1, conflicting, received)
		if err != nil || !result.Accepted {
			t.Fatalf("%s err=%v result=%+v", relay, err, result)
		}
	}
	var count, rowVersion int64
	var lastEventID any
	if err := database.SQL.QueryRowContext(ctx, `SELECT occurrence_count, row_version, last_event_id FROM alert_intake_issues WHERE kind='identity_conflict'`).Scan(&count, &rowVersion, &lastEventID); err != nil {
		t.Fatal(err)
	}
	if count != 2 || rowVersion != 2 || lastEventID == nil {
		t.Fatalf("aggregate did not advance: count=%d rowVersion=%d lastEventID=%v", count, rowVersion, lastEventID)
	}
	var events int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_intake_issue_events`).Scan(&events); err != nil || events != 2 {
		t.Fatalf("events=%d err=%v", events, err)
	}
}

func TestRevokedCredentialRejectsDelivery(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "revoke-am")

	// Q217: a credential revoked before the delivery commits must reject the
	// delivery and create no delivery row (commit-order adjudication).
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.SQL.ExecContext(ctx, `UPDATE alert_source_credentials SET state='Retired', retired_at=?, row_version=row_version+1 WHERE id=?`, now, credentialID); err != nil {
		t.Fatal(err)
	}
	body := webhookBody("firing", map[string]string{"alertname": "CPU"}, "2026-08-17T10:00:00Z", "")
	result, err := service.Deliver(ctx, "relay-revoked", sourceID, credentialID, 1, body, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Rejected || result.DeliveryID != 0 {
		t.Fatalf("expected rejected with no delivery, got %+v", result)
	}
	var deliveryCount int
	_ = database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_deliveries`).Scan(&deliveryCount)
	if deliveryCount != 0 {
		t.Fatalf("revoked delivery must not create a delivery row: %d", deliveryCount)
	}
}

func TestTruncatedDeliveryFlagsIntegrity(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "truncate-am")

	// DATA-ALERT-003: truncatedAlerts>0 keeps the included items processed but
	// flags the delivery integrity='truncated' and raises a delivery_truncated
	// intake issue; no lifecycle inference for the missing items.
	body := []byte(`{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"CPU"},"startsAt":"2026-08-17T10:00:00Z","fingerprint":"` + fmt.Sprintf("%x", binary.BigEndian.Uint64(FingerprintOf(map[string]string{"alertname": "CPU"}))) + `"}],"truncatedAlerts":3}`)
	result, err := service.Deliver(ctx, "relay-truncated", sourceID, credentialID, 1, body, time.Now().UTC())
	if err != nil || !result.Accepted {
		t.Fatalf("truncated err=%v result=%+v", err, result)
	}
	var integrity string
	if err := database.SQL.QueryRowContext(ctx, `SELECT integrity FROM alert_deliveries`).Scan(&integrity); err != nil || integrity != "truncated" {
		t.Fatalf("integrity=%s err=%v", integrity, err)
	}
	var truncatedIssues int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_intake_issues WHERE kind='delivery_truncated'`).Scan(&truncatedIssues); err != nil || truncatedIssues != 1 {
		t.Fatalf("delivery_truncated issue missing: %d err=%v", truncatedIssues, err)
	}
	// The included item was still processed.
	var occurrences int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_occurrences`).Scan(&occurrences); err != nil || occurrences != 1 {
		t.Fatalf("occurrences=%d err=%v", occurrences, err)
	}
}

func TestFingerprintMismatchDoesNotBlockNormalItems(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "mismatch-am")

	// Q215: a well-formed hex fingerprint that cannot be reproduced from the
	// attached labels is a mismatch — recorded as an intake issue while the
	// sibling normal item still processes in the same delivery.
	bad := []byte(`{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"DB"},"startsAt":"2026-08-17T10:00:00Z","fingerprint":"0011223344556677"}],"truncatedAlerts":0}`)
	// Same-delivery mixed path: one good and one bad item in a single payload;
	// the mismatch must not block the normal item (DATA-ALERT-002).
	mixed := []byte(`{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"CPU"},"startsAt":"2026-08-17T10:00:00Z","fingerprint":"` + fmt.Sprintf("%x", binary.BigEndian.Uint64(FingerprintOf(map[string]string{"alertname": "CPU"}))) + `"},{"status":"firing","labels":{"alertname":"DB"},"startsAt":"2026-08-17T10:00:00Z","fingerprint":"0011223344556677"}],"truncatedAlerts":0}`)
	result, err := service.Deliver(ctx, "relay-mixed", sourceID, credentialID, 1, mixed, time.Now().UTC())
	if err != nil || !result.Accepted || result.Processed != 1 {
		t.Fatalf("mixed err=%v result=%+v", err, result)
	}
	// A second bad delivery aggregates onto the same mismatch issue key.
	result, err = service.Deliver(ctx, "relay-bad", sourceID, credentialID, 1, bad, time.Now().UTC())
	if err != nil || !result.Accepted {
		t.Fatalf("bad err=%v result=%+v", err, result)
	}
	var mismatchIssues int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_intake_issues WHERE kind='fingerprint_mismatch'`).Scan(&mismatchIssues); err != nil || mismatchIssues != 1 {
		t.Fatalf("fingerprint_mismatch issue missing: %d err=%v", mismatchIssues, err)
	}
	var occurrenceCount int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_occurrences`).Scan(&occurrenceCount); err != nil || occurrenceCount != 1 {
		t.Fatalf("occurrences=%d err=%v", occurrenceCount, err)
	}
}
