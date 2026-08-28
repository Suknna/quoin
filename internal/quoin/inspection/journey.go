package inspection

// run_check browser-child journey machinery (T24, CFG-INSPECTRUN-001): bare
// Queued children wait for identity-serial admission, which creates their
// journey Browser Operation and freezes the operation-bound
// inspection_collection_v1 snapshot in one transaction. Results commit through
// the shared scope-parameterized browser_journey_results closure and converge
// the Run (starting its report analysis when collection completes).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/businesssystem"
)

// JourneyCore is wired by the app package to the shared frozen journey
// closure owned by the business-system service (browser_journey_results is
// the single commit entry for every domain).
type JourneyCore func(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte, scope businesssystem.JourneyScope) error

// AdmitNextJourneyChild identity-serially turns one bare Queued run_check
// browser child into an operation-bound Journey dispatch input. Local gaps
// have already frozen and terminalized at Run creation; only ready/free
// children reach this sweep.
func (s *Service) AdmitNextJourneyChild(ctx context.Context) (bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var attemptID, runID, versionID, contractID, systemID int64
	err = conn.QueryRowContext(ctx, `
		SELECT a.id,a.scope_id,r.config_version_id,r.label_contract_version_id,r.business_system_id
		FROM execution_attempts a
		JOIN inspection_runs r ON r.id=a.scope_id AND r.state='Running'
		JOIN config_plans p ON p.config_version_id=r.config_version_id AND p.plan_key=r.plan_key
		JOIN config_checks c ON c.plan_id=p.id AND c.check_key=a.check_key AND c.kind='browser'
		JOIN browser_identities i ON i.business_system_id=r.business_system_id
		WHERE a.attempt_type='inspection_collection' AND a.scope_type='run_check' AND a.state='Queued'
		  AND NOT EXISTS(SELECT 1 FROM attempt_input_snapshots x WHERE x.attempt_id=a.id)
		  AND NOT EXISTS(SELECT 1 FROM browser_operations o WHERE o.owner_attempt_id=a.id)
		  AND NOT EXISTS(SELECT 1 FROM browser_operations active WHERE active.identity_id=i.id
		    AND (active.state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect') OR active.stop_confirmed_at IS NULL))
		ORDER BY a.id LIMIT 1`).Scan(&attemptID, &runID, &versionID, &contractID, &systemID)
	if err == sql.ErrNoRows {
		_, _ = conn.ExecContext(ctx, "COMMIT")
		committed = true
		return false, nil
	}
	if err != nil {
		return false, err
	}
	identity, hasIdentity, err := loadBrowserIdentity(ctx, conn, systemID)
	if err != nil {
		return false, err
	}
	if !hasIdentity || !identity.ProfileGenerationID.Valid || !identity.Generation.Valid {
		return false, fmt.Errorf("run_check browser child lost ready identity during admission")
	}
	journey, err := loadJourneyFacts(ctx, conn, attemptID)
	if err != nil {
		return false, err
	}
	now := s.nowText()
	insert, err := conn.ExecContext(ctx, `INSERT INTO browser_operations(identity_id,identity_revision_id,profile_generation_id,owner_attempt_id,kind,actor_user_id,actor_session_id,verification_manifest_item_id,clone_identity,state,journey_catalog_digest,journey_catalog_version,journey_id,journey_version,probe_phase,requested_at)
		VALUES(?,?,?,?, 'journey',NULL,NULL,NULL,NULL,'Queued',?,?,?,?,NULL,?)`,
		identity.IdentityID, identity.RevisionID, identity.ProfileGenerationID.Int64, attemptID,
		journey.catalogDigest, journey.catalogVersion, journey.journeyID, journey.journeyVersion, now)
	if err != nil {
		return false, err
	}
	operationID, err := insert.LastInsertId()
	if err != nil {
		return false, err
	}
	// The shared builder renders the exact frozen bytes for both admission
	// and rebuild, so dispatch and result adjudication stay byte-equal.
	body, err := renderJourneyInspectionBody(journey, identity, attemptID, operationID)
	if err != nil {
		return false, err
	}
	if err = freezeInput(ctx, conn, attemptID, "inspection_collection_v1", body, versionID, contractID, now); err != nil {
		return false, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}
	committed = true
	return true, nil
}

// journeyFacts are the durable facts the frozen input renders from.
type journeyFacts struct {
	planKey       string
	checkKey      string
	journeyID     string
	journeyParams string
	// admission rebuilds these from the embedded catalog.
	catalogDigest    string
	catalogVersion   string
	journeyVersion   int64
	identityID       int64
	identityRevision int64
	profileID        sql.NullInt64
	profile          sql.NullInt64
	startURL         string
	probeJourneyID   string
	probeVersion     int64
	probeParams      string
}

func loadJourneyFacts(ctx context.Context, conn *sql.Conn, attemptID int64) (journeyFacts, error) {
	var facts journeyFacts
	err := conn.QueryRowContext(ctx, `
		SELECT r.plan_key,a.check_key,c.journey_id,COALESCE(c.journey_params_json,'{}')
		FROM execution_attempts a
		JOIN inspection_runs r ON r.id=a.scope_id
		JOIN config_plans p ON p.config_version_id=r.config_version_id AND p.plan_key=r.plan_key
		JOIN config_checks c ON c.plan_id=p.id AND c.check_key=a.check_key
		WHERE a.id=? AND a.scope_type='run_check' AND c.kind='browser'`, attemptID).
		Scan(&facts.planKey, &facts.checkKey, &facts.journeyID, &facts.journeyParams)
	if err != nil {
		return facts, err
	}
	digest, version, journeyVersion, err := resolveJourneyBinding(facts.journeyID)
	if err != nil {
		return facts, err
	}
	facts.catalogDigest, facts.catalogVersion, facts.journeyVersion = digest, version, journeyVersion
	return facts, nil
}

// renderJourneyInspectionBody renders the canonical inspection_collection_v1
// bytes for one run_check browser child; admission and rebuild share it so the
// frozen digest and the rebuilt dispatch input are byte-identical.
func renderJourneyInspectionBody(facts journeyFacts, identity browserIdentity, attemptID, operationID int64) ([]byte, error) {
	probeParams := json.RawMessage(identity.ProbeParams)
	if len(probeParams) == 0 || string(probeParams) == "null" {
		probeParams = json.RawMessage("{}")
	}
	journeyParams := json.RawMessage(facts.journeyParams)
	if len(journeyParams) == 0 || string(journeyParams) == "null" {
		journeyParams = json.RawMessage("{}")
	}
	binding := func(id string, version int64, raw json.RawMessage) map[string]any {
		return map[string]any{"id": id, "version": version, "params": raw,
			"catalog": map[string]any{"digest": facts.catalogDigest, "version": facts.catalogVersion}}
	}
	return json.Marshal(map[string]any{
		"schemaKind": "inspection_collection_v1", "attemptId": attemptID, "operationId": operationID,
		"identity": map[string]any{
			"identityId": identity.IdentityID, "identityRevisionId": identity.RevisionID,
			"profileGenerationId": identity.ProfileGenerationID.Int64, "profileGeneration": identity.Generation.Int64,
			"startUrl": identity.StartURL,
		},
		"journey":             binding(facts.journeyID, facts.journeyVersion, journeyParams),
		"authenticationProbe": binding(identity.ProbeJourneyID, identity.ProbeVersion, probeParams),
		"planKey":             facts.planKey, "checkKey": facts.checkKey,
	})
}

// rebuildJourneyInspectionInput reconstructs the frozen operation-bound
// inspection_collection_v1 bytes from the same durable facts admission froze.
func (s *Service) rebuildJourneyInspectionInput(ctx context.Context, attemptID int64) ([]byte, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var operationID int64
	if err = conn.QueryRowContext(ctx, `SELECT id FROM browser_operations WHERE owner_attempt_id=? AND kind='journey'`, attemptID).Scan(&operationID); err != nil {
		return nil, err
	}
	facts, err := loadJourneyFacts(ctx, conn, attemptID)
	if err != nil {
		return nil, err
	}
	var systemID int64
	if err = conn.QueryRowContext(ctx, `
		SELECT r.business_system_id FROM execution_attempts a JOIN inspection_runs r ON r.id=a.scope_id WHERE a.id=?`, attemptID).Scan(&systemID); err != nil {
		return nil, err
	}
	identity, hasIdentity, err := loadBrowserIdentity(ctx, conn, systemID)
	if err != nil {
		return nil, err
	}
	if !hasIdentity {
		return nil, fmt.Errorf("attempt %d lost its browser identity", attemptID)
	}
	return renderJourneyInspectionBody(facts, identity, attemptID, operationID)
}

// CommitJourneyProposal commits a run_check browser child result through the
// shared frozen journey closure, then converges the Run (starting its report
// analysis when collection completes) inside the same transaction.
func (s *Service) CommitJourneyProposal(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	converge := func(ctx context.Context, conn *sql.Conn, runID int64) error {
		if err := s.convergeOn(ctx, conn, runID); err != nil {
			return err
		}
		return s.startReportAnalysisOn(ctx, conn, runID, s.nowText())
	}
	if s.JourneyCore == nil {
		return fmt.Errorf("journey commit core is not wired")
	}
	return s.JourneyCore(ctx, attemptID, bootID, epoch, raw, businesssystem.JourneyScope{
		ScopeType:          "run_check",
		RebuildFrozenInput: s.rebuildJourneyInspectionInput,
		EvidenceTarget:     "inspection_run",
		EvidenceParams: func(planKey, checkKey string) string {
			params, _ := json.Marshal(map[string]string{"check_key": checkKey})
			return string(params)
		},
		Converge: converge,
	})
}

// RecordJourneyTechnicalGap settles one terminally interrupted run_check
// browser child without inventing Evidence, then converges the Run.
func (s *Service) RecordJourneyTechnicalGap(ctx context.Context, attemptID int64, reason string) error {
	if reason != "cancelled" {
		reason = "interrupted"
	}
	conn, err := s.db.Conn(ctx)
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
	var runID int64
	var checkKey, state string
	if err = conn.QueryRowContext(ctx, `
		SELECT a.scope_id,a.check_key,a.state FROM execution_attempts a
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='run_check'`, attemptID).
		Scan(&runID, &checkKey, &state); err != nil {
		return err
	}
	if state != "Failed" && state != "Cancelled" && state != "Interrupted" {
		return fmt.Errorf("attempt %d is not terminal", attemptID)
	}
	var settled int
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM inspection_check_results WHERE run_id=? AND check_key=?`, runID, checkKey).Scan(&settled); err != nil {
		return err
	}
	if settled == 0 {
		if _, err = conn.ExecContext(ctx, `
			INSERT INTO inspection_check_results(run_id,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at)
			VALUES(?,?,'gap',NULL,?,NULL,?,?)`, runID, checkKey, attemptID, reason, s.nowText()); err != nil {
			return err
		}
	}
	if err = s.convergeOn(ctx, conn, runID); err != nil {
		return err
	}
	if err = s.startReportAnalysisOn(ctx, conn, runID, s.nowText()); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// ConvergeRun re-evaluates an Inspection after a browser journey ledger
// committed (or a terminal cancellation gap settled).
func (s *Service) ConvergeRun(ctx context.Context, runID int64) error {
	conn, err := s.db.Conn(ctx)
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
	if err = s.convergeOn(ctx, conn, runID); err != nil {
		return err
	}
	if err = s.startReportAnalysisOn(ctx, conn, runID, s.nowText()); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	committed = err == nil
	return err
}
