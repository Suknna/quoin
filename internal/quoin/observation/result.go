// Result adjudication: the typed source_observation_result_v1 proposal from
// the Plinth supervisor becomes Evidence, the object row's terminal status,
// and the observed_source_objects identity projection in one transaction.
// Envelope validation, boot/epoch fencing, replay idempotence and Run
// convergence follow the same frozen pattern as the inspection PromQL
// closure; the completeness rule is the ADR-0004 core: only a successful
// (complete) outcome may re-project the object set — failure, partial
// results and truncation leave every previously observed resource untouched.
package observation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// discoveredObject is one observed object of a successful discovery pass.
// Identity is the plugin-derived identity label set (exactly the frozen
// identity labels); labels carries the complete source label set.
type discoveredObject struct {
	Identity    map[string]string `json:"identity"`
	Labels      map[string]string `json:"labels"`
	DisplayName string            `json:"displayName,omitempty"`
}

// resultProposal is the sealed supervisor payload shape.
type resultProposal struct {
	SchemaKind       string             `json:"schemaKind"`
	AttemptID        int64              `json:"attemptId"`
	ObservationRunID int64              `json:"observationRunId"`
	ObjectType       string             `json:"objectType"`
	Outcome          string             `json:"outcome"`
	ObservedAt       string             `json:"observedAt"`
	Objects          []discoveredObject `json:"objects"`
	Warnings         []string           `json:"warnings"`
	Errors           []string           `json:"errors"`
	GapReason        *string            `json:"gapReason"`
}

// validGapReasons is the closed gap vocabulary of observation_run_objects.
var validGapReasons = map[string]bool{
	"query_failed": true, "partial_response": true, "no_data": true,
	"runtime_unavailable": true, "plugin_unavailable": true,
	"cancelled": true, "interrupted": true,
}

// CommitProposal atomically persists one supervisor result. A duplicate
// proposal replays only when it sealed the same immutable digest.
func (service *Service) CommitProposal(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	var proposal resultProposal
	if err := json.Unmarshal(raw, &proposal); err != nil {
		return fmt.Errorf("source observation result is not valid JSON: %w", err)
	}
	if proposal.SchemaKind != ResultSchemaKind || proposal.AttemptID != attemptID || proposal.ObservationRunID < 1 || proposal.ObjectType == "" {
		return errors.New("source observation result has an invalid identity envelope")
	}
	if _, err := time.Parse(time.RFC3339Nano, proposal.ObservedAt); err != nil {
		return fmt.Errorf("source observation observedAt is not RFC3339: %w", err)
	}
	switch proposal.Outcome {
	case "success":
		if proposal.GapReason != nil || len(proposal.Errors) != 0 {
			return errors.New("successful source observation result has invalid gap shape")
		}
		seen := map[string]bool{}
		for _, object := range proposal.Objects {
			if len(object.Identity) == 0 {
				return errors.New("successful source observation object lacks its identity labels")
			}
			key := IdentityKeyEncoding(object.Identity)
			if key == "" || seen[key] {
				return fmt.Errorf("source observation result repeats or empties identity %q", key)
			}
			seen[key] = true
			if object.Labels == nil {
				return errors.New("source observation object must carry its full label set")
			}
		}
	case "error", "gap":
		if len(proposal.Objects) != 0 || proposal.GapReason == nil || !validGapReasons[*proposal.GapReason] {
			return errors.New("non-success source observation result has invalid gap shape")
		}
	default:
		return fmt.Errorf("source observation result has invalid outcome %q", proposal.Outcome)
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

	var runID int64
	var objectType string
	err = conn.QueryRowContext(ctx, `
		SELECT a.scope_id, o.object_type
		FROM execution_attempts a
		JOIN observation_run_objects o ON o.attempt_id=a.id
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='observation_run'`, attemptID).Scan(&runID, &objectType)
	if err != nil {
		return fmt.Errorf("source observation result does not close onto a frozen object child: %w", err)
	}
	if proposal.ObservationRunID != runID || proposal.ObjectType != objectType {
		return errors.New("source observation result identity does not match frozen attempt")
	}
	digest := sha256.Sum256(raw)
	var existing []byte
	// Only a sealed digest counts as committed: the child row exists from
	// admission with a NULL digest, so an unsealed row is simply not a replay.
	replayErr := conn.QueryRowContext(ctx, `SELECT result_digest FROM observation_run_objects WHERE observation_run_id=? AND object_type=? AND result_digest IS NOT NULL`, runID, objectType).Scan(&existing)
	if replayErr == nil {
		// Idempotent replay adjudicates before any liveness fence: a committed
		// result can never be overwritten, only acknowledged.
		if string(existing) == string(digest[:]) {
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				return err
			}
			committed = true
			return nil
		}
		return errors.New("source observation result replay digest conflicts")
	}
	if !errors.Is(replayErr, sql.ErrNoRows) {
		return replayErr
	}
	// Boot/epoch/cancel fence: the frozen commit trigger performs the terminal
	// transition, so enforce the dispatch binding here as the guard.
	var bound int
	if err := conn.QueryRowContext(ctx, `SELECT 1 FROM execution_attempts WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`, attemptID, bootID, epoch).Scan(&bound); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return attempt.ErrLateResult
		}
		return err
	}
	var connectionID int64
	if err := conn.QueryRowContext(ctx, `SELECT connection_id FROM observation_runs WHERE id=?`, runID).Scan(&connectionID); err != nil {
		return err
	}
	var evidenceID any
	if proposal.Outcome == "success" {
		// The complete, bounded observation pass is sealed as Evidence: the
		// frozen proposal is the fact, warnings travel alongside it.
		params, _ := json.Marshal(map[string]any{"objectType": objectType, "objectCount": len(proposal.Objects)})
		warnings, _ := json.Marshal(proposal.Warnings)
		payload, _ := json.Marshal(map[string]any{"schemaKind": ResultSchemaKind, "objectType": objectType, "observedAt": proposal.ObservedAt, "objects": proposal.Objects})
		insert, err := conn.ExecContext(ctx, `
			INSERT INTO evidence(attempt_id,target_type,target_id,params_json,observed_at,result_json,warnings_json,integrity,created_at)
			VALUES(?,'observation_run',?,?,?,?,?,'complete',?)`,
			attemptID, runID, string(params), proposal.ObservedAt, string(payload), string(warnings), service.nowText())
		if err != nil {
			return err
		}
		id, err := insert.LastInsertId()
		if err != nil {
			return err
		}
		evidenceID = id
	}
	status := "ok"
	if proposal.Outcome == "error" {
		status = "error"
	} else if proposal.Outcome == "gap" {
		status = "gap"
	}
	var nullableGap any
	if proposal.GapReason != nil {
		nullableGap = *proposal.GapReason
	}
	warningsJSON, _ := json.Marshal(proposal.Warnings)
	if _, err := conn.ExecContext(ctx, `
		UPDATE observation_run_objects
		SET status=?,gap_reason=?,evidence_id=?,result_digest=?,warnings_json=?
		WHERE observation_run_id=? AND object_type=?`,
		status, nullableGap, evidenceID, digest[:], string(warningsJSON), runID, objectType); err != nil {
		return err
	}
	if proposal.Outcome == "success" {
		if err := projectObservedObjects(ctx, conn, service.nowText(), connectionID, objectType, proposal); err != nil {
			return err
		}
	}
	// The attempt terminal transition commits first: run convergence counts
	// active children, so it must observe this child already terminal or the
	// Run would wait on itself forever.
	if err := attempt.NewService(service.db).CommitResultOn(ctx, conn, attemptID, bootID, epoch, proposal.Outcome != "error", "tool_error"); err != nil {
		return err
	}
	if err := service.convergeRunOn(ctx, conn, runID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// projectObservedObjects re-projects one complete object-type pass. The pass
// is complete by construction (the proposal envelope is success), so marking
// previously observed identities absent (current=0) is the only place
// "not observed anymore" may ever be expressed; a missed identity keeps its
// history, is never deleted, and never becomes stale by inference.
func projectObservedObjects(ctx context.Context, conn *sql.Conn, now string, connectionID int64, objectType string, proposal resultProposal) error {
	if _, err := conn.ExecContext(ctx, `UPDATE observed_source_objects SET current=0 WHERE connection_id=? AND object_type=? AND current=1`, connectionID, objectType); err != nil {
		return err
	}
	for _, object := range proposal.Objects {
		identityKey := IdentityKeyEncoding(object.Identity)
		identityDigest := sha256.Sum256([]byte(identityKey))
		labelsJSON, err := json.Marshal(object.Labels)
		if err != nil {
			return err
		}
		var displayName any
		if object.DisplayName != "" {
			displayName = object.DisplayName
		}
		// SQLite preserves a stale last_insert_rowid() on the conflict-update
		// path. RETURNING identifies this UPSERT's actual row in either path.
		var resourceID int64
		if err := conn.QueryRowContext(ctx, `
			INSERT INTO observed_source_objects(connection_id,object_type,identity_key,identity_digest,display_name,labels_json,observed_at,current,last_successful_refresh_at,created_at)
			VALUES(?,?,?,?,?,?,?,1,?,?)
			ON CONFLICT(connection_id,object_type,identity_key) DO UPDATE SET
				identity_digest=excluded.identity_digest,display_name=excluded.display_name,
				labels_json=excluded.labels_json,observed_at=excluded.observed_at,current=1,
				last_successful_refresh_at=excluded.last_successful_refresh_at
			RETURNING id`,
			connectionID, objectType, identityKey, fmt.Sprintf("%x", identityDigest), displayName, string(labelsJSON), proposal.ObservedAt, proposal.ObservedAt, now).Scan(&resourceID); err != nil {
			return err
		}
	}
	return nil
}

// convergeRunOn completes the Run when no child is active anymore: any failed
// execution fails the Run, any incomplete pass yields CompletedWithWarnings,
// and only then is the Run terminal with an honest result detail.
func (service *Service) convergeRunOn(ctx context.Context, conn *sql.Conn, runID int64) error {
	var active int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE scope_type='observation_run' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling')`, runID).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return nil
	}
	var executionErrors, incomplete int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FILTER(WHERE status='error'),COUNT(*) FILTER(WHERE status='gap') FROM observation_run_objects WHERE observation_run_id=?`, runID).Scan(&executionErrors, &incomplete); err != nil {
		return err
	}
	state := "Completed"
	var detail any
	if executionErrors > 0 {
		state, detail = "Failed", "one or more source observation executions failed"
	} else if incomplete > 0 {
		// The schema keeps result_detail NULL for CompletedWithWarnings: the
		// per-object rows carry the exact gap reasons and warnings, so the
		// Run header never duplicates or drifts from the frozen facts.
		state = "CompletedWithWarnings"
	}
	if _, err := conn.ExecContext(ctx, `UPDATE observation_runs SET state=?,result_detail=?,row_version=row_version+1 WHERE id=? AND state='Running'`, state, detail, runID); err != nil {
		return err
	}
	return nil
}
