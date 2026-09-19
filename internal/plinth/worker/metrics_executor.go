package worker

// metrics 插件的执行绑定（ADR-0004）：prometheus/thanos 在本进程的
// ExecutionBundle.ToolExecutor。执行体只面对插件契约——冻结的连接配置经
// plugins.Call 传入、凭据只经 SecretResolver 边界解析、长输出经宿主提供
// 的 ToolWorkspace 溢出到 tool_result Artifact——工具的密封（terminal
// state、evidence、artifact locator）仍由本进程的 typed 分发表完成。授予
// 解析（Runner.CallFor）与查询执行是宿主机器，不是第二个插件机制。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	plinthtools "github.com/Suknna/quoin/internal/plinth/tools"
	"github.com/Suknna/quoin/internal/plugins"
)

// metrics secret 槽位是插件的命名秘密槽；槽名即引用名（Call.SecretRefs 的
// value），解析器按同一命名空间分发。
const (
	metricSecretSlotUsername    = "username"
	metricSecretSlotPassword    = "password"
	metricSecretSlotBearerToken = "bearerToken"
)

// ExecutionBundles returns the plugins.ToolExecutor implementations the
// metrics plugins bind in this process, keyed by plugin ID. The prometheus
// and thanos plugins share one PromQL query contract, so both bundles bind
// the same executor; authorization resolves the actual source connection.
func ExecutionBundles() map[string]plugins.ToolExecutor {
	executor := metricsToolExecutor{}
	return map[string]plugins.ToolExecutor{
		"prometheus": executor,
		"thanos":     executor,
	}
}

// metricsToolExecutor executes the plugins' PromQL query tool against the
// frozen connection of THIS call. It is stateless: every execution input
// arrives through the plugin contract (Call settings/secrets, tool
// arguments, host workspace seam).
type metricsToolExecutor struct{}

// ExecuteTool runs one instant query and returns the canonical
// thanos_query_result_v1 payload plus the committed artifact id when the
// raw response spilled. Structured failures travel IN the payload
// (success=false), matching the tool's frozen failure shape.
func (metricsToolExecutor) ExecuteTool(ctx context.Context, call *plugins.Call, request plugins.ToolRequest) (*plugins.ToolResult, error) {
	query, _ := argumentString(request.ArgumentsJSON, "query")
	if query == "" {
		return failureResult("invalid_arguments", "query 必须是非空字符串"), nil
	}
	if request.Workspace == nil {
		return failureResult("supervisor_error", "supervisor 未提供本次执行的工作区"), nil
	}
	var config plinthconnections.ThanosConfig
	if err := json.Unmarshal(call.Settings, &config); err != nil {
		return failureResult("invalid_connection_config", "指标连接配置无法解析: "+err.Error()), nil
	}
	if config.BaseURL == "" {
		return failureResult("invalid_connection_config", "指标连接配置缺少 baseUrl"), nil
	}
	secret, err := metricsCallSecret(ctx, call)
	if err != nil {
		// 凭据解析失败 fail closed：绝不以匿名/空凭据继续查询。
		return failureResult("grant_missing", err.Error()), nil
	}
	canonical, artifactID, execErr := plinthtools.ExecuteThanosQuery(ctx, plinthtools.ThanosQueryParams{
		Config: config, Secret: secret, Query: query,
		WorkspaceDir: request.Workspace.Dir,
		AttemptID:    request.Workspace.AttemptID, ToolCallID: request.Workspace.ToolCallID,
		Upload: func(ctx context.Context, attemptID, toolCallID int64, path string) (int64, error) {
			return request.Workspace.UploadFile(ctx, path, "application/json")
		},
	})
	if execErr != nil {
		return failureResult("thanos_execution_failed", execErr.Error()), nil
	}
	return payloadResult(canonical, artifactID)
}

// CallFor resolves the frozen connection settings and the grant-backed
// secret resolver for one typed tool execution from the tool call's sealed
// grants (ARCH-INPUT-003: the authorization transaction froze exactly the
// connections; the executor never sees a credential reference).
func (runner *Runner) CallFor(execution *TypedToolContext) (*plugins.Call, error) {
	meta, ok := runner.tools[execution.ToolCallID]
	if !ok || len(meta.grants) == 0 {
		return nil, &pluginCallError{code: "grant_missing", detail: "该工具调用没有冻结的连接 grant"}
	}
	grant := meta.grants[0]
	grantCtx, grantCancel := context.WithTimeout(execution.BaseCtx, 15*time.Second)
	grantPayload, err := runner.Client.FetchCredentialGrant(grantCtx, &runtimev1.FetchCredentialGrantRequest{
		GrantId: grant.GetGrantId(), AttemptId: execution.AttemptID, BootId: runner.Binding.BootID, ConnectionEpoch: runner.Binding.Epoch,
	})
	grantCancel()
	if err != nil {
		return nil, &pluginCallError{code: "grant_missing", detail: "获取指标连接凭据 grant 失败: " + err.Error()}
	}
	if grantPayload.GetThanos() == nil {
		return nil, &pluginCallError{code: "grant_missing", detail: "grant 回复未携带指标连接凭据"}
	}
	secrets := map[string][]byte{
		metricSecretSlotUsername:    []byte(grantPayload.GetThanos().GetUsername()),
		metricSecretSlotPassword:    []byte(grantPayload.GetThanos().GetPassword()),
		metricSecretSlotBearerToken: []byte(grantPayload.GetThanos().GetBearerToken()),
	}
	return &plugins.Call{
		Settings: json.RawMessage(grantPayload.GetRevisionConfigJson()),
		SecretRefs: map[string]string{
			metricSecretSlotUsername:    metricSecretSlotUsername,
			metricSecretSlotPassword:    metricSecretSlotPassword,
			metricSecretSlotBearerToken: metricSecretSlotBearerToken,
		},
		Secrets: secretResolverFunc(func(ref string) ([]byte, error) {
			clear, ok := secrets[ref]
			if !ok {
				return nil, fmt.Errorf("unknown metric secret slot %q", ref)
			}
			return clear, nil
		}),
	}, nil
}

// secretResolverFunc adapts a function to the plugin secret boundary.
type secretResolverFunc func(ref string) ([]byte, error)

func (f secretResolverFunc) Resolve(_ context.Context, ref string) ([]byte, error) { return f(ref) }

// metricsCallSecret resolves the metric credentials of one call; any missing
// slot is a hard error, never an anonymous fallback.
func metricsCallSecret(ctx context.Context, call *plugins.Call) (plinthconnections.ThanosSecret, error) {
	if call.Secrets == nil {
		return plinthconnections.ThanosSecret{}, fmt.Errorf("plugin %q call has no secret resolver", call.PluginID)
	}
	username, err := call.Secrets.Resolve(ctx, metricSecretSlotUsername)
	if err != nil {
		return plinthconnections.ThanosSecret{}, fmt.Errorf("metric secret slot %q unavailable: %w", metricSecretSlotUsername, err)
	}
	password, err := call.Secrets.Resolve(ctx, metricSecretSlotPassword)
	if err != nil {
		return plinthconnections.ThanosSecret{}, fmt.Errorf("metric secret slot %q unavailable: %w", metricSecretSlotPassword, err)
	}
	return plinthconnections.ThanosSecret{Username: string(username), Password: string(password)}, nil
}

// argumentString extracts one top-level string argument from the canonical
// arguments object.
func argumentString(argumentsJSON json.RawMessage, key string) (string, bool) {
	var arguments map[string]any
	if json.Unmarshal(argumentsJSON, &arguments) != nil {
		return "", false
	}
	value, _ := arguments[key].(string)
	return value, value != ""
}

// failureResult is a structured return_to_model failure; the sealed payload
// stays the tool's own failure shape.
func failureResult(errorCode, errorDetail string) *plugins.ToolResult {
	payload, _ := json.Marshal(map[string]any{"success": false, "errorCode": errorCode, "errorDetail": errorDetail})
	return &plugins.ToolResult{Success: false, Payload: payload, ErrorCode: errorCode, ErrorDetail: errorDetail}
}

// payloadResult classifies one canonical payload: structured failures
// (success=false) carry their code/detail on the result, and the payload
// always travels unchanged for sealing.
func payloadResult(canonical json.RawMessage, artifactID int64) (*plugins.ToolResult, error) {
	var payload struct {
		Success     bool   `json:"success"`
		ErrorCode   string `json:"errorCode"`
		ErrorDetail string `json:"errorDetail"`
	}
	if err := json.Unmarshal(canonical, &payload); err != nil {
		return nil, fmt.Errorf("结果序列化失败: %s", err.Error())
	}
	return &plugins.ToolResult{
		Success: payload.Success, Payload: canonical, ArtifactID: artifactID,
		ErrorCode: payload.ErrorCode, ErrorDetail: payload.ErrorDetail,
	}, nil
}
