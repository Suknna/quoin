package businesssystem

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

type resourceRefreshProposal struct {
	SchemaKind           string `json:"schemaKind"`
	AttemptID            int64  `json:"attemptId"`
	ResourceRefreshRunID int64  `json:"resourceRefreshRunId"`
	DiscoveryKey         string `json:"discoveryKey"`
	Outcome              string `json:"outcome"`
	ObservedAt           string `json:"observedAt"`
	Series               []struct {
		Labels    map[string]string `json:"labels"`
		Value     string            `json:"value"`
		Timestamp float64           `json:"timestamp"`
	} `json:"series"`
	Warnings  []string `json:"warnings"`
	Errors    []string `json:"errors"`
	GapReason *string  `json:"gapReason"`
}

func (service *Service) CommitResourceRefreshProposal(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	var p resourceRefreshProposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if p.SchemaKind != "resource_discovery_result_v1" || p.AttemptID != attemptID || p.ResourceRefreshRunID < 1 || p.DiscoveryKey == "" {
		return fmt.Errorf("invalid resource refresh result identity")
	}
	if _, err := time.Parse(time.RFC3339Nano, p.ObservedAt); err != nil {
		return fmt.Errorf("resource refresh observedAt is not RFC3339: %w", err)
	}
	validGap := map[string]bool{"query_failed": true, "partial_response": true, "cancelled": true, "interrupted": true}
	switch p.Outcome {
	case "success":
		if p.GapReason != nil || len(p.Errors) != 0 {
			return fmt.Errorf("successful resource refresh result has invalid gap shape")
		}
	case "error", "gap":
		if len(p.Series) != 0 || p.GapReason == nil || !validGap[*p.GapReason] {
			return fmt.Errorf("non-success resource refresh result has invalid gap shape")
		}
	default:
		return fmt.Errorf("resource refresh result has invalid outcome")
	}
	if _, err := time.Parse(time.RFC3339Nano, p.ObservedAt); err != nil {
		return err
	}
	if p.Outcome != "success" && p.Outcome != "gap" && p.Outcome != "error" {
		return fmt.Errorf("invalid resource refresh outcome")
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var runID, systemID int64
	var key, identityJSON string
	if err := conn.QueryRowContext(ctx, `SELECT a.scope_id,r.business_system_id,a.discovery_key,d.identity_labels_json FROM execution_attempts a JOIN resource_refresh_runs r ON r.id=a.scope_id JOIN config_discoveries d ON d.config_version_id=r.config_version_id AND d.discovery_key=a.discovery_key WHERE a.id=? AND a.scope_type='resource_refresh_run'`, attemptID).Scan(&runID, &systemID, &key, &identityJSON); err != nil {
		return err
	}
	if runID != p.ResourceRefreshRunID || key != p.DiscoveryKey {
		return fmt.Errorf("resource refresh result identity does not match attempt")
	}
	var identities []string
	if err := json.Unmarshal([]byte(identityJSON), &identities); err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	var existing []byte
	err = conn.QueryRowContext(ctx, `SELECT result_digest FROM observed_refresh_log WHERE attempt_id=?`, attemptID).Scan(&existing)
	if err == nil {
		if string(existing) == string(digest[:]) {
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				return err
			}
			committed = true
			return nil
		}
		return fmt.Errorf("resource refresh proposal conflicts with its sealed result")
	}
	if err != sql.ErrNoRows {
		return err
	}
	complete := p.Outcome == "success"
	if complete {
		if _, err := conn.ExecContext(ctx, `UPDATE observed_resources SET current=0 WHERE business_system_id=? AND discovery_key=? AND current=1`, systemID, key); err != nil {
			return err
		}
		for _, series := range p.Series {
			identityKey, identityLabels, err := resourceIdentity(identities, series.Labels)
			if err != nil {
				return err
			}
			labelsJSON, _ := json.Marshal(series.Labels)
			idDigest := sha256.Sum256([]byte(identityKey))
			insert, err := conn.ExecContext(ctx, `INSERT INTO observed_resources(business_system_id,discovery_key,identity_key,identity_digest,labels_json,observed_at,current,last_successful_refresh_at,created_at) VALUES(?,?,?,?,?,?,1,?,?) ON CONFLICT(business_system_id,discovery_key,identity_key) DO UPDATE SET labels_json=excluded.labels_json,observed_at=excluded.observed_at,current=1,last_successful_refresh_at=excluded.last_successful_refresh_at`, systemID, key, identityKey, fmt.Sprintf("%x", idDigest), string(labelsJSON), p.ObservedAt, p.ObservedAt, service.nowText())
			if err != nil {
				return err
			}
			resourceID, err := insert.LastInsertId()
			if err != nil {
				return err
			}
			if resourceID != 0 {
				for _, name := range identities {
					if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO observed_resource_identity_labels(observed_resource_id,name,value) VALUES(?,?,?)`, resourceID, name, identityLabels[name]); err != nil {
						return err
					}
				}
			}
		}
	}
	warnings, _ := json.Marshal(p.Warnings)
	var errorDetail any
	if !complete {
		errorDetail = strings.Join(append(p.Errors, p.Warnings...), "; ")
		if errorDetail == "" {
			errorDetail = "resource discovery incomplete"
		}
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO observed_refresh_log(resource_refresh_run_id,attempt_id,business_system_id,discovery_key,started_at,completed_at,complete,warnings_json,error_detail,result_digest) VALUES(?,?,?,?,?,?,?,?,?,?)`, runID, attemptID, systemID, key, p.ObservedAt, service.nowText(), boolInt(complete), string(warnings), errorDetail, digest[:]); err != nil {
		return err
	}
	if err := attempt.NewService(service.db).CommitResultOn(ctx, conn, attemptID, bootID, epoch, p.Outcome != "error", "tool_error"); err != nil {
		return err
	}
	var active int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE scope_type='resource_refresh_run' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling')`, runID).Scan(&active); err != nil {
		return err
	}
	if active == 0 {
		var executionErrors, incomplete int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FILTER(WHERE a.state='Failed'),COUNT(*) FILTER(WHERE l.complete=0) FROM execution_attempts a JOIN observed_refresh_log l ON l.attempt_id=a.id WHERE a.scope_type='resource_refresh_run' AND a.scope_id=?`, runID).Scan(&executionErrors, &incomplete); err != nil {
			return err
		}
		state := "Completed"
		var detail any
		if executionErrors > 0 {
			state, detail = "Failed", "one or more discovery executions failed"
		} else if incomplete > 0 {
			state = "CompletedWithWarnings"
		}
		if _, err := conn.ExecContext(ctx, `UPDATE resource_refresh_runs SET state=?,result_detail=?,row_version=row_version+1 WHERE id=? AND state='Running'`, state, detail, runID); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}
func resourceIdentity(names []string, labels map[string]string) (string, map[string]string, error) {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	out := map[string]string{}
	parts := make([]string, 0, len(sorted))
	for _, name := range sorted {
		value, ok := labels[name]
		if !ok {
			return "", nil, fmt.Errorf("discovery result omits identity label %q", name)
		}
		out[name] = value
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "\x1f"), out, nil
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
