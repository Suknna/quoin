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
	RunID int64 `json:"-"`
	ID    string `json:"id"`
	// BusinessSystemKey 仅历史 Run（旧业务声明计划）携带；计划 Run 为空。
	BusinessSystemKey *string                  `json:"businessSystemKey,omitempty"`
	ConnectionName    *string                  `json:"connectionName,omitempty"`
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
	// ID is the immutable report locator consumed by diagnosis feedback, unlike
	// the human-facing (runId, version) route locator.
	ID             string   `json:"id"`
	RunID          string   `json:"runId"`
	Version        int64    `json:"version"`
	EvidenceDigest string   `json:"evidenceDigest"`
	EvidenceIDs    []string `json:"evidenceIds"`
	ModelID        string   `json:"modelId"`
	Content        string   `json:"content"`
	CreatedAt      string   `json:"createdAt"`
}

func locatorID(id int64) string { return strconv.FormatInt(id, 10) }

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
	// New inspection children carry only config-version lineage. contractID stays
	// in the function signature while historical run rows still reference it.
	return nil
}

// convergeOn closes the Run once every configured check has settled:
// Completed requires all-ok coverage, CompletedWithGaps at least one explicit
// gap (trg_inspection_runs_result_set_complete re-validates both). Plan runs
// count their run-frozen catalog; legacy declaration runs keep their
// config_checks join.
func (s *Service) convergeOn(ctx context.Context, conn *sql.Conn, runID int64) error {
	var pending, gaps int
	err := conn.QueryRowContext(ctx, `
		SELECT
		  CASE WHEN r.plan_id IS NOT NULL
		    THEN (SELECT COUNT(*) FROM inspection_run_checks c WHERE c.run_id=r.id)
		    ELSE (SELECT COUNT(*) FROM config_checks c JOIN config_plans p ON p.id=c.plan_id
		          WHERE p.config_version_id=r.config_version_id AND p.plan_key=r.plan_key)
		  END
		  - (SELECT COUNT(*) FROM inspection_check_results x WHERE x.run_id=?),
		  (SELECT COUNT(*) FROM inspection_check_results x WHERE x.run_id=? AND x.status <> 'ok')
		FROM inspection_runs r WHERE r.id=?`, runID, runID, runID).Scan(&pending, &gaps)
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
	BusinessSystemKey *string `json:"businessSystemKey,omitempty"`
	ConnectionName    *string `json:"connectionName,omitempty"`
	PlanKey           string  `json:"planKey"`
	State             string  `json:"state"`
	RowVersion        int64   `json:"rowVersion"`
	TriggerKind       string  `json:"triggerKind"`
	ScheduledFor      *string `json:"scheduledFor,omitempty"`
	EvidenceAt        *string `json:"evidenceAt,omitempty"`
	CreatedAt         string  `json:"createdAt"`
}

// RuntimeAvailability is sampled by the scheduling runtime at the boundary.
// A false slot produces a durable runtime_unavailable check gap rather than a
// queued execution that would silently run later.
type RuntimeAvailability struct {
	Plinth bool
	Lintel bool
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
