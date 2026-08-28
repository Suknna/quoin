// Package inspection owns manual Inspection Run creation, run_check child
// Attempts, PromQL ResultProposal closure, and Run convergence over the frozen
// inspection contracts (CFG-INSPECTRUN-001). Browser children freeze the real
// inspection_collection_v1 journey input; admission/dispatch wiring is added
// by the runtime slices.
package inspection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

var ErrNotFound = errors.New("inspection run source not found")
var ErrCommandReused = errors.New("client command id reused with a different request")

// RejectionError carries a deterministic, command-ledger-recorded rejection.
type RejectionError struct {
	Code, Detail, SystemKey string
	ObjectID                int64
}

func (e *RejectionError) Error() string { return e.Detail }

type Service struct {
	db  *sql.DB
	now func() time.Time
	// JourneyCore commits run_check browser results through the shared frozen
	// journey closure; wired by the app package.
	JourneyCore JourneyCore
}

func NewService(db *sql.DB) *Service { return &Service{db: db, now: time.Now} }
func (s *Service) DB() *sql.DB       { return s.db }
func (s *Service) nowText() string   { return s.now().UTC().Format(time.RFC3339Nano) }

type CheckDetail struct {
	CheckKey, Kind, Status, GapReason, AttemptState string
	EvidenceID, AttemptID                           *int64
}

type RunDetail struct {
	RunID           int64          `json:"runId"`
	SystemKey       string         `json:"systemKey"`
	PlanKey         string         `json:"planKey"`
	ConfigVersionID int64          `json:"configVersionId"`
	State           string         `json:"state"`
	RowVersion      int64          `json:"rowVersion"`
	EvidenceAt      *string        `json:"evidenceAt"`
	CreatedAt       string         `json:"createdAt"`
	Checks          []CheckDetail  `json:"checks"`
	Report          *ReportSummary `json:"report"`
}

type ReportSummary struct {
	Version   int64  `json:"version"`
	ModelID   string `json:"modelId"`
	Content   string `json:"content"`
	CreatedAt string `json:"createdAt"`
}

type planCheck struct {
	key, kind, mode, expression, journeyID, params string
	rangeSeconds, stepSeconds                      sql.NullInt64
}

// CreateInspectionRun starts one manual Run of a published plan. Every check
// becomes a run_check child in the same transaction: PromQL children freeze
// inspection_promql_execution_v1 plus their config_thanos_query grant, browser
// children freeze the real inspection_collection_v1 journey input and settle
// deterministically (identity_busy / authentication_required) or stay Queued
// for admission.
func (s *Service) CreateInspectionRun(ctx context.Context, principalID int64, clientCommandID, systemKey, planKey string) (RunDetail, error) {
	const command = "inspection_run.create"
	digest := auth.DigestCommand(command, map[string]any{"systemKey": systemKey, "planKey": planKey})
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
	var systemID, versionID, contractID, planID int64
	err = conn.QueryRowContext(ctx, `
		SELECT s.id, s.current_config_version_id, v.label_contract_version_id, p.id
		FROM business_systems s
		JOIN business_system_config_versions v ON v.id = s.current_config_version_id AND v.state='published'
		JOIN config_plans p ON p.config_version_id = v.id AND p.plan_key = ?
		WHERE s.key = ?`, planKey, systemKey).Scan(&systemID, &versionID, &contractID, &planID)
	if errors.Is(err, sql.ErrNoRows) {
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest, &RejectionError{Code: "not_found", Detail: "找不到已发布的巡检计划", SystemKey: systemKey}, &committed)
	}
	if err != nil {
		return RunDetail{}, err
	}
	checks, err := loadChecks(ctx, conn, planID)
	if err != nil {
		return RunDetail{}, err
	}
	if len(checks) == 0 {
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest, &RejectionError{Code: "empty_plan", Detail: "巡检计划没有检查项", SystemKey: systemKey}, &committed)
	}
	now := s.nowText()
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO inspection_runs(business_system_id,plan_key,config_version_id,label_contract_version_id,trigger_kind,state,created_at)
		VALUES(?,?,?,?,'manual','Queued',?)`, systemID, planKey, versionID, contractID, now)
	if err != nil {
		var active int64
		_ = conn.QueryRowContext(ctx, `SELECT id FROM inspection_runs WHERE business_system_id=? AND plan_key=? AND state IN ('Queued','Running')`, systemID, planKey).Scan(&active)
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest, &RejectionError{Code: "active_conflict", Detail: "该巡检计划已有进行中的 Run", SystemKey: systemKey, ObjectID: active}, &committed)
	}
	runID, err := insert.LastInsertId()
	if err != nil {
		return RunDetail{}, err
	}
	if _, err = conn.ExecContext(ctx, `UPDATE inspection_runs SET state='Running', evidence_at=?, row_version=row_version+1 WHERE id=? AND state='Queued'`, now, runID); err != nil {
		return RunDetail{}, err
	}
	for _, check := range checks {
		if check.kind == "promql" {
			err = s.promqlChild(ctx, conn, runID, versionID, contractID, check, now)
		} else {
			err = s.browserChild(ctx, conn, runID, versionID, contractID, planKey, systemID, check, now)
		}
		if err != nil {
			return RunDetail{}, err
		}
	}
	if err = s.convergeOn(ctx, conn, runID); err != nil {
		return RunDetail{}, err
	}
	detail, err := s.detailOn(ctx, conn, systemKey, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if err = s.audit(ctx, conn, principalID, clientCommandID, command, runID, now); err != nil {
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

func loadChecks(ctx context.Context, conn *sql.Conn, planID int64) ([]planCheck, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT check_key, kind, COALESCE(query_mode,''), COALESCE(expression,''), COALESCE(journey_id,''), COALESCE(journey_params_json,'{}'), range_seconds, step_seconds
		FROM config_checks WHERE plan_id=? ORDER BY check_key`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []planCheck
	for rows.Next() {
		var c planCheck
		if err = rows.Scan(&c.key, &c.kind, &c.mode, &c.expression, &c.journeyID, &c.params, &c.rangeSeconds, &c.stepSeconds); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// promqlChild freezes one run_check PromQL attempt: the deployment Thanos
// grant and the typed input carrying the run's evidence_at.
func (s *Service) promqlChild(ctx context.Context, conn *sql.Conn, runID, versionID, contractID int64, check planCheck, now string) error {
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,check_key,state,quoin_release_version,created_at)
		VALUES('inspection_collection','run_check',?,?,'Queued',?,?)`, runID, check.key, attempt.ReleaseVersion(), now)
	if err != nil {
		return err
	}
	attemptID, err := insert.LastInsertId()
	if err != nil {
		return err
	}
	grant, err := thanos.ResolveConfigGrant(ctx, conn, attemptID)
	if err != nil {
		return err
	}
	var rangeSeconds, stepSeconds *int64
	if check.rangeSeconds.Valid {
		rangeSeconds = &check.rangeSeconds.Int64
	}
	if check.stepSeconds.Valid {
		stepSeconds = &check.stepSeconds.Int64
	}
	var evidenceAt string
	if err = conn.QueryRowContext(ctx, `SELECT evidence_at FROM inspection_runs WHERE id=?`, runID).Scan(&evidenceAt); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"schemaKind": "inspection_promql_execution_v1", "attemptId": attemptID, "inspectionRunId": runID,
		"checkKey": check.key, "evidenceAt": evidenceAt, "grantId": grant.GrantID,
		"query": map[string]any{"mode": check.mode, "expression": check.expression, "rangeSeconds": rangeSeconds, "stepSeconds": stepSeconds},
	})
	if err != nil {
		return err
	}
	return freezeInput(ctx, conn, attemptID, "inspection_promql_execution_v1", body, versionID, contractID, now)
}

// browserChild freezes the real inspection_collection_v1 journey input and
// settles the deterministic local outcomes the frozen SQL admits:
// identity_busy closes as a Queued local gap (transport-successful), an
// identity without any published profile terminal-fails as an
// authentication_required gap, and a ready free identity leaves the child
// Queued for journey admission.
func (s *Service) browserChild(ctx context.Context, conn *sql.Conn, runID, versionID, contractID int64, planKey string, systemID int64, check planCheck, now string) error {
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,check_key,state,quoin_release_version,created_at)
		VALUES('inspection_collection','run_check',?,?,'Queued',?,?)`, runID, check.key, attempt.ReleaseVersion(), now)
	if err != nil {
		return err
	}
	attemptID, err := insert.LastInsertId()
	if err != nil {
		return err
	}
	identity, hasIdentity, err := loadBrowserIdentity(ctx, conn, systemID)
	if err != nil {
		return err
	}
	if !hasIdentity {
		// No browser identity at all: freeze the check/catalog binding and
		// settle terminally, exactly like a profile-less identity.
		catalogDigest, catalogVersion, _, err := resolveJourneyBinding(check.journeyID)
		if err != nil {
			return err
		}
		body, err := json.Marshal(map[string]any{
			"schemaKind": "inspection_collection_v1", "attemptId": attemptID, "operationId": nil,
			"identity": nil, "journey": nil, "authenticationProbe": nil,
			"catalog": map[string]any{"digest": catalogDigest, "version": catalogVersion},
			"planKey": planKey, "checkKey": check.key,
		})
		if err != nil {
			return err
		}
		if err = freezeInput(ctx, conn, attemptID, "inspection_collection_v1", body, versionID, contractID, now); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, `UPDATE execution_attempts SET state='Failed', ended_at=?, row_version=row_version+1 WHERE id=? AND state='Queued'`, now, attemptID); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `
			INSERT INTO inspection_check_results(run_id,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at)
			VALUES(?,?,'gap',NULL,?,NULL,'authentication_required',?)`, runID, check.key, attemptID, now)
		return err
	}
	catalogDigest, catalogVersion, journeyVersion, err := resolveJourneyBinding(check.journeyID)
	if err != nil {
		return err
	}
	probeParams := json.RawMessage(identity.ProbeParams)
	if len(probeParams) == 0 || string(probeParams) == "null" {
		probeParams = json.RawMessage("{}")
	}
	params := json.RawMessage(check.params)
	if len(params) == 0 || string(params) == "null" {
		params = json.RawMessage("{}")
	}
	binding := func(id string, version int64, raw json.RawMessage) map[string]any {
		return map[string]any{"id": id, "version": version, "params": raw,
			"catalog": map[string]any{"digest": catalogDigest, "version": catalogVersion}}
	}
	body, err := json.Marshal(map[string]any{
		"schemaKind": "inspection_collection_v1", "attemptId": attemptID, "operationId": nil,
		"identity": map[string]any{
			"identityId": identity.IdentityID, "identityRevisionId": identity.RevisionID,
			"profileGenerationId": identity.ProfileGenerationID, "profileGeneration": identity.Generation,
			"startUrl": identity.StartURL,
		},
		"journey":             binding(check.journeyID, journeyVersion, params),
		"authenticationProbe": binding(identity.ProbeJourneyID, identity.ProbeVersion, probeParams),
		"planKey":             planKey, "checkKey": check.key,
	})
	if err != nil {
		return err
	}
	ready := identity.ProfileGenerationID.Valid && identity.Generation.Valid
	if !ready {
		// No published profile can never authenticate; settle terminally so
		// the Run keeps a converging path (terminal browser arm of
		// trg_inspection_check_results_closure).
		if _, err = conn.ExecContext(ctx, `UPDATE execution_attempts SET state='Failed', ended_at=?, row_version=row_version+1 WHERE id=? AND state='Queued'`, now, attemptID); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `
			INSERT INTO inspection_check_results(run_id,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at)
			VALUES(?,?,'gap',NULL,?,NULL,'authentication_required',?)`, runID, check.key, attemptID, now)
		return err
	}
	var busy bool
	if err = conn.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM browser_operations WHERE identity_id=? AND (state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect') OR stop_confirmed_at IS NULL))`,
		identity.IdentityID).Scan(&busy); err != nil {
		return err
	}
	if busy {
		// A busy identity is a local identity_busy gap on the still-Queued
		// child; the local-journey commit trigger closes the attempt.
		inputDigest := sha256.Sum256(body)
		if _, err = conn.ExecContext(ctx, `
			INSERT INTO inspection_check_results(run_id,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at)
			VALUES(?,?,'gap',NULL,?,?, 'identity_busy',?)`, runID, check.key, attemptID, inputDigest[:], now); err != nil {
			return err
		}
	}
	// A ready, free identity remains a bare Queued child. Identity-serial
	// admission creates its browser operation and freezes the operation-bound
	// inspection_collection_v1 snapshot in one transaction; the immutable
	// dispatch input must carry the real operationId.
	return nil
}

func freezeInput(ctx context.Context, conn *sql.Conn, attemptID int64, kind string, body []byte, versionID, contractID int64, now string) error {
	digest := sha256.Sum256(body)
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at)
		VALUES(?,?, 'v1',?,?)`, attemptID, kind, hex.EncodeToString(digest[:]), now)
	if err != nil {
		return err
	}
	snapshotID, err := insert.LastInsertId()
	if err != nil {
		return err
	}
	versionDigest := sha256.Sum256([]byte(fmt.Sprintf("business-system-config-version:%d", versionID)))
	if _, err = conn.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,business_system_config_version_id)
		VALUES(?,1,'config_version',?,?)`, snapshotID, hex.EncodeToString(versionDigest[:]), versionID); err != nil {
		return err
	}
	contractDigest := sha256.Sum256([]byte(fmt.Sprintf("label-contract-version:%d", contractID)))
	_, err = conn.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,label_contract_version_id)
		VALUES(?,2,'label_contract',?,?)`, snapshotID, hex.EncodeToString(contractDigest[:]), contractID)
	return err
}

// convergeOn closes the Run once every configured plan check has settled:
// Completed requires all-ok coverage, CompletedWithGaps at least one explicit
// gap (trg_inspection_runs_result_set_complete re-validates both).
func (s *Service) convergeOn(ctx context.Context, conn *sql.Conn, runID int64) error {
	var pending, gaps int
	err := conn.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM config_checks c JOIN config_plans p ON p.id=c.plan_id
		   JOIN inspection_runs r ON r.config_version_id=p.config_version_id AND r.plan_key=p.plan_key
		   WHERE r.id=?) - (SELECT COUNT(*) FROM inspection_check_results x WHERE x.run_id=?),
		  (SELECT COUNT(*) FROM inspection_check_results x WHERE x.run_id=? AND x.status <> 'ok')
		FROM inspection_runs WHERE id=?`, runID, runID, runID, runID).Scan(&pending, &gaps)
	if err != nil {
		return err
	}
	if pending != 0 {
		return nil
	}
	state := "Completed"
	if gaps > 0 {
		state = "CompletedWithGaps"
	}
	if _, err = conn.ExecContext(ctx, `UPDATE inspection_runs SET state=?, row_version=row_version+1 WHERE id=? AND state='Running'`, state, runID); err != nil {
		return err
	}
	// A closed collection immediately owns its analysis attempt; the frozen
	// snapshot carries the preallocated Report version and the full locator
	// set (RUNTIME-TASK-013).
	return s.startReportAnalysisOn(ctx, conn, runID, s.nowText())
}

func (s *Service) replay(ctx context.Context, principalID int64, clientCommandID, digest string) (RunDetail, bool, error) {
	record, found, err := auth.LookupCommand(ctx, s.db, principalID, clientCommandID)
	if err != nil {
		return RunDetail{}, false, err
	}
	return decodeReplay(record, found, digest)
}

func (s *Service) replayOn(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, digest string) (RunDetail, bool, error) {
	record, found, err := auth.LookupCommandOn(ctx, conn, principalID, clientCommandID)
	if err != nil {
		return RunDetail{}, false, err
	}
	return decodeReplay(record, found, digest)
}

func decodeReplay(record auth.CommandRecord, found bool, digest string) (RunDetail, bool, error) {
	if !found {
		return RunDetail{}, false, nil
	}
	if record.RequestDigest != digest {
		return RunDetail{}, true, ErrCommandReused
	}
	if record.Outcome == auth.OutcomeRejectedKnown {
		var rejection RejectionError
		if err := json.Unmarshal([]byte(record.ResultPayload), &rejection); err != nil {
			return RunDetail{}, true, err
		}
		return RunDetail{}, true, &rejection
	}
	var detail RunDetail
	if err := json.Unmarshal([]byte(record.ResultPayload), &detail); err != nil {
		return RunDetail{}, true, err
	}
	return detail, true, nil
}

func (s *Service) reject(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, command, digest string, rejection *RejectionError, committed *bool) (RunDetail, error) {
	payload, _ := json.Marshal(rejection)
	if err := auth.RecordCommand(ctx, conn, principalID, clientCommandID, command, digest, auth.OutcomeRejectedKnown, "inspection_run", rejection.ObjectID, string(payload)); err != nil {
		return RunDetail{}, err
	}
	if err := s.auditRejected(ctx, conn, principalID, clientCommandID, rejection.ObjectID); err != nil {
		return RunDetail{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return RunDetail{}, err
	}
	*committed = true
	return RunDetail{}, rejection
}

func (s *Service) audit(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, command string, objectID int64, now string) error {
	_, err := conn.ExecContext(ctx, `
		INSERT INTO audit_events(actor_type,actor_id,action,client_command_id,outcome,domain_ref_type,domain_ref_id,created_at)
		VALUES('user',?,?,?,'success','inspection_run',?,?)`, principalID, command, clientCommandID, objectID, now)
	return err
}

func (s *Service) auditRejected(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID string, objectID int64) error {
	_, err := conn.ExecContext(ctx, `
		INSERT INTO audit_events(actor_type,actor_id,action,client_command_id,outcome,domain_ref_type,domain_ref_id,created_at)
		VALUES('user',?,'inspection_run.create',?,'rejected','inspection_run',?,?)`, principalID, clientCommandID, objectID, s.nowText())
	return err
}

func (s *Service) recordCommand(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, command, digest string, objectID int64, detail RunDetail) error {
	payload, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	return auth.RecordCommand(ctx, conn, principalID, clientCommandID, command, digest, auth.OutcomeCommitted, "inspection_run", objectID, string(payload))
}

func (s *Service) detailOn(ctx context.Context, conn *sql.Conn, systemKey string, runID int64) (RunDetail, error) {
	var detail RunDetail
	var evidenceAt sql.NullString
	err := conn.QueryRowContext(ctx, `
		SELECT r.id, r.plan_key, r.config_version_id, r.state, r.row_version, r.evidence_at, r.created_at
		FROM inspection_runs r JOIN business_systems s ON s.id=r.business_system_id
		WHERE s.key=? AND r.id=?`, systemKey, runID).
		Scan(&detail.RunID, &detail.PlanKey, &detail.ConfigVersionID, &detail.State, &detail.RowVersion, &evidenceAt, &detail.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RunDetail{}, ErrNotFound
	}
	if err != nil {
		return RunDetail{}, err
	}
	detail.SystemKey = systemKey
	if evidenceAt.Valid {
		detail.EvidenceAt = &evidenceAt.String
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT c.check_key, c.kind, COALESCE(x.status,''), COALESCE(x.gap_reason,''), COALESCE(a.state,''), x.evidence_id, x.attempt_id
		FROM config_checks c
		JOIN config_plans p ON p.id=c.plan_id
		JOIN inspection_runs r ON r.config_version_id=p.config_version_id AND r.plan_key=p.plan_key
		LEFT JOIN inspection_check_results x ON x.run_id=r.id AND x.check_key=c.check_key
		LEFT JOIN execution_attempts a ON a.id=x.attempt_id
		WHERE r.id=? ORDER BY c.check_key`, runID)
	if err != nil {
		return detail, err
	}
	defer rows.Close()
	for rows.Next() {
		var check CheckDetail
		var evidenceID, attemptID sql.NullInt64
		if err = rows.Scan(&check.CheckKey, &check.Kind, &check.Status, &check.GapReason, &check.AttemptState, &evidenceID, &attemptID); err != nil {
			return detail, err
		}
		if evidenceID.Valid {
			check.EvidenceID = &evidenceID.Int64
		}
		if attemptID.Valid {
			check.AttemptID = &attemptID.Int64
		}
		detail.Checks = append(detail.Checks, check)
	}
	if err = rows.Err(); err != nil {
		return detail, err
	}
	var report ReportSummary
	err = conn.QueryRowContext(ctx, `
		SELECT version, model_id, content, created_at FROM inspection_reports WHERE run_id=? ORDER BY version DESC LIMIT 1`, runID).
		Scan(&report.Version, &report.ModelID, &report.Content, &report.CreatedAt)
	if err == nil {
		detail.Report = &report
	} else if !errors.Is(err, sql.ErrNoRows) {
		return detail, err
	}
	return detail, nil
}

// GetRun returns one run detail bound to the system key.
func (s *Service) GetRun(ctx context.Context, systemKey string, runID int64) (RunDetail, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return RunDetail{}, err
	}
	defer conn.Close()
	return s.detailOn(ctx, conn, systemKey, runID)
}

type RunSummary struct {
	RunID                     int64 `json:"runId"`
	PlanKey, State, CreatedAt string
	RowVersion                int64 `json:"rowVersion"`
}

// ListRuns returns the system's runs newest first with a (created_at, id)
// keyset cursor (HTTP-PAGE-005 order).
func (s *Service) ListRuns(ctx context.Context, systemKey, cursor string, limit int) ([]RunSummary, bool, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	query := `SELECT r.id, r.plan_key, r.state, r.row_version, r.created_at
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
		if err = rows.Scan(&item.RunID, &item.PlanKey, &item.State, &item.RowVersion, &item.CreatedAt); err != nil {
			return nil, false, err
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
