package deployment

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

// Cancel stops unfinished items and finalizes by the already-observed facts:
// every item without a result receives an immutable
// operator_cancelled/not_run result (inside the fixed window), dispatched
// browser work is marked for convergence by its own stop path, and the
// receipt is written by the initiating Admin Session. An already-finalized
// invocation replays its receipt idempotently.
func (service *Service) Cancel(ctx context.Context, sessionID, principalID int64, invocationID int64, clientCommandID string) (*VerificationDetail, error) {
	requestDigest := auth.DigestCommand(commandCancel, map[string]any{"clientCommandId": clientCommandID, "invocation": invocationID, "session": sessionID})
	if record, found, err := auth.LookupCommand(ctx, service.db, principalID, clientCommandID); err != nil {
		return nil, err
	} else if found {
		if record.RequestDigest != requestDigest || record.ResultObjectType != "verification_invocation" {
			return nil, service.fail(errConflict, "client command %q was already used by a different request", clientCommandID)
		}
		return service.Load(ctx, record.ResultObjectID)
	}
	if err := service.withConn(ctx, func(conn *sql.Conn) error {
		if _, found, lookupErr := auth.LookupCommandOn(ctx, conn, principalID, clientCommandID); lookupErr != nil {
			return lookupErr
		} else if found {
			return nil
		}
		if err := service.requireActiveAdmin(ctx, conn, sessionID, principalID); err != nil {
			return err
		}
		manifest, err := service.loadManifestOn(ctx, conn, invocationID)
		if err != nil {
			return err
		}
		if manifest.sessionID != sessionID || manifest.principalID != principalID {
			return service.fail(errInvalid, "only the initiating Admin Session may cancel this invocation")
		}
		var receipt int64
		err = conn.QueryRowContext(ctx, `SELECT id FROM verification_finalization_receipts WHERE invocation_id=?`, invocationID).Scan(&receipt)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		now := service.now().UTC()
		if now.After(manifest.deadline) {
			return service.fail(errConflict, "invocation missed its fixed deadline; late cancellation cannot synthesize results")
		}
		timestamp := now.Format(time.RFC3339Nano)
		items, err := conn.QueryContext(ctx, `SELECT i.id,i.input_digest FROM verification_invocation_items i
			WHERE i.invocation_id=? AND NOT EXISTS (SELECT 1 FROM verification_item_results r WHERE r.item_id=i.id)
			AND NOT EXISTS (SELECT 1 FROM browser_operations o WHERE o.verification_manifest_item_id=i.id)
			ORDER BY i.item_seq`, invocationID)
		if err != nil {
			return err
		}
		type pending struct {
			id          int64
			inputDigest string
		}
		unfinished := []pending{}
		for items.Next() {
			entry := pending{}
			if err := items.Scan(&entry.id, &entry.inputDigest); err != nil {
				items.Close()
				return err
			}
			unfinished = append(unfinished, entry)
		}
		items.Close()
		if err := items.Err(); err != nil {
			return err
		}
		for _, entry := range unfinished {
			digest, err := canonicalDigest(map[string]any{
				"itemId": entry.id, "inputDigest": entry.inputDigest, "category": "operator_cancelled",
			})
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO verification_item_results(item_id,input_digest,result_digest,producer_type,outcome,category,observed_at,committed_at,evidence_index_digest)
				VALUES(?,?,?,'quoin','warned','operator_cancelled',?,?,?)`,
				entry.id, entry.inputDigest, digest, timestamp, timestamp, digest); err != nil {
				return translateInsert(err, "cancellation result")
			}
		}
		payload := fmt.Sprintf(`{"invocationId":%d}`, invocationID)
		if err := auth.RecordCommand(ctx, conn, principalID, clientCommandID, commandCancel, requestDigest, "committed", "verification_invocation", invocationID, payload); err != nil {
			return err
		}
		return service.recordAudit(ctx, conn, "user", principalID, "deployment_verification.cancel", clientCommandID, "success", "verification_invocation", invocationID)
	}); err != nil {
		return nil, err
	}
	// Cancel dispatched browser work so cleanup convergence can finish. Only
	// never-dispatched operations may be terminalized locally; a started
	// operation owns an unknown-outcome physical fence and goes through the
	// runtime stop dispatcher hook.
	service.cancelUndispatchedBrowserWork(ctx, invocationID)
	if service.browserCanceller != nil {
		service.browserCanceller(ctx, invocationID)
	}
	session := sessionID
	detail, err := service.Finalize(ctx, invocationID, &session)
	if err != nil {
		if err == ErrInProgress {
			// Dispatched work has not yet reached a terminal state; the
			// receipt follows its convergence (or the deadline sweeper).
			return service.Load(ctx, invocationID)
		}
		serviceErr, ok := err.(*Error)
		if ok && serviceErr == ErrInProgress {
			return service.Load(ctx, invocationID)
		}
		return nil, err
	}
	return detail, nil
}

func (service *Service) loadManifestOn(ctx context.Context, conn *sql.Conn, invocationID int64) (*manifestRecord, error) {
	record := &manifestRecord{id: invocationID}
	var deadlineText string
	err := conn.QueryRowContext(ctx, `SELECT admin_session_id,principal_user_id,release_subject_digest,deployment_config_digest,public_origin_digest,
		catalog_digest,result_profile_digest,manifest_digest,applicable_set_digest,item_set_digest,canonical_input_digest,started_at,deadline_at
		FROM verification_invocation_manifests WHERE id=?`, invocationID).
		Scan(&record.sessionID, &record.principalID, &record.releaseSubjectDigest, &record.deploymentConfigDigest, &record.publicOriginDigest,
			&record.catalogDigest, &record.resultProfileDigest, &record.manifestDigest, &record.applicableSetDigest, &record.itemSetDigest, &record.canonicalInputDigest, &record.startedAt, &deadlineText)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	deadline, err := time.Parse(time.RFC3339Nano, deadlineText)
	if err != nil {
		return nil, err
	}
	record.deadline = deadline
	return record, nil
}

// cancelUndispatchedBrowserWork terminalizes deployment verification
// operations that never physically started; the physical fence makes any
// started operation the stop dispatcher's responsibility instead.
func (service *Service) cancelUndispatchedBrowserWork(ctx context.Context, invocationID int64) {
	now := service.now().UTC().Format(time.RFC3339Nano)
	_, _ = service.db.ExecContext(ctx, `UPDATE browser_operations SET state='Cancelled',ended_at=?,terminal_reason='cancelled',
		stop_confirmed_at=?,stop_confirmation_basis='not_dispatched'
		WHERE kind='deployment_verification' AND start_dispatched_at IS NULL
		AND state IN ('Queued','WaitingForCapacity')
		AND verification_manifest_item_id IN (SELECT id FROM verification_invocation_items WHERE invocation_id=?)`, now, now, invocationID)
}
