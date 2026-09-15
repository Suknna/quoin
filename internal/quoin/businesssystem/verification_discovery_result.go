package businesssystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

type verificationDiscoveryProposal struct {
	SchemaKind        string `json:"schemaKind"`
	AttemptID         int64  `json:"attemptId"`
	VerificationRunID int64  `json:"verificationRunId"`
	DiscoveryKey      string `json:"discoveryKey"`
	Outcome           string `json:"outcome"`
	ObservedAt        string `json:"observedAt"`
	Series            []struct {
		Labels    map[string]string `json:"labels"`
		Value     string            `json:"value"`
		Timestamp float64           `json:"timestamp"`
	} `json:"series"`
	Warnings  []string `json:"warnings"`
	Errors    []string `json:"errors"`
	GapReason *string  `json:"gapReason"`
}

func validateDiscoverySeriesScope(ctx context.Context, conn execution.Executor, configVersionID int64, discoveryKey string, series []struct {
	Labels    map[string]string `json:"labels"`
	Value     string            `json:"value"`
	Timestamp float64           `json:"timestamp"`
}) error {
	var systemKey, identityJSON string
	if err := conn.QueryRowContext(ctx, `SELECT v.system_key,d.identity_labels_json FROM business_system_config_versions v JOIN config_discoveries d ON d.config_version_id=v.id AND d.discovery_key=? WHERE v.id=?`, discoveryKey, configVersionID).Scan(&systemKey, &identityJSON); err != nil {
		return err
	}
	// Resource scope selectors are already compiler-injected. Validation below
	// checks identity completeness without a global label-contract authority.
	_ = systemKey
	var identityLabels []string
	if err := json.Unmarshal([]byte(identityJSON), &identityLabels); err != nil {
		return err
	}
	for _, item := range series {
		for _, identityLabel := range identityLabels {
			if strings.TrimSpace(item.Labels[identityLabel]) == "" {
				return fmt.Errorf("verification discovery series lacks a declared identity label")
			}
		}
		for name, value := range item.Labels {
			if strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
				return fmt.Errorf("verification discovery series has an invalid label")
			}
		}
	}
	return nil
}

// CommitVerificationDiscoveryProposal seals a real selector response into
// draft-only Evidence and never calls the observed-resource projector.
func (service *Service) CommitVerificationDiscoveryProposal(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var p verificationDiscoveryProposal
	if err := decoder.Decode(&p); err != nil || decoder.More() {
		return fmt.Errorf("invalid verification discovery result shape")
	}
	if p.SchemaKind != "config_verification_discovery_result_v1" || p.AttemptID != attemptID || p.VerificationRunID < 1 || p.DiscoveryKey == "" {
		return fmt.Errorf("invalid verification discovery identity")
	}
	if _, err := time.Parse(time.RFC3339Nano, p.ObservedAt); err != nil {
		return err
	}
	validGap := map[string]bool{"query_failed": true, "partial_response": true, "cancelled": true, "interrupted": true, "no_data": true}
	switch p.Outcome {
	case "success":
		if p.GapReason != nil || len(p.Warnings) != 0 || len(p.Errors) != 0 || len(p.Series) == 0 {
			return fmt.Errorf("successful verification discovery must contain complete non-empty series and no gaps")
		}
	case "error", "gap":
		if p.GapReason == nil || !validGap[*p.GapReason] || len(p.Series) != 0 {
			return fmt.Errorf("invalid verification discovery gap")
		}
	default:
		return fmt.Errorf("invalid verification discovery outcome")
	}
	scope, err := service.resultContext(ctx, attemptID)
	if err != nil {
		return err
	}
	if _, err := execution.Execute(scope, service.runner, service.opDiscoveryResult,
		func(conn *execution.Tx) (struct{}, error) {
			return service.commitDiscoveryResultOn(scope, conn, attemptID, bootID, epoch, raw, p)
		},
		func(struct{}) int64 { return 0 }); err != nil {
		if errors.Is(err, execution.ErrNoTransition) {
			// The identical proposal already sealed the same immutable digest:
			// an idempotent replay records nothing.
			return nil
		}
		return err
	}
	return nil
}

// commitDiscoveryResultOn is CommitVerificationDiscoveryProposal's business
// stage on the runner-owned transaction.
func (service *Service) commitDiscoveryResultOn(ctx context.Context, conn execution.Executor, attemptID int64, bootID string, epoch uint64, raw []byte, p verificationDiscoveryProposal) (struct{}, error) {
	var runID, versionID int64
	var key string
	if err := conn.QueryRowContext(ctx, `SELECT a.scope_id,r.config_version_id,a.discovery_key FROM execution_attempts a JOIN config_verification_runs r ON r.id=a.scope_id WHERE a.id=? AND a.scope_type='config_verification_run' AND a.discovery_key IS NOT NULL`, attemptID).Scan(&runID, &versionID, &key); err != nil {
		return struct{}{}, err
	}
	if runID != p.VerificationRunID || key != p.DiscoveryKey {
		return struct{}{}, fmt.Errorf("verification discovery identity does not match attempt")
	}
	if p.Outcome == "success" {
		if err := validateDiscoverySeriesScope(ctx, conn, versionID, key, p.Series); err != nil {
			return struct{}{}, err
		}
	}
	digest := sha256.Sum256(raw)
	var existing []byte
	if err := conn.QueryRowContext(ctx, `SELECT result_digest FROM config_verification_discovery_results WHERE attempt_id=?`, attemptID).Scan(&existing); err == nil {
		if string(existing) == string(digest[:]) {
			return struct{}{}, fmt.Errorf("%w: identical discovery proposal already sealed", execution.ErrNoTransition)
		}
		return struct{}{}, fmt.Errorf("verification discovery proposal conflicts")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return struct{}{}, err
	}
	status, gap := "ok", any(nil)
	var evidenceID any
	if p.Outcome == "success" {
		params, _ := json.Marshal(map[string]string{"discoveryKey": key})
		result, _ := json.Marshal(map[string]any{"series": p.Series})
		insert, err := conn.ExecContext(ctx, `INSERT INTO evidence(attempt_id,target_type,target_id,params_json,observed_at,result_json,integrity,created_at) VALUES(?,'config_verification_run',?,?,?,?,?,?)`, attemptID, runID, string(params), p.ObservedAt, string(result), "complete", service.nowText())
		if err != nil {
			return struct{}{}, err
		}
		evidenceID, _ = insert.LastInsertId()
	} else {
		status = "gap"
		gap = *p.GapReason
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO config_verification_discovery_results(verification_run_id,discovery_key,attempt_id,evidence_id,status,gap_reason,result_digest,created_at) VALUES(?,?,?,?,?,?,?,?)`, runID, key, attemptID, evidenceID, status, gap, digest[:], service.nowText()); err != nil {
		return struct{}{}, err
	}
	if err := attempt.NewService(service.db).CommitResultOn(ctx, conn, attemptID, bootID, epoch, p.Outcome == "success", "tool_error"); err != nil {
		return struct{}{}, err
	}
	if err := convergeVerificationRunOn(ctx, conn, runID); err != nil {
		return struct{}{}, err
	}
	_ = versionID
	return struct{}{}, nil
}
