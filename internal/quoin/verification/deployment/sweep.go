package deployment

import (
	"context"
	"database/sql"
	"time"
)

// SweepDeadline closes one invocation at its fixed boundary: undecidable
// items receive not_run with the deadline as the observation time (the
// profile's unfinished-item category), then the system receipt is written if
// the writer can still commit inside the window. A sweeper that finds the
// deadline already past leaves the invocation receiptless — late final
// results are forbidden and a new invocation is required (VERIFY-DA-TIME-001).
func (service *Service) SweepDeadline(ctx context.Context, invocationID int64) error {
	manifest, err := service.loadManifest(ctx, invocationID)
	if err != nil {
		if err == ErrNotFound {
			return nil
		}
		return err
	}
	if _, ok, err := service.receiptFor(ctx, invocationID); err != nil {
		return err
	} else if ok {
		return nil
	}
	now := service.now().UTC()
	boundary := manifest.deadline
	if now.After(boundary.Add(time.Second)) {
		return nil // missed window: never produce a late final result
	}
	_, decidable, err := service.verdicts(ctx, invocationID)
	if err != nil && err != ErrInProgress {
		return err
	}
	if decidable {
		if err := service.assertObservationClosure(ctx, invocationID); err == nil {
			_, finalizeErr := service.Finalize(ctx, invocationID, nil)
			return finalizeErr
		}
	}
	deadlineText := boundary.UTC().Format(time.RFC3339Nano)
	synthesisErr := service.withConn(ctx, func(conn *sql.Conn) error {
		var receipt int64
		err := conn.QueryRowContext(ctx, `SELECT id FROM verification_finalization_receipts WHERE invocation_id=?`, invocationID).Scan(&receipt)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		items, err := conn.QueryContext(ctx, `SELECT i.id,i.input_digest FROM verification_invocation_items i
			WHERE i.invocation_id=? AND NOT EXISTS (SELECT 1 FROM verification_item_results r WHERE r.item_id=i.id) ORDER BY i.item_seq`, invocationID)
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
				"itemId": entry.id, "inputDigest": entry.inputDigest, "category": "not_run",
			})
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO verification_item_results(item_id,input_digest,result_digest,producer_type,outcome,category,observed_at,committed_at,evidence_index_digest)
				VALUES(?,?,?,'quoin','warned','not_run',?,?,?)`,
				entry.id, entry.inputDigest, digest, deadlineText, deadlineText, digest); err != nil {
				return translateInsert(err, "deadline not_run result")
			}
		}
		return nil
	})
	if synthesisErr != nil {
		return synthesisErr
	}
	_, finalizeErr := service.Finalize(ctx, invocationID, nil)
	if finalizeErr != nil && finalizeErr != ErrInProgress {
		return finalizeErr
	}
	return nil
}
