package browser

import (
	"context"
	"time"
)

type Completion struct {
	BootID         string
	ID             int64
	Epoch          uint64
	Outcome        string
	TerminalReason string
	Digest         []byte
	EndedAt        time.Time
	Probes         []ProbeResult
}

// HandleCompletion atomically persists a Lintel browser terminal proposal and
// its typed probe observations. Only an explicit Unauthenticated observation
// changes an identity to AuthenticationRequired (enforced by the SQL trigger).
func (service *Service) HandleCompletion(ctx context.Context, completion Completion) error {
	if completion.ID < 1 || completion.BootID == "" || completion.Epoch == 0 || len(completion.Digest) != 32 || completion.EndedAt.IsZero() {
		return ErrInvalid
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var state, boot, kind, journey, catalogDigest, catalogVersion string
	var storedEpoch, revisionID, journeyVersion int64
	var existing []byte
	err = conn.QueryRowContext(ctx, `SELECT state,lintel_boot_id,lintel_connection_epoch,kind,identity_revision_id,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,completion_digest FROM browser_operations WHERE id=?`, completion.ID).Scan(&state, &boot, &storedEpoch, &kind, &revisionID, &journey, &journeyVersion, &catalogDigest, &catalogVersion, &existing)
	if err != nil {
		return err
	}
	if boot != completion.BootID || int64(completion.Epoch) < storedEpoch {
		return ErrConflict
	}
	if state != "Running" && !(kind == "manual_login" && state == "AwaitingReconnect") {
		if (state == "Succeeded" || state == "Failed") && string(existing) == string(completion.Digest) {
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return err
			}
			committed = true
			return nil
		}
		return ErrConflict
	}
	if kind == "manual_login" {
		if completion.Outcome != "Failed" || completion.TerminalReason != "browser_crashed" || len(completion.Probes) != 0 {
			return ErrInvalid
		}
		if _, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='Failed',reconnect_deadline=NULL,ended_at=?,terminal_reason='browser_crashed',completion_digest=?,row_version=row_version+1 WHERE id=? AND state IN ('Running','AwaitingReconnect')`, completion.EndedAt.UTC().Format(time.RFC3339Nano), completion.Digest, completion.ID); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if kind != "authentication_probe" || len(completion.Probes) != 1 {
		if kind == "journey" {
			// Journey operations terminalize through the journey ledger for real
			// results; only physical loss completions (crash/startup failure) arrive
			// here, and they can never claim success.
			if completion.Outcome != "Failed" || len(completion.Probes) != 0 {
				return ErrInvalid
			}
			switch completion.TerminalReason {
			case "browser_crashed", "runtime_unavailable", "protocol_error", "artifact_commit_failed":
			default:
				return ErrInvalid
			}
			if _, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='Failed',ended_at=?,terminal_reason=?,completion_digest=?,row_version=row_version+1 WHERE id=? AND state='Running'`, completion.EndedAt.UTC().Format(time.RFC3339Nano), completion.TerminalReason, completion.Digest, completion.ID); err != nil {
				return err
			}
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return err
			}
			committed = true
			return nil
		}
		return ErrInvalid
	}
	probe := completion.Probes[0]
	if probe.Phase != "revision_change" || probe.JourneyID != journey || probe.JourneyVersion != journeyVersion || probe.CatalogDigest != catalogDigest || probe.CatalogVersion != catalogVersion || probe.ObservedAt == "" || (probe.Result != "Authenticated" && probe.Result != "Unauthenticated" && probe.Result != "Indeterminate") || (probe.Result == "Indeterminate") != (probe.ReasonCode != nil) {
		return ErrInvalid
	}
	terminalState := "Succeeded"
	if completion.Outcome != "Succeeded" {
		terminalState = "Failed"
	}
	if terminalState == "Succeeded" && (probe.Result != "Authenticated" || completion.TerminalReason != "") {
		return ErrInvalid
	}
	if terminalState == "Failed" && completion.TerminalReason == "" {
		return ErrInvalid
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO browser_probe_results(operation_id,probe_seq,phase,identity_revision_id,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,result,reason_code,observed_at) VALUES(?,1,'revision_change',?,?,?,?,?,?,?,?)`, completion.ID, revisionID, journey, journeyVersion, catalogDigest, catalogVersion, probe.Result, probe.ReasonCode, probe.ObservedAt); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state=?,ended_at=?,terminal_reason=?,completion_digest=?,row_version=row_version+1 WHERE id=? AND state='Running'`, terminalState, completion.EndedAt.UTC().Format(time.RFC3339Nano), nullString(completion.TerminalReason), completion.Digest, completion.ID); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
