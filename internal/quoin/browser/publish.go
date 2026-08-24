package browser

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

type PublishResult struct {
	OperationID      int64
	CommandID        string
	Generation       uint64
	ChromiumRevision string
	ManifestDigest   []byte
	Accepted         bool
	Probe            ProbeResult
	BootID           string
	Epoch            uint64
}

// HandlePublishResult admits only the Runtime result that matches a persisted
// user command and the operation's frozen Lintel binding. The schema triggers
// then atomically publish the generation, make the identity Ready, and close
// the manual-login operation.
func (service *Service) HandlePublishResult(ctx context.Context, result PublishResult) error {
	if result.OperationID < 1 || result.CommandID == "" || result.BootID == "" || result.Epoch == 0 {
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
	var identityID, revisionID, actorID int64
	var state, boot, catalogDigest, catalogVersion, journeyID string
	var journeyVersion, epoch int64
	err = conn.QueryRowContext(ctx, `SELECT o.identity_id,o.identity_revision_id,o.actor_user_id,o.state,o.lintel_boot_id,o.lintel_connection_epoch,o.journey_catalog_digest,o.journey_catalog_version,r.probe_journey_id,r.probe_journey_version FROM browser_operations o JOIN browser_identity_revisions r ON r.id=o.identity_revision_id WHERE o.id=?`, result.OperationID).Scan(&identityID, &revisionID, &actorID, &state, &boot, &epoch, &catalogDigest, &catalogVersion, &journeyID, &journeyVersion)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if boot != result.BootID || int64(result.Epoch) < epoch {
		return ErrConflict
	}
	if !result.Accepted || result.Generation == 0 || result.ChromiumRevision == "" || len(result.ManifestDigest) != 32 || result.Probe.Phase != "publish" || result.Probe.Result != "Authenticated" || result.Probe.JourneyID != journeyID || result.Probe.JourneyVersion != journeyVersion || result.Probe.CatalogDigest != catalogDigest || result.Probe.CatalogVersion != catalogVersion || result.Probe.ObservedAt == "" {
		return ErrInvalid
	}
	var command int
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_commands WHERE principal_type='user' AND principal_id=? AND client_command_id=? AND command_type='publish_browser_profile' AND result_object_type='browser_operation' AND result_object_id=?`, actorID, result.CommandID, result.OperationID).Scan(&command); err != nil {
		return err
	}
	if command != 1 {
		return ErrConflict
	}
	if state == "Succeeded" {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if state != "Running" && state != "AwaitingReconnect" {
		return ErrConflict
	}
	observed := result.Probe.ObservedAt
	if _, err = conn.ExecContext(ctx, `INSERT INTO browser_probe_results(operation_id,probe_seq,phase,identity_revision_id,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,result,reason_code,observed_at) VALUES(?,1,'publish',?,?,?,?,?,'Authenticated',NULL,?)`, result.OperationID, revisionID, journeyID, journeyVersion, catalogDigest, catalogVersion, observed); err != nil {
		return err
	}
	now := service.now().UTC().Format(time.RFC3339Nano)
	if _, err = conn.ExecContext(ctx, `INSERT INTO browser_profile_generations(identity_id,identity_revision_id,generation,chromium_revision,profile_manifest_digest,probe_journey_id,probe_journey_version,probe_catalog_digest,probe_catalog_version,published_operation_id,published_by,published_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, identityID, revisionID, result.Generation, result.ChromiumRevision, hex.EncodeToString(result.ManifestDigest), journeyID, journeyVersion, catalogDigest, catalogVersion, result.OperationID, actorID, now); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func timestampString(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

// HandlePublishRejected commits only a matching persisted publish command;
// malformed or unsolicited Runtime frames leave the operation Running.
// HandlePublishUnauthenticated records the publish probe fact without ending
// the active manual login. The operator may continue interacting and issue the
// same publish command again after completing authentication.
func (service *Service) HandlePublishUnauthenticated(ctx context.Context, operationID int64, commandID, bootID string, epoch uint64, probe ProbeResult) error {
	if probe.Phase != "publish" || probe.Result != "Unauthenticated" || probe.ObservedAt == "" || operationID < 1 || commandID == "" || bootID == "" || epoch == 0 {
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
	var revisionID, actorID int64
	var state, storedBoot, digest, version, journey string
	var journeyVersion, storedEpoch int64
	if err = conn.QueryRowContext(ctx, `SELECT o.identity_revision_id,o.actor_user_id,o.state,o.lintel_boot_id,o.lintel_connection_epoch,o.journey_catalog_digest,o.journey_catalog_version,r.probe_journey_id,r.probe_journey_version FROM browser_operations o JOIN browser_identity_revisions r ON r.id=o.identity_revision_id WHERE o.id=?`, operationID).Scan(&revisionID, &actorID, &state, &storedBoot, &storedEpoch, &digest, &version, &journey, &journeyVersion); err != nil {
		return err
	}
	if (state != "Running" && state != "AwaitingReconnect") || storedBoot != bootID || int64(epoch) < storedEpoch || probe.JourneyID != journey || probe.JourneyVersion != journeyVersion || probe.CatalogDigest != digest || probe.CatalogVersion != version {
		return ErrConflict
	}
	var commands int
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_commands WHERE principal_type='user' AND principal_id=? AND client_command_id=? AND command_type='publish_browser_profile' AND result_object_id=?`, actorID, commandID, operationID).Scan(&commands); err != nil {
		return err
	}
	if commands != 1 {
		return ErrConflict
	}
	var next int64
	if err = conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(probe_seq),0)+1 FROM browser_probe_results WHERE operation_id=?`, operationID).Scan(&next); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO browser_probe_results(operation_id,probe_seq,phase,identity_revision_id,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,result,reason_code,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, operationID, next, "publish", revisionID, journey, journeyVersion, digest, version, "Unauthenticated", nil, probe.ObservedAt); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func (service *Service) HandlePublishRejected(ctx context.Context, operationID int64, commandID, bootID string, epoch uint64, unavailable bool) error {
	if operationID < 1 || commandID == "" || bootID == "" || epoch == 0 {
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
	var actorID int64
	var state, storedBoot string
	var storedEpoch int64
	if err = conn.QueryRowContext(ctx, `SELECT actor_user_id,state,lintel_boot_id,lintel_connection_epoch FROM browser_operations WHERE id=?`, operationID).Scan(&actorID, &state, &storedBoot, &storedEpoch); err != nil {
		return err
	}
	if storedBoot != bootID || int64(epoch) < storedEpoch {
		return ErrConflict
	}
	var commands int
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_commands WHERE principal_type='user' AND principal_id=? AND client_command_id=? AND command_type='publish_browser_profile' AND result_object_id=?`, actorID, commandID, operationID).Scan(&commands); err != nil {
		return err
	}
	if commands != 1 {
		return ErrConflict
	}
	if state == "Running" || state == "AwaitingReconnect" {
		reason := "protocol_error"
		if unavailable {
			reason = "authentication_probe_unavailable"
		}
		now := service.now().UTC().Format(time.RFC3339Nano)
		if _, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='Failed',reconnect_deadline=NULL,ended_at=?,terminal_reason=?,row_version=row_version+1 WHERE id=? AND state IN ('Running','AwaitingReconnect')`, now, reason, operationID); err != nil {
			return err
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}
