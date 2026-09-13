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
	"fmt"
	"strconv"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/auth"
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
func (service *Service) RunVerification(ctx context.Context, principalID int64, clientCommandID, systemKey string, versionID int64) (VerificationRunDetail, error) {
	commandDigest := commandDigestOf("config_verification.run", map[string]any{
		"systemKey": systemKey, "versionId": versionID,
	})
	if detail, replayed, err := service.replayVerification(ctx, principalID, clientCommandID, commandDigest); replayed || err != nil {
		return detail, err
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return VerificationRunDetail{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return VerificationRunDetail{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	// The outer replay check can race a request which commits while this one
	// waits for BEGIN IMMEDIATE. Do not create a second run for the same key.
	if replayed, alreadyCommitted, replayErr := service.replayVerificationOn(ctx, conn, principalID, clientCommandID, commandDigest); replayErr != nil {
		return VerificationRunDetail{}, replayErr
	} else if alreadyCommitted {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return VerificationRunDetail{}, err
		}
		committed = true
		return replayed, nil
	}
	systemID, contractID, err := draftBinding(ctx, conn, systemKey, versionID)
	if err != nil {
		if rejection, known := verificationRejectionFor(err, systemKey, versionID); known {
			return VerificationRunDetail{}, service.rejectVerification(ctx, conn, principalID, clientCommandID, "config_verification.run", commandDigest, rejection, &committed)
		}
		return VerificationRunDetail{}, err
	}
	// Browser checks execute through the Lintel journey executor: every check
	// freezes a lintel-dispatched child Attempt and its journey operation, or
	// settles deterministically as a local gap inside this transaction
	// (CFG-VERIFYRUN-002, DATA-BROWSER-003/006).
	var browserCount int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM config_checks c JOIN config_plans p ON p.id=c.plan_id
		WHERE p.config_version_id=? AND c.kind='browser'`, versionID).Scan(&browserCount); err != nil {
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
			return VerificationRunDetail{}, service.rejectVerification(ctx, conn, principalID, clientCommandID, "config_verification.run", commandDigest, rejection, &committed)
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
	if promQLCount > 0 || browserCount > 0 || discoveryCount > 0 {
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
		if browserCount > 0 {
			if _, err := createBrowserVerificationAttempts(ctx, conn, runID, versionID, contractID, systemID, now); err != nil {
				return VerificationRunDetail{}, err
			}
			// The run's browser checks share one Browser Identity and execute
			// serially; admit the first child (and freeze its snapshot) inside
			// the creation transaction, later children follow on Stop fences.
			if _, err := admitNextJourneyChildOn(ctx, conn, now); err != nil {
				return VerificationRunDetail{}, err
			}
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
	if err := service.audit(ctx, conn, principalID, "config_verification.run", runID, now); err != nil {
		return VerificationRunDetail{}, err
	}
	detail, err := verificationDetailOn(ctx, conn, systemID, versionID, runID)
	if err != nil {
		return VerificationRunDetail{}, err
	}
	if err := recordCommandResult(ctx, conn, principalID, clientCommandID, "config_verification.run", commandDigest, "config_verification_run", runID, detail); err != nil {
		return VerificationRunDetail{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return VerificationRunDetail{}, err
	}
	committed = true
	return detail, nil
}

// CancelVerification applies the one-shot cancel fence: the command must
// carry the current rowVersion and only a non-terminal run can be cancelled
// (DATA-CONFIG-007). T17 has no dispatched child attempts yet, so the frozen
// child-fence trigger is trivially satisfied.
func (service *Service) CancelVerification(ctx context.Context, principalID int64, clientCommandID, systemKey string, versionID, runID, expectedRowVersion int64) (VerificationRunDetail, error) {
	commandDigest := commandDigestOf("config_verification.cancel", map[string]any{
		"systemKey": systemKey, "versionId": versionID, "runId": runID, "expectedRowVersion": expectedRowVersion,
	})
	if detail, replayed, err := service.replayVerification(ctx, principalID, clientCommandID, commandDigest); replayed || err != nil {
		return detail, err
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return VerificationRunDetail{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return VerificationRunDetail{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	// Recheck after writer acquisition so one command key cannot cancel twice
	// or turn a replay into a stale row-version conflict.
	if replayed, alreadyCommitted, replayErr := service.replayVerificationOn(ctx, conn, principalID, clientCommandID, commandDigest); replayErr != nil {
		return VerificationRunDetail{}, replayErr
	} else if alreadyCommitted {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return VerificationRunDetail{}, err
		}
		committed = true
		return replayed, nil
	}
	systemID, _, err := ownedVersion(ctx, conn, systemKey, versionID)
	if err != nil {
		if rejection, known := verificationRejectionFor(err, systemKey, versionID); known {
			return VerificationRunDetail{}, service.rejectVerification(ctx, conn, principalID, clientCommandID, "config_verification.cancel", commandDigest, rejection, &committed)
		}
		return VerificationRunDetail{}, err
	}
	if _, err := verificationRow(ctx, conn, systemID, versionID, runID); err != nil {
		if rejection, known := verificationRejectionFor(err, systemKey, runID); known {
			return VerificationRunDetail{}, service.rejectVerification(ctx, conn, principalID, clientCommandID, "config_verification.cancel", commandDigest, rejection, &committed)
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
			return VerificationRunDetail{}, service.rejectVerification(ctx, conn, principalID, clientCommandID, "config_verification.cancel", commandDigest, rejection, &committed)
		}
		return VerificationRunDetail{}, mapped
	}
	affected, _ := update.RowsAffected()
	if affected == 0 {
		rejection := verificationRejection{
			Code:      "row_version_conflict",
			Detail:    "验证 Run 已变化或已进入终态，请刷新后重试",
			SystemKey: systemKey,
			ObjectID:  runID,
		}
		return VerificationRunDetail{}, service.rejectVerification(ctx, conn, principalID, clientCommandID, "config_verification.cancel", commandDigest, rejection, &committed)
	}
	if err := service.audit(ctx, conn, principalID, "config_verification.cancel", runID, service.nowText()); err != nil {
		return VerificationRunDetail{}, err
	}
	detail, err := verificationDetailOn(ctx, conn, systemID, versionID, runID)
	if err != nil {
		return VerificationRunDetail{}, err
	}
	if err := recordCommandResult(ctx, conn, principalID, clientCommandID, "config_verification.cancel", commandDigest, "config_verification_run", runID, detail); err != nil {
		return VerificationRunDetail{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return VerificationRunDetail{}, err
	}
	committed = true
	detail.CancellingAttemptIDs = dispatchIDs
	return detail, nil
}

// GetVerification returns one run detail bound to (system, version).
func (service *Service) GetVerification(ctx context.Context, systemKey string, versionID, runID int64) (VerificationRunDetail, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return VerificationRunDetail{}, err
	}
	defer conn.Close()
	systemID, _, err := ownedVersion(ctx, conn, systemKey, versionID)
	if err != nil {
		return VerificationRunDetail{}, err
	}
	return verificationDetailOn(ctx, conn, systemID, versionID, runID)
}

// ListVerifications returns the run history for one config version, newest
// first with a (created_at, id) keyset cursor.
func (service *Service) ListVerifications(ctx context.Context, systemKey string, versionID int64, cursor string, limit int) ([]VerificationRunSummary, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()
	systemID, _, err := ownedVersion(ctx, conn, systemKey, versionID)
	if err != nil {
		return nil, false, err
	}
	query := `
		SELECT id,purpose,config_version_id,label_contract_version_id,state,row_version,evidence_at,created_at
		FROM config_verification_runs WHERE business_system_id=? AND config_version_id=?`
	args := []any{systemID, versionID}
	if cursor != "" {
		createdAt, lastID, parseErr := parseVerificationCursor(cursor)
		if parseErr != nil {
			return nil, false, parseErr
		}
		query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, createdAt, createdAt, lastID)
	}
	// HTTP-PAGE-005 freezes (created_at DESC, id DESC); cursor comparison uses
	// that exact composite order so timestamps shared by multiple rows are safe.
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []VerificationRunSummary{}
	for rows.Next() {
		summary, scanErr := scanVerificationSummary(rows)
		if scanErr != nil {
			return nil, false, scanErr
		}
		items = append(items, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := false
	if len(items) > limit {
		items = items[:limit]
		more = true
	}
	return items, more, nil
}

func parseVerificationCursor(cursor string) (string, int64, error) {
	createdAt, id, found := strings.Cut(cursor, "\x00")
	if !found || createdAt == "" {
		return "", 0, fmt.Errorf("invalid verification cursor")
	}
	value, err := strconv.ParseInt(id, 10, 64)
	if err != nil || value <= 0 {
		return "", 0, fmt.Errorf("invalid verification cursor")
	}
	return createdAt, value, nil
}

// --- internal plumbing ------------------------------------------------------

// commandDigestOf wraps auth.DigestCommand for the verification commands.
func commandDigestOf(commandType string, fields map[string]any) string {
	return auth.DigestCommand(commandType, fields)
}

// replayVerification returns the stored outcome when the same command key
// was already committed (HTTP-COMMAND-003).
func (service *Service) replayVerification(ctx context.Context, principalID int64, clientCommandID, digest string) (VerificationRunDetail, bool, error) {
	record, found, err := auth.LookupCommand(ctx, service.db, principalID, clientCommandID)
	if err != nil {
		return VerificationRunDetail{}, false, err
	}
	return replayVerificationRecord(record, found, digest)
}

// replayVerificationOn performs the required second ledger check from the
// connection that already owns SQLite's writer serialization.
func (service *Service) replayVerificationOn(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, digest string) (VerificationRunDetail, bool, error) {
	record, found, err := auth.LookupCommandOn(ctx, conn, principalID, clientCommandID)
	if err != nil {
		return VerificationRunDetail{}, false, err
	}
	return replayVerificationRecord(record, found, digest)
}

func replayVerificationRecord(record auth.CommandRecord, found bool, digest string) (VerificationRunDetail, bool, error) {
	if !found {
		return VerificationRunDetail{}, false, nil
	}
	if record.RequestDigest != digest {
		return VerificationRunDetail{}, true, ErrCommandReused
	}
	if record.Outcome == auth.OutcomeRejectedKnown {
		var rejection verificationRejection
		if err := decodeStored(record.ResultPayload, &rejection); err != nil {
			return VerificationRunDetail{}, true, errors.New("rejected config verification command has an unreadable result")
		}
		return VerificationRunDetail{}, true, rejection.asError()
	}
	var replayed VerificationRunDetail
	if err := decodeStored(record.ResultPayload, &replayed); err != nil {
		return VerificationRunDetail{}, true, errors.New("committed config verification command has an unreadable result")
	}
	if replayed.ID == "" {
		return VerificationRunDetail{}, true, errors.New("committed config verification command has no result")
	}
	return replayed, true, nil
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

// rejectVerification commits the command ledger and its rejected audit event
// in the same otherwise-empty IMMEDIATE transaction (DATA-COMMAND-004).
func (service *Service) rejectVerification(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, commandType, digest string, rejection verificationRejection, committed *bool) error {
	if err := auth.RecordCommand(ctx, conn, principalID, clientCommandID, commandType, digest, auth.OutcomeRejectedKnown, rejection.ObjectType, rejection.ObjectID, encode(rejection)); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO audit_events(actor_type,actor_id,action,client_command_id,outcome,domain_ref_type,domain_ref_id,created_at)
		VALUES('user',?,?,?,'rejected',?,?,?)`,
		principalID, commandType, clientCommandID, rejection.ObjectType, rejection.ObjectID, service.nowText()); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	*committed = true
	return rejection.asError()
}

// recordCommandResult persists the command outcome inside the open
// transaction (DATA-AUDIT-004/005).
func recordCommandResult(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, commandType, digest, objectType string, objectID int64, result any) error {
	return auth.RecordCommand(ctx, conn, principalID, clientCommandID, commandType, digest, "committed", objectType, objectID, encode(result))
}

// draftBinding resolves (business_system_id, label_contract_version_id) for a
// version that must be an unpublished draft of the named system; the frozen
// INSERT closure trigger re-verifies the same facts inside the transaction.
func draftBinding(ctx context.Context, conn *sql.Conn, systemKey string, versionID int64) (int64, int64, error) {
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
func ownedVersion(ctx context.Context, conn *sql.Conn, systemKey string, versionID int64) (int64, int64, error) {
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

func verificationRow(ctx context.Context, conn *sql.Conn, systemID, versionID, runID int64) (int64, error) {
	var id int64
	err := conn.QueryRowContext(ctx, `SELECT id FROM config_verification_runs WHERE id=? AND business_system_id=? AND config_version_id=?`, runID, systemID, versionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

func scanVerificationSummary(rows *sql.Rows) (VerificationRunSummary, error) {
	var (
		summary    VerificationRunSummary
		id         int64
		versionID  int64
		contractID sql.NullInt64
		evidenceAt sql.NullString
	)
	if err := rows.Scan(&id, &summary.Purpose, &versionID, &contractID, &summary.State, &summary.RowVersion, &evidenceAt, &summary.CreatedAt); err != nil {
		return VerificationRunSummary{}, err
	}
	summary.ID = strconv.FormatInt(id, 10)
	summary.ConfigVersionID = strconv.FormatInt(versionID, 10)
	if contractID.Valid {
		summary.LabelContractVersionID = strconv.FormatInt(contractID.Int64, 10)
	}
	if evidenceAt.Valid {
		value := evidenceAt.String
		summary.EvidenceAt = &value
	}
	return summary, nil
}

func verificationDetailOn(ctx context.Context, conn *sql.Conn, systemID, versionID, runID int64) (VerificationRunDetail, error) {
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
		SELECT r.plan_key,r.check_key,r.status,r.evidence_id,r.gap_reason,j.error_detail
		FROM config_verification_run_check_results r
		LEFT JOIN browser_journey_results j ON j.attempt_id=r.attempt_id
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

func verificationDiscoverySamples(ctx context.Context, conn *sql.Conn, runID int64) ([]VerificationIdentitySample, error) {
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
