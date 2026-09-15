package knowledge

// create.go implements create-or-return for the three diagnosis sources
// (DATA-KNOWLEDGE-006): the command first looks for an existing candidate
// of the same immutable source and returns it (200); only when none
// exists does it validate the source state (a rejected source can no
// longer create) and insert the new AwaitingConfirmation row with the
// frozen original suggestion projection. The whole decision runs inside
// the shared runner transaction: replay, rejections, ledger and audit are
// the runner's.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// CreateResult reports the candidate plus whether this command created it
// (201) or returned an existing record (200).
type CreateResult struct {
	Candidate CandidateSummary
	Created   bool
}

// CreateFromAnalysisOutput resolves the analysis' sealed first success
// output and creates or returns its candidate.
func (service *Service) CreateFromAnalysisOutput(ctx context.Context, principalID int64, commandID string, occurrenceID, analysisID int64) (CreateResult, error) {
	digest := commandDigest(opCreate, map[string]any{"sourceType": SourceAnalysisOutput, "occurrenceId": occurrenceID, "analysisId": analysisID})
	return service.runCreate(ctx, principalID, commandID, digest, SourceAnalysisOutput, func(tx *execution.Tx) (int64, Suggestion, *execution.Rejection, error) {
		var outputOccurrence, outputID int64
		var modelID, content, createdAt string
		err := tx.QueryRowContext(ctx, `
			SELECT a.occurrence_id, o.id, o.model_id, o.content, o.created_at
			FROM initial_analysis_outputs o JOIN initial_analyses a ON a.id=o.analysis_id
			WHERE o.analysis_id=?`, analysisID).Scan(&outputOccurrence, &outputID, &modelID, &content, &createdAt)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && outputOccurrence != occurrenceID) {
			// The output is missing or belongs to another occurrence: the
			// path is not this object's home.
			return 0, Suggestion{}, &execution.Rejection{Code: codeNotFound, Detail: ErrNotFound.Error()}, nil
		}
		if err != nil {
			return 0, Suggestion{}, nil, err
		}
		suggestion := Suggestion{
			V: suggestionVersion,
			Source: suggestionSource{
				Type:      SourceAnalysisOutput,
				ID:        fmt.Sprintf("%d", outputID),
				ModelID:   modelID,
				CreatedAt: createdAt,
				Locator:   map[string]any{"occurrenceId": occurrenceID, "analysisId": analysisID},
			},
			Title: deriveTitle(content),
			Body:  content,
		}
		return outputID, suggestion, nil, nil
	})
}

// CreateFromInvestigationMessage validates the active assistant message
// belongs to the investigation and creates or returns its candidate.
func (service *Service) CreateFromInvestigationMessage(ctx context.Context, principalID int64, commandID string, investigationID, messageID int64) (CreateResult, error) {
	digest := commandDigest(opCreate, map[string]any{"sourceType": SourceMessage, "investigationId": investigationID, "sourceId": messageID})
	return service.runCreate(ctx, principalID, commandID, digest, SourceMessage, func(tx *execution.Tx) (int64, Suggestion, *execution.Rejection, error) {
		var messageInvestigation int64
		var role, status, content, createdAt string
		err := tx.QueryRowContext(ctx, `
			SELECT m.investigation_id, m.role, m.status, m.content, m.created_at
			FROM investigation_messages m WHERE m.id=?`, messageID).Scan(&messageInvestigation, &role, &status, &content, &createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, Suggestion{}, &execution.Rejection{Code: codeNotFound, Detail: ErrNotFound.Error()}, nil
		}
		if err != nil {
			return 0, Suggestion{}, nil, err
		}
		if messageInvestigation != investigationID {
			return 0, Suggestion{}, &execution.Rejection{Code: codeNotFound, Detail: ErrNotFound.Error()}, nil
		}
		if role != "assistant" || status != "active" {
			return 0, Suggestion{}, &execution.Rejection{Code: codeSourceShape, Detail: ErrSourceShape.Error()}, nil
		}
		suggestion := Suggestion{
			V: suggestionVersion,
			Source: suggestionSource{
				Type:    SourceMessage,
				ID:      fmt.Sprintf("%d", messageID),
				Locator: map[string]any{"investigationId": investigationID},
			},
			Title: deriveTitle(content),
			Body:  content,
		}
		return messageID, suggestion, nil, nil
	})
}

// CreateFromReport resolves the run's immutable report version and
// creates or returns its candidate.
func (service *Service) CreateFromReport(ctx context.Context, principalID int64, commandID string, runID, reportVersion int64) (CreateResult, error) {
	digest := commandDigest(opCreate, map[string]any{"sourceType": SourceReport, "runId": runID, "reportVersion": reportVersion})
	return service.runCreate(ctx, principalID, commandID, digest, SourceReport, func(tx *execution.Tx) (int64, Suggestion, *execution.Rejection, error) {
		var reportID int64
		var modelID, content, createdAt string
		err := tx.QueryRowContext(ctx, `
			SELECT r.id, r.model_id, r.content, r.created_at
			FROM inspection_reports r WHERE r.run_id=? AND r.version=?`, runID, reportVersion).Scan(&reportID, &modelID, &content, &createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, Suggestion{}, &execution.Rejection{Code: codeNotFound, Detail: ErrNotFound.Error()}, nil
		}
		if err != nil {
			return 0, Suggestion{}, nil, err
		}
		suggestion := Suggestion{
			V: suggestionVersion,
			Source: suggestionSource{
				Type:      SourceReport,
				ID:        fmt.Sprintf("%d", reportID),
				ModelID:   modelID,
				CreatedAt: createdAt,
				Locator:   map[string]any{"runId": runID, "reportVersion": reportVersion},
			},
			Title: deriveTitle(content),
			Body:  content,
		}
		return reportID, suggestion, nil, nil
	})
}

// runCreate is the shared runner assembly of the create-or-return command:
// dedupe first (the partial unique index is the authority), reject-check the
// source only for a genuinely new candidate, then insert with the frozen
// suggestion. The replay payload keeps the frozen {summary, created} shape
// so pre-migration ledger rows still replay exactly.
func (service *Service) runCreate(ctx context.Context, principalID int64, commandID, digest, sourceType string, resolve func(tx *execution.Tx) (int64, Suggestion, *execution.Rejection, error)) (CreateResult, error) {
	outcome, err := execution.Run(ctx, service.runner, service.create, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: commandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (createReplayPayload, execution.Change, error) {
		sourceID, suggestion, rejection, err := resolve(tx)
		if err != nil {
			return createReplayPayload{}, execution.Changed, err
		}
		if rejection != nil {
			return createReplayPayload{}, execution.Changed, rejection
		}
		// Dedupe first: any-state existing candidate of this source returns
		// (the deterministic create-or-return outcome is ledger-recorded).
		if existing, found, err := service.candidateBySource(ctx, tx, sourceType, sourceID); err != nil {
			return createReplayPayload{}, execution.Changed, err
		} else if found {
			return createReplayPayload{Summary: existing, Created: false}, execution.Changed, nil
		}
		// Only a new candidate validates the source state.
		var rejected int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM diagnosis_feedback
			WHERE target_type=? AND target_id=? AND value='rejected'`, sourceType, sourceID).Scan(&rejected); err != nil {
			return createReplayPayload{}, execution.Changed, err
		}
		if rejected > 0 {
			return createReplayPayload{}, execution.Changed, &execution.Rejection{Code: codeSourceRejected, Detail: ErrSourceRejected.Error()}
		}
		projection, err := suggestionJSON(suggestion)
		if err != nil {
			return createReplayPayload{}, execution.Changed, err
		}
		now := service.nowText()
		insert, err := tx.ExecContext(ctx, `
			INSERT INTO knowledge_candidates(source_type,source_id,generation,state,original_suggestion_json,draft_title,draft_body,draft_revision,created_by,created_at)
			VALUES(?,?,1,?,?,?,?,0,?,?)`,
			sourceType, sourceID, StateAwaiting, projection, suggestion.Title, suggestion.Body, principalID, now)
		if err != nil {
			return createReplayPayload{}, execution.Changed, err
		}
		candidateID, err := insert.LastInsertId()
		if err != nil {
			return createReplayPayload{}, execution.Changed, err
		}
		summary, err := scanCandidateOn(ctx, tx, candidateID)
		if err != nil {
			return createReplayPayload{}, execution.Changed, err
		}
		return createReplayPayload{Summary: summary, Created: true}, execution.Changed, nil
	}, func(payload createReplayPayload) int64 {
		if payload.Summary.ID == "" {
			return 0
		}
		id, _ := strconv.ParseInt(payload.Summary.ID, 10, 64)
		return id
	})
	if err != nil {
		return CreateResult{}, service.translateCommandError(ctx, err)
	}
	return CreateResult{Candidate: outcome.Result.Summary, Created: outcome.Result.Created}, nil
}
