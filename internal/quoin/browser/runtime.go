package browser

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// DispatchInput is the immutable operation projection needed to construct a
// StartBrowserOperation. It is read only after the durable Starting fence has
// committed; a stale Lintel response is rejected by the stored boot/epoch.
type DispatchInput struct {
	OperationID, IdentityID, RevisionID int64
	ActorUserID, ActorSessionID         *int64
	ProfileGenerationID                 int64
	Kind, CatalogDigest, CatalogVersion string
	StartURL                            string
	Probe                               ProbeConfig
	RequestedAt                         string
	BootID                              string
	Epoch                               uint64
	CanonicalJSON                       []byte
}

type PublishRequest struct {
	OperationID, IdentityID, RevisionID int64
	ExpectedGenerationID                int64
	NewGeneration                       uint64
	CommandID, BootID                   string
	Epoch                               uint64
	AlreadyPublished                    bool
}

// PrepareDispatch atomically claims only the global FIFO head for the current
// Lintel binding. start_dispatched_at is a durable unknown-outcome fence: a
// send failure must not turn a possibly delivered Start back into Queued.
func (service *Service) PrepareDispatch(ctx context.Context, operationID int64, bootID string, epoch uint64) (DispatchInput, error) {
	if operationID < 1 || bootID == "" || epoch == 0 {
		return DispatchInput{}, ErrInvalid
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return DispatchInput{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return DispatchInput{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var state, dispatchedBoot string
	var dispatchedEpoch uint64
	if err = conn.QueryRowContext(ctx, `SELECT state,COALESCE(lintel_boot_id,''),COALESCE(lintel_connection_epoch,0) FROM browser_operations WHERE id=?`, operationID).Scan(&state, &dispatchedBoot, &dispatchedEpoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DispatchInput{}, ErrNotFound
		}
		return DispatchInput{}, err
	}
	if state == "Starting" {
		// Unknown-outcome Start may be replayed only on the same boot after a
		// reconnect. Preserve the original audit binding while sending on the
		// new active epoch; Lintel's Start is keyed by operation ID.
		if dispatchedBoot != bootID || epoch < dispatchedEpoch {
			return DispatchInput{}, ErrConflict
		}
	} else {
		if state != "Queued" && state != "WaitingForCapacity" {
			return DispatchInput{}, ErrConflict
		}
		// Dispatch is a second authorization boundary. A queued manual login
		// must not escape a Session revoke that committed after HTTP accepted the
		// request but before this durable Start fence.
		var manualLoginCount int
		if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_operations o JOIN sessions s ON s.id=o.actor_session_id JOIN users u ON u.id=s.user_id WHERE o.id=? AND o.kind='manual_login' AND s.revoked_at IS NULL AND u.enabled=1 AND s.auth_revision_at_issue=u.auth_revision`, operationID).Scan(&manualLoginCount); err != nil {
			return DispatchInput{}, err
		}
		var kind string
		if err = conn.QueryRowContext(ctx, `SELECT kind FROM browser_operations WHERE id=?`, operationID).Scan(&kind); err != nil {
			return DispatchInput{}, err
		}
		if kind == "manual_login" && manualLoginCount != 1 {
			return DispatchInput{}, ErrSessionRevoked
		}
		var earlier int
		if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_operations WHERE id < ? AND state IN ('Queued','WaitingForCapacity')`, operationID).Scan(&earlier); err != nil {
			return DispatchInput{}, err
		}
		if earlier != 0 {
			return DispatchInput{}, ErrConflict
		}
		now := service.now().UTC().Format(time.RFC3339Nano)
		result, updateErr := conn.ExecContext(ctx, `UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id=?,lintel_connection_epoch=?,row_version=row_version+1 WHERE id=? AND state IN ('Queued','WaitingForCapacity')`, now, bootID, epoch, operationID)
		if updateErr != nil {
			return DispatchInput{}, updateErr
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return DispatchInput{}, ErrConflict
		}
	}
	var input DispatchInput
	var profile, profileGeneration, actorUser, actorSession sql.NullInt64
	var params string
	if err = conn.QueryRowContext(ctx, `SELECT o.id,o.identity_id,o.identity_revision_id,o.actor_user_id,o.actor_session_id,o.profile_generation_id,g.generation,o.kind,o.journey_catalog_digest,o.journey_catalog_version,o.requested_at,r.start_url,r.probe_journey_id,r.probe_journey_version,r.probe_params_json FROM browser_operations o JOIN browser_identity_revisions r ON r.id=o.identity_revision_id LEFT JOIN browser_profile_generations g ON g.id=o.profile_generation_id WHERE o.id=?`, operationID).Scan(&input.OperationID, &input.IdentityID, &input.RevisionID, &actorUser, &actorSession, &profile, &profileGeneration, &input.Kind, &input.CatalogDigest, &input.CatalogVersion, &input.RequestedAt, &input.StartURL, &input.Probe.JourneyID, &input.Probe.Version, &params); err != nil {
		return DispatchInput{}, err
	}
	if profile.Valid {
		input.ProfileGenerationID = profile.Int64
	}
	if actorUser.Valid {
		input.ActorUserID = &actorUser.Int64
	}
	if actorSession.Valid {
		input.ActorSessionID = &actorSession.Int64
	}
	if input.Kind == "manual_login" && (input.ActorUserID == nil || input.ActorSessionID == nil) {
		return DispatchInput{}, ErrInvalid
	}
	if input.Kind == "authentication_probe" && (input.ActorUserID != nil || input.ActorSessionID != nil || !profileGeneration.Valid) {
		return DispatchInput{}, ErrInvalid
	}
	input.Probe.Params = json.RawMessage(params)
	input.BootID, input.Epoch = bootID, epoch
	input.CanonicalJSON, err = json.Marshal(struct {
		SchemaKind  string `json:"schemaKind"`
		OperationID int64  `json:"operationId"`
		Identity    struct {
			IdentityID          int64  `json:"identityId"`
			IdentityRevisionID  int64  `json:"identityRevisionId"`
			ProfileGenerationID *int64 `json:"profileGenerationId"`
			StartURL            string `json:"startUrl"`
		} `json:"identity"`
		ActorUserID         *int64       `json:"actorUserId,omitempty"`
		AuthenticationProbe *ProbeConfig `json:"authenticationProbe,omitempty"`
		Probe               *ProbeConfig `json:"probe,omitempty"`
	}{SchemaKind: map[bool]string{true: "manual_login_v1", false: "authentication_probe_v1"}[input.Kind == "manual_login"], OperationID: input.OperationID})
	if err != nil {
		return DispatchInput{}, err
	}
	// Build the full JSON explicitly so the configured probe and identity
	// binding are included; the typed struct above only selects the schema kind.
	identity := map[string]any{"identityId": input.IdentityID, "identityRevisionId": input.RevisionID, "startUrl": input.StartURL}
	if profile.Valid {
		identity["profileGenerationId"] = profile.Int64
	}
	if profileGeneration.Valid {
		identity["profileGeneration"] = profileGeneration.Int64
	}
	if input.Kind == "manual_login" {
		input.CanonicalJSON, err = json.Marshal(map[string]any{"schemaKind": "manual_login_v1", "operationId": operationID, "identity": identity, "actorUserId": input.ActorUserID, "actorSessionId": input.ActorSessionID, "authenticationProbe": map[string]any{"id": input.Probe.JourneyID, "version": input.Probe.Version, "params": json.RawMessage(input.Probe.Params), "catalog": map[string]any{"digest": input.CatalogDigest, "version": input.CatalogVersion}}})
	} else if input.Kind == "authentication_probe" {
		input.CanonicalJSON, err = json.Marshal(map[string]any{"schemaKind": "authentication_probe_v1", "operationId": operationID, "phase": "revision_change", "identity": identity, "probe": map[string]any{"id": input.Probe.JourneyID, "version": input.Probe.Version, "params": json.RawMessage(input.Probe.Params), "catalog": map[string]any{"digest": input.CatalogDigest, "version": input.CatalogVersion}}})
	} else {
		err = ErrInvalid
	}
	if err != nil {
		return DispatchInput{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return DispatchInput{}, err
	}
	committed = true
	return input, nil
}

func (service *Service) HandleStartAck(ctx context.Context, operationID int64, bootID string, epoch uint64, accepted bool, reason string, startedAt time.Time) error {
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
	var state, storedBoot, kind string
	var storedEpoch uint64
	if err = conn.QueryRowContext(ctx, `SELECT state,lintel_boot_id,lintel_connection_epoch,kind FROM browser_operations WHERE id=?`, operationID).Scan(&state, &storedBoot, &storedEpoch, &kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	// The initial epoch is an immutable dispatch audit binding. A same-boot
	// reconnect may return its stable Ack on a later epoch; reject only another
	// boot or an epoch older than the original dispatch.
	if storedBoot != bootID || epoch < storedEpoch {
		return ErrConflict
	}
	if state != "Starting" {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		committed = true
		return nil
	}
	now := service.now().UTC().Format(time.RFC3339Nano)
	if accepted {
		if startedAt.IsZero() {
			return ErrInvalid
		}
		_, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='Running',started_at=?,row_version=row_version+1 WHERE id=? AND state='Starting'`, startedAt.UTC().Format(time.RFC3339Nano), operationID)
	} else if reason == "no_capacity" {
		_, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='WaitingForCapacity',row_version=row_version+1 WHERE id=? AND state='Starting'`, operationID)
	} else {
		if reason == "" {
			reason = "internal"
		}
		terminal := "runtime_unavailable"
		switch reason {
		case "identity_busy", "input_unsupported", "reconcile_required", "stale_stream", "internal":
			terminal = "protocol_error"
		case "authentication_required":
			if kind == "authentication_probe" {
				terminal = "authentication_required"
			}
		case "profile_unavailable":
			if kind == "authentication_probe" {
				// The detailed inventory classification is durable evidence; until it
				// is available, profile_missing is the conservative closed result.
				terminal = "profile_missing"
			}
		}
		_, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='Failed',ended_at=?,terminal_reason=?,start_rejected_at=?,start_reject_reason=?,stop_confirmed_at=?,stop_confirmation_basis='start_rejected',row_version=row_version+1 WHERE id=? AND state='Starting'`, now, terminal, now, reason, now, operationID)
	}
	if err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// PreparePublish persists idempotency before the control message leaves Quoin.
func (service *Service) PreparePublish(ctx context.Context, systemKey string, operationID, actorID, expectedVersion int64, commandID string) (PublishRequest, error) {
	if commandID == "" {
		return PublishRequest{}, ErrInvalid
	}
	digest := commandDigest(systemKey, operationID, expectedVersion)
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return PublishRequest{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return PublishRequest{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var r PublishRequest
	replayed, _, err := replayCommand(ctx, conn, actorID, commandID, "publish_browser_profile", digest)
	if err != nil {
		return PublishRequest{}, err
	}
	if replayed {
		// The command was accepted but Lintel's result may have been lost.
		// Reconstruct the same deterministic generation for a safe resend.
		var profileID sql.NullInt64
		var maxGeneration uint64
		if err = conn.QueryRowContext(ctx, `SELECT o.id,o.identity_id,o.identity_revision_id,i.current_profile_generation_id,COALESCE((SELECT MAX(generation) FROM browser_profile_generations WHERE identity_id=o.identity_id),0) FROM browser_operations o JOIN browser_identities i ON i.id=o.identity_id JOIN business_systems s ON s.id=i.business_system_id WHERE o.id=? AND s.key=?`, operationID, systemKey).Scan(&r.OperationID, &r.IdentityID, &r.RevisionID, &profileID, &maxGeneration); err != nil {
			return PublishRequest{}, err
		}
		if profileID.Valid {
			r.ExpectedGenerationID = profileID.Int64
		}
		var published int
		if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_profile_generations WHERE published_operation_id=?`, operationID).Scan(&published); err != nil {
			return PublishRequest{}, err
		}
		r.AlreadyPublished = published == 1
		r.NewGeneration = maxGeneration + 1
		r.CommandID = commandID
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return PublishRequest{}, err
		}
		committed = true
		return r, nil
	}
	var userID sql.NullInt64
	err = conn.QueryRowContext(ctx, `SELECT o.id,o.identity_id,o.identity_revision_id,o.actor_user_id,o.state,o.row_version,COALESCE(i.current_profile_generation_id,0),COALESCE((SELECT MAX(generation) FROM browser_profile_generations WHERE identity_id=o.identity_id),0)+1 FROM browser_operations o JOIN browser_identities i ON i.id=o.identity_id JOIN business_systems s ON s.id=i.business_system_id WHERE o.id=? AND s.key=?`, operationID, systemKey).Scan(&r.OperationID, &r.IdentityID, &r.RevisionID, &userID, new(string), new(int64), &r.ExpectedGenerationID, &r.NewGeneration)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PublishRequest{}, ErrNotFound
		}
		return PublishRequest{}, err
	}
	var state string
	var version int64
	if err = conn.QueryRowContext(ctx, `SELECT state,row_version FROM browser_operations WHERE id=?`, operationID).Scan(&state, &version); err != nil {
		return PublishRequest{}, err
	}
	if !userID.Valid || userID.Int64 != actorID {
		return PublishRequest{}, ErrNotFound
	}
	if version != expectedVersion {
		return PublishRequest{}, &RowVersionError{Current: version}
	}
	if state != "Running" {
		return PublishRequest{}, ErrConflict
	}
	now := service.now().UTC().Format(time.RFC3339Nano)
	if err = recordCommand(ctx, conn, actorID, commandID, "publish_browser_profile", digest, "browser_operation", operationID, now); err != nil {
		return PublishRequest{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return PublishRequest{}, err
	}
	committed = true
	r.CommandID = commandID
	return r, nil
}
