package browser

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type StopRequest struct {
	OperationID int64
	BootID      string
	Epoch       uint64
}

func (service *Service) PrepareStop(ctx context.Context, operationID int64) (StopRequest, error) {
	return service.prepareStop(ctx, operationID, "", 0)
}

// PrepareStopForBoot targets the currently attached boot when the immutable
// start binding belongs to a predecessor. This preserves the original binding
// as history while requiring the current boot to prove physical cleanup.
func (service *Service) PrepareStopForBoot(ctx context.Context, operationID int64, bootID string, epoch uint64) (StopRequest, error) {
	if bootID == "" || epoch == 0 {
		return StopRequest{}, ErrInvalid
	}
	return service.prepareStop(ctx, operationID, bootID, epoch)
}

func (service *Service) prepareStop(ctx context.Context, operationID int64, successorBootID string, successorEpoch uint64) (StopRequest, error) {
	var r StopRequest
	var epoch int64
	var terminalReason string
	err := service.db.QueryRowContext(ctx, `SELECT id,lintel_boot_id,lintel_connection_epoch,COALESCE(terminal_reason,'') FROM browser_operations WHERE id=? AND state IN ('Succeeded','Failed','Cancelled','Interrupted') AND start_dispatched_at IS NOT NULL AND stop_confirmed_at IS NULL`, operationID).Scan(&r.OperationID, &r.BootID, &epoch, &terminalReason)
	if errors.Is(err, sql.ErrNoRows) {
		return StopRequest{}, ErrConflict
	}
	if err != nil {
		return StopRequest{}, err
	}
	r.Epoch = uint64(epoch)
	if successorBootID != "" && successorEpoch != 0 && r.BootID != successorBootID {
		r.BootID, r.Epoch = successorBootID, successorEpoch
	} else if terminalReason == "new_boot" {
		return StopRequest{}, ErrConflict
	}
	return r, nil
}

// HandleStopAck only releases the identity/slot fence after Lintel reports a
// complete typed cleanup observation for the matching dispatch binding.
func (service *Service) HandleStopAck(ctx context.Context, operationID int64, bootID string, epoch uint64, succeeded bool, stoppedAt time.Time, cleanupHash []byte) error {
	if operationID < 1 || bootID == "" || epoch == 0 || !succeeded || stoppedAt.IsZero() || len(cleanupHash) != 32 {
		return ErrInvalid
	}
	// A new Lintel boot is the only process capable of proving cleanup after
	// its predecessor disappeared. The runtime handler admits frames only from
	// the currently attached boot before calling here; retain the original
	// immutable start binding but accept the successor's explicit cleanup proof.
	// The SQL contract only persists cleanup_state_hash for same/new-boot
	// cleanup bases; ordinary stop_ack keeps the typed frame hash on the wire
	// but stores no disallowed second representation.
	var hash any
	var terminalReason string
	if err := service.db.QueryRowContext(ctx, `SELECT COALESCE(terminal_reason,'') FROM browser_operations WHERE id=?`, operationID).Scan(&terminalReason); err != nil {
		return err
	}
	if terminalReason == "new_boot" {
		hash = cleanupHash
	}
	result, err := service.db.ExecContext(ctx, `UPDATE browser_operations SET stop_confirmed_at=?,stop_confirmation_basis=CASE WHEN terminal_reason='new_boot' THEN 'new_boot_cleanup_confirmed' ELSE 'stop_ack' END,cleanup_state_hash=?,row_version=row_version+1 WHERE id=? AND state IN ('Succeeded','Failed','Cancelled','Interrupted') AND stop_confirmed_at IS NULL`, stoppedAt.UTC().Format(time.RFC3339Nano), hash, operationID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrConflict
	}
	return nil
}
