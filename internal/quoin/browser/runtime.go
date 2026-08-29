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

// PrepareDispatch preserves the legacy unlimited-capacity seam for focused
// durable-state tests. Production dispatch always calls
// PrepareDispatchWithCapacity with Lintel's Hello-frozen slot total.
func (service *Service) PrepareDispatch(ctx context.Context, operationID int64, bootID string, epoch uint64) (DispatchInput, error) {
	return service.prepareDispatch(ctx, operationID, bootID, epoch, ^uint32(0))
}

// PrepareDispatchWithCapacity atomically claims the global FIFO head only when
// Quoin's current Lintel capacity projection has a free physical slot.
func (service *Service) PrepareDispatchWithCapacity(ctx context.Context, operationID int64, bootID string, epoch uint64, capacity uint32) (DispatchInput, error) {
	if capacity == 0 {
		return DispatchInput{}, ErrRuntimeOffline
	}
	return service.prepareDispatch(ctx, operationID, bootID, epoch, capacity)
}

// prepareDispatch is the shared durable-fence implementation. start_dispatched_at
// is an unknown-outcome fence: a send failure must not turn a possibly delivered
// Start back into Queued.
func (service *Service) prepareDispatch(ctx context.Context, operationID int64, bootID string, epoch uint64, capacity uint32) (DispatchInput, error) {
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
	var state, dispatchedBoot, ownerState, kind string
	var dispatchedEpoch uint64
	if err = conn.QueryRowContext(ctx, `SELECT o.state,COALESCE(o.lintel_boot_id,''),COALESCE(o.lintel_connection_epoch,0),o.kind,COALESCE(parent.state,'')
		FROM browser_operations o LEFT JOIN execution_attempts parent ON parent.id=o.owner_attempt_id WHERE o.id=?`, operationID).Scan(&state, &dispatchedBoot, &dispatchedEpoch, &kind, &ownerState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DispatchInput{}, ErrNotFound
		}
		return DispatchInput{}, err
	}
	// An accepted browser child is durable before its Plinth Ack is delivered.
	// If the parent reaches a natural terminal state in that narrow interval,
	// never send a new Start: its cancellation trigger owns child closure and
	// reconciliation will release any already-live operation.
	if kind == "exploration" && ownerState != "Running" {
		return DispatchInput{}, ErrConflict
	}
	// The initial Journey Start requires a Queued owner. Its unknown-outcome
	// same-boot replay occurs after dispatchBrowserOperation has bound that owner
	// to Lintel (Assigned), so retain that exact state only for Starting replay.
	if kind == "journey" && ownerState != "Queued" && !(state == "Starting" && ownerState == "Assigned") {
		return DispatchInput{}, ErrConflict
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
		// Starting, live, reconnecting and terminal-but-unconfirmed operations
		// occupy a physical slot. WaitingForCapacity does not: an explicit
		// NO_CAPACITY acknowledgement proves no process was created.
		var occupied uint64
		if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_operations WHERE id<>? AND (state IN ('Starting','Running','AwaitingReconnect') OR (state IN ('Succeeded','Failed','Cancelled','Interrupted') AND start_dispatched_at IS NOT NULL AND stop_confirmed_at IS NULL))`, operationID).Scan(&occupied); err != nil {
			return DispatchInput{}, err
		}
		if occupied >= uint64(capacity) {
			if state == "Queued" {
				if _, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='WaitingForCapacity',row_version=row_version+1 WHERE id=? AND state='Queued'`, operationID); err != nil {
					return DispatchInput{}, err
				}
			}
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return DispatchInput{}, err
			}
			committed = true
			return DispatchInput{}, ErrCapacityUnavailable
		}
		now := service.now().UTC().Format(time.RFC3339Nano)
		var result sql.Result
		if state == "WaitingForCapacity" && dispatchedBoot == bootID {
			// Preserve the first Start's audit binding after Lintel explicitly
			// reported NO_CAPACITY. The resend travels on a later epoch of this
			// same boot, which HandleStartAck permits.
			result, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='Starting',row_version=row_version+1 WHERE id=? AND state='WaitingForCapacity'`, operationID)
		} else if state == "WaitingForCapacity" && dispatchedBoot != "" {
			// NO_CAPACITY proves that the old boot created no process. A successor
			// boot may therefore take over this still-unstarted physical attempt;
			// keep the original dispatch timestamp but replace its stale fence.
			result, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='Starting',lintel_boot_id=?,lintel_connection_epoch=?,row_version=row_version+1 WHERE id=? AND state='WaitingForCapacity'`, bootID, epoch, operationID)
		} else {
			result, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id=?,lintel_connection_epoch=?,row_version=row_version+1 WHERE id=? AND state IN ('Queued','WaitingForCapacity')`, now, bootID, epoch, operationID)
		}
		if err != nil {
			return DispatchInput{}, err
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
	if (input.Kind == "authentication_probe" || input.Kind == "exploration" || input.Kind == "journey") && (input.ActorUserID != nil || input.ActorSessionID != nil || !profileGeneration.Valid) {
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
	} else if input.Kind == "exploration" {
		input.CanonicalJSON, err = json.Marshal(map[string]any{"schemaKind": "exploration_v1", "operationId": operationID, "identity": identity, "authenticationProbe": map[string]any{"id": input.Probe.JourneyID, "version": input.Probe.Version, "params": json.RawMessage(input.Probe.Params), "catalog": map[string]any{"digest": input.CatalogDigest, "version": input.CatalogVersion}}})
	} else if input.Kind == "journey" {
		input.CanonicalJSON, err = service.buildJourneyOperationInput(ctx, conn, input, identity)
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

// buildJourneyOperationInput assembles the frozen inspection_collection_v1
// Start payload of a journey operation from the same durable rows that froze
// the child Attempt's snapshot (the operation's own catalog binding is the
// execution authority, DATA-BROWSER-001).
func (service *Service) buildJourneyOperationInput(ctx context.Context, conn *sql.Conn, input DispatchInput, identity map[string]any) ([]byte, error) {
	var ownerAttempt sql.NullInt64
	var journeyID sql.NullString
	var journeyVersion sql.NullInt64
	var planKey, checkKey, params string
	if err := conn.QueryRowContext(ctx, `
		SELECT o.owner_attempt_id,o.journey_id,o.journey_version,COALESCE(a.plan_key,r.plan_key),a.check_key,COALESCE(c.journey_params_json,'{}')
		FROM browser_operations o
		JOIN execution_attempts a ON a.id=o.owner_attempt_id
		LEFT JOIN config_verification_runs t ON t.id=a.scope_id AND a.scope_type='config_verification_run'
		LEFT JOIN inspection_runs r ON r.id=a.scope_id AND a.scope_type='run_check'
		JOIN config_plans p ON p.config_version_id=COALESCE(t.config_version_id,r.config_version_id) AND p.plan_key=COALESCE(a.plan_key,r.plan_key)
		JOIN config_checks c ON c.plan_id=p.id AND c.check_key=a.check_key AND c.kind='browser'
		WHERE o.id=? AND a.scope_type IN ('config_verification_run','run_check')`, input.OperationID).Scan(&ownerAttempt, &journeyID, &journeyVersion, &planKey, &checkKey, &params); err != nil {
		return nil, err
	}
	if !ownerAttempt.Valid || ownerAttempt.Int64 < 1 || !journeyID.Valid || !journeyVersion.Valid || journeyVersion.Int64 < 1 {
		return nil, ErrInvalid
	}
	return json.Marshal(map[string]any{
		"schemaKind": "inspection_collection_v1", "attemptId": ownerAttempt.Int64, "operationId": input.OperationID,
		"identity":            identity,
		"journey":             map[string]any{"id": journeyID.String, "version": journeyVersion.Int64, "params": json.RawMessage(params), "catalog": map[string]any{"digest": input.CatalogDigest, "version": input.CatalogVersion}},
		"authenticationProbe": map[string]any{"id": input.Probe.JourneyID, "version": input.Probe.Version, "params": json.RawMessage(input.Probe.Params), "catalog": map[string]any{"digest": input.CatalogDigest, "version": input.CatalogVersion}},
		"planKey":             planKey, "checkKey": checkKey,
	})
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
		case "identity_busy", "input_unsupported", "reconcile_required", "stale_stream", "download_blocked", "internal":
			terminal = "protocol_error"
		case "authentication_required":
			if kind == "authentication_probe" {
				terminal = "authentication_required"
			}
		case "profile_unavailable":
			if kind == "authentication_probe" || kind == "exploration" {
				// The inventory report is the durable authority for why the frozen
				// profile cannot be opened. Preserve its exact classification instead
				// of collapsing a corrupt manifest or Chromium mismatch into missing.
				terminal = profileUnavailableTerminalReason(ctx, conn, operationID, bootID, epoch)
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

// profileUnavailableTerminalReason maps only the current boot's durable
// inventory observation to a Browser Operation terminal reason. If a start
// rejection races a completed inventory report, profile_missing remains the
// conservative reason; it never fabricates a more specific diagnosis.
func profileUnavailableTerminalReason(ctx context.Context, conn *sql.Conn, operationID int64, bootID string, epoch uint64) string {
	var result string
	err := conn.QueryRowContext(ctx, `SELECT r.result
		FROM browser_operations o
		JOIN browser_profile_reconciliations r ON r.profile_generation_id=o.profile_generation_id
		WHERE o.id=? AND r.boot_id=? AND r.connection_epoch<=?
		ORDER BY r.id DESC LIMIT 1`, operationID, bootID, epoch).Scan(&result)
	if err != nil {
		return "profile_missing"
	}
	switch result {
	case "manifest_invalid":
		return "profile_manifest_invalid"
	case "chromium_revision_mismatch":
		return "chromium_revision_mismatch"
	case "missing":
		return "profile_missing"
	default:
		return "profile_missing"
	}
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
