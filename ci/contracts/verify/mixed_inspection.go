package main

import (
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
)

// The frozen SQL contract computes canonical digest projections with a
// sha256 SQL function; engines must register it deterministically.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("sha256", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("sha256: expected 1 argument")
		}
		var data []byte
		switch value := args[0].(type) {
		case string:
			data = []byte(value)
		case []byte:
			data = value
		default:
			return nil, fmt.Errorf("sha256: unsupported argument type %T", args[0])
		}
		sum := sha256.Sum256(data)
		return sum[:], nil
	})
}

const mixedInspectionTime = "2026-08-28T00:00:00Z"
const mixedInspectionDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// verifyMixedInspectionPromQLSQLite proves the real SQL closure for a mixed
// Inspection Run. The fixture starts from the full schema and removes only
// parent lifecycle bootstrap triggers; this lets it materialize an already
// Running Run while retaining the child Attempt, grant, Evidence, and result
// triggers under verification.
func verifyMixedInspectionPromQLSQLite(root string) error {
	schema, err := os.ReadFile(filepath.Join(root, "docs/specs/quoin-v1/contracts/sql/schema.sql"))
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:mixed-inspection-contract?mode=memory&cache=shared")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := mixedExec(db, string(schema)); err != nil {
		return err
	}
	for _, trigger := range []string{
		"trg_inspection_runs_closure",
		"trg_inspection_runs_insert_state",
	} {
		if err := mixedExec(db, "DROP TRIGGER "+trigger); err != nil {
			return err
		}
	}
	if err := bootstrapMixedInspectionRun(db); err != nil {
		return err
	}
	if err := registerMixedRuntimeSlot(db, "plinth"); err != nil {
		return err
	}
	if err := registerMixedRuntimeSlot(db, "lintel"); err != nil {
		return err
	}
	if err := bootstrapMixedThanosConnection(db); err != nil {
		return err
	}
	if err := mixedMustRejectCause(db, "execution_attempt must be created Queued before input freeze and dispatch", `
		INSERT INTO execution_attempts(attempt_type, scope_type, scope_id, plan_key, check_key, discovery_key, state, row_version, runtime_slot, requested_by_tool_call_id, connection_epoch, boot_id, lease_until, accepted_at, quoin_release_version, runtime_release_version, agent_version, termination_reason, created_at)
		VALUES ('inspection_collection', 'run_check', 1, NULL, 'promql', NULL, 'Assigned', 1, 'lintel', NULL, 1, 'bypass-boot', ?, NULL, 'quoin-v1', 'lintel-v1', NULL, NULL, ?)`, mixedInspectionTime, mixedInspectionTime); err != nil {
		return fmt.Errorf("manual Inspection accepted an INSERT-time dispatch bypass: %w", err)
	}

	promqlAttemptID, err := insertMixedRunCheckAttempt(db, "promql")
	if err != nil {
		return err
	}
	if err := freezeMixedInput(db, promqlAttemptID, "inspection_promql_execution_v1"); err != nil {
		return err
	}
	if err := mixedMustRejectCause(db, "attempt cannot dispatch without frozen input, release binding, and required model grant", `
		UPDATE execution_attempts
		SET state='Assigned', runtime_slot='plinth', boot_id='plinth-missing-grant', connection_epoch=1,
			lease_until=?, runtime_release_version='plinth-v1', row_version=2
		WHERE id=?`, mixedInspectionTime, promqlAttemptID); err != nil {
		return fmt.Errorf("PromQL run_check dispatched without a Thanos grant: %w", err)
	}
	if err := mixedExec(db, `
		INSERT INTO attempt_connection_grants(
			attempt_id, purpose, business_system_id, connection_id, connection_revision_id,
			credential_generation_id, qualified_probe_result_id, created_by_tool_call_id, created_at
		) VALUES (?, 'config_thanos_query', NULL, 1, 1, 1, NULL, NULL, ?)`, promqlAttemptID, mixedInspectionTime); err != nil {
		return err
	}
	if err := mixedMustRejectCause(db, "inspection run check attempt slot must match its check kind", `
		UPDATE execution_attempts
		SET state='Assigned', runtime_slot='lintel', boot_id='lintel-boot', connection_epoch=1,
			lease_until=?, runtime_release_version='lintel-v1', row_version=2
		WHERE id=?`, mixedInspectionTime, promqlAttemptID); err != nil {
		return fmt.Errorf("PromQL run_check accepted Lintel after its input and grant were frozen: %w", err)
	}
	if err := mixedExec(db, `UPDATE connections SET enabled=0, row_version=3 WHERE id=1`); err != nil {
		return err
	}
	if err := mixedMustRejectCause(db, "attempt cannot dispatch without frozen input, release binding, and required model grant", `
		UPDATE execution_attempts
		SET state='Assigned', runtime_slot='plinth', boot_id='plinth-disabled-connection', connection_epoch=1,
			lease_until=?, runtime_release_version='plinth-v1', row_version=2
		WHERE id=?`, mixedInspectionTime, promqlAttemptID); err != nil {
		return fmt.Errorf("PromQL run_check dispatched with a disabled Thanos connection: %w", err)
	}
	if err := mixedExec(db, `UPDATE connections SET enabled=1, row_version=4 WHERE id=1`); err != nil {
		return err
	}
	if err := mixedExec(db, `
		UPDATE execution_attempts
		SET state='Assigned', runtime_slot='plinth', boot_id='plinth-boot', connection_epoch=1,
			lease_until=?, runtime_release_version='plinth-v1', row_version=2
		WHERE id=?`, mixedInspectionTime, promqlAttemptID); err != nil {
		return err
	}
	if err := mixedMustReject(db, `
		UPDATE execution_attempts
		SET state='Cancelled', ended_at=?, termination_reason='cancelled', row_version=3
		WHERE id=?`, mixedInspectionTime, promqlAttemptID); err != nil {
		return fmt.Errorf("Assigned attempt bypassed the Cancelling cancellation fence: %w", err)
	}
	if err := mixedExec(db, `UPDATE execution_attempts SET state='Running', accepted_at=?, row_version=3 WHERE id=?`, mixedInspectionTime, promqlAttemptID); err != nil {
		return err
	}
	if err := mixedMustReject(db, `
		UPDATE execution_attempts
		SET state='Succeeded', ended_at=?, row_version=4
		WHERE id=?`, mixedInspectionTime, promqlAttemptID); err != nil {
		return fmt.Errorf("PromQL run_check succeeded without a check result: %w", err)
	}
	if err := insertMixedArtifactEvidence(db, promqlAttemptID); err != nil {
		return err
	}
	if err := mixedMustReject(db, `
		INSERT INTO inspection_check_results(run_id, check_key, status, evidence_id, attempt_id, result_digest, gap_reason, created_at)
		VALUES (1, 'promql', 'ok', 1, ?, zeroblob(32), NULL, ?)`, promqlAttemptID, mixedInspectionTime); err != nil {
		return fmt.Errorf("PromQL run_check accepted Artifact-backed Evidence: %w", err)
	}
	for _, gapReason := range []string{"identity_busy", "runtime_unavailable", "authentication_required", "authentication_probe_unavailable", "artifact_commit_failed", "journey_failed", "unknown_gap"} {
		if err := mixedMustRejectCause(db, "inspection result must be one exact PromQL result", `
			INSERT INTO inspection_check_results(run_id, check_key, status, evidence_id, attempt_id, result_digest, gap_reason, created_at)
			VALUES (1, 'promql', 'gap', NULL, ?, zeroblob(32), ?, ?)`, promqlAttemptID, gapReason, mixedInspectionTime); err != nil {
			return fmt.Errorf("PromQL run_check accepted non-PromQL gap code %q: %w", gapReason, err)
		}
	}
	// A rejected ResultProposal rolls back the Evidence it would otherwise bind;
	// no partial domain output may survive its outer transaction.
	promqlTx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := promqlTx.Exec(`
		INSERT INTO evidence(attempt_id, tool_call_id, target_type, target_id, params_json, observed_at,
			result_json, artifact_id, warnings_json, errors_json, integrity, created_at)
		VALUES (?, NULL, 'inspection_run', 1, '{"plan_key":"plan","check_key":"promql"}', ?, '{"resultType":"vector","result":[{"metric":{"job":"quoin"},"value":[0,"1"]}]}', NULL, '[]', '[]', 'complete', ?)`,
		promqlAttemptID, mixedInspectionTime, mixedInspectionTime); err != nil {
		_ = promqlTx.Rollback()
		return err
	}
	if _, err := promqlTx.Exec(`
		INSERT INTO inspection_check_results(run_id, check_key, status, evidence_id, attempt_id, result_digest, gap_reason, created_at)
		VALUES (1, 'promql', 'gap', NULL, ?, zeroblob(32), 'identity_busy', ?)`, promqlAttemptID, mixedInspectionTime); err == nil {
		_ = promqlTx.Rollback()
		return fmt.Errorf("PromQL invalid ResultProposal unexpectedly committed")
	}
	if err := promqlTx.Rollback(); err != nil {
		return err
	}
	var rolledBackEvidence int
	if err := db.QueryRow(`SELECT COUNT(*) FROM evidence WHERE attempt_id=? AND artifact_id IS NULL`, promqlAttemptID).Scan(&rolledBackEvidence); err != nil {
		return err
	}
	if rolledBackEvidence != 0 {
		return fmt.Errorf("rejected PromQL ResultProposal left %d Evidence rows", rolledBackEvidence)
	}
	if err := mixedTransaction(db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO evidence(attempt_id, tool_call_id, target_type, target_id, params_json, observed_at,
				result_json, artifact_id, warnings_json, errors_json, integrity, created_at)
			VALUES (?, NULL, 'inspection_run', 1, '{"plan_key":"plan","check_key":"promql"}', ?, '{"resultType":"vector","result":[{"metric":{"job":"quoin"},"value":[0,"1"]}]}', NULL, '[]', '[]', 'complete', ?)`,
			promqlAttemptID, mixedInspectionTime, mixedInspectionTime); err != nil {
			return err
		}
		_, err := tx.Exec(`
			INSERT INTO inspection_check_results(run_id, check_key, status, evidence_id, attempt_id, result_digest, gap_reason, created_at)
			VALUES (1, 'promql', 'ok', 2, ?, zeroblob(32), NULL, ?)`, promqlAttemptID, mixedInspectionTime)
		return err
	}); err != nil {
		return err
	}
	var promqlState string
	if err := db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, promqlAttemptID).Scan(&promqlState); err != nil {
		return err
	}
	if promqlState != "Succeeded" {
		return fmt.Errorf("PromQL result did not atomically close its Attempt: state=%s", promqlState)
	}

	browserAttemptID, err := insertMixedRunCheckAttempt(db, "browser")
	if err != nil {
		return err
	}
	if err := freezeMixedInput(db, browserAttemptID, "inspection_collection_v1"); err != nil {
		return err
	}
	if err := mixedMustRejectCause(db, "browser Attempt may dispatch only after its Browser Operation owns a physical slot", `
		UPDATE execution_attempts
		SET state='Assigned', runtime_slot='lintel', boot_id='lintel-without-journey', connection_epoch=1,
			lease_until=?, runtime_release_version='lintel-v1', row_version=2
		WHERE id=?`, mixedInspectionTime, browserAttemptID); err != nil {
		return fmt.Errorf("browser run_check dispatched without a running matching Journey: %w", err)
	}
	if err := bootstrapMixedBrowserJourney(db, browserAttemptID); err != nil {
		return err
	}
	if err := mixedMustReject(db, `
		INSERT INTO attempt_connection_grants(
			attempt_id, purpose, business_system_id, connection_id, connection_revision_id,
			credential_generation_id, qualified_probe_result_id, created_by_tool_call_id, created_at
		) VALUES (?, 'config_thanos_query', NULL, 1, 1, 1, NULL, NULL, ?)`, browserAttemptID, mixedInspectionTime); err != nil {
		return fmt.Errorf("browser run_check accepted a Thanos grant: %w", err)
	}
	if err := mixedMustRejectCause(db, "inspection run check attempt slot must match its check kind", `
		UPDATE execution_attempts
		SET state='Assigned', runtime_slot='plinth', boot_id='plinth-browser-boot', connection_epoch=1,
			lease_until=?, runtime_release_version='plinth-v1', row_version=2
		WHERE id=?`, mixedInspectionTime, browserAttemptID); err != nil {
		return fmt.Errorf("browser run_check accepted Plinth after its input and Journey were frozen: %w", err)
	}
	if err := mixedExec(db, `
		UPDATE execution_attempts
		SET state='Assigned', runtime_slot='lintel', boot_id='lintel-browser-boot', connection_epoch=1,
			lease_until=?, runtime_release_version='lintel-v1', row_version=2
		WHERE id=?`, mixedInspectionTime, browserAttemptID); err != nil {
		return err
	}
	if err := mixedExec(db, `UPDATE execution_attempts SET state='Running', accepted_at=?, row_version=3 WHERE id=?`, mixedInspectionTime, browserAttemptID); err != nil {
		return err
	}
	if err := mixedMustReject(db, `UPDATE execution_attempts SET state='Succeeded', ended_at=?, row_version=4 WHERE id=?`, mixedInspectionTime, browserAttemptID); err != nil {
		return fmt.Errorf("browser run_check succeeded while its Journey Operation was active: %w", err)
	}
	for sequence, phase := range []string{"admission", "completion"} {
		if err := mixedExec(db, `
			INSERT INTO browser_probe_results(operation_id, probe_seq, phase, identity_revision_id, journey_id, journey_version, journey_catalog_digest, journey_catalog_version, result, reason_code, observed_at)
			VALUES (2, ?, ?, 1, 'probe', 1, ?, 'v1', 'Authenticated', NULL, ?)`,
			sequence+1, phase, mixedInspectionDigest, mixedInspectionTime); err != nil {
			return err
		}
	}
	// Browser Evidence and Journey ResultProposal are likewise one outer
	// transaction: a rejected ledger write leaves neither Evidence nor state.
	browserTx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := browserTx.Exec(`
		INSERT INTO evidence(attempt_id, tool_call_id, target_type, target_id, params_json, observed_at,
			result_json, artifact_id, warnings_json, errors_json, integrity, created_at)
		VALUES (?, NULL, 'inspection_run', 1, '{"plan_key":"plan","check_key":"browser"}', ?, '{}', NULL, '[]', '[]', 'complete', ?)`,
		browserAttemptID, mixedInspectionTime, mixedInspectionTime); err != nil {
		_ = browserTx.Rollback()
		return err
	}
	if _, err := browserTx.Exec(`
		INSERT INTO browser_journey_results(operation_id, attempt_id, result_digest, outcome, primary_evidence_id, gap_code, original_gap_code, terminal_reason, error_detail, created_at)
		VALUES (2, ?, zeroblob(32), 'gap', 3, 'journey_failed', NULL, 'journey_failed', 'forced rollback', ?)`, browserAttemptID, mixedInspectionTime); err == nil {
		_ = browserTx.Rollback()
		return fmt.Errorf("invalid browser ResultProposal unexpectedly committed")
	}
	if err := browserTx.Rollback(); err != nil {
		return err
	}
	var rolledBackBrowserEvidence int
	if err := db.QueryRow(`SELECT COUNT(*) FROM evidence WHERE attempt_id=?`, browserAttemptID).Scan(&rolledBackBrowserEvidence); err != nil {
		return err
	}
	if rolledBackBrowserEvidence != 0 {
		return fmt.Errorf("rejected Journey ResultProposal left %d Evidence rows", rolledBackBrowserEvidence)
	}
	var rollbackAttemptState, rollbackOperationState string
	if err := db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, browserAttemptID).Scan(&rollbackAttemptState); err != nil {
		return err
	}
	if err := db.QueryRow(`SELECT state FROM browser_operations WHERE id=2`).Scan(&rollbackOperationState); err != nil {
		return err
	}
	if rollbackAttemptState != "Running" || rollbackOperationState != "Running" {
		return fmt.Errorf("rejected Journey ResultProposal changed state: attempt=%s operation=%s", rollbackAttemptState, rollbackOperationState)
	}
	if err := mixedTransaction(db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO evidence(attempt_id, tool_call_id, target_type, target_id, params_json, observed_at,
				result_json, artifact_id, warnings_json, errors_json, integrity, created_at)
			VALUES (?, NULL, 'inspection_run', 1, '{"plan_key":"plan","check_key":"browser"}', ?, '{}', NULL, '[]', '[]', 'complete', ?)`,
			browserAttemptID, mixedInspectionTime, mixedInspectionTime); err != nil {
			return err
		}
		_, err := tx.Exec(`
			INSERT INTO browser_journey_results(operation_id, attempt_id, result_digest, outcome, primary_evidence_id, gap_code, original_gap_code, terminal_reason, error_detail, created_at)
			VALUES (2, ?, zeroblob(32), 'success', 3, NULL, NULL, NULL, NULL, ?)`, browserAttemptID, mixedInspectionTime)
		return err
	}); err != nil {
		return err
	}
	var browserAttemptState, browserOperationState string
	if err := db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, browserAttemptID).Scan(&browserAttemptState); err != nil {
		return err
	}
	if err := db.QueryRow(`SELECT state FROM browser_operations WHERE id=2`).Scan(&browserOperationState); err != nil {
		return err
	}
	if browserAttemptState != "Succeeded" || browserOperationState != "Succeeded" {
		return fmt.Errorf("Journey result did not atomically close browser state: attempt=%s operation=%s", browserAttemptState, browserOperationState)
	}
	var stopConfirmedAt, stopConfirmationBasis sql.NullString
	if err := db.QueryRow(`SELECT stop_confirmed_at, stop_confirmation_basis FROM browser_operations WHERE id=2`).Scan(&stopConfirmedAt, &stopConfirmationBasis); err != nil {
		return err
	}
	if stopConfirmedAt.Valid || stopConfirmationBasis.Valid {
		return fmt.Errorf("Journey ResultProposal must not fabricate physical stop confirmation")
	}
	if err := mixedExec(db, `
		UPDATE browser_operations
		SET stop_confirmed_at=?, stop_confirmation_basis='stop_ack', row_version=row_version+1
		WHERE id=2`, mixedInspectionTime); err != nil {
		return err
	}
	if err := db.QueryRow(`SELECT stop_confirmed_at, stop_confirmation_basis FROM browser_operations WHERE id=2`).Scan(&stopConfirmedAt, &stopConfirmationBasis); err != nil {
		return err
	}
	if !stopConfirmedAt.Valid || stopConfirmationBasis.String != "stop_ack" {
		return fmt.Errorf("Journey physical stop confirmation was not durably recorded")
	}
	if err := mixedMustReject(db, `
		INSERT INTO browser_journey_results(operation_id, attempt_id, result_digest, outcome, primary_evidence_id, gap_code, original_gap_code, terminal_reason, error_detail, created_at)
		VALUES (2, ?, zeroblob(32), 'success', 3, NULL, NULL, NULL, NULL, ?)`, browserAttemptID, mixedInspectionTime); err != nil {
		return fmt.Errorf("browser run_check accepted a duplicate Journey ResultProposal: %w", err)
	}

	if err := mixedExec(db, `UPDATE inspection_runs SET state='Completed', row_version=row_version+1 WHERE id=1`); err != nil {
		return err
	}
	if err := verifyMixedInspectionReportClosure(db); err != nil {
		return err
	}

	if err := mixedExec(db, `
		INSERT INTO inspection_runs(id, business_system_id, plan_key, config_version_id, label_contract_version_id,
			trigger_kind, scheduled_for, state, row_version, evidence_at, rerun_of_id, created_at)
		VALUES (2, 1, 'plan', 1, 1, 'manual', NULL, 'Completed', 1, ?, NULL, ?)`, mixedInspectionTime, mixedInspectionTime); err != nil {
		return err
	}
	if err := mixedMustReject(db, `
		INSERT INTO execution_attempts(attempt_type, scope_type, scope_id, plan_key, check_key, discovery_key,
			state, row_version, runtime_slot, requested_by_tool_call_id, connection_epoch, boot_id, lease_until,
			accepted_at, quoin_release_version, runtime_release_version, agent_version, termination_reason, created_at)
		VALUES ('inspection_collection', 'run_check', 2, NULL, 'promql', NULL,
			'Queued', 1, NULL, NULL, NULL, NULL, NULL, NULL, 'quoin-v1', NULL, NULL, NULL, ?)`, mixedInspectionTime); err != nil {
		return fmt.Errorf("terminal Inspection Run accepted a run_check Attempt: %w", err)
	}
	if err := mixedMustReject(db, `
		INSERT INTO execution_attempts(attempt_type, scope_type, scope_id, plan_key, check_key, discovery_key,
			state, row_version, runtime_slot, requested_by_tool_call_id, connection_epoch, boot_id, lease_until,
			accepted_at, quoin_release_version, runtime_release_version, agent_version, termination_reason, created_at)
		VALUES ('inspection_collection', 'run', 1, NULL, NULL, NULL,
			'Queued', 1, NULL, NULL, NULL, NULL, NULL, NULL, 'quoin-v1', NULL, NULL, NULL, ?)`, mixedInspectionTime); err != nil {
		return fmt.Errorf("Inspection collection accepted the non-run_check scope: %w", err)
	}
	return nil
}

func verifyMixedInspectionReportClosure(db *sql.DB) error {
	// The report fixture preserves Report/result/state closures. It bypasses only
	// the separately proven model-provider qualification machinery needed to seed
	// one accepted model call without duplicating that connection contract here.
	for _, trigger := range []string{"trg_execution_attempts_dispatch_ready", "trg_attempt_connection_grant_closure", "trg_model_call_grant_closure", "trg_model_call_success_output", "trg_model_call_success_input"} {
		if err := mixedExec(db, "DROP TRIGGER "+trigger); err != nil {
			return err
		}
	}
	ledgerReject := "inspection Report ResultProposal must close one running Run analysis and only its immutable references"
	ledgerInsert := `INSERT INTO inspection_report_result_ledgers(attempt_id,inspection_run_id,report_version,model_call_id,result_digest,evidence_digest,content,prompt_digest,evidence_ids_json,artifact_ids_json,knowledge_version_ids_json,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`
	evidenceDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("[2,3]")))

	// The frozen preallocated version (2) deliberately disagrees with the ledger
	// claim (1) while the Report count still allows 1: only the structured
	// snapshot fact rejects this proposal.
	mismatchAttempt, mismatchCall, err := seedReportAnalysisLifecycle(db, 2, []int64{1}, nil, nil)
	if err != nil {
		return err
	}
	mismatchResult := sha256.Sum256([]byte(reportResultCanonical(mismatchAttempt, mismatchCall, evidenceDigest, mixedInspectionDigest, "immutable report", "[2,3]")))
	if err := mixedMustRejectCause(db, ledgerReject, ledgerInsert,
		mismatchAttempt, 1, 1, mismatchCall, mismatchResult[:], evidenceDigest, "immutable report", mixedInspectionDigest, "[2,3]", "[]", "[]", mixedInspectionTime); err != nil {
		return fmt.Errorf("report frozen version fence: %w", err)
	}
	if err := mixedExec(db, `UPDATE execution_attempts SET state='Failed', ended_at=?, termination_reason='replaced', row_version=row_version+1 WHERE id=?`, mixedInspectionTime, mismatchAttempt); err != nil {
		return fmt.Errorf("cancel mismatch attempt: %w", err)
	}

	// Count predicate isolated: two frozen Run locators including the right one.
	noRunAttempt, noRunCall, err := seedReportAnalysisLifecycle(db, 1, []int64{1, 1}, nil, nil)
	if err != nil {
		return err
	}
	noRunResult := sha256.Sum256([]byte(reportResultCanonical(noRunAttempt, noRunCall, evidenceDigest, mixedInspectionDigest, "immutable report", "[2,3]")))
	if err := mixedMustRejectCause(db, ledgerReject, ledgerInsert,
		noRunAttempt, 1, 1, noRunCall, noRunResult[:], evidenceDigest, "immutable report", mixedInspectionDigest, "[2,3]", "[]", "[]", mixedInspectionTime); err != nil {
		return fmt.Errorf("report frozen Run locator count fence: %w", err)
	}
	if err := mixedExec(db, `UPDATE execution_attempts SET state='Failed', ended_at=?, termination_reason='replaced', row_version=row_version+1 WHERE id=?`, mixedInspectionTime, noRunAttempt); err != nil {
		return fmt.Errorf("cancel run-locator-count attempt: %w", err)
	}

	// A real second Run closes its own PromQL check with real Evidence. That
	// Evidence is frozen into the analysis input, but it never belongs to Run 1's
	// check results: only the run-fact binding can reject the proposal.
	foreignEvidence, err := seedForeignRunEvidence(db)
	if err != nil {
		return err
	}
	foreignAttempt, foreignCall, err := seedReportAnalysisLifecycle(db, 1, []int64{1}, []int64{foreignEvidence}, nil)
	if err != nil {
		return err
	}
	foreignJSON := fmt.Sprintf("[2,3,%d]", foreignEvidence)
	foreignDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(foreignJSON)))
	foreignResult := sha256.Sum256([]byte(reportResultCanonical(foreignAttempt, foreignCall, foreignDigest, mixedInspectionDigest, "immutable report", foreignJSON)))
	if err := mixedMustRejectCause(db, ledgerReject, ledgerInsert,
		foreignAttempt, 1, 1, foreignCall, foreignResult[:], foreignDigest, "immutable report", mixedInspectionDigest, foreignJSON, "[]", "[]", mixedInspectionTime); err != nil {
		return fmt.Errorf("report cross-Run Evidence fence: %w", err)
	}
	if err := mixedExec(db, `UPDATE execution_attempts SET state='Failed', ended_at=?, termination_reason='replaced', row_version=row_version+1 WHERE id=?`, mixedInspectionTime, foreignAttempt); err != nil {
		return fmt.Errorf("cancel foreign attempt: %w", err)
	}

	// Binding predicate isolated: exactly one frozen Run locator, but it points
	// at the real foreign Run 3 instead of the ledger's Run 1.
	bindingAttempt, bindingCall, err := seedReportAnalysisLifecycle(db, 1, []int64{3}, nil, nil)
	if err != nil {
		return err
	}
	bindingResult := sha256.Sum256([]byte(reportResultCanonical(bindingAttempt, bindingCall, evidenceDigest, mixedInspectionDigest, "immutable report", "[2,3]")))
	if err := mixedMustRejectCause(db, ledgerReject, ledgerInsert,
		bindingAttempt, 1, 1, bindingCall, bindingResult[:], evidenceDigest, "immutable report", mixedInspectionDigest, "[2,3]", "[]", "[]", mixedInspectionTime); err != nil {
		return fmt.Errorf("report frozen Run locator binding fence: %w", err)
	}
	if err := mixedExec(db, `UPDATE execution_attempts SET state='Failed', ended_at=?, termination_reason='replaced', row_version=row_version+1 WHERE id=?`, mixedInspectionTime, bindingAttempt); err != nil {
		return fmt.Errorf("cancel run-locator-binding attempt: %w", err)
	}

	// Evidence-owned content artifacts (T24c): the amended arm admits an
	// artifact owned by the run's Evidence when frozen into the analysis
	// input, and rejects a foreign evidence-owned artifact.
	if err := seedEvidenceOwnedArtifact(db); err != nil {
		return err
	}
	artifactPositive, artifactCall, err := seedReportAnalysisLifecycle(db, 1, []int64{1}, nil, []int64{9})
	if err != nil {
		return err
	}
	{
		evidenceJSON := "[2,3]"
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(evidenceJSON)))
		canonical := fmt.Sprintf("inspection_report_result_v1|%d|1|%d|success|immutable report|%s|[9]|[]|%s|%s", artifactPositive, artifactCall, evidenceJSON, digest, mixedInspectionDigest)
		resultDigest := sha256.Sum256([]byte(canonical))
		if err := mixedExec(db, ledgerInsert, artifactPositive, 1, 1, artifactCall, resultDigest[:], digest, "immutable report", mixedInspectionDigest, evidenceJSON, "[9]", "[]", mixedInspectionTime); err != nil {
			return fmt.Errorf("report evidence-owned artifact commit: %w", err)
		}
	}
	// The next analysis version may only cite run-owned facts: a foreign
	// evidence-owned artifact stays rejected even when frozen into the input.
	if err := seedForeignEvidenceArtifact(db); err != nil {
		return err
	}
	t24cForeignAttempt, t24cForeignCall, err := seedReportAnalysisLifecycle(db, 2, []int64{1}, nil, []int64{10})
	if err != nil {
		return err
	}
	{
		evidenceJSON := "[2,3]"
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(evidenceJSON)))
		canonical := fmt.Sprintf("inspection_report_result_v1|%d|1|%d|success|immutable report|%s|[10]|[]|%s|%s", t24cForeignAttempt, t24cForeignCall, evidenceJSON, digest, mixedInspectionDigest)
		resultDigest := sha256.Sum256([]byte(canonical))
		if err := mixedMustRejectCause(db, ledgerReject, ledgerInsert, t24cForeignAttempt, 1, 2, t24cForeignCall, resultDigest[:], digest, "immutable report", mixedInspectionDigest, evidenceJSON, "[10]", "[]", mixedInspectionTime); err != nil {
			return fmt.Errorf("report foreign evidence-owned artifact fence: %w", err)
		}
	}
	if err := mixedExec(db, `UPDATE execution_attempts SET state='Failed', ended_at=?, termination_reason='replaced', row_version=row_version+1 WHERE id=?`, mixedInspectionTime, t24cForeignAttempt); err != nil {
		return fmt.Errorf("cancel t24c foreign artifact attempt: %w", err)
	}

	// Locator typing on a fully valid lifecycle: every non-type arm (version,
	// Run locator, coverage, digests) must pass, so removing the x.type fence
	// would let these payloads through. The string "2" and the real 2.0 both
	// numerically cover Run 1's required Evidence 2 under SQLite affinity.
	typeAttempt, typeCall, err := seedReportAnalysisLifecycle(db, 1, []int64{1}, nil, nil)
	if err != nil {
		return err
	}
	for _, payload := range []struct{ json string }{{`["2",3]`}, {"[2.0,3]"}} {
		payloadDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(payload.json)))
		payloadResult := sha256.Sum256([]byte(reportResultCanonical(typeAttempt, typeCall, payloadDigest, mixedInspectionDigest, "immutable report", payload.json)))
		if err := mixedMustRejectCause(db, ledgerReject, ledgerInsert,
			typeAttempt, 1, 1, typeCall, payloadResult[:], payloadDigest, "immutable report", mixedInspectionDigest, payload.json, "[]", "[]", mixedInspectionTime); err != nil {
			return fmt.Errorf("report non-integer locator fence (%s): %w", payload.json, err)
		}
	}
	wrongEvidenceDigest := strings.Repeat("c", 64)
	wrongEvidenceResult := sha256.Sum256([]byte(reportResultCanonical(typeAttempt, typeCall, wrongEvidenceDigest, mixedInspectionDigest, "immutable report", "[2,3]")))
	if err := mixedMustRejectCause(db, ledgerReject, ledgerInsert,
		typeAttempt, 1, 1, typeCall, wrongEvidenceResult[:], wrongEvidenceDigest, "immutable report", mixedInspectionDigest, "[2,3]", "[]", "[]", mixedInspectionTime); err != nil {
		return fmt.Errorf("report evidence digest provenance fence: %w", err)
	}
	if err := mixedMustRejectCause(db, ledgerReject, ledgerInsert,
		typeAttempt, 1, 1, typeCall, make([]byte, 32), evidenceDigest, "immutable report", mixedInspectionDigest, "[2,3]", "[]", "[]", mixedInspectionTime); err != nil {
		return fmt.Errorf("report result digest provenance fence: %w", err)
	}
	if err := mixedExec(db, `UPDATE execution_attempts SET state='Failed', ended_at=?, termination_reason='replaced', row_version=row_version+1 WHERE id=?`, mixedInspectionTime, typeAttempt); err != nil {
		return fmt.Errorf("cancel locator-type attempt: %w", err)
	}

	// Happy path: every frozen fact lines up.
	attemptID, modelCallID, err := seedReportAnalysisLifecycle(db, 2, []int64{1}, nil, nil)
	if err != nil {
		return err
	}
	wrongPrompt := strings.Repeat("b", 64)
	wrongPromptResult := sha256.Sum256([]byte(reportResultCanonical(attemptID, modelCallID, evidenceDigest, wrongPrompt, "immutable report", "[2,3]")))
	if err := mixedMustRejectCause(db, ledgerReject, ledgerInsert,
		attemptID, 1, 1, modelCallID, wrongPromptResult[:], evidenceDigest, "immutable report", wrongPrompt, "[2,3]", "[]", "[]", mixedInspectionTime); err != nil {
		return fmt.Errorf("report prompt provenance fence: %w", err)
	}
	if err := mixedMustRejectCause(db, "Succeeded inspection analysis Attempt must atomically commit its immutable Report ledger", `UPDATE execution_attempts SET state='Succeeded', ended_at=?, row_version=4 WHERE id=?`, mixedInspectionTime, attemptID); err != nil {
		return fmt.Errorf("report analysis direct success fence: %w", err)
	}
	validResult := sha256.Sum256([]byte(reportResultCanonical(attemptID, modelCallID, evidenceDigest, mixedInspectionDigest, "immutable report", "[2,3]")))
	if err := mixedTransaction(db, func(tx *sql.Tx) error {
		_, err := tx.Exec(ledgerInsert, attemptID, 1, 2, modelCallID, validResult[:], evidenceDigest, "immutable report", mixedInspectionDigest, "[2,3]", "[]", "[]", mixedInspectionTime)
		return err
	}); err != nil {
		return fmt.Errorf("commit Report ResultProposal: %w", err)
	}
	var reports int
	var state string
	if err := db.QueryRow(`SELECT count(*) FROM inspection_reports WHERE attempt_id=?`, attemptID).Scan(&reports); err != nil {
		return err
	}
	if err := db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
		return err
	}
	if reports != 1 || state != "Succeeded" {
		return fmt.Errorf("Report ResultProposal did not atomically close Report/Attempt: reports=%d state=%s", reports, state)
	}
	if err := mixedMustRejectCause(db, "inspection Report Evidence reference must exactly match its immutable ResultProposal ledger", `INSERT INTO inspection_report_evidence(report_id,evidence_id,ordinal) SELECT id,1,2 FROM inspection_reports WHERE attempt_id=?`, attemptID); err != nil {
		return fmt.Errorf("Report Evidence append fence: %w", err)
	}
	if err := mixedMustRejectCause(db, "inspection Report Artifact reference must exactly match its immutable ResultProposal ledger", `INSERT INTO inspection_report_artifacts(report_id,artifact_id,ordinal) SELECT id,1,0 FROM inspection_reports WHERE attempt_id=?`, attemptID); err != nil {
		return fmt.Errorf("Report Artifact append fence: %w", err)
	}
	if err := mixedMustReject(db, ledgerInsert, attemptID, 1, 2, modelCallID, make([]byte, 32), evidenceDigest, "mutated", mixedInspectionDigest, "[2,3]", "[]", "[]", mixedInspectionTime); err != nil {
		return fmt.Errorf("Report ResultProposal accepted mutated replay: %w", err)
	}
	return nil
}

// seedForeignRunEvidence reopens the second Inspection Run, closes its PromQL
// check through the real collection/evidence/check-result closure, and returns
// the resulting Evidence id.
func seedForeignRunEvidence(db *sql.DB) (int64, error) {
	if err := mixedExec(db, `INSERT INTO inspection_runs(id, business_system_id, plan_key, config_version_id, label_contract_version_id,
		trigger_kind, scheduled_for, state, row_version, evidence_at, rerun_of_id, created_at)
		VALUES (3, 1, 'plan', 1, 1, 'manual', NULL, 'Running', 1, ?, NULL, ?)`, mixedInspectionTime, mixedInspectionTime); err != nil {
		return 0, fmt.Errorf("create foreign run: %w", err)
	}
	result, err := db.Exec(`INSERT INTO execution_attempts(attempt_type, scope_type, scope_id, plan_key, check_key, discovery_key, state, row_version, runtime_slot, requested_by_tool_call_id, connection_epoch, boot_id, lease_until, accepted_at, quoin_release_version, runtime_release_version, agent_version, termination_reason, created_at)
		VALUES ('inspection_collection', 'run_check', 3, NULL, 'promql', NULL, 'Queued', 1, NULL, NULL, NULL, NULL, NULL, NULL, 'quoin-v1', NULL, NULL, NULL, ?)`, mixedInspectionTime)
	if err != nil {
		return 0, fmt.Errorf("create foreign collection attempt: %w", err)
	}
	collectionID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := mixedExec(db, `INSERT INTO attempt_input_snapshots(attempt_id, schema_kind, renderer_version, content_digest, created_at)
		VALUES (?, 'inspection_promql_execution_v1', 'v1', ?, ?)`, collectionID, mixedInspectionDigest, mixedInspectionTime); err != nil {
		return 0, fmt.Errorf("freeze foreign collection input: %w", err)
	}
	if err := mixedExec(db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,inspection_run_id) SELECT id, 1, 'inspection_run', ?, 3 FROM attempt_input_snapshots WHERE attempt_id=?`, mixedInspectionDigest, collectionID); err != nil {
		return 0, fmt.Errorf("freeze foreign collection Run item: %w", err)
	}
	if err := mixedExec(db, `INSERT INTO attempt_connection_grants(attempt_id, purpose, business_system_id, connection_id, connection_revision_id, credential_generation_id, qualified_probe_result_id, created_by_tool_call_id, created_at)
		VALUES (?, 'config_thanos_query', NULL, 1, 1, 1, NULL, NULL, ?)`, collectionID, mixedInspectionTime); err != nil {
		return 0, fmt.Errorf("seed foreign collection grant: %w", err)
	}
	if err := mixedExec(db, `UPDATE execution_attempts SET state='Assigned', runtime_slot='plinth', boot_id='plinth-foreign-boot', connection_epoch=1, lease_until=?, runtime_release_version='plinth-v1', row_version=2 WHERE id=?`, "2026-08-29T00:00:00Z", collectionID); err != nil {
		return 0, fmt.Errorf("assign foreign collection: %w", err)
	}
	if err := mixedExec(db, `UPDATE execution_attempts SET state='Running', accepted_at=?, row_version=3 WHERE id=?`, mixedInspectionTime, collectionID); err != nil {
		return 0, fmt.Errorf("start foreign collection: %w", err)
	}
	var evidenceID int64
	if err := mixedTransaction(db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO evidence(attempt_id, tool_call_id, target_type, target_id, params_json, observed_at, result_json, artifact_id, warnings_json, errors_json, integrity, created_at)
			VALUES (?, NULL, 'inspection_run', 3, '{"plan_key":"plan","check_key":"promql"}', ?, '{"resultType":"vector","result":[{"metric":{"job":"quoin"},"value":[0,"1"]}]}', NULL, '[]', '[]', 'complete', ?)`,
			collectionID, mixedInspectionTime, mixedInspectionTime); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT id FROM evidence WHERE attempt_id=? ORDER BY id DESC LIMIT 1`, collectionID).Scan(&evidenceID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO inspection_check_results(run_id, check_key, status, evidence_id, attempt_id, result_digest, gap_reason, created_at)
			VALUES (3, 'promql', 'ok', ?, ?, zeroblob(32), NULL, ?)`, evidenceID, collectionID, mixedInspectionTime)
		return err
	}); err != nil {
		return 0, fmt.Errorf("commit foreign collection result: %w", err)
	}
	// The foreign Run deliberately stays Running: the Report closure only ever
	// validates the ledger's own Run, and this Evidence must remain real yet
	// never enter Run 1's frozen result set.
	return evidenceID, nil
}

// reportResultCanonical mirrors the frozen SQL canonical projection that binds
// result_digest to the exact typed ResultProposal payload.
func reportResultCanonical(attemptID, modelCallID int64, evidenceDigest, promptDigest, content, evidenceJSON string) string {
	return fmt.Sprintf("inspection_report_result_v1|%d|1|%d|success|%s|%s|[]|[]|%s|%s", attemptID, modelCallID, content, evidenceJSON, evidenceDigest, promptDigest)
}

// seedReportAnalysisLifecycle freezes one inspection_analysis input snapshot
// (Run locator items, evidence, check results, structured preallocated Report
// version), then drives the Attempt and one succeeded model call. runLocators
// pins the exact frozen inspection_run_id item set (default [1]).
func seedReportAnalysisLifecycle(db *sql.DB, reportVersion int, runLocators, extraEvidenceIDs, extraArtifactIDs []int64) (int64, int64, error) {
	if runLocators == nil {
		runLocators = []int64{1}
	}
	result, err := db.Exec(`INSERT INTO execution_attempts(attempt_type, scope_type, scope_id, plan_key, check_key, discovery_key, state, row_version, runtime_slot, requested_by_tool_call_id, connection_epoch, boot_id, lease_until, accepted_at, quoin_release_version, runtime_release_version, agent_version, termination_reason, created_at)
		VALUES ('inspection_analysis', 'run', 1, NULL, NULL, NULL, 'Queued', 1, NULL, NULL, NULL, NULL, NULL, NULL, 'quoin-v1', NULL, NULL, NULL, ?)`, mixedInspectionTime)
	if err != nil {
		return 0, 0, fmt.Errorf("create report analysis Attempt: %w", err)
	}
	attemptID, err := result.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	if err := mixedExec(db, `INSERT INTO attempt_input_snapshots(attempt_id, schema_kind, renderer_version, content_digest, inspection_report_version, created_at)
		VALUES (?, 'inspection_analysis_v1', 'v1', ?, ?, ?)`, attemptID, mixedInspectionDigest, reportVersion, mixedInspectionTime); err != nil {
		return 0, 0, fmt.Errorf("freeze report input: %w", err)
	}
	for offset, runID := range runLocators {
		itemSeq := 1
		if offset > 0 {
			itemSeq = 20 + offset
		}
		if err := mixedExec(db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,inspection_run_id) SELECT id, ?, 'inspection_run', ?, ? FROM attempt_input_snapshots WHERE attempt_id=?`, itemSeq, mixedInspectionDigest, runID, attemptID); err != nil {
			return 0, 0, fmt.Errorf("freeze report Run input: %w", err)
		}
	}
	for sequence, evidenceID := range []int{2, 3} {
		if err := mixedExec(db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,evidence_id) SELECT id,?, 'inspection_evidence', ?, ? FROM attempt_input_snapshots WHERE attempt_id=?`, sequence+2, mixedInspectionDigest, evidenceID, attemptID); err != nil {
			return 0, 0, fmt.Errorf("freeze report Evidence input: %w", err)
		}
	}
	for sequence, checkResultID := range []int{1, 2} {
		if err := mixedExec(db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,inspection_check_result_id) SELECT id,?, 'inspection_check_result', ?, ? FROM attempt_input_snapshots WHERE attempt_id=?`, sequence+4, mixedInspectionDigest, checkResultID, attemptID); err != nil {
			return 0, 0, fmt.Errorf("freeze report check-result input: %w", err)
		}
	}
	for offset, evidenceID := range extraEvidenceIDs {
		if err := mixedExec(db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,evidence_id) SELECT id,?, 'inspection_evidence', ?, ? FROM attempt_input_snapshots WHERE attempt_id=?`, offset+6, mixedInspectionDigest, evidenceID, attemptID); err != nil {
			return 0, 0, fmt.Errorf("freeze report extra Evidence input: %w", err)
		}
	}
	for offset, artifactID := range extraArtifactIDs {
		if err := mixedExec(db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,artifact_id) SELECT id,?, 'inspection_evidence_content', ?, ? FROM attempt_input_snapshots WHERE attempt_id=?`, offset+7, mixedInspectionDigest, artifactID, attemptID); err != nil {
			return 0, 0, fmt.Errorf("freeze report Evidence content artifact input: %w", err)
		}
	}
	if err := mixedExec(db, `UPDATE execution_attempts SET state='Assigned', runtime_slot='plinth', boot_id='plinth-report-boot', connection_epoch=1, lease_until=?, runtime_release_version='plinth-v1', agent_version='agent-v1', row_version=2 WHERE id=?`, "2026-08-29T00:00:00Z", attemptID); err != nil {
		return 0, 0, fmt.Errorf("assign report analysis: %w", err)
	}
	if err := mixedExec(db, `UPDATE execution_attempts SET state='Running', accepted_at=?, row_version=3 WHERE id=?`, mixedInspectionTime, attemptID); err != nil {
		return 0, 0, fmt.Errorf("start report analysis: %w", err)
	}
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,provider_request_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,usage_json,latency_ms,status,termination_reason,started_at,ended_at) VALUES (?,1,0,'chat','model',1,NULL,'v1','agent-v1',?, 'v1', ?, ?, ?, 2,1,0,0,NULL,NULL,'running',NULL,?,NULL)`, attemptID, mixedInspectionDigest, mixedInspectionDigest, mixedInspectionDigest, mixedInspectionDigest, mixedInspectionTime)
	if err != nil {
		return 0, 0, fmt.Errorf("seed running report model call: %w", err)
	}
	modelCallID, err := call.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	if err := mixedExec(db, `UPDATE model_calls SET status='succeeded', usage_json='{}', latency_ms=1, ended_at=? WHERE id=?`, mixedInspectionTime, modelCallID); err != nil {
		return 0, 0, fmt.Errorf("seal report model call: %w", err)
	}
	return attemptID, modelCallID, nil
}

func bootstrapMixedInspectionRun(db *sql.DB) error {
	return mixedExecMany(db,
		`INSERT INTO users(id, username, display_name, role, enabled, auth_revision, password_phc, password_change_required, password_change_required_at, row_version, created_at, updated_at)
		 VALUES (1, 'contract-verifier', 'Contract Verifier', 'admin', 1, 1, '$argon2id$fixture', 0, NULL, 1, '2026-08-28T00:00:00Z', '2026-08-28T00:00:00Z')`,
		`INSERT INTO label_contracts(id, version, yaml_body, contract_json, digest, parser_version, schema_version, state, row_version, created_at, activated_at)
		 VALUES (1, 1, '{}', '{}', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'v1', 'v1', 'draft', 1, '2026-08-28T00:00:00Z', NULL)`,
		`INSERT INTO business_systems(id, key, display_name, enabled, row_version, current_config_version_id, timezone, resource_refresh_interval_seconds, created_at)
		 VALUES (1, 'system', 'System', 0, 1, NULL, NULL, NULL, '2026-08-28T00:00:00Z')`,
		`INSERT INTO business_system_config_versions(id, business_system_id, version_seq, state, yaml_body, parser_version, schema_version, label_contract_version_id, journey_catalog_digest, journey_catalog_version, digest, created_by, created_at, published_at, system_key, display_name, enabled, timezone, resource_refresh_interval_seconds)
		 VALUES (1, 1, 1, 'draft', '{}', 'v1', 'v1', 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'v1', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 1, '2026-08-28T00:00:00Z', NULL, 'system', 'System', 1, 'UTC', 60)`,
		`INSERT INTO config_plans(id, config_version_id, plan_key, display_name, cron) VALUES (1, 1, 'plan', 'Plan', NULL)`,
		`INSERT INTO config_checks(id, plan_id, check_key, display_name, analysis_question, kind, query_mode, expression, range_seconds, step_seconds, journey_id, journey_params_json)
		 VALUES (1, 1, 'promql', 'PromQL', 'Is it healthy?', 'promql', 'instant', 'up', NULL, NULL, NULL, NULL),
		        (2, 1, 'browser', 'Browser', 'Is it visible?', 'browser', NULL, NULL, NULL, NULL, 'journey', '{}')`,
		`INSERT INTO inspection_runs(id, business_system_id, plan_key, config_version_id, label_contract_version_id, trigger_kind, scheduled_for, state, row_version, evidence_at, rerun_of_id, created_at)
		 VALUES (1, 1, 'plan', 1, 1, 'manual', NULL, 'Running', 1, '2026-08-28T00:00:00Z', NULL, '2026-08-28T00:00:00Z')`,
	)
}

func registerMixedRuntimeSlot(db *sql.DB, slot string) error {
	if err := mixedExec(db, `INSERT INTO runtime_slots(slot, state, current_credential_id, pending_credential_id, retiring_credential_id, row_version, created_at)
		VALUES (?, 'unregistered', NULL, NULL, NULL, 1, ?)`, slot, mixedInspectionTime); err != nil {
		return err
	}
	if err := mixedExec(db, `INSERT INTO runtime_credentials(slot, generation, token_digest, created_at, confirmed_at, first_authenticated_at, retired_at, row_version)
		VALUES (?, 1, zeroblob(32), ?, ?, NULL, NULL, 1)`, slot, mixedInspectionTime, mixedInspectionTime); err != nil {
		return err
	}
	var id int64
	if err := db.QueryRow(`SELECT id FROM runtime_credentials WHERE slot=?`, slot).Scan(&id); err != nil {
		return err
	}
	return mixedExec(db, `UPDATE runtime_slots SET state='registered', current_credential_id=?, row_version=2 WHERE slot=?`, id, slot)
}

func insertMixedArtifactEvidence(db *sql.DB, attemptID int64) error {
	if err := mixedExec(db, "BEGIN"); err != nil {
		return err
	}
	rollback := func(err error) error {
		_, _ = db.Exec("ROLLBACK")
		return err
	}
	if err := mixedExec(db, "PRAGMA defer_foreign_keys = ON"); err != nil {
		return rollback(err)
	}
	if err := mixedExec(db, `
		INSERT INTO evidence(attempt_id, tool_call_id, target_type, target_id, params_json, observed_at,
			result_json, artifact_id, warnings_json, errors_json, integrity, created_at)
		VALUES (?, NULL, 'inspection_run', 1, '{"plan_key":"plan","check_key":"promql"}', ?, NULL, 1, '[]', '[]', 'complete', ?)`,
		attemptID, mixedInspectionTime, mixedInspectionTime); err != nil {
		return rollback(err)
	}
	if err := mixedExec(db, `
		INSERT INTO artifact_blobs(id, sha256, size_bytes, storage_key, created_at)
		VALUES (1, ?, 0, 'blobs/mixed-inspection', ?)`, mixedInspectionDigest, mixedInspectionTime); err != nil {
		return rollback(err)
	}
	if err := mixedExec(db, `
		INSERT INTO artifacts(id, blob_id, kind, media_type, sensitive, retention_kind, owner_type, owner_id, expires_at, body_expired, created_at, created_by)
		VALUES (1, 1, 'report_file', 'application/json', 0, 'long_term', 'evidence', 1, NULL, 0, ?, 1)`, mixedInspectionTime); err != nil {
		return rollback(err)
	}
	if err := mixedExec(db, "COMMIT"); err != nil {
		return rollback(err)
	}
	return nil
}

func freezeMixedInput(db *sql.DB, attemptID int64, schemaKind string) error {
	if err := mixedExec(db, `
		INSERT INTO attempt_input_snapshots(attempt_id, schema_kind, renderer_version, content_digest, created_at)
		VALUES(?, ?, 'v1', ?, ?)`, attemptID, schemaKind, mixedInspectionDigest, mixedInspectionTime); err != nil {
		return err
	}
	return mixedExec(db, `
		INSERT INTO attempt_input_items(snapshot_id, item_seq, item_role, source_digest, inspection_run_id)
		SELECT id, 1, 'inspection_run', ?, 1 FROM attempt_input_snapshots WHERE attempt_id=?`, mixedInspectionDigest, attemptID)
}

func bootstrapMixedBrowserJourney(db *sql.DB, attemptID int64) error {
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := mixedExec(db, `
		INSERT INTO sessions(id, user_id, session_token_digest, auth_revision_at_issue, client_label, created_at, last_active_at, idle_expires_at, absolute_expires_at, revoked_at)
		VALUES (1, 1, zeroblob(32), 1, 'contract-verifier', '2026-08-28T00:00:00Z', '2026-08-28T00:00:00Z', '2026-08-28T12:00:00Z', '2026-09-04T00:00:00Z', NULL)`); err != nil {
		return err
	}
	if err := mixedExec(db, `
		INSERT INTO browser_identity_revisions(id, business_system_id, revision, name, start_url, probe_journey_id, probe_journey_version, probe_params_json, journey_catalog_digest, journey_catalog_version, created_by, created_at)
		VALUES (1, 1, 1, 'Browser', 'https://example.test', 'probe', 1, '{}', ?, 'v1', 1, ?)`, digest, mixedInspectionTime); err != nil {
		return err
	}
	if err := mixedExec(db, `
		INSERT INTO browser_identities(id, business_system_id, current_revision_id, current_profile_generation_id, state, row_version, created_at)
		VALUES (1, 1, 1, NULL, 'AuthenticationRequired', 1, ?)`, mixedInspectionTime); err != nil {
		return err
	}
	if err := mixedExec(db, `
		INSERT INTO browser_operations(id, identity_id, identity_revision_id, profile_generation_id, owner_attempt_id, kind, actor_user_id, actor_session_id, verification_manifest_item_id, clone_identity, state, journey_catalog_digest, journey_catalog_version, journey_id, journey_version, probe_phase, requested_at, start_dispatched_at, lintel_boot_id, lintel_connection_epoch, started_at, reconnect_deadline, ended_at, stop_confirmed_at, stop_confirmation_basis, cleanup_state_hash, start_rejected_at, start_reject_reason, terminal_reason, trace_artifact_id, trace_integrity, completion_digest, row_version)
		VALUES (1, 1, 1, NULL, NULL, 'manual_login', 1, 1, NULL, NULL, 'Queued', ?, 'v1', NULL, NULL, NULL, ?, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 1)`, digest, mixedInspectionTime); err != nil {
		return err
	}
	if err := startMixedBrowserOperation(db, 1, "lintel-login-boot"); err != nil {
		return err
	}
	if err := mixedExec(db, `
		INSERT INTO browser_probe_results(operation_id, probe_seq, phase, identity_revision_id, journey_id, journey_version, journey_catalog_digest, journey_catalog_version, result, reason_code, observed_at)
		VALUES (1, 1, 'publish', 1, 'probe', 1, ?, 'v1', 'Authenticated', NULL, ?)`, digest, mixedInspectionTime); err != nil {
		return err
	}
	if err := mixedExec(db, `
		INSERT INTO browser_profile_generations(id, identity_id, identity_revision_id, generation, chromium_revision, profile_manifest_digest, probe_journey_id, probe_journey_version, probe_catalog_digest, probe_catalog_version, published_operation_id, published_by, published_at)
		VALUES (1, 1, 1, 1, 'chromium-v1', ?, 'probe', 1, ?, 'v1', 1, 1, ?)`, digest, digest, mixedInspectionTime); err != nil {
		return err
	}
	if err := mixedExec(db, `
		UPDATE browser_operations
		SET stop_confirmed_at=?, stop_confirmation_basis='stop_ack', row_version=5
		WHERE id=1`, mixedInspectionTime); err != nil {
		return err
	}
	if err := insertMixedJourneyOperation(db, attemptID, digest, "v1", "wrong-journey", 1); err == nil {
		return fmt.Errorf("browser run_check accepted a Journey not frozen in its check")
	}
	if err := insertMixedJourneyOperation(db, attemptID, digest, "v1", "journey", 1); err != nil {
		return err
	}
	return startMixedBrowserOperation(db, 2, "lintel-journey-boot")
}

func insertMixedJourneyOperation(db *sql.DB, attemptID int64, catalogDigest, catalogVersion, journeyID string, journeyVersion int64) error {
	return mixedExec(db, `
		INSERT INTO browser_operations(id, identity_id, identity_revision_id, profile_generation_id, owner_attempt_id, kind, actor_user_id, actor_session_id, verification_manifest_item_id, clone_identity, state, journey_catalog_digest, journey_catalog_version, journey_id, journey_version, probe_phase, requested_at, start_dispatched_at, lintel_boot_id, lintel_connection_epoch, started_at, reconnect_deadline, ended_at, stop_confirmed_at, stop_confirmation_basis, cleanup_state_hash, start_rejected_at, start_reject_reason, terminal_reason, trace_artifact_id, trace_integrity, completion_digest, row_version)
		VALUES (2, 1, 1, 1, ?, 'journey', NULL, NULL, NULL, NULL, 'Queued', ?, ?, ?, ?, NULL, ?, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 1)`,
		attemptID, catalogDigest, catalogVersion, journeyID, journeyVersion, mixedInspectionTime)
}

func startMixedBrowserOperation(db *sql.DB, operationID int64, bootID string) error {
	if err := mixedExec(db, `
		UPDATE browser_operations
		SET state='Starting', start_dispatched_at=?, lintel_boot_id=?, lintel_connection_epoch=1, row_version=2
		WHERE id=?`, mixedInspectionTime, bootID, operationID); err != nil {
		return err
	}
	return mixedExec(db, `
		UPDATE browser_operations SET state='Running', started_at=?, row_version=3 WHERE id=?`, mixedInspectionTime, operationID)
}

func bootstrapMixedThanosConnection(db *sql.DB) error {
	return mixedExecMany(db,
		`INSERT INTO root_key_state(id, binding_revision, verifier_nonce, verifier_ciphertext, bound_at) VALUES (1, 1, zeroblob(12), zeroblob(16), '2026-08-28T00:00:00Z')`,
		`INSERT INTO connections(id, name, type, enabled, row_version, revalidation_required, current_revision_id, current_credential_generation_id, created_at) VALUES (1, 'thanos', 'thanos', 1, 1, 0, NULL, NULL, '2026-08-28T00:00:00Z')`,
		`INSERT INTO connection_revisions(id, connection_id, revision_seq, config_json, created_by, created_at) VALUES (1, 1, 1, '{}', NULL, '2026-08-28T00:00:00Z')`,
		`INSERT INTO credential_generations(id, connection_id, generation_seq, envelope_version, key_binding_revision, nonce, ciphertext, created_by, created_at) VALUES (1, 1, 1, 1, 1, zeroblob(12), zeroblob(16), NULL, '2026-08-28T00:00:00Z')`,
		`UPDATE connections SET current_revision_id=1, current_credential_generation_id=1, row_version=2 WHERE id=1`,
	)
}

func insertMixedRunCheckAttempt(db *sql.DB, checkKey string) (int64, error) {
	result, err := db.Exec(`INSERT INTO execution_attempts(attempt_type, scope_type, scope_id, plan_key, check_key, discovery_key, state, row_version, runtime_slot, requested_by_tool_call_id, connection_epoch, boot_id, lease_until, accepted_at, quoin_release_version, runtime_release_version, agent_version, termination_reason, created_at)
		VALUES ('inspection_collection', 'run_check', 1, NULL, ?, NULL, 'Queued', 1, NULL, NULL, NULL, NULL, NULL, NULL, 'quoin-v1', NULL, NULL, NULL, ?)`, checkKey, mixedInspectionTime)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func mixedExecMany(db *sql.DB, statements ...string) error {
	for _, statement := range statements {
		if err := mixedExec(db, statement); err != nil {
			return err
		}
	}
	return nil
}

func mixedExec(db *sql.DB, statement string, args ...any) error {
	if _, err := db.Exec(statement, args...); err != nil {
		return fmt.Errorf("mixed Inspection SQL failed (%s): %w", compactSQL(statement), err)
	}
	return nil
}

func mixedTransaction(db *sql.DB, work func(*sql.Tx) error) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err := work(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func mixedMustReject(db *sql.DB, statement string, args ...any) error {
	if _, err := db.Exec(statement, args...); err == nil {
		return fmt.Errorf("mixed Inspection SQL unexpectedly succeeded: %s", compactSQL(statement))
	}
	return nil
}

func mixedMustRejectCause(db *sql.DB, cause, statement string, args ...any) error {
	_, err := db.Exec(statement, args...)
	if err == nil {
		return fmt.Errorf("mixed Inspection SQL unexpectedly succeeded: %s", compactSQL(statement))
	}
	if !strings.Contains(err.Error(), cause) {
		return fmt.Errorf("mixed Inspection SQL rejected for the wrong reason; want %q, got %w", cause, err)
	}
	return nil
}

func compactSQL(statement string) string {
	return strings.Join(strings.Fields(statement), " ")
}

// seedEvidenceOwnedArtifact materializes an evidence-owned content artifact
// (id 9) for the run's first evidence and freezes it into the analysis input.
func seedEvidenceOwnedArtifact(db *sql.DB) error {
	return mixedExecMany(db,
		`INSERT INTO artifact_blobs(id, sha256, size_bytes, storage_key, created_at) VALUES (9, 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 6, 'blobs/t24c-evidence', '2026-08-28T00:00:00Z')`,
		`INSERT INTO artifacts(id, blob_id, kind, media_type, sensitive, retention_kind, owner_type, owner_id, expires_at, body_expired, created_at, created_by) VALUES (9, 9, 'report_file', 'application/json', 0, 'long_term', 'evidence', 2, NULL, 0, '2026-08-28T00:00:00Z', 1)`,
	)
}

// seedForeignEvidenceArtifact materializes an artifact owned by a real
// Evidence of ANOTHER run (run 3's closed check) and freezes it into this
// input; the run-fact arm must reject it even though it is frozen.
func seedForeignEvidenceArtifact(db *sql.DB) error {
	return mixedExecMany(db,
		`INSERT INTO artifact_blobs(id, sha256, size_bytes, storage_key, created_at) VALUES (10, 'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc', 6, 'blobs/t24c-foreign', '2026-08-28T00:00:00Z')`,
		`INSERT INTO artifacts(id, blob_id, kind, media_type, sensitive, retention_kind, owner_type, owner_id, expires_at, body_expired, created_at, created_by)
		 SELECT 10, 10, 'report_file', 'application/json', 0, 'long_term', 'evidence', e.id, NULL, 0, '2026-08-28T00:00:00Z', 1
		 FROM evidence e WHERE e.target_type='inspection_run' AND e.target_id=3`,
	)
}
