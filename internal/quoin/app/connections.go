package app

// Connections HTTP surface (T07): createConnection, listConnections,
// getConnection, enable/disable, probeConnection (+attempt/result reads).
// Requests carry the frozen oneOf ConnectionInput; secrets live only in
// request memory and are sealed to CredentialGeneration rows (DATA-CONN-005).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	sharedops "github.com/Suknna/quoin/internal/ops"

	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/danielgtaylor/huma/v2"
)

type connectionConfigInput struct {
	Type                string `json:"type"`
	BaseURL             string `json:"baseUrl,omitempty"`
	TLSCaPem            string `json:"tlsCaPem,omitempty"`
	TLSServerName       string `json:"tlsServerName,omitempty"`
	TLSSkipVerify       bool   `json:"tlsSkipVerify,omitempty"`
	Username            string `json:"username,omitempty"`
	Password            string `json:"password,omitempty"`
	ContextName         string `json:"contextName,omitempty"`
	DefaultNamespace    string `json:"defaultNamespace,omitempty"`
	Kubeconfig          string `json:"kubeconfig,omitempty"`
	ChatModelID         string `json:"chatModelId,omitempty"`
	EmbeddingModelID    string `json:"embeddingModelId,omitempty"`
	ContextBudgetTokens int    `json:"contextBudgetTokens,omitempty"`
	MaxOutputTokens     int    `json:"maxOutputTokens,omitempty"`
	APIKey              string `json:"apiKey,omitempty"`
}

// splitConfig separates the non-secret projection from the secret carrier
// per the frozen oneOf variants.
func splitConfig(input connectionConfigInput) (nonSecret json.RawMessage, secret json.RawMessage, secretPresent bool, err error) {
	switch input.Type {
	case connections.TypeThanos:
		projection, _ := json.Marshal(map[string]any{
			"type": connections.TypeThanos, "baseUrl": input.BaseURL,
			"tlsCaPem": input.TLSCaPem, "tlsServerName": input.TLSServerName,
			"tlsSkipVerify": input.TLSSkipVerify, "username": input.Username,
		})
		if input.Password != "" {
			secret, _ = json.Marshal(map[string]string{"type": connections.TypeThanos, "username": input.Username, "password": input.Password})
			return projection, secret, true, nil
		}
		return projection, nil, false, nil
	case connections.TypeKubernetes:
		if input.Kubeconfig == "" {
			return nil, nil, false, errors.New("kubernetes requires a kubeconfig secret")
		}
		projection, _ := json.Marshal(map[string]any{
			"type": connections.TypeKubernetes, "contextName": input.ContextName, "defaultNamespace": input.DefaultNamespace,
		})
		secret, _ := json.Marshal(map[string]string{"type": connections.TypeKubernetes, "kubeconfig": input.Kubeconfig})
		return projection, secret, true, nil
	case connections.TypeModelProvider:
		if input.APIKey == "" {
			return nil, nil, false, errors.New("model provider requires an apiKey secret")
		}
		projection := map[string]any{
			"type": connections.TypeModelProvider, "baseUrl": input.BaseURL,
			"chatModelId": input.ChatModelID, "embeddingModelId": input.EmbeddingModelID,
		}
		if input.ContextBudgetTokens > 0 {
			projection["contextBudgetTokens"] = input.ContextBudgetTokens
		}
		if input.MaxOutputTokens > 0 {
			projection["maxOutputTokens"] = input.MaxOutputTokens
		}
		nonSecret, _ := json.Marshal(projection)
		secret, _ := json.Marshal(map[string]string{"type": connections.TypeModelProvider, "apiKey": input.APIKey})
		return nonSecret, secret, true, nil
	default:
		return nil, nil, false, errors.New("connection.type must be thanos, kubernetes or model_provider")
	}
}

func connectionError(err error) error {
	var version *connections.RowVersionError
	switch {
	case errors.As(err, &version):
		problemErr := problem(http.StatusConflict, "row_version_conflict", "该连接刚被其他操作修改，请刷新后重试。")
		problemErr.Conflict = map[string]any{"code": "row_version_conflict", "objectType": "connection", "objectId": strconv.FormatInt(version.ID, 10), "rowVersion": version.Current}
		return problemErr
	case errors.Is(err, connections.ErrNameTaken):
		problemErr := problem(http.StatusConflict, "active_conflict", "同名连接已存在，请换一个名称。")
		problemErr.Conflict = map[string]any{"code": "active_conflict", "objectType": "connection"}
		return problemErr
	case errors.Is(err, connections.ErrSingleEnabled):
		problemErr := problem(http.StatusConflict, "active_conflict", "同类型已有一个启用的连接；请先停用它。")
		problemErr.Conflict = map[string]any{"code": "active_conflict", "objectType": "connection"}
		return problemErr
	case errors.Is(err, connections.ErrActiveConflict):
		problemErr := problem(http.StatusConflict, "active_conflict", "当前状态下不能执行该操作。")
		problemErr.Conflict = map[string]any{"code": "active_conflict", "objectType": "connection"}
		return problemErr
	case errors.Is(err, connections.ErrNotFound):
		return problem(http.StatusNotFound, "not_found", "目标连接不存在。")
	case errors.Is(err, connections.ErrValidation):
		return problemUnprocessable("连接字段不满足要求，请检查后重试。")
	default:
		return problem(http.StatusInternalServerError, "unavailable", "暂时无法完成操作，请重试。")
	}
}

type connectionSummaryJSON struct {
	Name                 string          `json:"name"`
	Type                 string          `json:"type"`
	Enabled              bool            `json:"enabled"`
	RevalidationRequired bool            `json:"revalidationRequired"`
	CurrentRevisionID    string          `json:"currentRevisionId,omitempty"`
	CurrentGenerationID  string          `json:"currentCredentialGenerationId,omitempty"`
	RowVersion           int64           `json:"rowVersion"`
	Config               json.RawMessage `json:"config"`
}

func renderConnection(summary connections.Summary) connectionSummaryJSON {
	rendered := connectionSummaryJSON{
		Name: summary.Name, Type: summary.Type, Enabled: summary.Enabled,
		RevalidationRequired: summary.RevalidationRequired,
		RowVersion:           summary.RowVersion, Config: summary.Config,
	}
	if summary.CurrentRevisionID > 0 {
		rendered.CurrentRevisionID = strconv.FormatInt(summary.CurrentRevisionID, 10)
	}
	if summary.CurrentGenerationID > 0 {
		rendered.CurrentGenerationID = strconv.FormatInt(summary.CurrentGenerationID, 10)
	}
	return rendered
}

func (application *apiServer) registerConnectionRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/connections", OperationID: "listConnections"}, application.listConnections)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/connections", OperationID: "createConnection"}, application.createConnection)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/connections/{connectionName}", OperationID: "getConnection"}, application.getConnection)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/connections/{connectionName}/probe", OperationID: "probeConnection"}, application.probeConnection)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/connections/{connectionName}/enable", OperationID: "enableConnection"}, application.enableConnection)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/connections/{connectionName}/disable", OperationID: "disableConnection"}, application.disableConnection)
	application.connectionDetailRoutes(api)
}

func (application *apiServer) listConnections(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Cursor  string `query:"cursor"`
	Limit   int    `query:"limit"`
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items      []connectionSummaryJSON `json:"items"`
		NextCursor string                  `json:"nextCursor,omitempty"`
	}
}, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取连接列表"); err != nil {
		return nil, err
	}
	after := decodeCursor(input.Cursor)
	summaries, more, err := application.connections.List(ctx, after, input.Limit)
	if err != nil {
		return nil, connectionError(err)
	}
	output := &struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items      []connectionSummaryJSON `json:"items"`
			NextCursor string                  `json:"nextCursor,omitempty"`
		}
	}{CacheControl: "no-store"}
	for _, summary := range summaries {
		output.Body.Items = append(output.Body.Items, renderConnection(summary))
	}
	if more && len(summaries) > 0 {
		output.Body.NextCursor = encodeCursor(summaries[len(summaries)-1].Name)
	}
	return output, nil
}

func (application *apiServer) createConnection(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		ClientCommandID string                `json:"clientCommandId" minLength:"8" maxLength:"128"`
		Name            string                `json:"name" minLength:"1" maxLength:"200"`
		Connection      connectionConfigInput `json:"connection"`
	}
}) (*struct {
	Status int `header:"-"`
	Body   connectionSummaryJSON
}, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "创建连接")
	if err != nil {
		return nil, err
	}
	nonSecret, secret, secretPresent, splitErr := splitConfig(input.Body.Connection)
	if splitErr != nil {
		return nil, problemUnprocessable("连接配置不完整：" + splitErr.Error())
	}
	summary, createErr := application.connections.Create(ctx, connections.CreateInput{
		Name: input.Body.Name, Type: input.Body.Connection.Type,
		NonSecretJSON: nonSecret, Secret: secret, SecretPresent: secretPresent,
	}, session.User.ID, input.Body.ClientCommandID)
	if createErr != nil {
		return nil, connectionError(createErr)
	}
	return &struct {
		Status int `header:"-"`
		Body   connectionSummaryJSON
	}{Status: http.StatusCreated, Body: renderConnection(summary)}, nil
}

func (application *apiServer) getConnection(ctx context.Context, input *struct {
	Session        string `cookie:"__Host-quoin-session"`
	ConnectionName string `path:"connectionName"`
}) (*struct {
	CacheControl string               `header:"Cache-Control"`
	Body         connectionDetailJSON `json:"body"`
}, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取连接详情"); err != nil {
		return nil, err
	}
	summary, err := application.connections.Get(ctx, input.ConnectionName)
	if err != nil {
		return nil, connectionError(err)
	}
	revisions, generations, err := application.connections.Counts(ctx, summary.ID)
	if err != nil {
		return nil, connectionError(err)
	}
	detail := connectionDetailJSON{connectionSummaryJSON: renderConnection(summary), RevisionCount: revisions, GenerationCount: generations}
	if active, found, activeErr := application.connections.ActiveProbeAttempt(ctx, summary.ID); activeErr == nil && found {
		rendered := renderAttempt(active)
		detail.ActiveProbeAttempt = &rendered
	}
	return &struct {
		CacheControl string               `header:"Cache-Control"`
		Body         connectionDetailJSON `json:"body"`
	}{CacheControl: "no-store", Body: detail}, nil
}

func (application *apiServer) probeConnection(ctx context.Context, input *struct {
	Session        string `cookie:"__Host-quoin-session"`
	ConnectionName string `path:"connectionName"`
	Body           struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128"`
	}
}) (*struct {
	Status int `header:"-"`
	Body   struct {
		AttemptID string `json:"id"`
		State     string `json:"state"`
	}
}, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "发起连接探测"); err != nil {
		return nil, err
	}
	attemptID, err := application.connections.StartProbe(ctx, input.ConnectionName, application.runtime, application.dispatchProbe)
	if err != nil {
		return nil, connectionError(err)
	}
	state := "Queued"
	if view, viewErr := application.attemptState(ctx, attemptID); viewErr == nil {
		state = view
	}
	return &struct {
		Status int `header:"-"`
		Body   struct {
			AttemptID string `json:"id"`
			State     string `json:"state"`
		}
	}{Status: http.StatusAccepted, Body: struct {
		AttemptID string `json:"id"`
		State     string `json:"state"`
	}{AttemptID: strconv.FormatInt(attemptID, 10), State: state}}, nil
}

func (application *apiServer) enableConnection(ctx context.Context, input *struct {
	Session        string `cookie:"__Host-quoin-session"`
	ConnectionName string `path:"connectionName"`
	Body           struct {
		ClientCommandID        string `json:"clientCommandId" minLength:"8" maxLength:"128"`
		ExpectedRow            int64  `json:"expectedRowVersion" minimum:"1"`
		QualifiedProbeResultID string `json:"qualifiedProbeResultId,omitempty"`
	}
}) (*struct {
	Body connectionSummaryJSON
}, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "启用连接")
	if err != nil {
		return nil, err
	}
	var probeID int64
	if input.Body.QualifiedProbeResultID != "" {
		parsed, parseErr := strconv.ParseInt(input.Body.QualifiedProbeResultID, 10, 64)
		if parseErr != nil {
			return nil, problemUnprocessable("qualifiedProbeResultId 必须是十进制 locator。")
		}
		probeID = parsed
	}
	summary, err := application.connections.Enable(ctx, input.ConnectionName, input.Body.ExpectedRow, probeID, session.User.ID)
	if err != nil {
		return nil, connectionError(err)
	}
	return &struct {
		Body connectionSummaryJSON
	}{Body: renderConnection(summary)}, nil
}

func (application *apiServer) disableConnection(ctx context.Context, input *struct {
	Session        string `cookie:"__Host-quoin-session"`
	ConnectionName string `path:"connectionName"`
	Body           struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128"`
		ExpectedRow     int64  `json:"expectedRowVersion" minimum:"1"`
	}
}) (*struct {
	Body connectionSummaryJSON
}, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "停用连接"); err != nil {
		return nil, err
	}
	summary, err := application.connections.Disable(ctx, input.ConnectionName, input.Body.ExpectedRow)
	if err != nil {
		return nil, connectionError(err)
	}
	return &struct {
		Body connectionSummaryJSON
	}{Body: renderConnection(summary)}, nil
}

// dispatchProbe forwards the committed probe dispatch to the live Plinth
// control stream via the RuntimeService task slice; send failures are
// audited and the queued dispatcher retries when the stream reattaches.
func (application *apiServer) dispatchProbe(attemptID int64, summary connections.Summary, epoch uint64, bootID string, grantID int64, input []byte, contractDigest, actionSetID string, actionSetVersion int) {
	if dispatcher := application.probeDispatcher(); dispatcher != nil {
		if err := dispatcher(context.Background(), attemptID, summary, epoch, bootID, grantID, input); err != nil {
			sharedops.LogEvent("quoin", "error", "probe.dispatch_failed", err.Error())
		}
	}
}

// probeDispatcher returns the stream dispatcher owned by the runtime task
// slice, or nil when the gRPC surface is not mounted (unit harnesses).
func (application *apiServer) probeDispatcher() func(ctx context.Context, attemptID int64, summary connections.Summary, epoch uint64, bootID string, grantID int64, input []byte) error {
	return application.probeDispatchFunc
}

// attemptState reads the authoritative execution_attempts.state.
func (application *apiServer) attemptState(ctx context.Context, attemptID int64) (string, error) {
	var state string
	err := application.db.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state)
	return state, err
}

// connectionDetailJSON extends the summary with detail counters and the
// active probe attempt (ConnectionDetail contract).
type connectionDetailJSON struct {
	connectionSummaryJSON
	RevisionCount      int                 `json:"revisionCount"`
	GenerationCount    int                 `json:"generationCount"`
	ActiveProbeAttempt *attemptSummaryJSON `json:"activeProbeAttempt,omitempty"`
}

type attemptSummaryJSON struct {
	ID                string `json:"id"`
	Type              string `json:"type"`
	State             string `json:"state"`
	RowVersion        int64  `json:"rowVersion"`
	CreatedAt         string `json:"createdAt"`
	StartedAt         string `json:"startedAt,omitempty"`
	EndedAt           string `json:"endedAt,omitempty"`
	TerminationReason string `json:"terminationReason,omitempty"`
}

func renderAttempt(view connections.AttemptView) attemptSummaryJSON {
	return attemptSummaryJSON{
		ID: strconv.FormatInt(view.ID, 10), Type: "connection_probe", State: view.State,
		RowVersion: view.RowVersion, CreatedAt: view.CreatedAt, StartedAt: view.StartedAt,
		EndedAt: view.EndedAt, TerminationReason: view.TerminationReason,
	}
}

func (application *apiServer) connectionDetailRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/connections/{connectionName}/probe-attempts/{attemptId}", OperationID: "getConnectionProbeAttempt"}, application.getConnectionProbeAttempt)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/connections/{connectionName}/probe-attempts/{attemptId}/cancel", OperationID: "cancelConnectionProbeAttempt"}, application.cancelConnectionProbeAttempt)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/connections/{connectionName}/probe-results", OperationID: "listConnectionProbeResults"}, application.listConnectionProbeResults)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/connections/{connectionName}/revisions", OperationID: "listConnectionRevisions"}, application.listConnectionRevisions)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/connections/{connectionName}/generations", OperationID: "listCredentialGenerations"}, application.listCredentialGenerations)
}

func (application *apiServer) connectionByName(ctx context.Context, name string) (connections.Summary, error) {
	summary, err := application.connections.Get(ctx, name)
	if err != nil {
		return connections.Summary{}, connectionError(err)
	}
	return summary, nil
}

func (application *apiServer) getConnectionProbeAttempt(ctx context.Context, input *struct {
	Session        string `cookie:"__Host-quoin-session"`
	ConnectionName string `path:"connectionName"`
	AttemptID      string `path:"attemptId"`
}) (*struct {
	CacheControl string             `header:"Cache-Control"`
	Body         attemptSummaryJSON `json:"body"`
}, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "读取探测任务"); err != nil {
		return nil, err
	}
	summary, err := application.connectionByName(ctx, input.ConnectionName)
	if err != nil {
		return nil, err
	}
	attemptID, err := strconv.ParseInt(input.AttemptID, 10, 64)
	if err != nil {
		return nil, problemUnprocessable("attemptId 必须是十进制 locator。")
	}
	view, err := application.connections.Attempt(ctx, summary.ID, attemptID)
	if err != nil {
		return nil, connectionError(err)
	}
	return &struct {
		CacheControl string             `header:"Cache-Control"`
		Body         attemptSummaryJSON `json:"body"`
	}{CacheControl: "no-store", Body: renderAttempt(view)}, nil
}

func (application *apiServer) cancelConnectionProbeAttempt(ctx context.Context, input *struct {
	Session        string `cookie:"__Host-quoin-session"`
	ConnectionName string `path:"connectionName"`
	AttemptID      string `path:"attemptId"`
	Body           struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128"`
		ExpectedRow     int64  `json:"expectedRowVersion" minimum:"1"`
	}
}) (*struct {
	Body attemptSummaryJSON `json:"body"`
}, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "取消探测任务"); err != nil {
		return nil, err
	}
	summary, err := application.connectionByName(ctx, input.ConnectionName)
	if err != nil {
		return nil, err
	}
	attemptID, err := strconv.ParseInt(input.AttemptID, 10, 64)
	if err != nil {
		return nil, problemUnprocessable("attemptId 必须是十进制 locator。")
	}
	if err := application.connections.CancelProbe(ctx, attemptID, input.Body.ExpectedRow); err != nil {
		return nil, connectionError(err)
	}
	// The cancellation fence is committed; forward CancelAttempt to the
	// live stream when the attempt is bound to one (RUNTIME-CANCEL-001).
	view, viewErr := application.connections.Attempt(ctx, summary.ID, attemptID)
	if viewErr == nil {
		application.cancelProbeOnStream(view)
	}
	if viewErr != nil {
		return nil, connectionError(viewErr)
	}
	return &struct {
		Body attemptSummaryJSON `json:"body"`
	}{Body: renderAttempt(view)}, nil
}

// cancelProbeOnStream forwards the committed cancellation fence to the
// runtime that owns the attempt; unbound (Queued) attempts finalize
// immediately.
func (application *apiServer) cancelProbeOnStream(view connections.AttemptView) {
	if view.State != "Cancelling" {
		return
	}
	dispatcher := application.cancelDispatchFunc
	if dispatcher == nil {
		// No stream surface mounted: finalize locally (unit harnesses and
		// Queued attempts never reached a runtime).
		_ = application.connections.RecordCancelAck(context.Background(), view.ID)
		return
	}
	if err := dispatcher(context.Background(), view.ID); err != nil {
		sharedops.LogEvent("quoin", "error", "probe.cancel_dispatch", err.Error())
	}
}

type probeResultJSONRow struct {
	ID                     string          `json:"id"`
	AttemptID              string          `json:"attemptId"`
	ConnectionType         string          `json:"connectionType"`
	ConnectionRevisionID   string          `json:"connectionRevisionId"`
	CredentialGenerationID string          `json:"credentialGenerationId"`
	RootBindingRevision    int             `json:"rootBindingRevision"`
	ActionSetID            string          `json:"actionSetId"`
	ActionSetVersion       int             `json:"actionSetVersion"`
	ProbeContractDigest    string          `json:"probeContractDigest"`
	Outcome                string          `json:"outcome"`
	ResultDigest           string          `json:"resultDigest"`
	StartedAt              string          `json:"startedAt"`
	FinishedAt             string          `json:"finishedAt"`
	Details                json.RawMessage `json:"details"`
}

func (application *apiServer) listConnectionProbeResults(ctx context.Context, input *struct {
	Session        string `cookie:"__Host-quoin-session"`
	ConnectionName string `path:"connectionName"`
	Cursor         string `query:"cursor"`
	Limit          int    `query:"limit"`
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items      []probeResultJSONRow `json:"items"`
		NextCursor string               `json:"nextCursor,omitempty"`
	}
}, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取探测结果"); err != nil {
		return nil, err
	}
	summary, err := application.connectionByName(ctx, input.ConnectionName)
	if err != nil {
		return nil, err
	}
	results, more, err := application.connections.ProbeResults(ctx, summary.ID, decodeCursor(input.Cursor), input.Limit)
	if err != nil {
		return nil, connectionError(err)
	}
	output := &struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items      []probeResultJSONRow `json:"items"`
			NextCursor string               `json:"nextCursor,omitempty"`
		}
	}{CacheControl: "no-store"}
	for _, result := range results {
		details := json.RawMessage(result.DetailJSON)
		if len(details) == 0 {
			details = json.RawMessage("{}")
		}
		output.Body.Items = append(output.Body.Items, probeResultJSONRow{
			ID: strconv.FormatInt(result.ID, 10), AttemptID: strconv.FormatInt(result.AttemptID, 10),
			ConnectionType:         result.ConnectionType,
			ConnectionRevisionID:   strconv.FormatInt(result.ConnectionRevisionID, 10),
			CredentialGenerationID: strconv.FormatInt(result.CredentialGenerationID, 10),
			RootBindingRevision:    result.RootBindingRevision, ActionSetID: result.ActionSetID,
			ActionSetVersion: result.ActionSetVersion, ProbeContractDigest: result.ProbeContractDigest,
			Outcome: result.Outcome, ResultDigest: result.ResultDigest,
			StartedAt: result.StartedAt, FinishedAt: result.FinishedAt, Details: details,
		})
	}
	if more && len(results) > 0 {
		output.Body.NextCursor = strconv.FormatInt(results[len(results)-1].ID, 10)
	}
	return output, nil
}

type revisionJSONRow struct {
	ID          string          `json:"id"`
	RevisionSeq int64           `json:"revisionSeq"`
	Config      json.RawMessage `json:"config"`
	CreatedAt   string          `json:"createdAt"`
}

func (application *apiServer) listConnectionRevisions(ctx context.Context, input *struct {
	Session        string `cookie:"__Host-quoin-session"`
	ConnectionName string `path:"connectionName"`
	Cursor         string `query:"cursor"`
	Limit          int    `query:"limit"`
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items      []revisionJSONRow `json:"items"`
		NextCursor string            `json:"nextCursor,omitempty"`
	}
}, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取连接 revision 历史"); err != nil {
		return nil, err
	}
	summary, err := application.connectionByName(ctx, input.ConnectionName)
	if err != nil {
		return nil, err
	}
	revisions, more, err := application.connections.Revisions(ctx, summary.ID, decodeCursor(input.Cursor), input.Limit)
	if err != nil {
		return nil, connectionError(err)
	}
	output := &struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items      []revisionJSONRow `json:"items"`
			NextCursor string            `json:"nextCursor,omitempty"`
		}
	}{CacheControl: "no-store"}
	for _, revision := range revisions {
		output.Body.Items = append(output.Body.Items, revisionJSONRow{
			ID: strconv.FormatInt(revision.ID, 10), RevisionSeq: revision.RevisionSeq,
			Config: json.RawMessage(revision.ConfigJSON), CreatedAt: revision.CreatedAt,
		})
	}
	if more && len(revisions) > 0 {
		output.Body.NextCursor = strconv.FormatInt(revisions[len(revisions)-1].RevisionSeq, 10)
	}
	return output, nil
}

type generationJSONRow struct {
	ID            string `json:"id"`
	GenerationSeq int64  `json:"generationSeq"`
	CreatedBy     string `json:"createdBy,omitempty"`
	CreatedAt     string `json:"createdAt"`
}

func (application *apiServer) listCredentialGenerations(ctx context.Context, input *struct {
	Session        string `cookie:"__Host-quoin-session"`
	ConnectionName string `path:"connectionName"`
	Cursor         string `query:"cursor"`
	Limit          int    `query:"limit"`
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items      []generationJSONRow `json:"items"`
		NextCursor string              `json:"nextCursor,omitempty"`
	}
}, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取凭据 generation 历史"); err != nil {
		return nil, err
	}
	summary, err := application.connectionByName(ctx, input.ConnectionName)
	if err != nil {
		return nil, err
	}
	generations, more, err := application.connections.Generations(ctx, summary.ID, decodeCursor(input.Cursor), input.Limit)
	if err != nil {
		return nil, connectionError(err)
	}
	output := &struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items      []generationJSONRow `json:"items"`
			NextCursor string              `json:"nextCursor,omitempty"`
		}
	}{CacheControl: "no-store"}
	for _, generation := range generations {
		row := generationJSONRow{
			ID: strconv.FormatInt(generation.ID, 10), GenerationSeq: generation.GenerationSeq,
			CreatedAt: generation.CreatedAt,
		}
		if generation.CreatedBy > 0 {
			row.CreatedBy = strconv.FormatInt(generation.CreatedBy, 10)
		}
		output.Body.Items = append(output.Body.Items, row)
	}
	if more && len(generations) > 0 {
		output.Body.NextCursor = strconv.FormatInt(generations[len(generations)-1].GenerationSeq, 10)
	}
	return output, nil
}
