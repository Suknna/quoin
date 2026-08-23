package businesssystem

// The Config Verification PromQL execution slice freezes the published query
// projection and its deployment-wide Thanos grant into one supervisor-only
// inspection_collection attempt. Quoin deliberately persists no secret: the
// supervisor fetches its attempt-scoped grant only after dispatch.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

const (
	configVerificationSchemaKind      = "config_verification_execution_v1"
	configVerificationRendererVersion = "v1"
)

type configVerificationInput struct {
	SchemaKind        string                  `json:"schemaKind"`
	AttemptID         int64                   `json:"attemptId"`
	VerificationRunID int64                   `json:"verificationRunId"`
	PlanKey           string                  `json:"planKey"`
	CheckKey          string                  `json:"checkKey"`
	Query             configVerificationQuery `json:"query"`
	GrantID           int64                   `json:"grantId"`
}

type configVerificationQuery struct {
	Mode         string `json:"mode"`
	Expression   string `json:"expression"`
	RangeSeconds *int64 `json:"rangeSeconds"`
	StepSeconds  *int64 `json:"stepSeconds"`
}

type promQLVerificationCheck struct {
	PlanKey      string
	CheckKey     string
	Mode         string
	Expression   string
	RangeSeconds sql.NullInt64
	StepSeconds  sql.NullInt64
}

// createPromQLVerificationAttempts appends all PromQL child Attempts to the
// Run creation transaction. A missing usable Thanos connection aborts that
// transaction: a run cannot claim execution when it has no authorized path.
func createPromQLVerificationAttempts(ctx context.Context, conn *sql.Conn, runID, configVersionID, contractID int64, now string) (int, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT p.plan_key,c.check_key,c.query_mode,c.expression,c.range_seconds,c.step_seconds
		FROM config_checks c
		JOIN config_plans p ON p.id=c.plan_id
		WHERE p.config_version_id=? AND c.kind='promql'
		ORDER BY p.plan_key,c.check_key`, configVersionID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var check promQLVerificationCheck
		if err := rows.Scan(&check.PlanKey, &check.CheckKey, &check.Mode, &check.Expression, &check.RangeSeconds, &check.StepSeconds); err != nil {
			return 0, err
		}
		if err := createPromQLVerificationAttempt(ctx, conn, runID, configVersionID, contractID, check, now); err != nil {
			return 0, err
		}
		count++
	}
	return count, rows.Err()
}

func createPromQLVerificationAttempt(ctx context.Context, conn *sql.Conn, runID, configVersionID, contractID int64, check promQLVerificationCheck, now string) error {
	var rangeSeconds, stepSeconds *int64
	if check.RangeSeconds.Valid {
		rangeSeconds = &check.RangeSeconds.Int64
	}
	if check.StepSeconds.Valid {
		stepSeconds = &check.StepSeconds.Int64
	}
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,plan_key,check_key,state,quoin_release_version,created_at)
		VALUES('inspection_collection','config_verification_run',?,?,?,'Queued',?,?)`,
		runID, check.PlanKey, check.CheckKey, attempt.ReleaseVersion(), now)
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
	canonical, err := json.Marshal(configVerificationInput{
		SchemaKind: configVerificationSchemaKind, AttemptID: attemptID, VerificationRunID: runID,
		PlanKey: check.PlanKey, CheckKey: check.CheckKey, GrantID: grant.GrantID,
		Query: configVerificationQuery{Mode: check.Mode, Expression: check.Expression, RangeSeconds: rangeSeconds, StepSeconds: stepSeconds},
	})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	snapshot, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at)
		VALUES(?,?,?,?,?)`, attemptID, configVerificationSchemaKind, configVerificationRendererVersion, hex.EncodeToString(digest[:]), now)
	if err != nil {
		return err
	}
	snapshotID, err := snapshot.LastInsertId()
	if err != nil {
		return err
	}
	configDigest := sha256.Sum256([]byte(fmt.Sprintf("business-system-config-version:%d", configVersionID)))
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,business_system_config_version_id)
		VALUES(?,1,'config_version',?,?)`, snapshotID, hex.EncodeToString(configDigest[:]), configVersionID); err != nil {
		return err
	}
	contractDigest := sha256.Sum256([]byte(fmt.Sprintf("label-contract-version:%d", contractID)))
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,label_contract_version_id)
		VALUES(?,2,'label_contract',?,?)`, snapshotID, hex.EncodeToString(contractDigest[:]), contractID); err != nil {
		return err
	}
	return nil
}
