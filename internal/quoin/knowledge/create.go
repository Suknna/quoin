package knowledge

// create.go implements create-or-return for the three diagnosis sources
// (DATA-KNOWLEDGE-006): the command first looks for an existing candidate
// of the same immutable source and returns it (200); only when none
// exists does it validate the source state (a rejected source can no
// longer create) and insert the new AwaitingConfirmation row with the
// frozen original suggestion projection.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	return service.CreateFromAnalysisOutputAs(ctx, MutationActor{ID: principalID}, commandID, occurrenceID, analysisID)
}

func (service *Service) CreateFromAnalysisOutputAs(ctx context.Context, actor MutationActor, commandID string, occurrenceID, analysisID int64) (CreateResult, error) {
	principalID := actor.ID
	// The digest covers the semantic request fields; the resolved source is
	// deterministic from them (HTTP-COMMAND-002).
	digest := commandDigest(ledgerCreate, map[string]any{"sourceType": SourceAnalysisOutput, "occurrenceId": occurrenceID, "analysisId": analysisID})
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return CreateResult{}, err
	}
	defer conn.Close()
	var outputOccurrence int64
	var outputID int64
	var modelID, content, createdAt string
	err = conn.QueryRowContext(ctx, `
		SELECT a.occurrence_id, o.id, o.model_id, o.content, o.created_at
		FROM initial_analysis_outputs o JOIN initial_analyses a ON a.id=o.analysis_id
		WHERE o.analysis_id=?`, analysisID).Scan(&outputOccurrence, &outputID, &modelID, &content, &createdAt)
	if err == nil && outputOccurrence != occurrenceID {
		// The output exists but belongs to another occurrence: the path is
		// not this object's home.
		err = sql.ErrNoRows
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CreateResult{}, rejectCreateNotFound(ctx, conn, principalID, commandID, digest)
		}
		return CreateResult{}, err
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
	return service.createOrReturn(ctx, conn, actor, commandID, digest, SourceAnalysisOutput, outputID, suggestion)
}

// CreateFromInvestigationMessage validates the active assistant message
// belongs to the investigation and creates or returns its candidate.
func (service *Service) CreateFromInvestigationMessage(ctx context.Context, principalID int64, commandID string, investigationID, messageID int64) (CreateResult, error) {
	return service.CreateFromInvestigationMessageAs(ctx, MutationActor{ID: principalID}, commandID, investigationID, messageID)
}

func (service *Service) CreateFromInvestigationMessageAs(ctx context.Context, actor MutationActor, commandID string, investigationID, messageID int64) (CreateResult, error) {
	principalID := actor.ID
	digest := commandDigest(ledgerCreate, map[string]any{"sourceType": SourceMessage, "investigationId": investigationID, "sourceId": messageID})
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return CreateResult{}, err
	}
	defer conn.Close()
	var messageInvestigation int64
	var role, status, content, createdAt string
	err = conn.QueryRowContext(ctx, `
		SELECT m.investigation_id, m.role, m.status, m.content, m.created_at
		FROM investigation_messages m WHERE m.id=?`, messageID).Scan(&messageInvestigation, &role, &status, &content, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CreateResult{}, rejectCreateNotFound(ctx, conn, principalID, commandID, digest)
		}
		return CreateResult{}, err
	}
	if messageInvestigation != investigationID {
		return CreateResult{}, rejectCreateNotFound(ctx, conn, principalID, commandID, digest)
	}
	if role != "assistant" || status != "active" {
		return CreateResult{}, rejectCreateShape(ctx, conn, principalID, commandID, digest)
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
	return service.createOrReturn(ctx, conn, actor, commandID, digest, SourceMessage, messageID, suggestion)
}

// CreateFromReport resolves the run's immutable report version and
// creates or returns its candidate.
func (service *Service) CreateFromReport(ctx context.Context, principalID int64, commandID string, runID, reportVersion int64) (CreateResult, error) {
	return service.CreateFromReportAs(ctx, MutationActor{ID: principalID}, commandID, runID, reportVersion)
}

func (service *Service) CreateFromReportAs(ctx context.Context, actor MutationActor, commandID string, runID, reportVersion int64) (CreateResult, error) {
	principalID := actor.ID
	digest := commandDigest(ledgerCreate, map[string]any{"sourceType": SourceReport, "runId": runID, "reportVersion": reportVersion})
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return CreateResult{}, err
	}
	defer conn.Close()
	var reportID int64
	var modelID, content, createdAt string
	err = conn.QueryRowContext(ctx, `
		SELECT r.id, r.model_id, r.content, r.created_at
		FROM inspection_reports r WHERE r.run_id=? AND r.version=?`, runID, reportVersion).Scan(&reportID, &modelID, &content, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CreateResult{}, rejectCreateNotFound(ctx, conn, principalID, commandID, digest)
		}
		return CreateResult{}, err
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
	return service.createOrReturn(ctx, conn, actor, commandID, digest, SourceReport, reportID, suggestion)
}

// createOrReturn is the shared create-or-return core: dedupe first (the
// partial unique index is the authority), reject-check the source only
// for a genuinely new candidate, then insert with the frozen suggestion.
func (service *Service) createOrReturn(ctx context.Context, conn *sql.Conn, actor MutationActor, commandID, digest, sourceType string, sourceID int64, suggestion Suggestion) (CreateResult, error) {
	principalID := actor.ID
	if record, ok, err := authLookup(ctx, conn, principalID, commandID); err != nil {
		return CreateResult{}, err
	} else if ok {
		return replayCreateOutcome(record, digest)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return CreateResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if err := verifyMutationActorOn(ctx, conn, actor); err != nil {
		return CreateResult{}, err
	}
	if record, ok, err := authLookup(ctx, conn, principalID, commandID); err != nil {
		return CreateResult{}, err
	} else if ok {
		result, replayErr := replayCreateOutcome(record, digest)
		if replayErr == nil {
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				return CreateResult{}, err
			}
			committed = true
		}
		return result, replayErr
	}
	// Dedupe first: any-state existing candidate of this source returns
	// (the deterministic create-or-return outcome is ledger-recorded).
	if existing, found, err := service.candidateBySource(ctx, conn, sourceType, sourceID); err != nil {
		return CreateResult{}, err
	} else if found {
		existingID, parseErr := strconv.ParseInt(existing.ID, 10, 64)
		if parseErr != nil {
			return CreateResult{}, parseErr
		}
		if err := service.finishCreate(ctx, conn, principalID, commandID, digest, existingID, existing, false); err != nil {
			return CreateResult{}, err
		}
		return CreateResult{Candidate: existing, Created: false}, nil
	}
	// Only a new candidate validates the source state.
	var rejected int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM diagnosis_feedback
		WHERE target_type=? AND target_id=? AND value='rejected'`, sourceType, sourceID).Scan(&rejected); err != nil {
		return CreateResult{}, err
	}
	if rejected > 0 {
		return CreateResult{}, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerCreate, digest, 0, ErrSourceRejected)
	}
	projection, err := suggestionJSON(suggestion)
	if err != nil {
		return CreateResult{}, err
	}
	now := service.nowText()
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO knowledge_candidates(source_type,source_id,generation,state,original_suggestion_json,draft_title,draft_body,draft_revision,created_by,created_at)
		VALUES(?,?,1,?,?,?,?,0,?,?)`,
		sourceType, sourceID, StateAwaiting, projection, suggestion.Title, suggestion.Body, principalID, now)
	if err != nil {
		return CreateResult{}, err
	}
	candidateID, err := insert.LastInsertId()
	if err != nil {
		return CreateResult{}, err
	}
	if err := recordAudit(ctx, conn, principalID, commandID, ledgerCreate, sourceType, sourceID, nil, now); err != nil {
		return CreateResult{}, err
	}
	summary, err := scanCandidateOn(ctx, conn, candidateID)
	if err != nil {
		return CreateResult{}, err
	}
	if err := service.finishCreate(ctx, conn, principalID, commandID, digest, candidateID, summary, true); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Candidate: summary, Created: true}, nil
}

// finishCreate records the ledger row (carrying the original outcome
// payload) and commits the transaction.
func (service *Service) finishCreate(ctx context.Context, conn *sql.Conn, principalID int64, commandID, digest string, candidateID int64, summary CandidateSummary, created bool) error {
	payload, err := json.Marshal(createReplayPayload{Summary: summary, Created: created})
	if err != nil {
		return err
	}
	if err := recordCommand(ctx, conn, principalID, commandID, ledgerCreate, digest, authOutcomeCommitted, "knowledge_candidate", candidateID, string(payload)); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	return nil
}

// replayCreateOutcome replays a create command's recorded outcome: the
// committed create-or-return result verbatim, or the recorded rejection.
func replayCreateOutcome(record authRecord, digest string) (CreateResult, error) {
	if record.RequestDigest != digest || record.ResultObjectType != "knowledge_candidate" {
		return CreateResult{}, ErrCommandReused
	}
	switch record.Outcome {
	case authOutcomeCommitted:
		var payload createReplayPayload
		if err := json.Unmarshal([]byte(record.ResultPayload), &payload); err != nil {
			return CreateResult{}, err
		}
		return CreateResult{Candidate: payload.Summary, Created: payload.Created}, nil
	case authOutcomeRejected:
		return CreateResult{}, replayRejection(record.ResultPayload)
	default:
		return CreateResult{}, ErrCommandReused
	}
}

// rejectCreateNotFound records the deterministic 404 in a short
// transaction and returns the mapped error.
func rejectCreateNotFound(ctx context.Context, conn *sql.Conn, principalID int64, commandID, digest string) error {
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	return rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerCreate, digest, 0, ErrNotFound)
}

// rejectCreateShape records the deterministic 422 in a short transaction
// and returns the mapped error.
func rejectCreateShape(ctx context.Context, conn *sql.Conn, principalID int64, commandID, digest string) error {
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	return rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerCreate, digest, 0, ErrSourceShape)
}
