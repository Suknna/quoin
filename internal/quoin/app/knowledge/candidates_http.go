package knowledge

// candidates_http.go carries the knowledge-candidate and knowledge read
// routes of the T27 surface: create-or-return from the three diagnosis
// sources, the revisioned draft edit, the human confirmation boundary,
// exclusion, and the browse/version projections.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Suknna/quoin/internal/quoin/knowledge"
)

// candidateBody carries the per-response status in the Status field huma
// reads from output structs (200 create-or-return vs 201 created).
type candidateBody struct {
	Status int
	Body   knowledge.CandidateSummary
}

// createOutcome maps one create-or-return result onto the frozen status
// pair (201 created / 200 returned) or the deterministic problems.
func createOutcome(err error, created bool) (int, *problemError) {
	if err != nil {
		switch {
		case errors.Is(err, knowledge.ErrNotFound):
			return 0, problem(404, "not_found", "来源对象不存在。")
		case errors.Is(err, knowledge.ErrSourceShape):
			return 0, problemUnprocessable("只有不可变的助手结论可以整理为知识。")
		case errors.Is(err, knowledge.ErrSourceRejected):
			return 0, problem(409, "active_conflict", "该诊断已标记为不采纳，不能再整理为知识。")
		case errors.Is(err, knowledge.ErrCommandReused):
			return 0, commandReusedProblem()
		}
		return 0, problem(500, "unavailable", "暂时无法整理为知识，请重试。")
	}
	if created {
		return http.StatusCreated, nil
	}
	return http.StatusOK, nil
}

func (handler *Handler) createAnalysisKnowledgeCandidate(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	OccurrenceID string `path:"occurrenceId"`
	AnalysisID   string `path:"analysisId"`
	Body         struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	}
}) (*candidateBody, error) {
	principalID, authProblem := handler.user(ctx, input.Session)
	if authProblem != nil {
		return nil, authProblem
	}
	occurrenceID, idProblem := parseID(input.OccurrenceID)
	if idProblem != nil {
		return nil, idProblem
	}
	analysisID, idProblem := parseID(input.AnalysisID)
	if idProblem != nil {
		return nil, idProblem
	}
	result, err := handler.Knowledge.CreateFromAnalysisOutput(ctx, principalID, input.Body.ClientCommandID, occurrenceID, analysisID)
	status, outcomeProblem := createOutcome(err, result.Created)
	if outcomeProblem != nil {
		return nil, outcomeProblem
	}
	return &candidateBody{Status: status, Body: result.Candidate}, nil
}

func (handler *Handler) createInvestigationKnowledgeCandidate(ctx context.Context, input *struct {
	Session         string `cookie:"__Host-quoin-session"`
	InvestigationID string `path:"investigationId"`
	Body            struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		SourceType      string `json:"sourceType" enum:"initial_analysis_output,inspection_report,investigation_message"`
		SourceID        string `json:"sourceId"`
	}
}) (*candidateBody, error) {
	principalID, authProblem := handler.user(ctx, input.Session)
	if authProblem != nil {
		return nil, authProblem
	}
	investigationID, idProblem := parseID(input.InvestigationID)
	if idProblem != nil {
		return nil, idProblem
	}
	sourceID, idProblem := parseID(input.Body.SourceID)
	if idProblem != nil {
		return nil, idProblem
	}
	if input.Body.SourceType != knowledge.SourceMessage {
		return nil, problemUnprocessable("调查来源必须是 assistant 消息。")
	}
	result, err := handler.Knowledge.CreateFromInvestigationMessage(ctx, principalID, input.Body.ClientCommandID, investigationID, sourceID)
	status, outcomeProblem := createOutcome(err, result.Created)
	if outcomeProblem != nil {
		return nil, outcomeProblem
	}
	return &candidateBody{Status: status, Body: result.Candidate}, nil
}

func (handler *Handler) createReportKnowledgeCandidate(ctx context.Context, input *struct {
	Session       string `cookie:"__Host-quoin-session"`
	RunID         string `path:"runId"`
	ReportVersion string `path:"reportVersion"`
	Body          struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	}
}) (*candidateBody, error) {
	principalID, authProblem := handler.user(ctx, input.Session)
	if authProblem != nil {
		return nil, authProblem
	}
	runID, idProblem := parseID(input.RunID)
	if idProblem != nil {
		return nil, idProblem
	}
	reportVersion, idProblem := parseID(input.ReportVersion)
	if idProblem != nil {
		return nil, idProblem
	}
	result, err := handler.Knowledge.CreateFromReport(ctx, principalID, input.Body.ClientCommandID, runID, reportVersion)
	status, outcomeProblem := createOutcome(err, result.Created)
	if outcomeProblem != nil {
		return nil, outcomeProblem
	}
	return &candidateBody{Status: status, Body: result.Candidate}, nil
}

func candidateCommandProblem(err error) *problemError {
	var revision *knowledge.RevisionConflict
	var rowVersion *knowledge.RowVersionConflict
	var state *knowledge.StateConflict
	switch {
	case errors.Is(err, knowledge.ErrNotFound):
		return problem(404, "not_found", "候选不存在。")
	case errors.Is(err, knowledge.ErrCommandReused):
		return commandReusedProblem()
	case errors.Is(err, knowledge.ErrEmptyEdit):
		return problemUnprocessable("至少修改标题、正文或适用范围。")
	case errors.Is(err, knowledge.ErrInvalidScope):
		return problemUnprocessable("适用范围必须是键值对象。")
	case errors.As(err, &revision):
		conflict := problem(409, "row_version_conflict", "草稿已被更新，请基于最新版本修改。")
		conflict.Conflict = map[string]any{"code": "row_version_conflict", "currentRevision": revision.Current}
		return conflict
	case errors.As(err, &rowVersion):
		conflict := problem(409, "row_version_conflict", "候选已被更新，请刷新后重试。")
		conflict.Conflict = map[string]any{"code": "row_version_conflict", "currentRowVersion": rowVersion.Current}
		return conflict
	case errors.As(err, &state):
		return stateConflictProblem(state.State)
	}
	return problem(500, "unavailable", "暂时无法完成操作，请重试。")
}

type candidatePageBody struct {
	Body struct {
		Items      []knowledge.CandidateSummary `json:"items"`
		NextCursor string                       `json:"nextCursor,omitempty"`
	} `json:"body"`
}

func (handler *Handler) listKnowledgeCandidates(ctx context.Context, input *struct {
	Session    string `cookie:"__Host-quoin-session"`
	State      string `query:"state" enum:"AwaitingConfirmation,Confirmed,Excluded,Superseded,SourceInvalid"`
	SourceType string `query:"sourceType" enum:"initial_analysis_output,inspection_report,investigation_message,source_material,knowledge_version"`
	Cursor     string `query:"cursor"`
	Limit      int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}) (*candidatePageBody, error) {
	if _, authProblem := handler.user(ctx, input.Session); authProblem != nil {
		return nil, authProblem
	}
	filter := knowledge.ListFilter{State: input.State, SourceType: input.SourceType}
	var after *knowledge.CandidateCursor
	if input.Cursor != "" {
		var cursor knowledge.CandidateCursor
		if cursorProblem := decodeCursor(input.Cursor, &cursor); cursorProblem != nil {
			return nil, cursorProblem
		}
		// The cursor binds the active filters (HTTP-PAGE-001).
		if cursor.Filter != filter {
			return nil, problem(400, "malformed_request", "分页游标与当前筛选不一致。")
		}
		after = &cursor
	}
	items, next, err := handler.Knowledge.ListCandidates(ctx, filter, after, input.Limit)
	if err != nil {
		return nil, problem(500, "unavailable", "暂时无法读取知识候选。")
	}
	output := &candidatePageBody{}
	output.Body.Items = items
	if next != nil {
		next.Filter = filter
		output.Body.NextCursor = encodeCursor(*next)
	}
	return output, nil
}

func (handler *Handler) getKnowledgeCandidate(ctx context.Context, input *struct {
	Session     string `cookie:"__Host-quoin-session"`
	CandidateID string `path:"candidateId"`
}) (*struct {
	Body knowledge.CandidateDetail `json:"body"`
}, error) {
	if _, authProblem := handler.user(ctx, input.Session); authProblem != nil {
		return nil, authProblem
	}
	candidateID, idProblem := parseID(input.CandidateID)
	if idProblem != nil {
		return nil, idProblem
	}
	detail, err := handler.Knowledge.GetCandidate(ctx, candidateID)
	if err != nil {
		if errors.Is(err, knowledge.ErrNotFound) {
			return nil, problem(404, "not_found", "候选不存在。")
		}
		return nil, problem(500, "unavailable", "暂时无法读取知识候选。")
	}
	return &struct {
		Body knowledge.CandidateDetail `json:"body"`
	}{Body: detail}, nil
}

func (handler *Handler) editKnowledgeCandidateDraft(ctx context.Context, input *struct {
	Session     string `cookie:"__Host-quoin-session"`
	CandidateID string `path:"candidateId"`
	Body        struct {
		ClientCommandID  string           `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		ExpectedRevision int64            `json:"expectedRevision" minimum:"0"`
		Title            *string          `json:"title,omitempty"`
		Body             *string          `json:"body,omitempty"`
		Scope            *json.RawMessage `json:"scope,omitempty"`
	}
}) (*candidateBody, error) {
	principalID, authProblem := handler.user(ctx, input.Session)
	if authProblem != nil {
		return nil, authProblem
	}
	candidateID, idProblem := parseID(input.CandidateID)
	if idProblem != nil {
		return nil, idProblem
	}
	summary, err := handler.Knowledge.EditDraft(ctx, principalID, input.Body.ClientCommandID, candidateID, input.Body.ExpectedRevision, input.Body.Title, input.Body.Body, input.Body.Scope)
	if err != nil {
		return nil, candidateCommandProblem(err)
	}
	return &candidateBody{Status: http.StatusOK, Body: summary}, nil
}

func (handler *Handler) confirmKnowledgeCandidate(ctx context.Context, input *struct {
	Session     string `cookie:"__Host-quoin-session"`
	CandidateID string `path:"candidateId"`
	Body        struct {
		ClientCommandID  string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		ExpectedRevision int64  `json:"expectedRevision" minimum:"0"`
	}
}) (*candidateBody, error) {
	principalID, authProblem := handler.user(ctx, input.Session)
	if authProblem != nil {
		return nil, authProblem
	}
	candidateID, idProblem := parseID(input.CandidateID)
	if idProblem != nil {
		return nil, idProblem
	}
	summary, err := handler.Knowledge.Confirm(ctx, principalID, input.Body.ClientCommandID, candidateID, input.Body.ExpectedRevision)
	if err != nil {
		return nil, candidateCommandProblem(err)
	}
	return &candidateBody{Status: http.StatusOK, Body: summary}, nil
}

func (handler *Handler) excludeKnowledgeCandidate(ctx context.Context, input *struct {
	Session     string `cookie:"__Host-quoin-session"`
	CandidateID string `path:"candidateId"`
	Body        struct {
		ClientCommandID    string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
	}
}) (*candidateBody, error) {
	principalID, authProblem := handler.user(ctx, input.Session)
	if authProblem != nil {
		return nil, authProblem
	}
	candidateID, idProblem := parseID(input.CandidateID)
	if idProblem != nil {
		return nil, idProblem
	}
	summary, err := handler.Knowledge.Exclude(ctx, principalID, input.Body.ClientCommandID, candidateID, input.Body.ExpectedRowVersion)
	if err != nil {
		return nil, candidateCommandProblem(err)
	}
	return &candidateBody{Status: http.StatusOK, Body: summary}, nil
}

// searchBody carries the pre-marshaled envelope: the two frozen response
// shapes share one route, so the raw body passthrough keeps them disjoint.
type searchBody struct {
	Status int
	Body   json.RawMessage
}

func rawSearchBody(envelope any) (*searchBody, error) {
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, problem(500, "unavailable", "暂时无法读取知识。")
	}
	return &searchBody{Status: http.StatusOK, Body: body}, nil
}

type queryEnvelope struct {
	Mode             string                `json:"mode"`
	ExactTextMatches []knowledge.SearchHit `json:"exactTextMatches"`
	SemanticMatches  []knowledge.SearchHit `json:"semanticMatches"`
	NextCursor       string                `json:"nextCursor,omitempty"`
}

type browseEnvelope struct {
	Mode       string                       `json:"mode"`
	Items      []knowledge.KnowledgeSummary `json:"items"`
	NextCursor string                       `json:"nextCursor,omitempty"`
}

func (handler *Handler) searchKnowledge(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Q       string `query:"q"`
	Cursor  string `query:"cursor"`
	Limit   int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}) (*searchBody, error) {
	if _, authProblem := handler.user(ctx, input.Session); authProblem != nil {
		return nil, authProblem
	}
	if input.Q != "" {
		var after *knowledge.QueryCursor
		if input.Cursor != "" {
			var cursor knowledge.QueryCursor
			if cursorProblem := decodeCursor(input.Cursor, &cursor); cursorProblem != nil {
				return nil, cursorProblem
			}
			if cursor.Query != input.Q {
				return nil, problem(400, "malformed_request", "分页游标与当前检索不一致。")
			}
			after = &cursor
		}
		result, next, err := handler.Knowledge.Search(ctx, input.Q, after, input.Limit)
		if err != nil {
			return nil, problem(500, "unavailable", "暂时无法检索知识。")
		}
		envelope := queryEnvelope{Mode: "query", ExactTextMatches: result.ExactTextMatches, SemanticMatches: result.SemanticMatches}
		if next != nil {
			envelope.NextCursor = encodeCursor(*next)
		}
		return rawSearchBody(envelope)
	}
	var after *knowledge.KnowledgeCursor
	if input.Cursor != "" {
		var cursor knowledge.KnowledgeCursor
		if cursorProblem := decodeCursor(input.Cursor, &cursor); cursorProblem != nil {
			return nil, cursorProblem
		}
		after = &cursor
	}
	items, next, err := handler.Knowledge.Browse(ctx, after, input.Limit)
	if err != nil {
		return nil, problem(500, "unavailable", "暂时无法读取知识。")
	}
	envelope := browseEnvelope{Mode: "browse", Items: items}
	if next != nil {
		envelope.NextCursor = encodeCursor(*next)
	}
	return rawSearchBody(envelope)
}

func (handler *Handler) getKnowledge(ctx context.Context, input *struct {
	Session     string `cookie:"__Host-quoin-session"`
	KnowledgeID string `path:"knowledgeId"`
}) (*struct {
	Body knowledge.KnowledgeDetail `json:"body"`
}, error) {
	if _, authProblem := handler.user(ctx, input.Session); authProblem != nil {
		return nil, authProblem
	}
	knowledgeID, idProblem := parseID(input.KnowledgeID)
	if idProblem != nil {
		return nil, idProblem
	}
	detail, err := handler.Knowledge.GetKnowledge(ctx, knowledgeID)
	if err != nil {
		if errors.Is(err, knowledge.ErrNotFound) {
			return nil, problem(404, "not_found", "知识不存在。")
		}
		return nil, problem(500, "unavailable", "暂时无法读取知识。")
	}
	return &struct {
		Body knowledge.KnowledgeDetail `json:"body"`
	}{Body: detail}, nil
}

type versionPageBody struct {
	Body struct {
		Items      []knowledge.VersionSummary `json:"items"`
		NextCursor string                     `json:"nextCursor,omitempty"`
	} `json:"body"`
}

func (handler *Handler) listKnowledgeVersions(ctx context.Context, input *struct {
	Session     string `cookie:"__Host-quoin-session"`
	KnowledgeID string `path:"knowledgeId"`
	Cursor      string `query:"cursor"`
	Limit       int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}) (*versionPageBody, error) {
	if _, authProblem := handler.user(ctx, input.Session); authProblem != nil {
		return nil, authProblem
	}
	knowledgeID, idProblem := parseID(input.KnowledgeID)
	if idProblem != nil {
		return nil, idProblem
	}
	var after *knowledge.VersionCursor
	if input.Cursor != "" {
		var cursor knowledge.VersionCursor
		if cursorProblem := decodeCursor(input.Cursor, &cursor); cursorProblem != nil {
			return nil, cursorProblem
		}
		after = &cursor
	}
	items, next, err := handler.Knowledge.ListVersions(ctx, knowledgeID, after, input.Limit)
	if err != nil {
		return nil, problem(500, "unavailable", "暂时无法读取知识版本。")
	}
	output := &versionPageBody{}
	output.Body.Items = items
	if next != nil {
		output.Body.NextCursor = encodeCursor(*next)
	}
	return output, nil
}

func (handler *Handler) getKnowledgeVersion(ctx context.Context, input *struct {
	Session     string `cookie:"__Host-quoin-session"`
	KnowledgeID string `path:"knowledgeId"`
	VersionID   string `path:"versionId"`
}) (*struct {
	Body knowledge.VersionDetail `json:"body"`
}, error) {
	if _, authProblem := handler.user(ctx, input.Session); authProblem != nil {
		return nil, authProblem
	}
	knowledgeID, idProblem := parseID(input.KnowledgeID)
	if idProblem != nil {
		return nil, idProblem
	}
	versionID, idProblem := parseID(input.VersionID)
	if idProblem != nil {
		return nil, idProblem
	}
	detail, err := handler.Knowledge.GetVersion(ctx, knowledgeID, versionID)
	if err != nil {
		if errors.Is(err, knowledge.ErrNotFound) {
			return nil, problem(404, "not_found", "知识版本不存在。")
		}
		return nil, problem(500, "unavailable", "暂时无法读取知识版本。")
	}
	return &struct {
		Body knowledge.VersionDetail `json:"body"`
	}{Body: detail}, nil
}
