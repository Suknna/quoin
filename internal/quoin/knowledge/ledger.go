package knowledge

// ledger.go owns the durable client-command ledger discipline for the
// knowledge commands (HTTP-COMMAND-003/004): a committed command records
// the original candidate summary payload so a replay returns the exact
// committed result even after later commands changed the row; a
// deterministic business rejection records outcome=rejected_known with
// enough payload to replay the same rejection. Both land in the same
// short transaction as the domain write and its audit event.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// commandCommandType names the ledger command types.
const (
	ledgerCreate  = "knowledge_candidate.create"
	ledgerEdit    = "knowledge_candidate.draft_edit"
	ledgerConfirm = "knowledge_candidate.confirm"
	ledgerExclude = "knowledge_candidate.exclude"
)

// rejectionPayload captures a deterministic rejection for replay.
type rejectionPayload struct {
	Kind        string `json:"kind"`
	Current     int64  `json:"current,omitempty"`
	State       string `json:"state,omitempty"`
	Status      int    `json:"status"`
	Description string `json:"description"`
}

// commitCandidateCommand records the committed outcome with its payload
// and commits the caller's transaction.
func commitCandidateCommand(ctx context.Context, conn *sql.Conn, principalID int64, commandID, commandType, digest string, candidateID int64, summary CandidateSummary) error {
	payload, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	if err := recordCommand(ctx, conn, principalID, commandID, commandType, digest, authOutcomeCommitted, "knowledge_candidate", candidateID, string(payload)); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	return nil
}

// rejectCandidateCommand records the deterministic rejection and commits;
// the original domain error is returned unchanged for the handler mapping.
func rejectCandidateCommand(ctx context.Context, conn *sql.Conn, principalID int64, commandID, commandType, digest string, candidateID int64, rejection error) error {
	payload := rejectionPayload{Status: rejectionStatus(rejection), Description: rejection.Error()}
	var revision *RevisionConflict
	var rowVersion *RowVersionConflict
	var state *StateConflict
	switch {
	case errors.As(rejection, &revision):
		payload.Kind = "revision_conflict"
		payload.Current = revision.Current
	case errors.As(rejection, &rowVersion):
		payload.Kind = "row_version_conflict"
		payload.Current = rowVersion.Current
	case errors.As(rejection, &state):
		payload.Kind = "state_conflict"
		payload.State = state.State
	case errors.Is(rejection, ErrSourceRejected):
		payload.Kind = "source_rejected"
	case errors.Is(rejection, ErrSourceShape):
		payload.Kind = "source_shape"
	case errors.Is(rejection, ErrNotFound):
		payload.Kind = "not_found"
	case errors.Is(rejection, ErrEmptyEdit):
		payload.Kind = "empty_edit"
	default:
		payload.Kind = "unknown"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var objectID int64
	if candidateID > 0 {
		objectID = candidateID
	}
	if err := recordCommand(ctx, conn, principalID, commandID, commandType, digest, authOutcomeRejected, "knowledge_candidate", objectID, string(body)); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	return rejection
}

func rejectionStatus(rejection error) int {
	var revision *RevisionConflict
	var rowVersion *RowVersionConflict
	var state *StateConflict
	switch {
	case errors.As(rejection, &revision), errors.As(rejection, &rowVersion), errors.As(rejection, &state), errors.Is(rejection, ErrSourceRejected):
		return 409
	case errors.Is(rejection, ErrNotFound):
		return 404
	default:
		return 422
	}
}

// replayCandidateOutcome resolves a ledger hit: a committed record returns
// its original summary; a rejected_known record reconstructs the same
// typed rejection (HTTP-COMMAND-003/004).
func replayCandidateOutcome(record authRecord, digest string) (CandidateSummary, bool, error) {
	if record.RequestDigest != digest || record.ResultObjectType != "knowledge_candidate" {
		return CandidateSummary{}, false, ErrCommandReused
	}
	switch record.Outcome {
	case authOutcomeCommitted:
		var summary CandidateSummary
		if err := json.Unmarshal([]byte(record.ResultPayload), &summary); err != nil {
			return CandidateSummary{}, false, err
		}
		return summary, true, nil
	case authOutcomeRejected:
		return CandidateSummary{}, false, replayRejection(record.ResultPayload)
	default:
		return CandidateSummary{}, false, ErrCommandReused
	}
}

func replayRejection(payload string) error {
	var recorded rejectionPayload
	if err := json.Unmarshal([]byte(payload), &recorded); err != nil {
		return err
	}
	switch recorded.Kind {
	case "revision_conflict":
		return &RevisionConflict{Current: recorded.Current}
	case "row_version_conflict":
		return &RowVersionConflict{Current: recorded.Current}
	case "state_conflict":
		return &StateConflict{State: recorded.State}
	case "source_rejected":
		return ErrSourceRejected
	case "source_shape":
		return ErrSourceShape
	case "not_found":
		return ErrNotFound
	case "empty_edit":
		return ErrEmptyEdit
	default:
		return fmt.Errorf("recorded rejection: %s", recorded.Description)
	}
}
