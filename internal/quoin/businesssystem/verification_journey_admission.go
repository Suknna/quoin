package businesssystem

// Identity-serial admission of Config Verification browser children (T23,
// DATA-BROWSER-003/006): one Business System carries exactly one Browser
// Identity, so a run's browser checks execute one after another — each child
// receives its journey operation and its frozen inspection_collection_v1
// snapshot only when the identity carries no active or stop-unconfirmed
// operation.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	quoinconfig "github.com/Suknna/quoin/internal/quoin/config"
)

// AdmitNextJourneyChild admits the next identity-free browser child of a
// Running Config Verification Run: one shared Browser Identity serializes a
// run's browser checks, so each child gets its journey operation (and its
// frozen inspection_collection_v1 snapshot carrying that operation binding)
// only when the identity carries no active or stop-unconfirmed operation.
// Returns true when a child was admitted.
func (service *Service) AdmitNextJourneyChild(ctx context.Context) (bool, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	admitted, err := admitNextJourneyChildOn(ctx, conn, service.nowText())
	if err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, err
	}
	committed = true
	return admitted, nil
}

func admitNextJourneyChildOn(ctx context.Context, conn *sql.Conn, now string) (bool, error) {
	var attemptID int64
	var runID, configVersionID, contractID int64
	err := conn.QueryRowContext(ctx, `
		SELECT a.id, a.scope_id, t.config_version_id, t.label_contract_version_id
		FROM execution_attempts a
		JOIN config_verification_runs t ON t.id=a.scope_id AND t.state='Running'
		JOIN config_plans p ON p.config_version_id=t.config_version_id AND p.plan_key=a.plan_key
		JOIN config_checks c ON c.plan_id=p.id AND c.check_key=a.check_key AND c.kind='browser'
		JOIN browser_identities i ON i.business_system_id=t.business_system_id
		WHERE a.attempt_type='inspection_collection' AND a.scope_type='config_verification_run'
		  AND a.state='Queued'
		  AND NOT EXISTS (SELECT 1 FROM browser_operations o WHERE o.owner_attempt_id=a.id)
		  AND NOT EXISTS (SELECT 1 FROM config_verification_run_check_results r WHERE r.attempt_id=a.id)
		  AND NOT EXISTS (
			SELECT 1 FROM browser_operations active
			WHERE active.identity_id=i.id
			  AND (active.state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect') OR active.stop_confirmed_at IS NULL)
		  )
		ORDER BY a.id LIMIT 1`).Scan(&attemptID, &runID, &configVersionID, &contractID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	identity, input, err := rebuildJourneyAdmissionFacts(ctx, conn, attemptID)
	if err != nil {
		return false, err
	}
	if !identity.ProfileGenerationID.Valid || !identity.Generation.Valid {
		// The profile vanished between run creation and admission: settle the
		// deterministic authentication_required gap instead of waiting.
		if _, err := conn.ExecContext(ctx, `UPDATE execution_attempts SET state='Failed', ended_at=?, row_version=row_version+1 WHERE id=? AND state='Queued'`, now, attemptID); err != nil {
			return false, err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO config_verification_run_check_results(verification_run_id,plan_key,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at) VALUES(?,?,?,'gap',NULL,?,NULL,'authentication_required',?)`, runID, input.PlanKey, input.CheckKey, attemptID, now); err != nil {
			return false, err
		}
		if err := convergeVerificationRunOn(ctx, conn, runID); err != nil {
			return false, err
		}
		return true, nil
	}
	var busy int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM browser_operations WHERE identity_id=? AND (state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect') OR stop_confirmed_at IS NULL)`,
		identity.IdentityID).Scan(&busy); err != nil {
		return false, err
	}
	if busy > 0 {
		// Another operation (from this run's earlier check or an external
		// source) still owns the identity: retry on its Stop fence.
		return false, nil
	}
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO browser_operations(identity_id,identity_revision_id,profile_generation_id,owner_attempt_id,kind,actor_user_id,actor_session_id,verification_manifest_item_id,clone_identity,state,journey_catalog_digest,journey_catalog_version,journey_id,journey_version,probe_phase,requested_at)
		VALUES(?,?,?,?, 'journey',NULL,NULL,NULL,NULL,'Queued',?,?,?,?,NULL,?)`,
		identity.IdentityID, identity.RevisionID, identity.ProfileGenerationID.Int64, attemptID,
		input.Journey.Catalog.Digest, input.Journey.Catalog.Version, input.Journey.ID, input.Journey.Version, now)
	if err != nil {
		return false, err
	}
	operationID, err := insert.LastInsertId()
	if err != nil {
		return false, err
	}
	input.Identity.ProfileGenerationID = identity.ProfileGenerationID.Int64
	input.Identity.ProfileGeneration = identity.Generation.Int64
	input.OperationID = &operationID
	if err := freezeJourneyVerificationInput(ctx, conn, attemptID, configVersionID, contractID, input, now); err != nil {
		return false, err
	}
	return true, nil
}

// rebuildJourneyAdmissionFacts rebuilds one browser child's frozen input facts
// (without the operation binding) plus its identity snapshot for admission.
func rebuildJourneyAdmissionFacts(ctx context.Context, conn *sql.Conn, attemptID int64) (browserIdentitySnapshot, journeyVerificationInput, error) {
	var input journeyVerificationInput
	var identity browserIdentitySnapshot
	var planKey, checkKey, journeyID, params, probeParams, startURL string
	var identityID, revisionID, probeVersion int64
	var profileID, generation sql.NullInt64
	err := conn.QueryRowContext(ctx, `
		SELECT a.plan_key,a.check_key,c.journey_id,COALESCE(c.journey_params_json,'{}'),
		       i.id,r.id,i.current_profile_generation_id,g.generation,r.start_url,
		       r.probe_journey_id,r.probe_journey_version,COALESCE(r.probe_params_json,'{}')
		FROM execution_attempts a
		JOIN config_verification_runs t ON t.id=a.scope_id
		JOIN config_plans p ON p.config_version_id=t.config_version_id AND p.plan_key=a.plan_key
		JOIN config_checks c ON c.plan_id=p.id AND c.check_key=a.check_key AND c.kind='browser'
		JOIN browser_identities i ON i.business_system_id=t.business_system_id
		JOIN browser_identity_revisions r ON r.id=i.current_revision_id
		LEFT JOIN browser_profile_generations g ON g.id=i.current_profile_generation_id
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='config_verification_run'`, attemptID).
		Scan(&planKey, &checkKey, &journeyID, &params, &identityID, &revisionID, &profileID, &generation, &startURL, &identity.ProbeJourneyID, &probeVersion, &probeParams)
	if err != nil {
		return identity, input, err
	}
	identity.IdentityID, identity.RevisionID = identityID, revisionID
	identity.ProfileGenerationID, identity.Generation = profileID, generation
	identity.StartURL, identity.ProbeVersion, identity.ProbeParams = startURL, probeVersion, probeParams
	_, catalogVersion, catalogDigest, err := quoinconfig.JourneyCatalog()
	if err != nil {
		return identity, input, err
	}
	journeyVersion := quoinconfig.JourneyVersion(journeyID)
	input = journeyVerificationInput{
		SchemaKind: journeyVerificationSchemaKind, AttemptID: attemptID,
		Identity:            journeyVerificationIdentity{IdentityID: identityID, IdentityRevisionID: revisionID, StartURL: startURL},
		Journey:             journeyVerificationBinding{ID: journeyID, Version: journeyVersion, Params: json.RawMessage(params), Catalog: journeyCatalogRef{Digest: catalogDigest, Version: catalogVersion}},
		AuthenticationProbe: journeyVerificationBinding{ID: identity.ProbeJourneyID, Version: probeVersion, Params: json.RawMessage(probeParams), Catalog: journeyCatalogRef{Digest: catalogDigest, Version: catalogVersion}},
		PlanKey:             planKey, CheckKey: checkKey,
	}
	return identity, input, nil
}

// admitLocalGapIfAny settles the deterministic operation-less gaps of one
// browser child at run creation. It returns dispatched=false for children
// that must wait for identity-serial journey admission.
func admitLocalGapIfAny(ctx context.Context, conn *sql.Conn, runID, attemptID int64, identity browserIdentitySnapshot, identityBusy bool, input journeyVerificationInput, now string) (journeyVerificationInput, bool, error) {
	if !identity.ProfileGenerationID.Valid || !identity.Generation.Valid {
		// No published profile can ever exist for this frozen binding: the
		// journey operation schema forbids a null profile generation. Settle
		// deterministically instead of holding the run's active fence.
		if _, err := conn.ExecContext(ctx, `UPDATE execution_attempts SET state='Failed', ended_at=?, row_version=row_version+1 WHERE id=? AND state='Queued'`, now, attemptID); err != nil {
			return input, false, err
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO config_verification_run_check_results(verification_run_id,plan_key,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at) VALUES(?,?,?,'gap',NULL,?,NULL,'authentication_required',?)`, runID, input.PlanKey, input.CheckKey, attemptID, now)
		return input, false, err
	}
	if identityBusy {
		// DATA-BROWSER-003: Inspection/Config Verification never queues behind
		// an occupied identity. The local identity_busy ResultProposal is the
		// one operation-less legal gap; its digest closes the queued child.
		proposal, err := identityBusyProposal(attemptID, input.PlanKey, input.CheckKey)
		if err != nil {
			return input, false, err
		}
		digest := sha256.Sum256(proposal)
		_, err = conn.ExecContext(ctx, `INSERT INTO config_verification_run_check_results(verification_run_id,plan_key,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at) VALUES(?,?,?,'gap',NULL,?,?,'identity_busy',?)`, runID, input.PlanKey, input.CheckKey, attemptID, digest[:], now)
		return input, false, err
	}
	// Dispatchable children wait for identity-serial admission
	// (AdmitNextJourneyChild), which creates their operation and freezes
	// their snapshot together.
	return input, false, nil
}

// identityBusyProposal composes the canonical operation-less
// browser_journey_result_v1 identity_busy ResultProposal whose digest the
// check result retains (DATA-BROWSER-006).
func identityBusyProposal(attemptID int64, planKey, checkKey string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"schemaKind": "browser_journey_result_v1", "attemptId": attemptID, "operationId": nil,
		"outcome": "gap", "probeResults": []any{}, "evidence": []any{},
		"traceArtifactId": nil, "traceIntegrity": nil, "gapCode": "identity_busy",
		"originalGapCode": nil, "terminalReason": nil,
		"errorDetail": fmt.Sprintf("browser identity is busy; check %s/%s settled as identity_busy", planKey, checkKey),
	})
}

func browserVerificationChecks(ctx context.Context, conn *sql.Conn, configVersionID int64) ([]browserVerificationCheck, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT p.plan_key,c.check_key,c.journey_id,COALESCE(c.journey_params_json,'{}')
		FROM config_checks c JOIN config_plans p ON p.id=c.plan_id
		WHERE p.config_version_id=? AND c.kind='browser'
		ORDER BY p.plan_key,c.check_key`, configVersionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var checks []browserVerificationCheck
	for rows.Next() {
		var check browserVerificationCheck
		if err := rows.Scan(&check.PlanKey, &check.CheckKey, &check.JourneyID, &check.Params); err != nil {
			return nil, err
		}
		checks = append(checks, check)
	}
	return checks, rows.Err()
}

func loadBrowserIdentitySnapshot(ctx context.Context, conn *sql.Conn, systemID int64) (browserIdentitySnapshot, error) {
	var snapshot browserIdentitySnapshot
	err := conn.QueryRowContext(ctx, `
		SELECT i.id,i.current_revision_id,i.current_profile_generation_id,g.generation,i.state,r.start_url,r.probe_journey_id,r.probe_journey_version,COALESCE(r.probe_params_json,'{}')
		FROM browser_identities i JOIN browser_identity_revisions r ON r.id=i.current_revision_id
		LEFT JOIN browser_profile_generations g ON g.id=i.current_profile_generation_id
		WHERE i.business_system_id=?`, systemID).
		Scan(&snapshot.IdentityID, &snapshot.RevisionID, &snapshot.ProfileGenerationID, &snapshot.Generation, &snapshot.State, &snapshot.StartURL, &snapshot.ProbeJourneyID, &snapshot.ProbeVersion, &snapshot.ProbeParams)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, ErrBrowserIdentityMissing
	}
	return snapshot, err
}

// freezeJourneyVerificationInput seals the child's immutable
// inspection_collection_v1 snapshot and its config/contract lineage.
func freezeJourneyVerificationInput(ctx context.Context, conn *sql.Conn, attemptID, configVersionID, contractID int64, input journeyVerificationInput, now string) error {
	canonical, err := marshalJourneyVerificationInput(input)
	if err != nil {
		return err
	}
	if err := validateJourneyVerificationShape(canonical); err != nil {
		return fmt.Errorf("browser check input violates the frozen journey contract: %w; input=%s", err, canonical)
	}
	digest := sha256.Sum256(canonical)
	result, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at)
		VALUES(?,?,?,?,?)`, attemptID, journeyVerificationSchemaKind, "inspection_collection_v1", hex.EncodeToString(digest[:]), now)
	if err != nil {
		return err
	}
	snapshotID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	configDigest := sha256.Sum256(fmt.Appendf(nil, "business-system-config-version:%d", configVersionID))
	if _, err := conn.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,business_system_config_version_id) VALUES(?,1,'config_version',?,?)`, snapshotID, hex.EncodeToString(configDigest[:]), configVersionID); err != nil {
		return err
	}
	contractDigest := sha256.Sum256(fmt.Appendf(nil, "label-contract-version:%d", contractID))
	_, err = conn.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,label_contract_version_id) VALUES(?,2,'label_contract',?,?)`, snapshotID, hex.EncodeToString(contractDigest[:]), contractID)
	return err
}

// rebuildJourneyVerificationInput reconstructs the frozen canonical bytes of
// one browser-check child from durable rows (RUNTIME-TASK-011). The journey
// binding comes from the operation's frozen columns — never from the current
// embedded catalog — and the rebuilt bytes must match the sealed digest.
func (service *Service) rebuildJourneyVerificationInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var check struct {
		PlanKey, CheckKey, JourneyID, Params      string
		IdentityID, RevisionID, ProfileGeneration int64
		StartURL, ProbeJourneyID, ProbeParams     string
		ProbeVersion, JourneyVersion              int64
		CatalogDigest, CatalogVersion             string
		ProfileID                                 sql.NullInt64
		OperationID                               sql.NullInt64
	}
	err := service.db.QueryRowContext(ctx, `
		SELECT a.plan_key,a.check_key,c.journey_id,COALESCE(c.journey_params_json,'{}'),
		       COALESCE(o.identity_id,i.id),COALESCE(o.identity_revision_id,i.current_revision_id),g.generation,r.start_url,
		       r.probe_journey_id,r.probe_journey_version,COALESCE(r.probe_params_json,'{}'),
		       COALESCE(o.journey_version,0),COALESCE(o.journey_catalog_digest,''),COALESCE(o.journey_catalog_version,''),
		       COALESCE(o.profile_generation_id,i.current_profile_generation_id),o.id
		FROM execution_attempts a
		JOIN config_verification_runs t ON t.id=a.scope_id
		JOIN config_plans p ON p.config_version_id=t.config_version_id AND p.plan_key=a.plan_key
		JOIN config_checks c ON c.plan_id=p.id AND c.check_key=a.check_key AND c.kind='browser'
		JOIN browser_identities i ON i.business_system_id=t.business_system_id
		LEFT JOIN browser_operations o ON o.owner_attempt_id=a.id AND o.kind='journey'
		JOIN browser_identity_revisions r ON r.id=COALESCE(o.identity_revision_id,i.current_revision_id)
		LEFT JOIN browser_profile_generations g ON g.id=COALESCE(o.profile_generation_id,i.current_profile_generation_id)
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='config_verification_run'`, attemptID).
		Scan(&check.PlanKey, &check.CheckKey, &check.JourneyID, &check.Params,
			&check.IdentityID, &check.RevisionID, &check.ProfileGeneration, &check.StartURL,
			&check.ProbeJourneyID, &check.ProbeVersion, &check.ProbeParams,
			&check.JourneyVersion, &check.CatalogDigest, &check.CatalogVersion,
			&check.ProfileID, &check.OperationID)
	if err != nil {
		return nil, err
	}
	if !check.OperationID.Valid {
		// Operation-less children never dispatch; the current embedded values
		// reproduce their sealed local-gap binding only for audit rebuilds.
		var _, catalogVersion, catalogDigest string
		_, catalogVersion, catalogDigest, err = quoinconfig.JourneyCatalog()
		if err != nil {
			return nil, err
		}
		check.CatalogDigest, check.CatalogVersion = catalogDigest, catalogVersion
		check.JourneyVersion = quoinconfig.JourneyVersion(check.JourneyID)
	}
	var profileID int64
	if check.ProfileID.Valid {
		profileID = check.ProfileID.Int64
	}
	input := journeyVerificationInput{
		SchemaKind: journeyVerificationSchemaKind, AttemptID: attemptID,
		Identity: journeyVerificationIdentity{
			IdentityID: check.IdentityID, IdentityRevisionID: check.RevisionID,
			ProfileGenerationID: profileID, ProfileGeneration: check.ProfileGeneration, StartURL: check.StartURL,
		},
		Journey: journeyVerificationBinding{
			ID: check.JourneyID, Version: check.JourneyVersion, Params: json.RawMessage(check.Params),
			Catalog: journeyCatalogRef{Digest: check.CatalogDigest, Version: check.CatalogVersion},
		},
		AuthenticationProbe: journeyVerificationBinding{
			ID: check.ProbeJourneyID, Version: check.ProbeVersion, Params: json.RawMessage(check.ProbeParams),
			Catalog: journeyCatalogRef{Digest: check.CatalogDigest, Version: check.CatalogVersion},
		},
		PlanKey: check.PlanKey, CheckKey: check.CheckKey,
	}
	if check.OperationID.Valid {
		operationID := check.OperationID.Int64
		input.OperationID = &operationID
	}
	canonical, err := marshalJourneyVerificationInput(input)
	if err != nil {
		return nil, err
	}
	var expected string
	if err := service.db.QueryRowContext(ctx, `SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&expected); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != expected {
		return nil, fmt.Errorf("journey verification input digest no longer matches frozen snapshot")
	}
	return canonical, nil
}
