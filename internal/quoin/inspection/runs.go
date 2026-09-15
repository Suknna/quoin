package inspection

// Run read/cancel projections and the immutable report reads (T24,
// HTTP-INSPECT surface): LocatorId decimal-string wire shapes, keyset
// listings, and the report version history bound to the ledger. Reads run on
// the composition-injected read-only reader; the cancel command runs on the
// shared execution runner (ADR-0006).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// GetRun returns one run detail located by its immutable run id. Legacy
// declaration runs and plan runs share the same projection.
func (s *Service) GetRun(ctx context.Context, runID int64) (RunDetail, error) {
	return s.detailOn(ctx, s.reader, runID)
}

// ListRuns returns all runs newest first with a (created_at, id) keyset
// cursor (HTTP-PAGE-005 order); planKey optionally filters one plan.
func (s *Service) ListRuns(ctx context.Context, planKey, cursor string, limit int) ([]RunSummary, bool, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	query := `SELECT r.id, r.plan_key, bs.key, c.name, r.state, r.row_version, r.trigger_kind, r.scheduled_for, r.evidence_at, r.created_at
		FROM inspection_runs r
		LEFT JOIN business_systems bs ON bs.id = r.business_system_id
		LEFT JOIN connections c ON c.id = r.connection_id
		WHERE 1=1`
	args := []any{}
	if planKey != "" {
		query += ` AND r.plan_key=?`
		args = append(args, planKey)
	}
	if cursor != "" {
		createdAt, lastID, err := parseRunCursor(cursor)
		if err != nil {
			return nil, false, err
		}
		query += ` AND (r.created_at < ? OR (r.created_at = ? AND r.id < ?))`
		args = append(args, createdAt, createdAt, lastID)
	}
	query += ` ORDER BY r.created_at DESC, r.id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	summaries := []RunSummary{}
	for rows.Next() {
		var item RunSummary
		var id int64
		var systemKey, connectionName sql.NullString
		var scheduledFor, evidenceAt sql.NullString
		if err = rows.Scan(&id, &item.PlanKey, &systemKey, &connectionName, &item.State, &item.RowVersion, &item.TriggerKind, &scheduledFor, &evidenceAt, &item.CreatedAt); err != nil {
			return nil, false, err
		}
		item.ID = locatorID(id)
		if systemKey.Valid {
			item.BusinessSystemKey = &systemKey.String
		}
		if connectionName.Valid {
			item.ConnectionName = &connectionName.String
		}
		if scheduledFor.Valid {
			item.ScheduledFor = &scheduledFor.String
		}
		if evidenceAt.Valid {
			item.EvidenceAt = &evidenceAt.String
		}
		summaries = append(summaries, item)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(summaries) > limit
	if more {
		summaries = summaries[:limit]
	}
	return summaries, more, nil
}

func parseRunCursor(cursor string) (string, int64, error) {
	createdAt, id, found := strings.Cut(cursor, "\x00")
	if !found || createdAt == "" {
		return "", 0, fmt.Errorf("invalid inspection run cursor")
	}
	value, err := strconv.ParseInt(id, 10, 64)
	if err != nil || value <= 0 {
		return "", 0, fmt.Errorf("invalid inspection run cursor")
	}
	return createdAt, value, nil
}

// ListReports returns the run's immutable report versions, newest first with a
// (version DESC) cursor.
func (s *Service) ListReports(ctx context.Context, runID int64, limit int) ([]ReportSummaryItem, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT version, model_id, created_at FROM inspection_reports WHERE run_id=? ORDER BY version DESC LIMIT ?`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ReportSummaryItem{}
	for rows.Next() {
		var item ReportSummaryItem
		if err = rows.Scan(&item.Version, &item.ModelID, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// GetReport returns one immutable report version with its bound Evidence set.
func (s *Service) GetReport(ctx context.Context, runID, version int64) (ReportDetail, error) {
	var detail ReportDetail
	var reportID, runIDValue int64
	err := s.reader.QueryRowContext(ctx, `
		SELECT id, run_id, version, evidence_digest, model_id, content, created_at
		FROM inspection_reports WHERE run_id=? AND version=?`, runID, version).
		Scan(&reportID, &runIDValue, &detail.Version, &detail.EvidenceDigest, &detail.ModelID, &detail.Content, &detail.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportDetail{}, ErrNotFound
	}
	if err != nil {
		return ReportDetail{}, err
	}
	detail.ID = locatorID(reportID)
	detail.RunID = locatorID(runIDValue)
	rows, err := s.reader.QueryContext(ctx, `
		SELECT evidence_id FROM inspection_report_evidence WHERE report_id=(
			SELECT id FROM inspection_reports WHERE run_id=? AND version=?) ORDER BY ordinal`, runID, version)
	if err != nil {
		return ReportDetail{}, err
	}
	defer rows.Close()
	detail.EvidenceIDs = []string{}
	for rows.Next() {
		var evidenceID int64
		if err = rows.Scan(&evidenceID); err != nil {
			return ReportDetail{}, err
		}
		detail.EvidenceIDs = append(detail.EvidenceIDs, locatorID(evidenceID))
	}
	return detail, rows.Err()
}

// CancelOutcome separates the durable Run projection from the runtime work
// that still needs a best-effort cancellation delivery after commit.
type CancelOutcome struct {
	Detail             RunDetail
	DispatchAttemptIDs []int64
}

// CancelRun preserves the original domain API for callers that only need the
// current authoritative Run projection.
func (s *Service) CancelRun(ctx context.Context, principalID int64, clientCommandID string, runID, expectedRowVersion int64) (RunDetail, error) {
	outcome, err := s.CancelRunWithDispatch(ctx, principalID, clientCommandID, runID, expectedRowVersion)
	return outcome.Detail, err
}

// CancelRunWithDispatch commits one cancellation fence for either collection
// children of an active Run or its active report analysis (执行器账本命令:
// 台账与审计由执行器同事务持久化). Only Cancelling attempts are returned for
// external delivery; only Queued work closes in this transaction, while an
// Assigned DispatchAttempt may already be in flight.
func (s *Service) CancelRunWithDispatch(ctx context.Context, principalID int64, clientCommandID string, runID, expectedRowVersion int64) (CancelOutcome, error) {
	digest := auth.DigestCommand(CommandCancelRun, map[string]any{"runId": runID, "expectedRowVersion": expectedRowVersion})
	// dispatchIDs collects the Cancelling children of the first execution; a
	// replay skips the business stage and correctly returns an empty list.
	dispatchIDs := []int64{}
	outcome, err := execution.Run(ctx, s.runner, s.cancelRun, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (RunDetail, execution.Change, error) {
		var runState string
		var rowVersion int64
		err := tx.QueryRowContext(ctx, `
			SELECT r.state,r.row_version FROM inspection_runs r WHERE r.id=?`, runID).Scan(&runState, &rowVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return RunDetail{}, execution.Unchanged, ErrNotFound
		}
		if err != nil {
			return RunDetail{}, execution.Unchanged, err
		}
		if rowVersion != expectedRowVersion {
			return RunDetail{}, execution.Unchanged, &execution.Rejection{Code: "row_version_conflict", Detail: "巡检 Run 已变化或已进入终态，请刷新后重试", ObjectID: runID}
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT id FROM execution_attempts
			WHERE state IN ('Queued','Assigned','Running','Cancelling') AND (
				(scope_type='run_check' AND scope_id=?)
				OR (attempt_type='inspection_analysis' AND scope_type='run' AND scope_id=?)
			) ORDER BY id`, runID, runID)
		if err != nil {
			return RunDetail{}, execution.Unchanged, err
		}
		var childIDs []int64
		for rows.Next() {
			var childID int64
			if err = rows.Scan(&childID); err != nil {
				rows.Close()
				return RunDetail{}, execution.Unchanged, err
			}
			childIDs = append(childIDs, childID)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return RunDetail{}, execution.Unchanged, err
		}
		if err = rows.Close(); err != nil {
			return RunDetail{}, execution.Unchanged, err
		}
		if runState != "Queued" && runState != "Running" && len(childIDs) == 0 {
			// A terminal collection result committed first. With the caller's still
			// current version there is no cancellation work left, so commit this
			// command as a successful observation of the winner rather than turning
			// a result-vs-cancel race into a spurious 409. The observation changes
			// nothing; the runner classifies it as unchanged.
			if runState != "Completed" && runState != "CompletedWithGaps" {
				return RunDetail{}, execution.Unchanged, &execution.Rejection{Code: "row_version_conflict", Detail: "巡检 Run 已变化或已进入终态，请刷新后重试", ObjectID: runID}
			}
			detail, detailErr := s.detailOn(ctx, tx, runID)
			if detailErr != nil {
				return RunDetail{}, execution.Unchanged, detailErr
			}
			return detail, execution.Unchanged, nil
		}
		attempts := s.Attempts()
		for _, childID := range childIDs {
			state, fenceErr := attempts.CancelFenceOn(ctx, tx, childID)
			if fenceErr != nil {
				return RunDetail{}, execution.Unchanged, fenceErr
			}
			if state == "Cancelling" {
				dispatchIDs = append(dispatchIDs, childID)
			}
		}
		if runState == "Queued" || runState == "Running" {
			if _, err = tx.ExecContext(ctx, `
				UPDATE inspection_runs SET state='Cancelled',row_version=row_version+1
				WHERE id=? AND row_version=? AND state IN ('Queued','Running')`, runID, expectedRowVersion); err != nil {
				return RunDetail{}, execution.Unchanged, err
			}
		}
		detail, err := s.detailOn(ctx, tx, runID)
		if err != nil {
			return RunDetail{}, execution.Unchanged, err
		}
		return detail, execution.Changed, nil
	}, func(detail RunDetail) int64 { return runID })
	if err != nil {
		return CancelOutcome{}, translateCancelError(err)
	}
	if outcome.Replayed {
		// A replayed cancellation returns its stored projection; the best-effort
		// dispatch list belongs to the first execution only.
		detail := outcome.Result
		detail.RunID = mustLocator(detail.ID)
		return CancelOutcome{Detail: detail}, nil
	}
	return CancelOutcome{Detail: outcome.Result, DispatchAttemptIDs: dispatchIDs}, nil
}

// translateCancelError keeps the cancel command's historic not-found identity:
// a missing run surfaces the plain sentinel without a durable trace, exactly
// like the pre-runner path.
func translateCancelError(err error) error {
	if errors.Is(err, ErrNotFound) {
		return err
	}
	return translateCommandError(err)
}

// rowQuerier abstracts the one-connection pool so legacy/plan locator reads
// work identically on *sql.DB and the caller's exclusive *sql.Conn.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// businessSystemKeyByID and connectionNameByID resolve display locators for
// the mixed legacy/plan run projection.
func businessSystemKeyByID(ctx context.Context, db rowQuerier, id int64) (string, error) {
	var key string
	err := db.QueryRowContext(ctx, `SELECT key FROM business_systems WHERE id=?`, id).Scan(&key)
	return key, err
}

func connectionNameByID(ctx context.Context, db rowQuerier, id int64) (string, error) {
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM connections WHERE id=?`, id).Scan(&name)
	return name, err
}

func (s *Service) detailOn(ctx context.Context, q audit.Reader, runID int64) (RunDetail, error) {
	var detail RunDetail
	var evidenceAt, scheduledFor sql.NullString
	var systemID, connectionID sql.NullInt64
	err := q.QueryRowContext(ctx, `
		SELECT r.id, r.plan_key, r.business_system_id, r.connection_id, r.state, r.row_version, r.trigger_kind, r.scheduled_for, r.evidence_at, r.created_at
		FROM inspection_runs r WHERE r.id=?`, runID).
		Scan(&detail.RunID, &detail.PlanKey, &systemID, &connectionID, &detail.State, &detail.RowVersion, &detail.TriggerKind, &scheduledFor, &evidenceAt, &detail.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RunDetail{}, ErrNotFound
	}
	if err != nil {
		return RunDetail{}, err
	}
	detail.ID = locatorID(detail.RunID)
	if systemID.Valid {
		key, err := businessSystemKeyByID(ctx, q, systemID.Int64)
		if err != nil {
			return RunDetail{}, err
		}
		detail.BusinessSystemKey = &key
	}
	if connectionID.Valid {
		name, err := connectionNameByID(ctx, q, connectionID.Int64)
		if err != nil {
			return RunDetail{}, err
		}
		detail.ConnectionName = &name
	}
	if scheduledFor.Valid {
		detail.ScheduledFor = &scheduledFor.String
	}
	if evidenceAt.Valid {
		detail.EvidenceAt = &evidenceAt.String
	}
	rows, err := q.QueryContext(ctx, `
		SELECT k.check_key,
			COALESCE(x.status, CASE a.state WHEN 'Cancelling' THEN 'cancelling' WHEN 'Cancelled' THEN 'gap' ELSE '' END),
			x.evidence_id, COALESCE(x.gap_reason, CASE WHEN a.state='Cancelled' THEN 'cancelled' END)
		FROM inspection_run_checks k
		LEFT JOIN inspection_check_results x ON x.run_id=k.run_id AND x.check_key=k.check_key
		LEFT JOIN execution_attempts a ON a.scope_type='run_check' AND a.scope_id=k.run_id AND a.check_key=k.check_key
		WHERE k.run_id=? ORDER BY k.check_key`, runID)
	if err != nil {
		return detail, err
	}
	defer rows.Close()
	detail.Checks = []CheckResult{}
	for rows.Next() {
		var check CheckResult
		var evidenceID sql.NullInt64
		var gapReason sql.NullString
		if err = rows.Scan(&check.CheckKey, &check.Status, &evidenceID, &gapReason); err != nil {
			return detail, err
		}
		if evidenceID.Valid {
			locator := locatorID(evidenceID.Int64)
			check.EvidenceID = &locator
		}
		if gapReason.Valid {
			check.GapReason = &gapReason.String
		}
		detail.Checks = append(detail.Checks, check)
	}
	if err = rows.Err(); err != nil {
		return detail, err
	}
	if err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM inspection_reports WHERE run_id=?`, runID).Scan(&detail.ReportCount); err != nil {
		return detail, err
	}
	var activeAnalysis int
	if err = q.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM execution_attempts
			WHERE attempt_type='inspection_analysis' AND scope_type='run' AND scope_id=?
			AND state IN ('Queued','Assigned','Running','Cancelling')
		)`, runID).Scan(&activeAnalysis); err != nil {
		return detail, err
	}
	detail.AnalysisActive = activeAnalysis != 0
	var latestID int64
	var latestState string
	var terminationReason sql.NullString
	err = q.QueryRowContext(ctx, `
		SELECT id, state, termination_reason
		FROM execution_attempts
		WHERE attempt_type='inspection_analysis' AND scope_type='run' AND scope_id=?
		ORDER BY id DESC LIMIT 1`, runID).Scan(&latestID, &latestState, &terminationReason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return detail, err
	}
	if err == nil {
		detail.LatestAnalysis = &InspectionAttemptStatus{ID: locatorID(latestID), State: latestState}
		if terminationReason.Valid {
			detail.LatestAnalysis.TerminationReason = &terminationReason.String
		}
	}
	return detail, nil
}
