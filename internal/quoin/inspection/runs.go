package inspection

// Run read/cancel projections and the immutable report reads (T24,
// HTTP-INSPECT surface): LocatorId decimal-string wire shapes, keyset
// listings, and the report version history bound to the ledger.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/auth"
)

// GetRun returns one run detail bound to the system key.
func (s *Service) GetRun(ctx context.Context, systemKey string, runID int64) (RunDetail, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return RunDetail{}, err
	}
	defer conn.Close()
	return s.detailOn(ctx, conn, systemKey, runID)
}

// ListRuns returns the system's runs newest first with a (created_at, id)
// keyset cursor (HTTP-PAGE-005 order).
func (s *Service) ListRuns(ctx context.Context, systemKey, cursor string, limit int) ([]RunSummary, bool, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	query := `SELECT r.id, r.plan_key, r.state, r.row_version, r.trigger_kind, r.scheduled_for, r.evidence_at, r.created_at
		FROM inspection_runs r JOIN business_systems s ON s.id=r.business_system_id WHERE s.key=?`
	args := []any{systemKey}
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
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []RunSummary{}
	for rows.Next() {
		var item RunSummary
		var id int64
		var scheduledFor, evidenceAt sql.NullString
		if err = rows.Scan(&id, &item.PlanKey, &item.State, &item.RowVersion, &item.TriggerKind, &scheduledFor, &evidenceAt, &item.CreatedAt); err != nil {
			return nil, false, err
		}
		item.ID = locatorID(id)
		item.BusinessSystemKey = systemKey
		if scheduledFor.Valid {
			item.ScheduledFor = &scheduledFor.String
		}
		if evidenceAt.Valid {
			item.EvidenceAt = &evidenceAt.String
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, nil
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
	rows, err := s.db.QueryContext(ctx, `
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
	var runIDValue int64
	err := s.db.QueryRowContext(ctx, `
		SELECT run_id, version, evidence_digest, model_id, content, created_at
		FROM inspection_reports WHERE run_id=? AND version=?`, runID, version).
		Scan(&runIDValue, &detail.Version, &detail.EvidenceDigest, &detail.ModelID, &detail.Content, &detail.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportDetail{}, ErrNotFound
	}
	if err != nil {
		return ReportDetail{}, err
	}
	detail.RunID = locatorID(runIDValue)
	rows, err := s.db.QueryContext(ctx, `
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

// CancelRun applies the one-shot cancel fence: the command carries the current
// row version; Queued children close here, dispatched children move to
// Cancelling and the Run only becomes terminal once the fences settle.
func (s *Service) CancelRun(ctx context.Context, principalID int64, clientCommandID, systemKey string, runID, expectedRowVersion int64) (RunDetail, error) {
	const command = "inspection_run.cancel"
	digest := auth.DigestCommand(command, map[string]any{"systemKey": systemKey, "runId": runID, "expectedRowVersion": expectedRowVersion})
	if d, replayed, err := s.replay(ctx, principalID, clientCommandID, digest); replayed || err != nil {
		return d, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return RunDetail{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return RunDetail{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if d, replayed, err := s.replayOn(ctx, conn, principalID, clientCommandID, digest); replayed || err != nil {
		if replayed {
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return RunDetail{}, err
			}
			committed = true
		}
		return d, err
	}
	var systemID int64
	if err = conn.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, systemKey).Scan(&systemID); err != nil {
		return RunDetail{}, ErrNotFound
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT id, state FROM execution_attempts
		WHERE scope_type='run_check' AND scope_id=? AND state IN ('Queued','Assigned','Running')`, runID)
	if err != nil {
		return RunDetail{}, err
	}
	var childIDs []int64
	for rows.Next() {
		var childID int64
		var childState string
		if err = rows.Scan(&childID, &childState); err != nil {
			rows.Close()
			return RunDetail{}, err
		}
		childIDs = append(childIDs, childID)
	}
	if err = rows.Close(); err != nil {
		return RunDetail{}, err
	}
	attempts := attempt.NewService(s.db)
	for _, childID := range childIDs {
		if _, err = attempts.CancelFenceOn(ctx, conn, childID); err != nil {
			return RunDetail{}, err
		}
	}
	update, err := conn.ExecContext(ctx, `
		UPDATE inspection_runs SET state='Cancelled', row_version=row_version+1
		WHERE id=? AND business_system_id=? AND row_version=? AND state IN ('Queued','Running')`,
		runID, systemID, expectedRowVersion)
	if err != nil {
		return RunDetail{}, err
	}
	affected, _ := update.RowsAffected()
	if affected == 0 {
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest, &RejectionError{Code: "row_version_conflict", Detail: "巡检 Run 已变化或已进入终态，请刷新后重试", SystemKey: systemKey, ObjectID: runID}, &committed)
	}
	if err = s.audit(ctx, conn, principalID, clientCommandID, command, runID, s.nowText()); err != nil {
		return RunDetail{}, err
	}
	detail, err := s.detailOn(ctx, conn, systemKey, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if err = s.recordCommand(ctx, conn, principalID, clientCommandID, command, digest, runID, detail); err != nil {
		return RunDetail{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return RunDetail{}, err
	}
	committed = true
	return detail, nil
}

func (s *Service) detailOn(ctx context.Context, conn *sql.Conn, systemKey string, runID int64) (RunDetail, error) {
	var detail RunDetail
	var evidenceAt, scheduledFor sql.NullString
	err := conn.QueryRowContext(ctx, `
		SELECT r.id, r.plan_key, r.state, r.row_version, r.trigger_kind, r.scheduled_for, r.evidence_at, r.created_at
		FROM inspection_runs r JOIN business_systems b ON b.id=r.business_system_id
		WHERE b.key=? AND r.id=?`, systemKey, runID).
		Scan(&detail.RunID, &detail.PlanKey, &detail.State, &detail.RowVersion, &detail.TriggerKind, &scheduledFor, &evidenceAt, &detail.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RunDetail{}, ErrNotFound
	}
	if err != nil {
		return RunDetail{}, err
	}
	detail.ID = locatorID(detail.RunID)
	detail.BusinessSystemKey = systemKey
	if scheduledFor.Valid {
		detail.ScheduledFor = &scheduledFor.String
	}
	if evidenceAt.Valid {
		detail.EvidenceAt = &evidenceAt.String
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT c.check_key, COALESCE(x.status,''), x.evidence_id, x.gap_reason
		FROM config_checks c
		JOIN config_plans p ON p.id=c.plan_id
		JOIN inspection_runs r ON r.config_version_id=p.config_version_id AND r.plan_key=p.plan_key
		LEFT JOIN inspection_check_results x ON x.run_id=r.id AND x.check_key=c.check_key
		WHERE r.id=? ORDER BY c.check_key`, runID)
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
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM inspection_reports WHERE run_id=?`, runID).Scan(&detail.ReportCount); err != nil {
		return detail, err
	}
	return detail, nil
}
