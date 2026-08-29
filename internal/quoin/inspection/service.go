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
	JourneyCore    JourneyCore
	artifactWriter func(context.Context, *sql.Conn, int64, []byte) (int64, error)
}

// SetArtifactWriter injects the content-addressed Evidence materializer used
// to make frozen report inputs readable without exposing Quoin storage paths.
func (s *Service) SetArtifactWriter(writer func(context.Context, *sql.Conn, int64, []byte) (int64, error)) {
	s.artifactWriter = writer
}

func NewService(db *sql.DB) *Service { return &Service{db: db, now: time.Now} }
func (s *Service) DB() *sql.DB       { return s.db }
func (s *Service) nowText() string   { return s.now().UTC().Format(time.RFC3339Nano) }

// CheckResult is the frozen CheckResultSummary wire union: ok carries only
// the Evidence locator; error/gap carries only the reason.
type CheckResult struct {
	CheckKey   string  `json:"checkKey"`
	Status     string  `json:"status"`
	EvidenceID *string `json:"evidenceId,omitempty"`
	GapReason  *string `json:"gapReason,omitempty"`
}

type RunDetail struct {
	RunID             int64                    `json:"-"`
	ID                string                   `json:"id"`
	BusinessSystemKey string                   `json:"businessSystemKey"`
	PlanKey           string                   `json:"planKey"`
	State             string                   `json:"state"`
	RowVersion        int64                    `json:"rowVersion"`
	TriggerKind       string                   `json:"triggerKind"`
	ScheduledFor      *string                  `json:"scheduledFor,omitempty"`
	EvidenceAt        *string                  `json:"evidenceAt,omitempty"`
	CreatedAt         string                   `json:"createdAt"`
	Checks            []CheckResult            `json:"checks"`
	ReportCount       int                      `json:"reportCount"`
	AnalysisActive    bool                     `json:"analysisActive"`
	LatestAnalysis    *InspectionAttemptStatus `json:"latestAnalysis,omitempty"`
}

// InspectionAttemptStatus is the safe, read-only lifecycle projection for the
// most recent report analysis. It makes failures and cancellations recoverable
// in the Run UI without exposing model input or report content.
type InspectionAttemptStatus struct {
	ID                string  `json:"id"`
	State             string  `json:"state"`
	TerminationReason *string `json:"terminationReason,omitempty"`
}

type ReportSummaryItem struct {
	Version   int64  `json:"version"`
	ModelID   string `json:"modelId"`
	CreatedAt string `json:"createdAt"`
}

type ReportDetail struct {
	RunID          string   `json:"runId"`
	Version        int64    `json:"version"`
	EvidenceDigest string   `json:"evidenceDigest"`
	EvidenceIDs    []string `json:"evidenceIds"`
	ModelID        string   `json:"modelId"`
	Content        string   `json:"content"`
	CreatedAt      string   `json:"createdAt"`
}

func locatorID(id int64) string { return strconv.FormatInt(id, 10) }

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
			// A plan carrying PromQL checks cannot claim execution without an
			// authorized Thanos path. The whole creation rolls back (no orphan
			// run or children) and the typed error surfaces as a retryable
			// 503, mirroring Config Verification.
			if errors.Is(err, thanos.ErrThanosUnavailable) || errors.Is(err, thanos.ErrGrantNotCurrent) {
				return RunDetail{}, fmt.Errorf("%w: %s", err, "尚无可用的 Thanos 指标连接，请先创建并启用连接后重试")
			}
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
	if id, err := strconv.ParseInt(detail.ID, 10, 64); err == nil {
		detail.RunID = id
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

type RunSummary struct {
	ID                string  `json:"id"`
	BusinessSystemKey string  `json:"businessSystemKey"`
	PlanKey           string  `json:"planKey"`
	State             string  `json:"state"`
	RowVersion        int64   `json:"rowVersion"`
	TriggerKind       string  `json:"triggerKind"`
	ScheduledFor      *string `json:"scheduledFor,omitempty"`
	EvidenceAt        *string `json:"evidenceAt,omitempty"`
	CreatedAt         string  `json:"createdAt"`
}

// ScheduledPlan is a current, already-validated typed plan projection. Cron
// remains parsed by the scheduler, but the scheduler never reparses YAML.
type ScheduledPlan struct {
	SystemKey       string
	PlanKey         string
	Cron            string
	Timezone        string
	ConfigVersionID int64
}

// RuntimeAvailability is sampled by the scheduling runtime at the boundary.
// A false slot produces a durable runtime_unavailable check gap rather than a
// queued execution that would silently run later.
type RuntimeAvailability struct {
	Plinth bool
	Lintel bool
}

// ScheduledPlans lists only plans that are active through the current
// published configuration pointer. It deliberately has no scheduler cursor:
// immutable inspection_runs rows are the duplicate authority.
func (s *Service) ScheduledPlans(ctx context.Context) ([]ScheduledPlan, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.key,p.plan_key,p.cron,v.timezone,v.id
		FROM business_systems b
		JOIN business_system_config_versions v ON v.id=b.current_config_version_id AND v.state='published'
		JOIN config_plans p ON p.config_version_id=v.id
		WHERE b.enabled=1 AND p.cron IS NOT NULL
		  AND EXISTS (SELECT 1 FROM config_checks c WHERE c.plan_id=p.id)
		ORDER BY b.id,p.plan_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plans := []ScheduledPlan{}
	for rows.Next() {
		var plan ScheduledPlan
		if err := rows.Scan(&plan.SystemKey, &plan.PlanKey, &plan.Cron, &plan.Timezone, &plan.ConfigVersionID); err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

// CreateScheduledInspectionRun commits one due UTC occurrence. The scheduler
// supplies the immutable config version it observed; the transaction refuses
// to create an old-plan occurrence if publication changed before commit.
func (s *Service) CreateScheduledInspectionRun(ctx context.Context, plan ScheduledPlan, scheduledFor time.Time, availability RuntimeAvailability) (RunDetail, error) {
	scheduledFor = scheduledFor.UTC()
	if scheduledFor.Nanosecond() != 0 || scheduledFor.Second() != 0 {
		return RunDetail{}, fmt.Errorf("scheduled_for must be a minute boundary")
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

	scheduledText := scheduledFor.Format(time.RFC3339Nano)
	var existingID int64
	err = conn.QueryRowContext(ctx, `
		SELECT r.id FROM inspection_runs r
		JOIN business_systems b ON b.id=r.business_system_id
		WHERE b.key=? AND r.plan_key=? AND r.scheduled_for=?`, plan.SystemKey, plan.PlanKey, scheduledText).Scan(&existingID)
	if err == nil {
		detail, detailErr := s.detailOn(ctx, conn, plan.SystemKey, existingID)
		if detailErr != nil {
			return RunDetail{}, detailErr
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return RunDetail{}, err
		}
		committed = true
		return detail, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RunDetail{}, err
	}

	var systemID, versionID, contractID, planID int64
	err = conn.QueryRowContext(ctx, `
		SELECT b.id,v.id,v.label_contract_version_id,p.id
		FROM business_systems b
		JOIN business_system_config_versions v ON v.id=b.current_config_version_id AND v.state='published'
		JOIN config_plans p ON p.config_version_id=v.id AND p.plan_key=? AND p.cron IS NOT NULL
		WHERE b.key=? AND b.enabled=1 AND v.id=?`, plan.PlanKey, plan.SystemKey, plan.ConfigVersionID).
		Scan(&systemID, &versionID, &contractID, &planID)
	if errors.Is(err, sql.ErrNoRows) {
		// A concurrent publish/disable made the earlier read stale. This is not
		// an error and must not turn the previous configuration into a Run.
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return RunDetail{}, err
		}
		committed = true
		return RunDetail{}, nil
	}
	if err != nil {
		return RunDetail{}, err
	}
	checks, err := loadChecks(ctx, conn, planID)
	if err != nil {
		return RunDetail{}, err
	}
	if len(checks) == 0 {
		return RunDetail{}, fmt.Errorf("scheduled plan %s/%s has no checks", plan.SystemKey, plan.PlanKey)
	}

	now := s.nowText()
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO inspection_runs(business_system_id,plan_key,config_version_id,label_contract_version_id,trigger_kind,scheduled_for,state,created_at)
		VALUES(?,?,?,?, 'schedule',?,'Queued',?)`, systemID, plan.PlanKey, versionID, contractID, scheduledText, now)
	if err != nil {
		// The active unique index is the commit-order overlap decision. Only an
		// actual active Run converts this boundary into SkippedOverlap; never
		// disguise an unrelated database failure as a scheduling decision.
		var activeID int64
		activeErr := conn.QueryRowContext(ctx, `
			SELECT id FROM inspection_runs
			WHERE business_system_id=? AND plan_key=? AND state IN ('Queued','Running')`, systemID, plan.PlanKey).
			Scan(&activeID)
		if errors.Is(activeErr, sql.ErrNoRows) {
			return RunDetail{}, err
		}
		if activeErr != nil {
			return RunDetail{}, activeErr
		}
		if _, err = conn.ExecContext(ctx, `
			INSERT INTO inspection_runs(business_system_id,plan_key,config_version_id,label_contract_version_id,trigger_kind,scheduled_for,state,created_at)
			VALUES(?,?,?,?, 'schedule',?,'SkippedOverlap',?)`, systemID, plan.PlanKey, versionID, contractID, scheduledText, now); err != nil {
			return RunDetail{}, err
		}
		if err = conn.QueryRowContext(ctx, `SELECT id FROM inspection_runs WHERE business_system_id=? AND plan_key=? AND scheduled_for=?`, systemID, plan.PlanKey, scheduledText).Scan(&existingID); err != nil {
			return RunDetail{}, err
		}
		detail, err := s.detailOn(ctx, conn, plan.SystemKey, existingID)
		if err != nil {
			return RunDetail{}, err
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return RunDetail{}, err
		}
		committed = true
		return detail, nil
	}
	runID, err := insert.LastInsertId()
	if err != nil {
		return RunDetail{}, err
	}
	if _, err = conn.ExecContext(ctx, `UPDATE inspection_runs SET state='Running',evidence_at=?,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, runID); err != nil {
		return RunDetail{}, err
	}
	for _, check := range checks {
		if (check.kind == "promql" && !availability.Plinth) || (check.kind == "browser" && !availability.Lintel) {
			if err = s.runtimeUnavailableChild(ctx, conn, runID, check.key, now); err != nil {
				return RunDetail{}, err
			}
			continue
		}
		if check.kind == "promql" {
			err = s.promqlChild(ctx, conn, runID, versionID, contractID, check, now)
		} else {
			err = s.browserChild(ctx, conn, runID, versionID, contractID, plan.PlanKey, systemID, check, now)
		}
		if err != nil {
			// A missing or stale Thanos grant is a configuration failure, not a
			// Runtime-slot outage. Roll this occurrence back rather than forging a
			// boundary-time runtime_unavailable gap.
			return RunDetail{}, err
		}
	}
	if err = s.convergeOn(ctx, conn, runID); err != nil {
		return RunDetail{}, err
	}
	detail, err := s.detailOn(ctx, conn, plan.SystemKey, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return RunDetail{}, err
	}
	committed = true
	return detail, nil
}

// runtimeUnavailableChild records a boundary-time Runtime outage as a terminal
// technical child. The frozen result trigger requires every gap to identify an
// exact Attempt, even when no dispatch could be attempted.
func (s *Service) runtimeUnavailableChild(ctx context.Context, conn *sql.Conn, runID int64, checkKey, now string) error {
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,check_key,state,quoin_release_version,created_at)
		VALUES('inspection_collection','run_check',?,?,'Queued',?,?)`, runID, checkKey, attempt.ReleaseVersion(), now)
	if err != nil {
		return err
	}
	attemptID, err := insert.LastInsertId()
	if err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `UPDATE execution_attempts SET state='Failed',ended_at=?,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, attemptID); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `
		INSERT INTO inspection_check_results(run_id,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at)
		VALUES(?,?,'gap',NULL,?,NULL,'runtime_unavailable',?)`, runID, checkKey, attemptID, now)
	return err
}
