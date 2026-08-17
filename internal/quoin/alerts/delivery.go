package alerts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DeliveryResult is the authoritative per-delivery outcome returned to Stele.
type DeliveryResult struct {
	Accepted    bool
	Rejected    bool
	Unavailable bool
	Detail      string
	DeliveryID  int64
	Processed   int
	Issues      []IntakeIssueRef
	Occurrences []OccurrenceRef
	Status      string // accepted|rejected|unavailable (metrics label)
}

// IntakeIssueRef is a non-secret, machine-stable reference to one intake
// issue aggregate created or extended by a delivery.
type IntakeIssueRef struct {
	Kind            string `json:"kind"`
	IssueKey        string `json:"issueKey"`
	OccurrenceCount int    `json:"occurrenceCount"`
}

// OccurrenceRef identifies an occurrence affected by a delivery.
type OccurrenceRef struct {
	ID         int64  `json:"id"`
	State      string `json:"state"`
	RowVersion int64  `json:"rowVersion"`
}

// prepared is a single alert item after preflight: identity computed, status
// ok|identity_conflict|fingerprint_mismatch decided.
type prepared struct {
	index        int
	status       string // ok|identity_conflict|fingerprint_mismatch (alert_delivery_items.status)
	itemStatus   string // wire item status firing|resolved (observed_state)
	fingerprint  []byte
	startsAt     string
	endsAt       string
	labels       map[string]string
	canonical    string
	digest       string
	storedDigest string // identity_conflict: digest of the stored occurrence snapshot
	reason       string
	ok           bool
}

// Deliver processes one relayed webhook inside a single SQLite transaction:
// Delivery row, per-item results, Occurrence/Observation updates, intake
// issues, first-use credential marking, and audit all commit or none
// (DATA-ALERT-001/002, RUNTIME-STELE-003/004/005).
func (service *Service) Deliver(ctx context.Context, relayID string, sourceID, credentialID int64, snapshotVersion uint64, body []byte, receivedAt time.Time) (DeliveryResult, error) {
	webhook, err := ParseWebhook(body)
	if err != nil {
		if rejectErr := service.recordRejected(ctx, relayID, sourceID, credentialID, snapshotVersion, body, receivedAt); rejectErr != nil {
			return DeliveryResult{}, rejectErr
		}
		return DeliveryResult{Rejected: true, Status: "rejected", Detail: "webhook body is not valid Alertmanager JSON"}, nil
	}

	conn, err := service.db.Conn(ctx)
	if err != nil {
		return DeliveryResult{Unavailable: true, Status: "unavailable"}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return DeliveryResult{Unavailable: true, Status: "unavailable"}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	var enabled int
	var credentialState string
	err = conn.QueryRowContext(ctx, `SELECT s.enabled, c.state FROM alert_sources s JOIN alert_source_credentials c ON c.source_id = s.id AND c.id = ? WHERE s.id = ?`, credentialID, sourceID).Scan(&enabled, &credentialState)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (enabled != 1 || (credentialState != "Active" && credentialState != "PendingRetirement"))) {
		// Q217: the revocation race is adjudicated by SQLite commit order — a
		// credential revoked before this delivery commits is rejected here,
		// and no delivery row is created (RUNTIME-STELE-005).
		return DeliveryResult{Rejected: true, Status: "rejected", Detail: "credential or source is not currently accepted"}, nil
	}
	if err != nil {
		return DeliveryResult{Unavailable: true, Status: "unavailable"}, err
	}

	now := time.Now().UTC()
	committedAt := now.Format(time.RFC3339Nano)
	integrity := "complete"
	if webhook.TruncatedAlerts > 0 {
		integrity = "truncated"
	}
	result, err := conn.ExecContext(ctx, `INSERT INTO alert_deliveries(relay_id, source_id, credential_id, credential_snapshot_version, protocol, body, body_size_bytes, integrity, status, group_key, received_at, committed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		relayID, sourceID, credentialID, snapshotVersion, "alertmanager", body, len(body), integrity, "processed", webhook.GroupKey, receivedAt.UTC().Format(time.RFC3339Nano), committedAt)
	if err != nil {
		if isUniqueViolation(err) {
			var existingID int64
			if lookupErr := conn.QueryRowContext(ctx, `SELECT id FROM alert_deliveries WHERE relay_id = ?`, relayID).Scan(&existingID); lookupErr == nil {
				return DeliveryResult{Accepted: true, Status: "accepted", Detail: "duplicate relay; already committed", DeliveryID: existingID}, nil
			}
		}
		return DeliveryResult{Unavailable: true, Status: "unavailable"}, fmt.Errorf("persist delivery: %w", err)
	}
	deliveryID, err := result.LastInsertId()
	if err != nil {
		return DeliveryResult{Unavailable: true, Status: "unavailable"}, err
	}

	preparedItems := make([]prepared, 0, len(webhook.Alerts))
	for index, rawItem := range webhook.Alerts {
		item, prepErr := prepareItem(index, rawItem)
		if prepErr != nil {
			item.reason = prepErr.Error()
			item.status = "fingerprint_mismatch"
		}
		preparedItems = append(preparedItems, item)
	}

	processed := 0
	occurrences := []OccurrenceRef{}
	issues := []IntakeIssueRef{}
	for index := range preparedItems {
		item := &preparedItems[index]
		if item.status != "ok" {
			itemID, insertErr := insertDeliveryItem(ctx, conn, deliveryID, item.index, item.status, item.fingerprint, item.startsAt, item.endsAt, item.labels, item.reason)
			if insertErr != nil {
				return DeliveryResult{Unavailable: true, Status: "unavailable"}, insertErr
			}
			issueRef, issueErr := service.recordIssue(ctx, conn, sourceID, deliveryID, &itemID, *item, committedAt)
			if issueErr != nil {
				return DeliveryResult{Unavailable: true, Status: "unavailable"}, issueErr
			}
			issues = append(issues, issueRef)
			continue
		}
		finalStatus, itemID, insertErr := service.classifyAndInsertItem(ctx, conn, sourceID, deliveryID, item, receivedAt, committedAt)
		if insertErr != nil {
			return DeliveryResult{Unavailable: true, Status: "unavailable"}, insertErr
		}
		if finalStatus != "ok" {
			issueRef, issueErr := service.recordIssue(ctx, conn, sourceID, deliveryID, &itemID, *item, committedAt)
			if issueErr != nil {
				return DeliveryResult{Unavailable: true, Status: "unavailable"}, issueErr
			}
			issues = append(issues, issueRef)
			continue
		}
		occurrence, _, err := service.applyItem(ctx, conn, sourceID, *item, itemID, deliveryID, receivedAt, committedAt)
		if err != nil {
			return DeliveryResult{Unavailable: true, Status: "unavailable"}, err
		}
		processed++
		if occurrence != nil {
			occurrences = append(occurrences, *occurrence)
		}
	}

	if webhook.TruncatedAlerts > 0 {
		// DATA-ALERT-003: truncated deliveries are permanently flagged on the
		// Delivery (integrity='truncated') and raise one delivery_truncated
		// intake issue per delivery; no lifecycle inference is made for the
		// unknown missing items.
		issueRef, issueErr := service.recordTruncatedIssue(ctx, conn, sourceID, deliveryID, committedAt)
		if issueErr != nil {
			return DeliveryResult{Unavailable: true, Status: "unavailable"}, issueErr
		}
		issues = append(issues, issueRef)
	}

	if err := service.recordAudit(ctx, conn, "service", 0, "alert.delivery", "success", "alert_delivery", deliveryID, committedAt); err != nil {
		return DeliveryResult{Unavailable: true, Status: "unavailable"}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return DeliveryResult{Unavailable: true, Status: "unavailable"}, err
	}
	committed = true

	return DeliveryResult{
		Accepted: true, Status: "accepted", DeliveryID: deliveryID,
		Processed: processed, Issues: issues, Occurrences: occurrences,
	}, nil
}

func (service *Service) recordRejected(ctx context.Context, relayID string, sourceID, credentialID int64, snapshotVersion uint64, body []byte, receivedAt time.Time) error {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := conn.ExecContext(ctx, `INSERT INTO alert_deliveries(relay_id, source_id, credential_id, credential_snapshot_version, protocol, body, body_size_bytes, integrity, status, received_at, committed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		relayID, sourceID, credentialID, snapshotVersion, "alertmanager", body, len(body), "rejected", "rejected", receivedAt.UTC().Format(time.RFC3339Nano), now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil
		}
		return fmt.Errorf("persist rejected delivery: %w", err)
	}
	deliveryID, _ := result.LastInsertId()
	if err := service.recordAudit(ctx, conn, "service", 0, "alert.delivery_rejected", "success", "alert_delivery", deliveryID, now); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}

func prepareItem(index int, rawItem struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	Fingerprint  string            `json:"fingerprint"`
	GeneratorURL string            `json:"generatorURL"`
}) (prepared, error) {
	wireStatus := rawItem.Status
	if wireStatus != "firing" && wireStatus != "resolved" {
		wireStatus = "firing"
	}
	item := prepared{index: index, labels: rawItem.Labels, status: "ok", itemStatus: wireStatus, ok: true}
	normalized, err := NormalizeStartsAt(rawItem.StartsAt)
	if err != nil {
		item.status = "fingerprint_mismatch"
		item.reason = err.Error()
		return item, err
	}
	item.startsAt = normalized
	item.endsAt = rawItem.EndsAt
	if rawItem.Fingerprint != "" {
		declared, err := FingerprintFromHex(rawItem.Fingerprint)
		if err == nil {
			// Q215: the declared fingerprint must be reproducible from the
			// attached labels — a well-formed hex value that does not match
			// FingerprintOf(labels) cannot be verified and is a mismatch.
			recomputed := FingerprintOf(rawItem.Labels)
			item.fingerprint = declared
			if !bytes.Equal(declared, recomputed) {
				item.status = "fingerprint_mismatch"
				item.reason = "payload fingerprint does not match the attached labels"
				return item, fmt.Errorf("payload fingerprint does not match the attached labels")
			}
		} else {
			item.fingerprint = FingerprintOf(rawItem.Labels)
			item.status = "fingerprint_mismatch"
			item.reason = "payload fingerprint is not 16 hex characters"
			return item, err
		}
	} else {
		item.fingerprint = FingerprintOf(rawItem.Labels)
	}
	return item, nil
}

// applyItem applies one normal alert item to the Occurrence lifecycle inside
// the current transaction (DATA-ALERT-004/005/006/007).
// classifyAndInsertItem decides the final delivery-item status before any
// insert (alert_delivery_items is append-only): it compares the incoming
// labels snapshot against the stored occurrence snapshot for the identity
// triple and marks identity_conflict when they differ, then inserts the item
// row exactly once with the final status.
func (service *Service) classifyAndInsertItem(ctx context.Context, conn *sql.Conn, sourceID, deliveryID int64, item *prepared, receivedAt time.Time, committedAt string) (string, int64, error) {
	incomingCanonical, err := CanonicalLabels(item.labels)
	if err != nil {
		return "", 0, err
	}
	var storedCanonical sql.NullString
	var storedDigest sql.NullString
	err = conn.QueryRowContext(ctx, `SELECT labels_canonical, labels_digest FROM alert_occurrences WHERE source_id=? AND fingerprint=? AND starts_at=?`,
		sourceID, item.fingerprint, item.startsAt).Scan(&storedCanonical, &storedDigest)
	status := "ok"
	detail := ""
	if err == nil && storedCanonical.Valid && storedCanonical.String != incomingCanonical {
		status = "identity_conflict"
		detail = "labels snapshot mismatch"
		if storedDigest.Valid {
			item.storedDigest = storedDigest.String
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", 0, err
	}
	item.status = status
	item.canonical = incomingCanonical
	item.digest = DigestLabels(incomingCanonical)
	itemID, insertErr := insertDeliveryItem(ctx, conn, deliveryID, item.index, status, item.fingerprint, item.startsAt, item.endsAt, item.labels, detail)
	if insertErr != nil {
		return "", 0, insertErr
	}
	return status, itemID, nil
}

func (service *Service) applyItem(ctx context.Context, conn *sql.Conn, sourceID int64, item prepared, itemID, deliveryID int64, receivedAt time.Time, committedAt string) (*OccurrenceRef, string, error) {
	var occurrenceID int64
	var state string
	var rowVersion int64
	err := conn.QueryRowContext(ctx, `SELECT id, state, row_version FROM alert_occurrences WHERE source_id=? AND fingerprint=? AND starts_at=?`,
		sourceID, item.fingerprint, item.startsAt).Scan(&occurrenceID, &state, &rowVersion)
	if errors.Is(err, sql.ErrNoRows) {
		labelsCanonical, canonicalErr := CanonicalLabels(item.labels)
		if canonicalErr != nil {
			return nil, "", canonicalErr
		}
		digest := DigestLabels(labelsCanonical)
		// DATA-ALERT-006: a resolved-first delivery creates the occurrence
		// already closed (state='Resolved', resolved_at set) with a
		// resolved_first observation; the schema CHECK on alert_occurrences
		// requires resolved_at exactly when state='Resolved'.
		effect := "initial_firing"
		initialState := "Firing"
		var resolvedAt any
		if item.itemStatus == "resolved" {
			effect = "resolved_first"
			initialState = "Resolved"
			resolvedAt = committedAt
		}
		result, insertErr := conn.ExecContext(ctx, `INSERT INTO alert_occurrences(source_id, fingerprint, starts_at, state, row_version, labels_canonical, labels_digest, first_seen_at, last_state_change_at, resolved_at) VALUES(?,?,?,?,1,?,?,?,?,?)`,
			sourceID, item.fingerprint, item.startsAt, initialState, labelsCanonical, digest, committedAt, committedAt, resolvedAt)
		if insertErr != nil {
			return nil, "", insertErr
		}
		occurrenceID, _ = result.LastInsertId()
		state = initialState
		rowVersion = 1
		for name, value := range item.labels {
			if _, insertErr := conn.ExecContext(ctx, `INSERT INTO alert_occurrence_labels(occurrence_id, name, value) VALUES(?,?,?)`, occurrenceID, name, value); insertErr != nil {
				return nil, "", insertErr
			}
		}
		if err := insertObservation(ctx, conn, deliveryID, itemID, occurrenceID, item.itemStatus, item.startsAt, item.endsAt, receivedAt, committedAt, effect); err != nil {
			return nil, "", err
		}
		return &OccurrenceRef{ID: occurrenceID, State: state, RowVersion: rowVersion}, effect, nil
	}
	if err != nil {
		return nil, "", err
	}

	var storedCanonical string
	if err := conn.QueryRowContext(ctx, `SELECT labels_canonical FROM alert_occurrences WHERE id=?`, occurrenceID).Scan(&storedCanonical); err != nil {
		return nil, "", err
	}
	incomingCanonical, err := CanonicalLabels(item.labels)
	if err != nil {
		return nil, "", err
	}
	if storedCanonical != incomingCanonical {
		// classifyAndInsertItem already fenced this; a drift here means the
		// occurrence snapshot changed under this transaction — surface it.
		return nil, "", fmt.Errorf("occurrence labels snapshot drifted during delivery")
	}

	effect := "repeat_firing"
	newState := state
	if state == "Firing" && item.itemStatus == "resolved" {
		effect = "resolved"
		newState = "Resolved"
	} else if state == "Resolved" && item.itemStatus == "firing" {
		effect = "late_firing_after_resolved"
	} else if state == "Resolved" && item.itemStatus == "resolved" {
		effect = "resolved"
	}

	if newState != state {
		newRowVersion := rowVersion + 1
		var resolvedAt any
		if newState == "Resolved" {
			resolvedAt = committedAt
		}
		if _, err := conn.ExecContext(ctx, `UPDATE alert_occurrences SET state=?, row_version=?, last_state_change_at=?, resolved_at=? WHERE id=? AND row_version=?`,
			newState, newRowVersion, committedAt, resolvedAt, occurrenceID, rowVersion); err != nil {
			return nil, "", err
		}
		state = newState
		rowVersion = newRowVersion
	}

	if err := insertObservation(ctx, conn, deliveryID, itemID, occurrenceID, item.itemStatus, item.startsAt, item.endsAt, receivedAt, committedAt, effect); err != nil {
		return nil, "", err
	}
	return &OccurrenceRef{ID: occurrenceID, State: state, RowVersion: rowVersion}, effect, nil
}

func insertObservation(ctx context.Context, conn *sql.Conn, deliveryID, itemID, occurrenceID int64, observedState, startsAt, endsAt string, receivedAt time.Time, committedAt string, effect string) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO alert_observations(delivery_id, delivery_item_id, occurrence_id, observed_state, starts_at_source, ends_at_source, received_at, committed_at, effect) VALUES(?,?,?,?,?,?,?,?,?)`,
		deliveryID, itemID, occurrenceID, observedState, startsAt, nullString(endsAt), receivedAt.UTC().Format(time.RFC3339Nano), committedAt, effect)
	return err
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func insertDeliveryItem(ctx context.Context, conn *sql.Conn, deliveryID int64, index int, status string, fingerprint []byte, startsAt, endsAt string, labels map[string]string, errorDetail string) (int64, error) {
	canonical, err := CanonicalLabels(labels)
	if err != nil {
		return 0, err
	}
	result, err := conn.ExecContext(ctx, `INSERT INTO alert_delivery_items(delivery_id, item_index, status, fingerprint, starts_at, ends_at, labels_canonical, error_detail) VALUES(?,?,?,?,?,?,?,?)`,
		deliveryID, index, status, fingerprint, startsAt, nullString(endsAt), canonical, nullString(errorDetail))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (service *Service) recordIssue(ctx context.Context, conn *sql.Conn, sourceID, deliveryID int64, itemID *int64, item prepared, committedAt string) (IntakeIssueRef, error) {
	kind := "fingerprint_mismatch"
	if item.status == "identity_conflict" {
		kind = "identity_conflict"
	}
	fingerprintHex := hexEncode(item.fingerprint)
	canonical, digest := item.canonical, item.digest
	if canonical == "" {
		var err error
		canonical, err = CanonicalLabels(item.labels)
		if err != nil {
			return IntakeIssueRef{}, err
		}
		digest = DigestLabels(canonical)
	}
	nativeKey := canonicalOccurrenceKey(sourceID, item.fingerprint, item.startsAt)
	var issueKey string
	var err error
	switch kind {
	case "identity_conflict":
		expected := digest
		if item.storedDigest != "" {
			expected = item.storedDigest
		}
		issueKey, err = IssueKey("identity_conflict", map[string]string{
			"expected_labels_digest": expected, "native_occurrence_key": nativeKey, "received_labels_digest": digest, "v": "1",
		})
	case "fingerprint_mismatch":
		issueKey, err = IssueKey("fingerprint_mismatch", map[string]string{
			"labels_digest": digest, "native_occurrence_key": nativeKey, "received_fingerprint": fingerprintHex, "v": "1",
		})
	}
	if err != nil {
		return IntakeIssueRef{}, err
	}
	detail := map[string]any{
		"kind": kind, "sourceId": sourceID, "deliveryItemIndex": item.index,
		"receivedFingerprint":  fingerprintHex,
		"receivedLabelsDigest": digest, "nativeOccurrenceKey": nativeKey,
	}
	if kind == "identity_conflict" {
		expected := digest
		if item.storedDigest != "" {
			expected = item.storedDigest
		}
		detail["expectedLabelsDigest"] = expected
	}
	detailJSON, _ := json.Marshal(detail)
	var issueID int64
	var occurrenceCount int
	err = conn.QueryRowContext(ctx, `SELECT id, occurrence_count FROM alert_intake_issues WHERE source_id=? AND kind=? AND issue_key=? AND acknowledged_at IS NULL`,
		sourceID, kind, issueKey).Scan(&issueID, &occurrenceCount)
	if errors.Is(err, sql.ErrNoRows) {
		result, insertErr := conn.ExecContext(ctx, `INSERT INTO alert_intake_issues(source_id, delivery_id, delivery_item_id, kind, issue_key, detail_json, first_seen_at, last_seen_at, occurrence_count, row_version, created_at) VALUES(?,?,?,?,?,?,?,?,1,1,?)`,
			sourceID, deliveryID, nullableID(itemID), kind, issueKey, string(detailJSON), committedAt, committedAt, committedAt)
		if insertErr != nil {
			return IntakeIssueRef{}, insertErr
		}
		issueID, _ = result.LastInsertId()
		if _, err := conn.ExecContext(ctx, `INSERT INTO alert_intake_issue_events(issue_id, delivery_id, delivery_item_id, detail_json, observed_at) VALUES(?,?,?,?,?)`,
			issueID, deliveryID, nullableID(itemID), string(detailJSON), committedAt); err != nil {
			return IntakeIssueRef{}, err
		}
		occurrenceCount = 1
	} else if err != nil {
		return IntakeIssueRef{}, err
	} else {
		eventResult, err := conn.ExecContext(ctx, `INSERT INTO alert_intake_issue_events(issue_id, delivery_id, delivery_item_id, detail_json, observed_at) VALUES(?,?,?,?,?)`,
			issueID, deliveryID, nullableID(itemID), string(detailJSON), committedAt)
		if err != nil {
			return IntakeIssueRef{}, err
		}
		eventID, err := eventResult.LastInsertId()
		if err != nil {
			return IntakeIssueRef{}, err
		}
		// The frozen trigger trg_alert_intake_issues_repeat_update rejects the
		// UPDATE unless last_event_id advances to the just-inserted event, the
		// event belongs to this issue, and the counts line up.
		if _, err := conn.ExecContext(ctx, `UPDATE alert_intake_issues SET last_seen_at=?, occurrence_count=occurrence_count+1, row_version=row_version+1, last_event_id=? WHERE id=?`,
			committedAt, eventID, issueID); err != nil {
			return IntakeIssueRef{}, err
		}
		occurrenceCount++
	}
	return IntakeIssueRef{Kind: kind, IssueKey: issueKey, OccurrenceCount: occurrenceCount}, nil
}

// recordTruncatedIssue flags one delivery_truncated intake issue for a
// delivery with integrity='truncated' (DATA-ALERT-003). The issue is unique
// per delivery (ux_alert_intake_issue_truncated) and carries no item
// reference: truncation marks the delivery and the source, never a specific
// missing occurrence.
func (service *Service) recordTruncatedIssue(ctx context.Context, conn *sql.Conn, sourceID, deliveryID int64, committedAt string) (IntakeIssueRef, error) {
	issueKey, err := IssueKey("delivery_truncated", map[string]string{"kind": "delivery_truncated", "v": "1"})
	if err != nil {
		return IntakeIssueRef{}, err
	}
	detailJSON, _ := json.Marshal(map[string]any{
		"kind": "delivery_truncated", "sourceId": sourceID, "deliveryId": deliveryID,
	})
	result, err := conn.ExecContext(ctx, `INSERT INTO alert_intake_issues(source_id, delivery_id, kind, issue_key, detail_json, first_seen_at, last_seen_at, occurrence_count, row_version, created_at) VALUES(?,?,?,?,?,?,?,1,1,?)`,
		sourceID, deliveryID, "delivery_truncated", issueKey, string(detailJSON), committedAt, committedAt, committedAt)
	if err != nil {
		return IntakeIssueRef{}, err
	}
	issueID, err := result.LastInsertId()
	if err != nil {
		return IntakeIssueRef{}, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO alert_intake_issue_events(issue_id, delivery_id, detail_json, observed_at) VALUES(?,?,?,?)`,
		issueID, deliveryID, string(detailJSON), committedAt); err != nil {
		return IntakeIssueRef{}, err
	}
	return IntakeIssueRef{Kind: "delivery_truncated", IssueKey: issueKey, OccurrenceCount: 1}, nil
}

func nullableID(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func isUniqueViolation(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "constraint failed"))
}
