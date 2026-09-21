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

	"github.com/Suknna/quoin/internal/quoin/execution"
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

// Stable deterministic rejection codes of the relay path.
const (
	codeCredentialDenied = "credential_not_accepted"
	codeDuplicateRelay   = "duplicate_relay"
	codeWebhookInvalid   = "webhook_invalid"
)

// IntakeIssueRef is a non-secret, machine-stable reference to one intake
// issue aggregate created or extended by a delivery.
type IntakeIssueRef struct {
	Kind            string `json:"kind"`
	IssueKey        string `json:"issueKey"`
	OccurrenceCount int    `json:"occurrenceCount"`
}

// txQuerier 是 intake 流水线各段（Normalize/Enrich/Correlate 落库与来源级
// 接入问题）需要的最小事务面：执行器守卫的 *execution.Tx 与测试直连的
// *sql.DB 都满足；不引入包间依赖。
type txQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
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

// Deliver processes one relayed webhook inside one runner-owned IMMEDIATE
// transaction: Delivery row, per-item results, Occurrence/Observation updates,
// intake issues and the automatic audit event all commit or none
// (DATA-ALERT-001/002, RUNTIME-STELE-003/004/005, ADR-0006).
//
// Idempotency is the natural relay key: alert_deliveries.event_id UNIQUE
// adjudicates a redelivery inside the transaction, so no client-command
// ledger row exists — the delivery is a non-replayable ingestion whose
// dedup happens against the delivery table itself. The Stele service
// identity is verified upstream by the relay admission; this side only
// establishes the receiver-local machine scope (machineScope) whose system
// actor the operation authorization re-checks. A credential revoked before
// the commit rejects the delivery as a recorded deterministic rejection
// (Q217: commit-order adjudication); an unparsable body records the rejected
// delivery row plus the failure audit through the recorded-attempt path.
func (service *Service) Deliver(ctx context.Context, relayID string, sourceID, credentialID int64, snapshotVersion uint64, body []byte, receivedAt time.Time) (DeliveryResult, error) {
	webhook, parseErr := ParseWebhook(body)
	if parseErr != nil {
		return service.recordRejected(ctx, relayID, sourceID, credentialID, snapshotVersion, body, receivedAt)
	}
	ctx, err := service.machineScope(ctx)
	if err != nil {
		return DeliveryResult{Unavailable: true, Status: "unavailable"}, err
	}
	result, err := execution.Execute(ctx, service.runner, service.ops.delivery,
		func(tx *execution.Tx) (DeliveryResult, error) {
			return service.deliverOn(ctx, tx, webhook, relayID, sourceID, credentialID, snapshotVersion, body, receivedAt)
		},
		func(result DeliveryResult) int64 { return result.DeliveryID })
	if err != nil {
		var rejection *execution.Rejection
		if errors.As(err, &rejection) {
			// Deterministic rejection: recorded as a rejected audit fact in a
			// clean transaction and surfaced as the wire-level REJECTED
			// outcome — never as an infrastructure error.
			return DeliveryResult{Rejected: true, Status: "rejected", Detail: rejection.Detail}, nil
		}
		return DeliveryResult{Unavailable: true, Status: "unavailable"}, err
	}
	return result, nil
}

// deliverOn is the delivery business stage on the runner's guarded
// transaction. All rejection paths return *execution.Rejection or
// *execution.RecordedFailure so the runner owns the classification and the
// audit rows.
func (service *Service) deliverOn(ctx context.Context, tx *execution.Tx, webhook *AlertmanagerWebhook, relayID string, sourceID, credentialID int64, snapshotVersion uint64, body []byte, receivedAt time.Time) (DeliveryResult, error) {
	var enabled int
	var protocol, credentialState string
	err := tx.QueryRowContext(ctx, `SELECT s.enabled, s.protocol, c.state FROM alert_sources s JOIN alert_source_credentials c ON c.source_id = s.id AND c.id = ? WHERE s.id = ?`, credentialID, sourceID).Scan(&enabled, &protocol, &credentialState)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (enabled != 1 || (credentialState != "Active" && credentialState != "PendingRetirement"))) {
		// Q217: the revocation race is adjudicated by SQLite commit order — a
		// credential revoked before this delivery commits is rejected here,
		// and no delivery row is created (RUNTIME-STELE-005).
		return DeliveryResult{}, &execution.Rejection{Code: codeCredentialDenied, Detail: reasonCredentialDenied}
	}
	if err != nil {
		return DeliveryResult{}, err
	}
	var sourceKey string
	if err := tx.QueryRowContext(ctx, `SELECT source_key FROM alert_sources WHERE id=?`, sourceID).Scan(&sourceKey); err != nil {
		return DeliveryResult{}, err
	}

	now := service.clockText()
	committedAt := now
	integrity := "complete"
	if webhook.TruncatedAlerts > 0 {
		integrity = "truncated"
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO alert_deliveries(event_id, source_id, credential_id, credential_snapshot_version, protocol, body, body_size_bytes, integrity, status, group_key, received_at, committed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		relayID, sourceID, credentialID, snapshotVersion, "alertmanager", body, len(body), integrity, "processed", webhook.GroupKey, receivedAt.UTC().Format(time.RFC3339Nano), committedAt)
	if err != nil {
		if isUniqueViolation(err) {
			var existingID int64
			if lookupErr := tx.QueryRowContext(ctx, `SELECT id FROM alert_deliveries WHERE event_id = ?`, relayID).Scan(&existingID); lookupErr == nil {
				return DeliveryResult{Accepted: true, Status: "accepted", Detail: "duplicate relay; already committed", DeliveryID: existingID}, nil
			}
		}
		return DeliveryResult{}, fmt.Errorf("persist delivery: %w", err)
	}
	deliveryID, err := result.LastInsertId()
	if err != nil {
		return DeliveryResult{}, err
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

	// Normalize（ADR-0012 intake 流水线第一段）：整个交付解析一次归一化器，
	// 归一化结果按 alerts[i] 与 preparedItems[i] 的 index 一一对应；缺失或失败
	// 时全部条目使用缺省语义并在首观测发生时记 normalizer_missing 接入问题。
	normalization := normalizeDelivery(protocol, body)

	processed := 0
	occurrences := []OccurrenceRef{}
	issues := []IntakeIssueRef{}
	normalizerNeeded := false
	for index := range preparedItems {
		item := &preparedItems[index]
		if item.status != "ok" {
			itemID, insertErr := insertDeliveryItem(ctx, tx, deliveryID, item.index, item.status, item.fingerprint, item.startsAt, item.endsAt, item.labels, item.reason)
			if insertErr != nil {
				return DeliveryResult{}, insertErr
			}
			issueRef, issueErr := service.recordIssue(ctx, tx, sourceID, deliveryID, &itemID, *item, committedAt)
			if issueErr != nil {
				return DeliveryResult{}, issueErr
			}
			issues = append(issues, issueRef)
			continue
		}
		finalStatus, itemID, insertErr := service.classifyAndInsertItem(ctx, tx, sourceID, deliveryID, item, receivedAt, committedAt)
		if insertErr != nil {
			return DeliveryResult{}, insertErr
		}
		if finalStatus != "ok" {
			issueRef, issueErr := service.recordIssue(ctx, tx, sourceID, deliveryID, &itemID, *item, committedAt)
			if issueErr != nil {
				return DeliveryResult{}, issueErr
			}
			issues = append(issues, issueRef)
			continue
		}
		occurrence, effect, err := service.applyItem(ctx, tx, sourceID, sourceKey, *item, itemID, deliveryID, receivedAt, committedAt, normalization)
		if err != nil {
			return DeliveryResult{}, err
		}
		processed++
		if effect == "initial_firing" || effect == "resolved_first" {
			normalizerNeeded = true
		}
		if occurrence != nil {
			occurrences = append(occurrences, *occurrence)
		}
	}

	// 归一化缺失只在其降级真正生效（本交付创建了首观测）时记录一次；
	// 同源的重复交付按既有接入问题聚合计数推进。
	if !normalization.ok && normalizerNeeded {
		issueRef, issueErr := service.recordNormalizerMissingIssue(ctx, tx, sourceID, deliveryID, protocol, committedAt)
		if issueErr != nil {
			return DeliveryResult{}, issueErr
		}
		issues = append(issues, issueRef)
	}

	if webhook.TruncatedAlerts > 0 {
		// DATA-ALERT-003: truncated deliveries are permanently flagged on the
		// Delivery (integrity='truncated') and raise one delivery_truncated
		// intake issue per delivery; no lifecycle inference is made for the
		// unknown missing items.
		issueRef, issueErr := service.recordTruncatedIssue(ctx, tx, sourceID, deliveryID, committedAt)
		if issueErr != nil {
			return DeliveryResult{}, issueErr
		}
		issues = append(issues, issueRef)
	}

	return DeliveryResult{
		Accepted: true, Status: "accepted", DeliveryID: deliveryID,
		Processed: processed, Issues: issues, Occurrences: occurrences,
	}, nil
}

// recordRejected persists one unparsable webhook as a rejected delivery row
// and records the failed intake attempt through the runner's recorded-attempt
// path: the intended state change (the rejected delivery row) commits and the
// automatic audit records the failure. A redelivered rejected relay id is a
// deterministic duplicate rejection — nothing new is persisted.
func (service *Service) recordRejected(ctx context.Context, relayID string, sourceID, credentialID int64, snapshotVersion uint64, body []byte, receivedAt time.Time) (DeliveryResult, error) {
	ctx, err := service.machineScope(ctx)
	if err != nil {
		return DeliveryResult{Unavailable: true, Status: "unavailable"}, err
	}
	_, err = execution.Execute(ctx, service.runner, service.ops.delivery,
		func(tx *execution.Tx) (bool, error) {
			result, insertErr := tx.ExecContext(ctx, `INSERT INTO alert_deliveries(event_id, source_id, credential_id, credential_snapshot_version, protocol, body, body_size_bytes, integrity, status, received_at, committed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				relayID, sourceID, credentialID, snapshotVersion, "alertmanager", body, len(body), "rejected", "rejected", receivedAt.UTC().Format(time.RFC3339Nano), service.clockText())
			if insertErr != nil {
				if isUniqueViolation(insertErr) {
					return false, &execution.Rejection{Code: codeDuplicateRelay, Detail: "duplicate relay; rejected delivery already recorded"}
				}
				return false, fmt.Errorf("persist rejected delivery: %w", insertErr)
			}
			deliveryID, _ := result.LastInsertId()
			return false, &execution.RecordedFailure{
				Code:     codeWebhookInvalid,
				Detail:   "webhook body is not valid Alertmanager JSON",
				ObjectID: deliveryID,
			}
		},
		func(bool) int64 { return 0 })
	if err != nil {
		var failure *execution.RecordedFailure
		var rejection *execution.Rejection
		switch {
		case errors.As(err, &failure):
			// The recorded attempt committed: the wire outcome stays REJECTED.
			return DeliveryResult{Rejected: true, Status: "rejected", Detail: failure.Detail}, nil
		case errors.As(err, &rejection):
			return DeliveryResult{Rejected: true, Status: "rejected", Detail: rejection.Detail}, nil
		}
		return DeliveryResult{Unavailable: true, Status: "unavailable"}, err
	}
	return DeliveryResult{Rejected: true, Status: "rejected", Detail: "webhook body is not valid Alertmanager JSON"}, nil
}

func prepareItem(index int, rawItem struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	Fingerprint  string            `json:"fingerprint"`
	GeneratorURL string            `json:"generatorURL"`
},
) (prepared, error) {
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
func (service *Service) classifyAndInsertItem(ctx context.Context, conn execution.Executor, sourceID, deliveryID int64, item *prepared, receivedAt time.Time, committedAt string) (string, int64, error) {
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

// applyItem applies one normal alert item to the Occurrence lifecycle inside
// the current transaction (DATA-ALERT-004/005/006/007). The first observation
// (sql.ErrNoRows branch) executes the ADR-0012 intake pipeline stages that
// freeze delivery-time evidence: Normalize (统一语义列), Enrich (富化文档) and
// Correlate (全部命中视图)；Dedup 即外层的身份三元组幂等与观测机制本身。
func (service *Service) applyItem(ctx context.Context, conn execution.Executor, sourceID int64, sourceKey string, item prepared, itemID, deliveryID int64, receivedAt time.Time, committedAt string, normalization deliveryNormalization) (*OccurrenceRef, string, error) {
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
		// Normalize：按 payload alerts[i] 对位冻结统一语义（缺失/失败时为缺省值）。
		severity, title, annotationsCanonical, resource := normalization.semanticsFor(item.index)

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
		result, insertErr := conn.ExecContext(ctx, `INSERT INTO alert_occurrences(source_id, fingerprint, starts_at, state, row_version, labels_canonical, labels_digest, severity, title, annotations_canonical, resource, first_seen_at, last_state_change_at, resolved_at) VALUES(?,?,?,?,1,?,?,?,?,?,?,?,?,?)`,
			sourceID, item.fingerprint, item.startsAt, initialState, labelsCanonical, digest, severity, title, annotationsCanonical, resource, committedAt, committedAt, resolvedAt)
		if insertErr != nil {
			return nil, "", insertErr
		}
		occurrenceID, _ = result.LastInsertId()

		// Enrich：与首观测同事务求值并冻结富化文档（即使无规则命中也写一行，
		// 区分"无规则命中"与"未求值"）。
		enrichmentJSON, enrichErr := evaluateEnrichment(ctx, conn, sourceKey, item.labels)
		if enrichErr != nil {
			return nil, "", enrichErr
		}
		if err := persistEnrichment(ctx, conn, occurrenceID, enrichmentJSON, committedAt); err != nil {
			return nil, "", err
		}

		// Correlate：全部命中视图各冻结一行关联证据，视图后续编辑不回写。
		matched, correlateErr := correlateViews(ctx, conn, sourceID, item.labels)
		if correlateErr != nil {
			return nil, "", correlateErr
		}
		if err := persistCorrelations(ctx, conn, occurrenceID, matched, committedAt); err != nil {
			return nil, "", err
		}
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

func insertObservation(ctx context.Context, conn execution.Executor, deliveryID, itemID, occurrenceID int64, observedState, startsAt, endsAt string, receivedAt time.Time, committedAt string, effect string) error {
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

func insertDeliveryItem(ctx context.Context, conn execution.Executor, deliveryID int64, index int, status string, fingerprint []byte, startsAt, endsAt string, labels map[string]string, errorDetail string) (int64, error) {
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

func (service *Service) recordIssue(ctx context.Context, conn execution.Executor, sourceID, deliveryID int64, itemID *int64, item prepared, committedAt string) (IntakeIssueRef, error) {
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

// recordNormalizerMissingIssue 标记一次 ADR-0012 归一化缺失（ADR-0012
// Normalize 段）：来源协议没有 AlertNormalizer 或归一化失败时，首观测以缺省
// 语义冻结并记录本问题。问题是来源级的（issue_key 绑定协议，不指向具体
// 条目），闭合到该源本次已处理的 Delivery；同源重复交付沿既有聚合计数推进。
func (service *Service) recordNormalizerMissingIssue(ctx context.Context, conn txQuerier, sourceID, deliveryID int64, protocol, committedAt string) (IntakeIssueRef, error) {
	issueKey, err := IssueKey("normalizer_missing", map[string]string{"kind": "normalizer_missing", "protocol": protocol, "v": "1"})
	if err != nil {
		return IntakeIssueRef{}, err
	}
	detailJSON, _ := json.Marshal(map[string]any{
		"kind": "normalizer_missing", "sourceId": sourceID, "deliveryId": deliveryID, "protocol": protocol,
	})
	var issueID int64
	var occurrenceCount int
	err = conn.QueryRowContext(ctx, `SELECT id, occurrence_count FROM alert_intake_issues WHERE source_id=? AND kind=? AND issue_key=? AND acknowledged_at IS NULL`,
		sourceID, "normalizer_missing", issueKey).Scan(&issueID, &occurrenceCount)
	if errors.Is(err, sql.ErrNoRows) {
		result, insertErr := conn.ExecContext(ctx, `INSERT INTO alert_intake_issues(source_id, delivery_id, kind, issue_key, detail_json, first_seen_at, last_seen_at, occurrence_count, row_version, created_at) VALUES(?,?,?,?,?,?,?,1,1,?)`,
			sourceID, deliveryID, "normalizer_missing", issueKey, string(detailJSON), committedAt, committedAt, committedAt)
		if insertErr != nil {
			return IntakeIssueRef{}, insertErr
		}
		issueID, _ = result.LastInsertId()
		if _, err := conn.ExecContext(ctx, `INSERT INTO alert_intake_issue_events(issue_id, delivery_id, detail_json, observed_at) VALUES(?,?,?,?)`,
			issueID, deliveryID, string(detailJSON), committedAt); err != nil {
			return IntakeIssueRef{}, err
		}
		occurrenceCount = 1
	} else if err != nil {
		return IntakeIssueRef{}, err
	} else {
		eventResult, err := conn.ExecContext(ctx, `INSERT INTO alert_intake_issue_events(issue_id, delivery_id, detail_json, observed_at) VALUES(?,?,?,?)`,
			issueID, deliveryID, string(detailJSON), committedAt)
		if err != nil {
			return IntakeIssueRef{}, err
		}
		eventID, err := eventResult.LastInsertId()
		if err != nil {
			return IntakeIssueRef{}, err
		}
		// 冻结触发器要求 last_event_id 前进到刚插入的事件且计数吻合。
		if _, err := conn.ExecContext(ctx, `UPDATE alert_intake_issues SET last_seen_at=?, occurrence_count=occurrence_count+1, row_version=row_version+1, last_event_id=? WHERE id=?`,
			committedAt, eventID, issueID); err != nil {
			return IntakeIssueRef{}, err
		}
		occurrenceCount++
	}
	return IntakeIssueRef{Kind: "normalizer_missing", IssueKey: issueKey, OccurrenceCount: occurrenceCount}, nil
}

// recordTruncatedIssue flags one delivery_truncated intake issue for a
// delivery with integrity='truncated' (DATA-ALERT-003). The issue is unique
// per delivery (ux_alert_intake_issue_truncated) and carries no item
// reference: truncation marks the delivery and the source, never a specific
// missing occurrence.
func (service *Service) recordTruncatedIssue(ctx context.Context, conn execution.Executor, sourceID, deliveryID int64, committedAt string) (IntakeIssueRef, error) {
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
