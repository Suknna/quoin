// Config Verification Run lifecycle (T17, DATA-CONFIG-007): the prepublish
// run is the only mechanical execution model whose Passed state can back a
// Label Contract activation. T17 owns the lifecycle machine — a bound draft
// with zero checks completes deterministically inside the create command
// (nothing external to wait for), while a draft carrying checks stays Queued
// until the PromQL/browser executors arrive with their own tickets. Every
// state change lands in task_change_log in the same transaction.

package businesssystem

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// VerificationRunSummary is the ConfigVerificationRunSummary projection.
type VerificationRunSummary struct {
	ID                     string  `json:"id"`
	Purpose                string  `json:"purpose"`
	ConfigVersionID        string  `json:"configVersionId"`
	LabelContractVersionID string  `json:"labelContractVersionId"`
	State                  string  `json:"state"`
	RowVersion             int64   `json:"rowVersion"`
	EvidenceAt             *string `json:"evidenceAt,omitempty"`
	CreatedAt              string  `json:"createdAt"`
}

// VerificationCheckResult is one append-only per-check outcome row. GapDetail
// carries the journey ledger's bounded human diagnosis when the check closed
// through browser_journey_results (DATA-BROWSER-011: the machine code stays
// the authority; this text is the upper-layer diagnostic).
type VerificationCheckResult struct {
	PlanKey    string  `json:"planKey"`
	CheckKey   string  `json:"checkKey"`
	Status     string  `json:"status"`
	EvidenceID *string `json:"evidenceId,omitempty"`
	GapReason  *string `json:"gapReason,omitempty"`
	GapDetail  *string `json:"gapDetail,omitempty"`
}

// VerificationRunDetail is the ConfigVerificationRunDetail projection; the
// terminal states carry resultDetail (schema CHECK).
type VerificationQueryResult struct {
	PlanKey     string `json:"planKey"`
	CheckKey    string `json:"checkKey"`
	ResultType  string `json:"resultType"`
	SampleCount int    `json:"sampleCount"`
	// Samples is a bounded, non-secret preview of the returned vector/matrix
	// payload. The full result stays in Evidence and is not a formal resource.
	Samples json.RawMessage `json:"samples"`
}

type VerificationIdentitySample struct {
	DiscoveryKey string            `json:"discoveryKey"`
	Labels       map[string]string `json:"labels"`
}

type VerificationRunDetail struct {
	VerificationRunSummary
	MetricsConnectionID  string                       `json:"metricsConnectionId"`
	CheckResults         []VerificationCheckResult    `json:"checkResults"`
	QueryResults         []VerificationQueryResult    `json:"queryResults"`
	IdentitySamples      []VerificationIdentitySample `json:"identitySamples"`
	ResultDetail         *string                      `json:"resultDetail,omitempty"`
	CancellingAttemptIDs []int64                      `json:"-"`
}

// RunVerification creates the prepublish run for one unpublished draft and
// executes it as far as this build's deterministic executor reaches: zero
// check configs run to Passed inside the same transaction, PromQL checks
// dispatch through the Plinth supervisor, and drafts carrying browser checks
// are deterministically rejected until the Lintel executor lands
// (CFG-VERIFYRUN-001/002).
// RunVerification runs through the shared execution runner as a durable,
// replayable admin ledger command (ADR-0006): the runner owns the transaction,
// the replay check, the command ledger row and the automatic audit row. A
// deterministic rejection rolls the business stage back to a savepoint before
// the rejection is recorded; the surfaced *execution.Rejection is mapped back
// to this family's stable domain errors.
func (service *Service) RunVerification(ctx context.Context, principalID int64, clientCommandID, systemKey string, versionID int64) (VerificationRunDetail, error) {
	command := execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          commandDigestOf(opVerificationRunName, map[string]any{"systemKey": systemKey, "versionId": versionID}),
	}
	outcome, err := execution.Run(ctx, service.runner, service.opVerificationRun, command,
		func(tx *execution.Tx) (VerificationRunDetail, execution.Change, error) {
			detail, runErr := service.runVerificationOn(ctx, tx, principalID, systemKey, versionID)
			if runErr != nil {
				return VerificationRunDetail{}, execution.Unchanged, runErr
			}
			return detail, execution.Changed, nil
		},
		func(detail VerificationRunDetail) int64 { return detail.numericID() })
	if err != nil {
		return VerificationRunDetail{}, service.verificationOutcomeError(err, systemKey)
	}
	return outcome.Result, nil
}

// numericID parses the detail's string id for the audit domain reference.
func (detail VerificationRunDetail) numericID() int64 {
	id, _ := strconv.ParseInt(detail.ID, 10, 64)
	return id
}

// verificationOutcomeError maps the shared runner's outcomes back to this
// family's stable exported errors: recorded rejections replay as their
// original domain error (legacy ledger payloads carry the full typed form),
// and ledger key conflicts keep the historic ErrCommandReused.
func (service *Service) verificationOutcomeError(err error, systemKey string) error {
	if errors.Is(err, execution.ErrCommandReused) {
		return ErrCommandReused
	}
	var rejection *execution.Rejection
	if errors.As(err, &rejection) {
		var legacy verificationRejection
		if rejection.Raw != "" {
			if decodeErr := decodeStored(rejection.Raw, &legacy); decodeErr == nil && legacy.Code != "" {
				if legacy.SystemKey == "" {
					legacy.SystemKey = systemKey
				}
				return legacy.asError()
			}
		}
		if rejection.Code == "not_found" {
			return ErrNotFound
		}
		return &ConflictError{Code: rejection.Code, Detail: rejection.Detail, SystemKey: systemKey, ObjectID: rejection.ObjectID}
	}
	return err
}

// runVerificationOn is RunVerification's business stage on the runner-owned
// transaction. Deterministic denials return *execution.Rejection so the
// runner records them in the clean transaction.
func (service *Service) runVerificationOn(ctx context.Context, conn execution.Executor, principalID int64, systemKey string, versionID int64) (VerificationRunDetail, error) {
	systemID, contractID, err := draftBinding(ctx, conn, systemKey, versionID)
	if err != nil {
		if rejection, known := verificationRejectionFor(err, systemKey, versionID); known {
			return VerificationRunDetail{}, rejection.asRejection()
		}
		return VerificationRunDetail{}, err
	}
	now := service.nowText()
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO config_verification_runs(purpose,business_system_id,config_version_id,label_contract_version_id,state,row_version,created_by,created_at)
		VALUES('prepublish',?,?,NULL,'Queued',1,?,?)`,
		systemID, versionID, principalID, now)
	if err != nil {
		if strings.Contains(err.Error(), "ux_config_verification_run_active") || strings.Contains(err.Error(), "UNIQUE constraint failed: config_verification_runs") {
			// Surface the active run itself so the client can open it
			// (active_conflict, HTTP-ERROR-004), and retain this deterministic
			// rejection in the command ledger before returning it.
			var activeID int64
			_ = conn.QueryRowContext(ctx, `SELECT id FROM config_verification_runs WHERE business_system_id=? AND config_version_id=? AND state IN ('Queued','Running')`, systemID, versionID).Scan(&activeID)
			rejection := verificationRejection{
				Code:      "active_conflict",
				Detail:    "该配置版本已有进行中的验证 Run，请先查看或取消它",
				SystemKey: systemKey,
				ObjectID:  activeID,
			}
			return VerificationRunDetail{}, rejection.asRejection()
		}
		return VerificationRunDetail{}, mapVerificationAbort(err)
	}
	runID, err := insert.LastInsertId()
	if err != nil {
		return VerificationRunDetail{}, err
	}
	// task_change_log rows are derived by the frozen schema triggers
	// (trg_task_change_log_config_verification_run_{insert,state}) inside the
	// same statements.
	checkCount := 0
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM config_checks c JOIN config_plans p ON p.id=c.plan_id WHERE p.config_version_id=?`, versionID).Scan(&checkCount); err != nil {
		return VerificationRunDetail{}, err
	}
	var promQLCount, discoveryCount int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM config_checks c JOIN config_plans p ON p.id=c.plan_id
		WHERE p.config_version_id=? AND c.kind='promql'`, versionID).Scan(&promQLCount); err != nil {
		return VerificationRunDetail{}, err
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM config_discoveries WHERE config_version_id=?`, versionID).Scan(&discoveryCount); err != nil {
		return VerificationRunDetail{}, err
	}
	if promQLCount > 0 || discoveryCount > 0 {
		// The scope trigger only permits child work beneath an active Run.
		// This parent transition and every child/grant insert still share the
		// creation transaction, so an unavailable connection rolls all of it back.
		if _, err := conn.ExecContext(ctx, `UPDATE config_verification_runs SET state='Running', evidence_at=?, row_version=2 WHERE id=? AND state='Queued'`, now, runID); err != nil {
			return VerificationRunDetail{}, mapVerificationAbort(err)
		}
		if promQLCount > 0 {
			if _, err := createPromQLVerificationAttempts(ctx, conn, runID, versionID, contractID, now); err != nil {
				return VerificationRunDetail{}, err
			}
		}
		if err := createDiscoveryVerificationAttempts(ctx, conn, runID, versionID, contractID, now); err != nil {
			return VerificationRunDetail{}, err
		}
	} else if checkCount == 0 {
		// The deterministic completion path: with no check to execute, the
		// run traverses Running (evidence_at sealed) to Passed; the frozen
		// Passed trigger accepts a fully-covered (empty) check set.
		if _, err := conn.ExecContext(ctx, `UPDATE config_verification_runs SET state='Running', evidence_at=?, row_version=2 WHERE id=? AND state='Queued'`, now, runID); err != nil {
			return VerificationRunDetail{}, mapVerificationAbort(err)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE config_verification_runs SET state='Passed', row_version=3 WHERE id=? AND state='Running'`, runID); err != nil {
			return VerificationRunDetail{}, mapVerificationAbort(err)
		}
	}
	return verificationDetailOn(ctx, conn, systemID, versionID, runID)
}

// CancelVerification applies the one-shot cancel fence: the command must
// carry the current rowVersion and only a non-terminal run can be cancelled
// (DATA-CONFIG-007). T17 has no dispatched child attempts yet, so the frozen
// child-fence trigger is trivially satisfied.
func (service *Service) CancelVerification(ctx context.Context, principalID int64, clientCommandID, systemKey string, versionID, runID, expectedRowVersion int64) (VerificationRunDetail, error) {
	command := execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest: commandDigestOf(opVerificationCancelName, map[string]any{
			"systemKey": systemKey, "versionId": versionID, "runId": runID, "expectedRowVersion": expectedRowVersion,
		}),
	}
	outcome, err := execution.Run(ctx, service.runner, service.opVerificationCancel, command,
		func(tx *execution.Tx) (VerificationRunDetail, execution.Change, error) {
			detail, cancelErr := service.cancelVerificationOn(ctx, tx, principalID, systemKey, versionID, runID, expectedRowVersion)
			if cancelErr != nil {
				return VerificationRunDetail{}, execution.Unchanged, cancelErr
			}
			return detail, execution.Changed, nil
		},
		func(detail VerificationRunDetail) int64 { return detail.numericID() })
	if err != nil {
		return VerificationRunDetail{}, service.verificationOutcomeError(err, systemKey)
	}
	return outcome.Result, nil
}

// cancelVerificationOn is CancelVerification's business stage on the
// runner-owned transaction. Deterministic denials return *execution.Rejection
// so the runner records them in the clean transaction.
func (service *Service) cancelVerificationOn(ctx context.Context, conn execution.Executor, principalID int64, systemKey string, versionID, runID, expectedRowVersion int64) (VerificationRunDetail, error) {
	systemID, _, err := ownedVersion(ctx, conn, systemKey, versionID)
	if err != nil {
		if rejection, known := verificationRejectionFor(err, systemKey, versionID); known {
			return VerificationRunDetail{}, rejection.asRejection()
		}
		return VerificationRunDetail{}, err
	}
	if _, err := verificationRow(ctx, conn, systemID, versionID, runID); err != nil {
		if rejection, known := verificationRejectionFor(err, systemKey, runID); known {
			return VerificationRunDetail{}, rejection.asRejection()
		}
		return VerificationRunDetail{}, err
	}
	// Cancellation is fenced at the parent, but child Attempts remain the
	// attempt authority. Queued children close atomically here; assigned or
	// running children move to Cancelling and keep the parent non-terminal
	// until the supervisor's CancelAck arrives.
	rows, err := conn.QueryContext(ctx, `SELECT id,state FROM execution_attempts WHERE scope_type='config_verification_run' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling')`, runID)
	if err != nil {
		return VerificationRunDetail{}, err
	}
	var childIDs, dispatchIDs []int64
	for rows.Next() {
		var childID int64
		var childState string
		if err := rows.Scan(&childID, &childState); err != nil {
			rows.Close()
			return VerificationRunDetail{}, err
		}
		childIDs = append(childIDs, childID)
		if childState == "Assigned" || childState == "Running" {
			dispatchIDs = append(dispatchIDs, childID)
		}
	}
	if err := rows.Close(); err != nil {
		return VerificationRunDetail{}, err
	}
	attemptService := attempt.NewService(service.db)
	for _, childID := range childIDs {
		if _, err := attemptService.CancelFenceOn(ctx, conn, childID); err != nil {
			return VerificationRunDetail{}, err
		}
	}
	update, err := conn.ExecContext(ctx, `
		UPDATE config_verification_runs SET state='Cancelled', result_detail='已由管理员取消', row_version=row_version+1
		WHERE id=? AND business_system_id=? AND config_version_id=? AND row_version=? AND state IN ('Queued','Running')`,
		runID, systemID, versionID, expectedRowVersion)
	if err != nil {
		mapped := mapVerificationAbort(err)
		if rejection, known := verificationRejectionFor(mapped, systemKey, runID); known {
			return VerificationRunDetail{}, rejection.asRejection()
		}
		return VerificationRunDetail{}, mapped
	}
	affected, _ := update.RowsAffected()
	if affected == 0 {
		return VerificationRunDetail{}, (&verificationRejection{
			Code:      "row_version_conflict",
			Detail:    "验证 Run 已变化或已进入终态，请刷新后重试",
			SystemKey: systemKey,
			ObjectID:  runID,
		}).asRejection()
	}
	detail, err := verificationDetailOn(ctx, conn, systemID, versionID, runID)
	if err != nil {
		return VerificationRunDetail{}, err
	}
	detail.CancellingAttemptIDs = dispatchIDs
	return detail, nil
}

// --- internal plumbing ------------------------------------------------------

// commandDigestOf wraps auth.DigestCommand for the verification commands.
func commandDigestOf(commandType string, fields map[string]any) string {
	return auth.DigestCommand(commandType, fields)
}

// verificationRejection is the non-secret durable form of a deterministic
// validation/fence rejection. Replaying it must rebuild the original domain
// error rather than executing a command after its precondition has changed.
type verificationRejection struct {
	Code       string `json:"code"`
	Detail     string `json:"detail"`
	SystemKey  string `json:"systemKey"`
	ObjectID   int64  `json:"objectId"`
	ObjectType string `json:"objectType"`
}

// asRejection converts the typed rejection into the shared runner's durable
// rejection so the runner records it in the clean transaction.
func (rejection verificationRejection) asRejection() *execution.Rejection {
	return &execution.Rejection{Code: rejection.Code, Detail: rejection.Detail, ObjectID: rejection.ObjectID}
}

func (rejection verificationRejection) asError() error {
	if rejection.Code == "not_found" {
		return ErrNotFound
	}
	return &ConflictError{
		Code:      rejection.Code,
		Detail:    rejection.Detail,
		SystemKey: rejection.SystemKey,
		ObjectID:  rejection.ObjectID,
	}
}

func verificationRejectionFor(err error, systemKey string, objectID int64) (verificationRejection, bool) {
	if errors.Is(err, ErrNotFound) {
		return verificationRejection{
			Code: "not_found", Detail: "找不到指定的业务系统、配置版本或验证 Run",
			SystemKey: systemKey, ObjectID: objectID, ObjectType: "config_verification_run",
		}, true
	}
	var conflict *ConflictError
	if errors.As(err, &conflict) {
		return verificationRejection{
			Code: conflict.Code, Detail: conflict.Detail, SystemKey: conflict.SystemKey,
			ObjectID: conflict.ObjectID, ObjectType: "config_verification_run",
		}, true
	}
	return verificationRejection{}, false
}

// draftBinding resolves (business_system_id, label_contract_version_id) for a
// version that must be an unpublished draft of the named system; the frozen
// INSERT closure trigger re-verifies the same facts inside the transaction.
func draftBinding(ctx context.Context, conn execution.Executor, systemKey string, versionID int64) (int64, int64, error) {
	var systemID int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, systemKey).Scan(&systemID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, ErrNotFound
		}
		return 0, 0, err
	}
	var (
		state     string
		published sql.NullString
	)
	err := conn.QueryRowContext(ctx, `
		SELECT state,published_at FROM business_system_config_versions
		WHERE id=? AND business_system_id=?`, versionID, systemID).Scan(&state, &published)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	if err != nil {
		return 0, 0, err
	}
	if state != "draft" || published.Valid {
		return 0, 0, &ConflictError{
			Code:      "row_version_conflict",
			Detail:    "该配置版本已发布或不再是最新的未发布草稿，Config Verification Run 只能绑定未发布草稿",
			SystemKey: systemKey, ObjectID: versionID,
		}
	}
	return systemID, 0, nil
}

// ownedVersion resolves the system row for a version that belongs to it,
// without requiring the draft state (reads and cancels apply to any run of
// the bound version).
func ownedVersion(ctx context.Context, conn audit.Reader, systemKey string, versionID int64) (int64, int64, error) {
	var systemID int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, systemKey).Scan(&systemID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, ErrNotFound
		}
		return 0, 0, err
	}
	var exists int
	err := conn.QueryRowContext(ctx, `SELECT 1 FROM business_system_config_versions WHERE id=? AND business_system_id=?`, versionID, systemID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	if err != nil {
		return 0, 0, err
	}
	return systemID, 0, nil
}

func verificationRow(ctx context.Context, conn audit.Reader, systemID, versionID, runID int64) (int64, error) {
	var id int64
	err := conn.QueryRowContext(ctx, `SELECT id FROM config_verification_runs WHERE id=? AND business_system_id=? AND config_version_id=?`, runID, systemID, versionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

func verificationDetailOn(ctx context.Context, conn audit.Reader, systemID, versionID, runID int64) (VerificationRunDetail, error) {
	var (
		detail       VerificationRunDetail
		id           int64
		contractID   sql.NullInt64
		evidenceAt   sql.NullString
		resultDetail sql.NullString
	)
	err := conn.QueryRowContext(ctx, `
		SELECT r.id,r.purpose,r.config_version_id,r.label_contract_version_id,r.state,r.row_version,r.evidence_at,r.created_at,r.result_detail,v.metrics_connection_id
		FROM config_verification_runs r JOIN business_system_config_versions v ON v.id=r.config_version_id
		WHERE r.id=? AND r.business_system_id=? AND r.config_version_id=?`,
		runID, systemID, versionID).Scan(&id, &detail.Purpose, &versionID, &contractID, &detail.State, &detail.RowVersion, &evidenceAt, &detail.CreatedAt, &resultDetail, &detail.MetricsConnectionID)
	if errors.Is(err, sql.ErrNoRows) {
		return VerificationRunDetail{}, ErrNotFound
	}
	if err != nil {
		return VerificationRunDetail{}, err
	}
	detail.ID = strconv.FormatInt(id, 10)
	detail.ConfigVersionID = strconv.FormatInt(versionID, 10)
	if contractID.Valid {
		detail.LabelContractVersionID = strconv.FormatInt(contractID.Int64, 10)
	}
	if evidenceAt.Valid {
		value := evidenceAt.String
		detail.EvidenceAt = &value
	}
	if resultDetail.Valid {
		value := resultDetail.String
		detail.ResultDetail = &value
	}
	detail.CheckResults = []VerificationCheckResult{}
	rows, err := conn.QueryContext(ctx, `
		SELECT r.plan_key,r.check_key,r.status,r.evidence_id,r.gap_reason,NULL
		FROM config_verification_run_check_results r
		WHERE r.verification_run_id=? ORDER BY r.plan_key,r.check_key`, runID)
	if err != nil {
		return VerificationRunDetail{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var result VerificationCheckResult
		var evidenceID sql.NullInt64
		var gapReason, gapDetail sql.NullString
		if err := rows.Scan(&result.PlanKey, &result.CheckKey, &result.Status, &evidenceID, &gapReason, &gapDetail); err != nil {
			return VerificationRunDetail{}, err
		}
		if evidenceID.Valid {
			value := strconv.FormatInt(evidenceID.Int64, 10)
			result.EvidenceID = &value
		}
		if gapReason.Valid {
			value := gapReason.String
			result.GapReason = &value
		}
		if gapDetail.Valid {
			value := gapDetail.String
			result.GapDetail = &value
		}
		detail.CheckResults = append(detail.CheckResults, result)
	}
	if err := rows.Err(); err != nil {
		return VerificationRunDetail{}, err
	}
	queryRows, err := conn.QueryContext(ctx, `
			SELECT r.plan_key,r.check_key,e.result_json
			FROM config_verification_run_check_results r
			JOIN evidence e ON e.id=r.evidence_id
			WHERE r.verification_run_id=? ORDER BY r.plan_key,r.check_key`, runID)
	if err != nil {
		return VerificationRunDetail{}, err
	}
	defer queryRows.Close()
	for queryRows.Next() {
		var result VerificationQueryResult
		var raw string
		if err := queryRows.Scan(&result.PlanKey, &result.CheckKey, &raw); err != nil {
			return VerificationRunDetail{}, err
		}
		result.ResultType, result.SampleCount, result.Samples = verificationResultPreview([]byte(raw))
		detail.QueryResults = append(detail.QueryResults, result)
	}
	if err := queryRows.Err(); err != nil {
		return VerificationRunDetail{}, err
	}
	// Identity samples are derived only from real returned PromQL samples
	// captured as verification Evidence. They never create or update formal
	// observed resources; a declaration that produced no matching sample yields
	// no identity sample rather than a fabricated empty label map.
	identitySamples, err := verificationDiscoverySamples(ctx, conn, runID)
	if err != nil {
		return VerificationRunDetail{}, err
	}
	detail.IdentitySamples = identitySamples
	return detail, nil
}

func verificationDiscoverySamples(ctx context.Context, conn audit.Reader, runID int64) ([]VerificationIdentitySample, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT r.discovery_key,e.result_json
		FROM config_verification_discovery_results r JOIN evidence e ON e.id=r.evidence_id
		WHERE r.verification_run_id=? AND r.status='ok' ORDER BY r.discovery_key`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var samples []VerificationIdentitySample
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		var result struct {
			Series []struct {
				Labels map[string]string `json:"labels"`
			} `json:"series"`
		}
		if json.Unmarshal([]byte(raw), &result) != nil {
			continue
		}
		for _, series := range result.Series {
			samples = append(samples, VerificationIdentitySample{DiscoveryKey: key, Labels: series.Labels})
			if len(samples) == 20 {
				return samples, nil
			}
		}
	}
	return samples, rows.Err()
}

func verificationResultPreview(raw []byte) (string, int, json.RawMessage) {
	var envelope struct {
		Data struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
		ResultType string            `json:"resultType"`
		Result     []json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return "unknown", 0, json.RawMessage("[]")
	}
	kind, values := envelope.Data.ResultType, envelope.Data.Result
	if kind == "" {
		kind, values = envelope.ResultType, envelope.Result
	}
	count := len(values)
	if count > 20 {
		values = values[:20]
	}
	preview, err := json.Marshal(values)
	if err != nil {
		preview = []byte("[]")
	}
	return kind, count, preview
}

// mapVerificationAbort converts the frozen verification triggers' RAISE
// messages into typed errors.
func mapVerificationAbort(err error) error {
	message := err.Error()
	switch {
	case strings.Contains(message, "config_verification_run purpose must bind the corresponding draft"):
		return &ConflictError{Code: "row_version_conflict", Detail: "验证 Run 只能绑定未发布草稿与目标契约"}
	case strings.Contains(message, "illegal config_verification_run state transition"),
		strings.Contains(message, "config_verification_run cannot become terminal"),
		strings.Contains(message, "config_verification_run is terminal"):
		return &ConflictError{Code: "row_version_conflict", Detail: "验证 Run 状态已变化，请刷新后重试"}
	default:
		return err
	}
}
