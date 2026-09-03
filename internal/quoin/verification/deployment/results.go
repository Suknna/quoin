package deployment

import (
	"context"
	"database/sql"
	"time"
)

// ResultInput is one immutable item result append. producer/outcome/category
// vocabularies and their coupling are frozen by the SQL contract; the service
// re-derives digests where it owns them.
type ResultInput struct {
	InvocationID        int64
	ItemID              int64
	InputDigest         string
	ResultDigest        string
	ProducerType        string
	Outcome             string
	Category            string
	ObservedAt          string
	EvidenceIndexDigest string
	ArtifactID          *int64
}

// AppendResult appends one immutable result. Same (item, input, result)
// digest replays idempotently; a different result digest for the same item
// is retained together with an immutable conflict row (auto-created by the
// AFTER INSERT trigger) and forces the failed outcome at finalization.
func (service *Service) AppendResult(ctx context.Context, input ResultInput) (int64, bool, error) {
	var resultID int64
	err := service.withConn(ctx, func(conn *sql.Conn) error {
		var invocation int64
		var frozenInput string
		err := conn.QueryRowContext(ctx, `SELECT i.invocation_id,i.input_digest FROM verification_invocation_items i WHERE i.id=?`, input.ItemID).Scan(&invocation, &frozenInput)
		if err == sql.ErrNoRows {
			return service.fail(errNotFound, "verification item %d not found", input.ItemID)
		}
		if err != nil {
			return err
		}
		if invocation != input.InvocationID {
			return service.fail(errInvalid, "item %d does not belong to invocation %d", input.ItemID, input.InvocationID)
		}
		if frozenInput != input.InputDigest {
			return service.fail(errInvalid, "input digest does not match the frozen manifest item")
		}
		// Idempotent replay of the identical result.
		var existing int64
		err = conn.QueryRowContext(ctx, `SELECT id FROM verification_item_results WHERE item_id=? AND input_digest=? AND result_digest=?`,
			input.ItemID, input.InputDigest, input.ResultDigest).Scan(&existing)
		if err == nil {
			resultID = existing
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		committedAt := service.now().UTC().Format(time.RFC3339Nano)
		insert, err := conn.ExecContext(ctx, `INSERT INTO verification_item_results(item_id,input_digest,result_digest,producer_type,outcome,category,observed_at,committed_at,evidence_index_digest,artifact_id)
			VALUES(?,?,?,?,?,?,?,?,?,?)`,
			input.ItemID, input.InputDigest, input.ResultDigest, input.ProducerType, input.Outcome, input.Category, input.ObservedAt, committedAt, input.EvidenceIndexDigest, input.ArtifactID)
		if err != nil {
			return translateInsert(err, "verification result")
		}
		resultID, err = insert.LastInsertId()
		return err
	})
	if err != nil {
		return 0, false, err
	}
	// Observe drift for the touched object so a drifted subject is marked
	// before anything else reads the invocation.
	_ = service.observeDrifts(ctx, input.InvocationID)
	return resultID, true, nil
}

// ObservationInput is the typed UI observation submission. The result digest
// is generated server-side from the typed fields; the submitter never sends
// a conclusion summary.
type ObservationInput struct {
	InvocationID         int64
	ItemID               int64
	InputDigest          string
	AdminSessionID       int64
	VisualResult         string
	MotionResult         string
	FocusOcclusionResult string
	Note                 *string
}

// SubmitObservation records one typed UI observation bound to the initiating
// Admin Session. It creates the immutable result row and the observation row
// in one transaction; the SQL trigger validates the session, cell freshness
// and result coupling.
func (service *Service) SubmitObservation(ctx context.Context, input ObservationInput) (int64, error) {
	outcome := "passed"
	category := "passed"
	if input.VisualResult != "passed" || input.MotionResult != "passed" || input.FocusOcclusionResult != "passed" {
		outcome = "failed"
		category = "functional_assertion_failed"
	}
	digestFields := map[string]any{
		"itemId": input.ItemID, "inputDigest": input.InputDigest,
		"visualResult": input.VisualResult, "motionResult": input.MotionResult,
		"focusOcclusionResult": input.FocusOcclusionResult, "adminSessionId": input.AdminSessionID,
	}
	if input.Note != nil {
		digestFields["note"] = *input.Note
	}
	resultDigest, err := canonicalDigest(digestFields)
	if err != nil {
		return 0, err
	}
	// Evidence index of an observation is its own typed field set.
	evidenceDigest, err := canonicalDigest(map[string]any{
		"itemId": input.ItemID, "visualResult": input.VisualResult,
		"motionResult": input.MotionResult, "focusOcclusionResult": input.FocusOcclusionResult,
	})
	if err != nil {
		return 0, err
	}
	observedAt := service.now().UTC().Format(time.RFC3339Nano)
	var resultID int64
	var existing bool
	err = service.withConn(ctx, func(conn *sql.Conn) error {
		var invocation int64
		var frozenInput, objectKind string
		err := conn.QueryRowContext(ctx, `SELECT i.invocation_id,i.input_digest,i.object_kind FROM verification_invocation_items i WHERE i.id=?`, input.ItemID).
			Scan(&invocation, &frozenInput, &objectKind)
		if err == sql.ErrNoRows {
			return service.fail(errNotFound, "verification item %d not found", input.ItemID)
		}
		if err != nil {
			return err
		}
		if invocation != input.InvocationID {
			return service.fail(errInvalid, "item %d does not belong to invocation %d", input.ItemID, input.InvocationID)
		}
		if objectKind != "ui_observation" {
			return service.fail(errInvalid, "typed observations only apply to ui_observation items")
		}
		if frozenInput != input.InputDigest {
			return service.fail(errInvalid, "input digest does not match the frozen manifest item")
		}
		// Idempotent retry: same typed fields return the original result.
		err = conn.QueryRowContext(ctx, `SELECT id FROM verification_item_results WHERE item_id=? AND input_digest=? AND result_digest=?`,
			input.ItemID, input.InputDigest, resultDigest).Scan(&resultID)
		if err == nil {
			existing = true
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		committedAt := service.now().UTC().Format(time.RFC3339Nano)
		insert, err := conn.ExecContext(ctx, `INSERT INTO verification_item_results(item_id,input_digest,result_digest,producer_type,outcome,category,observed_at,committed_at,evidence_index_digest)
			VALUES(?,?,?,'admin_observation',?,?,?,?,?)`,
			input.ItemID, input.InputDigest, resultDigest, outcome, category, observedAt, committedAt, evidenceDigest)
		if err != nil {
			return translateInsert(err, "typed observation result")
		}
		resultID, err = insert.LastInsertId()
		if err != nil {
			return err
		}
		var note any
		if input.Note != nil {
			note = *input.Note
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO verification_typed_observations(result_id,admin_session_id,visual_result,motion_result,focus_occlusion_result,note,submitted_at)
			VALUES(?,?,?,?,?,?,?)`, resultID, input.AdminSessionID, input.VisualResult, input.MotionResult, input.FocusOcclusionResult, note, committedAt)
		return err
	})
	if err != nil {
		return 0, err
	}
	if !existing {
		_ = service.observeDrifts(ctx, input.InvocationID)
	}
	return resultID, nil
}
