package deployment

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// Category outcome/priority follows the frozen verification-result-profile:
// warned categories outrank passed, failed categories outrank warned, and the
// verifier conflict is the worst failed classification.
var categoryOutcome = map[string]string{
	"passed":                       "passed",
	"not_run":                      "warned",
	"operator_cancelled":           "warned",
	"environment_unavailable":      "warned",
	"infrastructure_interrupted":   "warned",
	"cleanup_indeterminate":        "warned",
	"subject_drift":                "warned",
	"functional_assertion_failed":  "failed",
	"cleanup_residue":              "failed",
	"verifier_conflict":            "failed",
	"verifier_invariant_violation": "failed",
}

var categoryPriority = map[string]int{
	"passed": 0, "not_run": 10, "operator_cancelled": 20, "environment_unavailable": 30,
	"infrastructure_interrupted": 40, "cleanup_indeterminate": 50, "subject_drift": 60,
	"functional_assertion_failed": 100, "cleanup_residue": 110, "verifier_conflict": 120,
	"verifier_invariant_violation": 130,
}

// ErrInProgress reports undecidable items (VERIFY-DA-004).
var ErrInProgress = &Error{Family: errState, Message: "verification_in_progress"}

type itemVerdict struct {
	itemID   int64
	scenario string
	category string
	outcome  string
}

// verdicts computes the per-item final classification. An item with two
// distinct immutable results is a verifier conflict.
func (service *Service) verdicts(ctx context.Context, invocationID int64) ([]itemVerdict, bool, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT r.item_id,i.scenario_id,r.category,r.result_digest
		FROM verification_item_results r JOIN verification_invocation_items i ON i.id=r.item_id
		WHERE i.invocation_id=? ORDER BY r.item_id, r.id`, invocationID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	type aggregate struct {
		scenario        string
		bestCategory    string
		distinctResults map[string]bool
	}
	items := map[int64]*aggregate{}
	for rows.Next() {
		var itemID int64
		var scenario, category, resultDigest string
		if err := rows.Scan(&itemID, &scenario, &category, &resultDigest); err != nil {
			return nil, false, err
		}
		entry, exists := items[itemID]
		if !exists {
			entry = &aggregate{scenario: scenario, bestCategory: category, distinctResults: map[string]bool{}}
			items[itemID] = entry
		}
		entry.distinctResults[resultDigest] = true
		if categoryPriority[category] > categoryPriority[entry.bestCategory] {
			entry.bestCategory = category
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	total := 0
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM verification_invocation_items WHERE invocation_id=?`, invocationID).Scan(&total); err != nil {
		return nil, false, err
	}
	if len(items) != total {
		return nil, false, ErrInProgress
	}
	verdicts := make([]itemVerdict, 0, len(items))
	for itemID, entry := range items {
		category := entry.bestCategory
		if len(entry.distinctResults) > 1 {
			category = "verifier_conflict"
		}
		outcome, known := categoryOutcome[category]
		if !known {
			return nil, false, service.fail(errState, "unknown result category %q", category)
		}
		verdicts = append(verdicts, itemVerdict{itemID: itemID, scenario: entry.scenario, category: category, outcome: outcome})
	}
	return verdicts, true, nil
}

// Finalize closes an invocation whose items are all decidable: it stages the
// canonical long-term bundle Artifact first, then commits the single receipt
// in one writer transaction after rechecking every digest (VERIFY-DA-004).
// bySession selects initiating-session vs system-deadline finalization; nil
// means the deadline path.
func (service *Service) Finalize(ctx context.Context, invocationID int64, bySession *int64) (*VerificationDetail, error) {
	if existing, ok, err := service.receiptFor(ctx, invocationID); err != nil {
		return nil, err
	} else if ok {
		return service.Load(ctx, existing)
	}
	if service.artifacts == nil {
		return nil, service.fail(errUnavailable, "artifact store is not wired")
	}
	manifest, err := service.loadManifest(ctx, invocationID)
	if err != nil {
		return nil, err
	}
	// Final drift sweep before the evidence freeze.
	if err := service.observeDrifts(ctx, invocationID); err != nil {
		return nil, err
	}
	verdicts, decidable, err := service.verdicts(ctx, invocationID)
	if err != nil {
		return nil, err
	}
	if !decidable {
		return nil, ErrInProgress
	}
	if err := service.assertObservationClosure(ctx, invocationID); err != nil {
		return nil, err
	}
	digests, err := service.collectionDigests(ctx, invocationID)
	if err != nil {
		return nil, err
	}
	driftCount := 0
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM verification_subject_drifts WHERE invocation_id=?`, invocationID).Scan(&driftCount); err != nil {
		return nil, err
	}
	conflictCount := 0
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM verification_result_conflicts WHERE item_id IN (SELECT id FROM verification_invocation_items WHERE invocation_id=?)`, invocationID).Scan(&conflictCount); err != nil {
		return nil, err
	}
	outcome := "passed"
	for _, verdict := range verdicts {
		if verdict.outcome == "failed" {
			outcome = "failed"
			break
		}
		if verdict.outcome == "warned" {
			outcome = "warned"
		}
	}
	if conflictCount > 0 {
		outcome = "failed"
	} else if driftCount > 0 && outcome == "passed" {
		outcome = "warned"
	}
	now := service.now().UTC()
	if now.After(manifest.deadline) {
		return nil, service.fail(errConflict, "invocation missed its fixed eight-hour deadline; start a new invocation")
	}
	// One frozen commit instant: the bundle artifact rows, the statement's
	// embedded times and the receipt all carry this single sample, so the
	// a.created_at <= finalized_at closure holds by construction.
	commitClock := now
	// The snapshot must cover every retained observation: deadline synthesis
	// stamps not_run results at the boundary and typed observations carry
	// nanosecond submission instants, so the receipt snapshots at the latest
	// observed fact, never before it.
	snapshot := commitClock
	var latestObserved string
	if err := service.db.QueryRowContext(ctx, `SELECT MAX(r.observed_at) FROM verification_item_results r
		JOIN verification_invocation_items i ON i.id=r.item_id WHERE i.invocation_id=?`, invocationID).Scan(&latestObserved); err != nil {
		return nil, err
	}
	if latest, err := time.Parse(time.RFC3339Nano, latestObserved); err == nil && latest.After(snapshot) {
		snapshot = latest
	}
	if snapshot.After(manifest.deadline) {
		snapshot = manifest.deadline
	}
	finalized := snapshot
	_ = commitClock
	statement, evidenceIndex, err := service.buildStatement(ctx, manifest, verdicts, digests, outcome, snapshot, finalized)
	if err != nil {
		return nil, err
	}
	body, err := canonicalJSON(statement)
	if err != nil {
		return nil, err
	}
	finalDigest := sha256Hex(body)
	artifactID, _, err := service.artifacts.CommitVerificationArtifact(ctx, "verification_bundle", "application/json", "long_term", invocationID, body, commitClock)
	if err != nil {
		return nil, fmt.Errorf("stage finalization bundle: %w", err)
	}
	evidenceBody, err := canonicalJSON(evidenceIndex)
	if err != nil {
		return nil, err
	}
	if _, _, err := service.artifacts.CommitVerificationArtifact(ctx, "verification_attachment", "application/vnd.quoin.verification-evidence.v1+json", "long_term", invocationID, evidenceBody, commitClock); err != nil {
		return nil, fmt.Errorf("stage evidence index: %w", err)
	}
	err = service.withConn(ctx, func(conn *sql.Conn) error {
		// Recheck the complete closure inside the writer transaction.
		var receipt int64
		err := conn.QueryRowContext(ctx, `SELECT id FROM verification_finalization_receipts WHERE invocation_id=?`, invocationID).Scan(&receipt)
		if err == nil {
			return errAlreadyFinalized
		}
		if err != sql.ErrNoRows {
			return err
		}
		if err := service.assertObservationClosureOn(ctx, conn, invocationID); err != nil {
			return err
		}
		redigests, err := service.collectionDigestsOn(ctx, conn, invocationID)
		if err != nil {
			return err
		}
		if *redigests != *digests {
			return service.fail(errConflict, "verification evidence changed during finalization")
		}
		finalizerType, finalizerSession := "system_deadline", any(nil)
		if bySession != nil {
			finalizerType = "initiating_admin_session"
			finalizerSession = *bySession
			if *bySession != manifest.sessionID {
				return service.fail(errInvalid, "only the initiating Admin Session may finalize")
			}
			if err := service.requireActiveAdmin(ctx, conn, *bySession, manifest.principalID); err != nil {
				return err
			}
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO verification_finalization_receipts(
			invocation_id,manifest_digest,applicable_set_digest,item_set_digest,result_set_digest,helper_import_set_digest,
			typed_observation_set_digest,conflict_set_digest,subject_drift_digest,overall_outcome,final_result_digest,
			canonical_artifact_id,snapshot_at,finalized_at,finalized_by_type,finalized_by_session_id)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			invocationID, manifest.manifestDigest, manifest.applicableSetDigest, manifest.itemSetDigest, digests.resultSet, digests.helperImportSet,
			digests.observationSet, digests.conflictSet, digests.driftSet, outcome, finalDigest,
			artifactID, snapshot.Format(time.RFC3339Nano), finalized.Format(time.RFC3339Nano), finalizerType, finalizerSession)
		if err != nil {
			return translateInsert(err, "verification finalization receipt")
		}
		return nil
	})
	if err != nil {
		if err == errAlreadyFinalized {
			return service.Load(ctx, invocationID)
		}
		return nil, err
	}
	return service.Load(ctx, invocationID)
}

var errAlreadyFinalized = &Error{Family: errConflict, Message: "verification invocation is already finalized"}

func (service *Service) receiptFor(ctx context.Context, invocationID int64) (int64, bool, error) {
	var receipt int64
	err := service.db.QueryRowContext(ctx, `SELECT id FROM verification_finalization_receipts WHERE invocation_id=?`, invocationID).Scan(&receipt)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return receipt, true, nil
}

type manifestRecord struct {
	id                     int64
	sessionID, principalID int64
	releaseSubjectDigest   string
	deploymentConfigDigest string
	publicOriginDigest     string
	catalogDigest          string
	resultProfileDigest    string
	manifestDigest         string
	applicableSetDigest    string
	itemSetDigest          string
	canonicalInputDigest   string
	startedAt              string
	deadline               time.Time
}

func (service *Service) loadManifest(ctx context.Context, invocationID int64) (*manifestRecord, error) {
	record := &manifestRecord{id: invocationID}
	var deadlineText string
	err := service.db.QueryRowContext(ctx, `SELECT admin_session_id,principal_user_id,release_subject_digest,deployment_config_digest,public_origin_digest,
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

type setDigests struct {
	resultSet       string
	helperImportSet string
	observationSet  string
	conflictSet     string
	driftSet        string
}

func (service *Service) collectionDigests(ctx context.Context, invocationID int64) (*setDigests, error) {
	return service.collectionDigestsOn(ctx, service.db, invocationID)
}

func (service *Service) collectionDigestsOn(ctx context.Context, reader commandReaderLike, invocationID int64) (*setDigests, error) {
	digests := &setDigests{}
	var err error
	if digests.resultSet, err = digestOn(ctx, reader, `SELECT id,item_id,input_digest,result_digest,outcome,category,committed_at FROM verification_item_results
		WHERE item_id IN (SELECT id FROM verification_invocation_items WHERE invocation_id=?) ORDER BY id`, invocationID); err != nil {
		return nil, err
	}
	if digests.helperImportSet, err = digestOn(ctx, reader, `SELECT id,request_digest,report_digest,received_at FROM verification_helper_imports WHERE invocation_id=? ORDER BY id`, invocationID); err != nil {
		return nil, err
	}
	if digests.observationSet, err = digestOn(ctx, reader, `SELECT id,result_id,visual_result,motion_result,focus_occlusion_result,submitted_at FROM verification_typed_observations
		WHERE result_id IN (SELECT id FROM verification_item_results WHERE item_id IN (SELECT id FROM verification_invocation_items WHERE invocation_id=?)) ORDER BY id`, invocationID); err != nil {
		return nil, err
	}
	if digests.conflictSet, err = digestOn(ctx, reader, `SELECT id,item_id,first_result_id,conflicting_result_id,created_at FROM verification_result_conflicts
		WHERE item_id IN (SELECT id FROM verification_invocation_items WHERE invocation_id=?) ORDER BY id`, invocationID); err != nil {
		return nil, err
	}
	if digests.driftSet, err = digestOn(ctx, reader, `SELECT id,object_kind,drift_field,item_id,frozen_digest,current_digest,observed_at FROM verification_subject_drifts WHERE invocation_id=? ORDER BY id`, invocationID); err != nil {
		return nil, err
	}
	return digests, nil
}

type commandReaderLike interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func digestOn(ctx context.Context, db commandReaderLike, query string, arguments ...any) (string, error) {
	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return "", err
	}
	entries := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return "", err
		}
		entry := map[string]any{}
		for index, column := range columns {
			switch value := values[index].(type) {
			case []byte:
				entry[column] = string(value)
			default:
				entry[column] = value
			}
		}
		entries = append(entries, entry)
	}
	return canonicalDigest(entries)
}

// assertObservationClosure rejects finalization while a ui_observation item
// still needs its human observation for a passed/functional result.
func (service *Service) assertObservationClosure(ctx context.Context, invocationID int64) error {
	return service.assertObservationClosureOn(ctx, service.db, invocationID)
}

func (service *Service) assertObservationClosureOn(ctx context.Context, reader commandReaderLike, invocationID int64) error {
	var missing int
	err := reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM verification_invocation_items i
		JOIN verification_item_results r ON r.item_id=i.id AND r.category IN ('passed','functional_assertion_failed')
		WHERE i.invocation_id=? AND i.object_kind='ui_observation'
		AND NOT EXISTS (SELECT 1 FROM verification_typed_observations o WHERE o.result_id=r.id)`, invocationID).Scan(&missing)
	if err != nil {
		return err
	}
	if missing > 0 {
		return ErrInProgress
	}
	return nil
}

var _ = json.Marshal
var _ = gen.VerificationCatalogYAML
