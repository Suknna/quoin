// Typed tool execution on the supervisor (ARCH-WORKER-003, ARCH-TOOL-002/003):
// the pending->running begin fence, the fixed supervisor_typed tool
// dispatch (artifact_read/artifact_grep/thanos_query), the attempt-scoped
// Artifact upload and the CompleteToolCall sealing with the committed
// payload, evidence ids and artifact locator. Split out of runner.go by
// domain responsibility (tool execution vs. attempt lifecycle/model loop).
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	workerv1 "github.com/Suknna/quoin/internal/gen/proto/plinth/worker/v1"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	plinthtools "github.com/Suknna/quoin/internal/plinth/tools"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
	"google.golang.org/grpc/metadata"
)

// executeTool persists pending->running and answers ToolCallStarted
// (ARCH-TOOL-002); supervisor_typed tools (artifact_read/artifact_grep)
// execute on the supervisor through the Attempt-scoped ArtifactService and
// converge on ToolResult directly (ARCH-OUTPUT-004).
func (runner *Runner) executeTool(ctx context.Context, writer *FrameWriter, attemptID, toolCallID int64, meta toolMeta) error {
	reply, err := runner.toolCallChannel().Request(ctx, &runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg: &runtimev1.ControlEnvelope_BeginToolCall{BeginToolCall: &runtimev1.BeginToolCall{
			AttemptId: attemptID, ToolCallId: toolCallID,
		}},
	})
	if err != nil {
		return err
	}
	ack := reply.GetBeginToolCallAck()
	if ack == nil || !ack.GetAccepted() {
		return fmt.Errorf("begin tool call %d rejected: %s", toolCallID, ack.GetDetail())
	}
	mode := runtimev1.ToolExecutionMode(runtimev1.ToolExecutionMode_value[meta.mode])
	if err := writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_ToolCallStarted{
		ToolCallStarted: &workerv1.ToolCallStarted{ToolCallId: toolCallID, ExecutionMode: mode},
	}}); err != nil {
		return err
	}
	if meta.mode != "TOOL_EXECUTION_MODE_SUPERVISOR_TYPED" {
		return nil
	}
	// A Quoin routing preflight is a normal, model-visible Tool Result. It
	// runs through the existing completion channel but cannot fetch a grant
	// or reach Kubernetes because it returns before typed dispatch.
	if meta.preflightCode != "" {
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, meta.name, meta.preflightCode, meta.preflightDetail)
	}
	return runner.executeTypedTool(ctx, writer, attemptID, toolCallID, meta.name)
}

// toolArguments rebuilds the persisted canonical arguments of one tool
// call (the runner never trusts the worker's copy).
func (runner *Runner) toolArguments(ctx context.Context, attemptID, toolCallID int64) (map[string]any, error) {
	meta, ok := runner.tools[toolCallID]
	if !ok {
		return nil, fmt.Errorf("tool call %d metadata missing", toolCallID)
	}
	return meta.arguments, nil
}

// executeTypedTool runs one supervisor_typed tool (artifact_read,
// artifact_grep or thanos_query) and seals it through CompleteToolCall;
// the committed ToolResult returns to the worker immediately
// (ARCH-TOOL-002/003).
func (runner *Runner) executeTypedTool(ctx context.Context, writer *FrameWriter, attemptID, toolCallID int64, toolName string) error {
	args, err := runner.toolArguments(ctx, attemptID, toolCallID)
	if err != nil {
		return err
	}
	var payload map[string]any
	schemaKind := toolName + "_result_v1"
	switch toolName {
	case "artifact_read", "artifact_grep":
		// The locator pre-parse is exclusive to the artifact tools: the
		// thanos_query arguments carry no artifactId and must reach the
		// query executor untouched.
		artifactIDText, _ := args["artifactId"].(string)
		artifactID, err := parseLocator(artifactIDText)
		if err != nil {
			return runner.failTypedTool(ctx, writer, attemptID, toolCallID, toolName, "invalid_arguments", err.Error())
		}
		if toolName == "artifact_grep" {
			pattern, _ := args["pattern"].(string)
			if pattern == "" {
				return runner.failTypedTool(ctx, writer, attemptID, toolCallID, toolName, "invalid_arguments", "pattern 必须是非空字符串")
			}
			rpcCtx, err := runner.artifactContext(ctx)
			if err != nil {
				return runner.failTypedTool(ctx, writer, attemptID, toolCallID, toolName, "grant_missing", "读取状态卷 token 失败")
			}
			response, rpcErr := runner.Artifacts.GrepText(rpcCtx, &runtimev1.ArtifactGrepTextRequest{
				AttemptId: attemptID, ArtifactId: artifactID, BootId: runner.Sink.BootID(),
				ConnectionEpoch: runner.Sink.Epoch(), Re2Pattern: pattern,
				MaxMatches: 200, ContextLines: 5,
			})
			if rpcErr != nil {
				return runner.failTypedTool(ctx, writer, attemptID, toolCallID, toolName, "artifact_grep_failed", rpcErr.Error())
			}
			var lines []string
			for _, match := range response.GetMatches() {
				lines = append(lines, fmt.Sprintf("%d:%s", match.GetLine(), string(match.GetContent())))
			}
			payload = map[string]any{
				"success": true, "output": joinLines(lines),
				"matchCount": len(lines), "truncated": response.GetTruncated(),
				"totalBytes": response.GetTotalSizeBytes(), "totalLines": response.GetTotalLines(),
				"artifact": map[string]any{"id": fmt.Sprint(response.GetArtifactId()), "mediaType": response.GetMediaType()},
			}
		} else {
			startLine := int64(1)
			maxLines := int64(2000)
			if offset, ok := args["offset"].(float64); ok && offset >= 1 {
				startLine = int64(offset)
			}
			if limit, ok := args["limit"].(float64); ok && limit >= 1 && limit <= 2000 {
				maxLines = int64(limit)
			}
			rpcCtx, err := runner.artifactContext(ctx)
			if err != nil {
				return runner.failTypedTool(ctx, writer, attemptID, toolCallID, toolName, "grant_missing", "读取状态卷 token 失败")
			}
			response, rpcErr := runner.Artifacts.ReadText(rpcCtx, &runtimev1.ArtifactReadTextRequest{
				AttemptId: attemptID, ArtifactId: artifactID, BootId: runner.Sink.BootID(),
				ConnectionEpoch: runner.Sink.Epoch(), StartLine: uint64(startLine), MaxLines: uint32(maxLines),
			})
			if rpcErr != nil {
				return runner.failTypedTool(ctx, writer, attemptID, toolCallID, toolName, "artifact_read_failed", rpcErr.Error())
			}
			payload = map[string]any{
				"success": true, "output": string(response.GetContent()),
				"startLine": response.GetStartLine(), "nextLine": response.GetNextLine(), "eof": response.GetEof(),
				"totalBytes": response.GetTotalSizeBytes(), "totalLines": response.GetTotalLines(),
				"artifact": map[string]any{"id": fmt.Sprint(response.GetArtifactId()), "mediaType": response.GetMediaType()},
			}
		}
	case "thanos_query":
		return runner.executeThanosQuery(ctx, writer, attemptID, toolCallID, args)
	case "kubernetes_read":
		return runner.executeKubernetesRead(ctx, writer, attemptID, toolCallID, args)
	default:
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, toolName, "unknown_tool", "supervisor tool "+toolName+" is not in the fixed catalog")
	}
	return runner.commitTypedTool(ctx, writer, attemptID, toolCallID, schemaKind, payload, 0)
}

// executeThanosQuery drives the frozen typed read-only Thanos observation
// (ARCH-WORKER-003/ARCH-CHAT-005): the supervisor fetches the credential
// through the tool's frozen grant, executes the instant query, streams
// long raw bodies into the tool_result Artifact store and seals the
// frozen thanos_query_result_v1 payload (ARCH-OUTPUT-001/005).
func (runner *Runner) executeThanosQuery(ctx context.Context, writer *FrameWriter, attemptID, toolCallID int64, args map[string]any) error {
	query, _ := args["query"].(string)
	if query == "" {
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, thanos.QueryToolName, "invalid_arguments", "query 必须是非空字符串")
	}
	meta, ok := runner.tools[toolCallID]
	if !ok || len(meta.grants) == 0 {
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, thanos.QueryToolName, "grant_missing", "该工具调用没有冻结的连接 grant")
	}
	grant := meta.grants[0]
	bearer, err := runner.toolCallChannel().BearerToken()
	if err != nil {
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, thanos.QueryToolName, "grant_missing", "读取状态卷 token 失败")
	}
	grantCtx, grantCancel := context.WithTimeout(ctx, 15*time.Second)
	grantPayload, err := runner.Client.FetchCredentialGrant(metadata.NewOutgoingContext(grantCtx, metadata.Pairs("authorization", "Bearer "+bearer)), &runtimev1.FetchCredentialGrantRequest{
		GrantId: grant.GetGrantId(), AttemptId: attemptID, BootId: runner.Binding.BootID, ConnectionEpoch: runner.Binding.Epoch,
	})
	grantCancel()
	if err != nil {
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, thanos.QueryToolName, "grant_missing", "获取 Thanos 凭据 grant 失败: "+err.Error())
	}
	if grantPayload.GetThanos() == nil {
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, thanos.QueryToolName, "grant_missing", "grant 回复未携带 Thanos 凭据")
	}
	var config plinthconnections.ThanosConfig
	if err := json.Unmarshal(grantPayload.GetRevisionConfigJson(), &config); err != nil {
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, thanos.QueryToolName, "invalid_connection_config", "Thanos 连接配置无法解析: "+err.Error())
	}
	if config.BaseURL == "" {
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, thanos.QueryToolName, "invalid_connection_config", "Thanos 连接配置缺少 baseUrl")
	}
	secret := plinthconnections.ThanosSecret{}
	if grantPayload.GetThanos() != nil {
		secret = plinthconnections.ThanosSecret{Username: grantPayload.GetThanos().GetUsername(), Password: grantPayload.GetThanos().GetPassword()}
	}
	canonical, artifactID, execErr := plinthtools.ExecuteThanosQuery(ctx, plinthtools.ThanosQueryParams{
		Config: config, Secret: secret, Query: query,
		WorkspaceDir: filepath.Join(runner.Config.WorkspaceRoot, fmt.Sprintf("attempt-%d", attemptID)),
		AttemptID:    attemptID, ToolCallID: toolCallID,
		Upload: func(ctx context.Context, attemptID, toolCallID int64, path string) (int64, error) {
			return runner.uploadWorkspaceFileAs(ctx, attemptID, toolCallID, path, "application/json")
		},
	})
	if execErr != nil {
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, thanos.QueryToolName, "thanos_execution_failed", execErr.Error())
	}
	var payload map[string]any
	if err := json.Unmarshal(canonical, &payload); err != nil {
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, thanos.QueryToolName, "invalid_result", "结果序列化失败")
	}
	return runner.commitTypedTool(ctx, writer, attemptID, toolCallID, thanos.QueryResultSchemaKind, payload, artifactID)
}

// executeKubernetesRead executes the fixed action once per Quoin-frozen
// connection grant. Individual failures are returned in-order, never retried
// or silently collapsed, and credentials stay in this supervisor method.
func (runner *Runner) executeKubernetesRead(ctx context.Context, writer *FrameWriter, attemptID, toolCallID int64, args map[string]any) error {
	meta, ok := runner.tools[toolCallID]
	if !ok || len(meta.grants) == 0 {
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, "kubernetes_read", "grant_missing", "该工具调用没有冻结的 Kubernetes 连接 grant")
	}
	operation, _ := args["operation"].(string)
	namespace, _ := args["namespace"].(string)
	name, _ := args["name"].(string)
	container, _ := args["container"].(string)
	request := plinthconnections.KubernetesReadRequest{Operation: operation, Namespace: namespace, Name: name, Container: container}
	bearer, err := runner.toolCallChannel().BearerToken()
	if err != nil {
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, "kubernetes_read", "grant_missing", "读取状态卷 token 失败")
	}
	results := make([]map[string]any, 0, len(meta.grants))
	allOK := true
	for _, grant := range meta.grants {
		grantCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		payload, fetchErr := runner.Client.FetchCredentialGrant(metadata.NewOutgoingContext(grantCtx, metadata.Pairs("authorization", "Bearer "+bearer)), &runtimev1.FetchCredentialGrantRequest{GrantId: grant.GetGrantId(), AttemptId: attemptID, BootId: runner.Binding.BootID, ConnectionEpoch: runner.Binding.Epoch})
		cancel()
		if fetchErr != nil || payload.GetKubernetes() == nil {
			allOK = false
			results = append(results, map[string]any{"success": false, "errorCode": "grant_missing", "errorDetail": "获取 Kubernetes 凭据 grant 失败"})
			continue
		}
		var config plinthconnections.KubernetesConfig
		if err := json.Unmarshal(payload.GetRevisionConfigJson(), &config); err != nil {
			allOK = false
			results = append(results, map[string]any{"success": false, "errorCode": "invalid_connection_config", "errorDetail": "Kubernetes 连接配置无法解析"})
			continue
		}
		body, readErr := plinthconnections.RunKubernetesRead(ctx, config, plinthconnections.KubernetesSecret{Kubeconfig: payload.GetKubernetes().GetKubeconfig()}, request)
		if readErr != nil {
			allOK = false
			// The primitive may include the remote URL in transport errors. That
			// diagnostic never crosses the model boundary.
			results = append(results, map[string]any{"success": false, "errorCode": "kubernetes_execution_failed", "errorDetail": "Kubernetes API 读取失败"})
			continue
		}
		results = append(results, map[string]any{"success": true, "output": string(body), "truncated": false})
	}
	fullPayload := map[string]any{"success": allOK, "operation": operation, "results": results}
	if !allOK {
		fullPayload["errorCode"] = "partial_failure"
		fullPayload["errorDetail"] = "一个或多个已绑定 Kubernetes 连接未能完成读取"
	}
	canonical, err := json.Marshal(fullPayload)
	if err != nil {
		return err
	}
	var artifactID int64
	payload := fullPayload
	if len(canonical) > 64*1024 {
		workspace := filepath.Join(runner.Config.WorkspaceRoot, fmt.Sprintf("attempt-%d", attemptID))
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			return err
		}
		path := filepath.Join(workspace, fmt.Sprintf("kubernetes-read-%d.json", toolCallID))
		if err := os.WriteFile(path, canonical, 0o600); err != nil {
			return err
		}
		defer os.Remove(path)
		artifactID, err = runner.uploadWorkspaceFileAs(ctx, attemptID, toolCallID, path, "application/json")
		if err != nil {
			return err
		}
		previewResults := make([]map[string]any, 0, len(results))
		for _, result := range results {
			preview := make(map[string]any, len(result))
			for key, value := range result {
				preview[key] = value
			}
			if output, ok := preview["output"].(string); ok && len(output) > 4096 {
				preview["output"] = output[:4096]
				preview["truncated"] = true
			}
			previewResults = append(previewResults, preview)
		}
		payload = map[string]any{"success": allOK, "operation": operation, "results": previewResults, "artifact": map[string]any{"id": fmt.Sprint(artifactID), "mediaType": "application/json"}}
		if !allOK {
			payload["errorCode"] = "partial_failure"
			payload["errorDetail"] = "一个或多个已绑定 Kubernetes 连接未能完成读取"
		}
	}
	return runner.commitTypedTool(ctx, writer, attemptID, toolCallID, "kubernetes_read_result_v1", payload, artifactID)
}

// failTypedTool seals a failed supervisor_typed tool with the
// return_to_model failure shape and relays the committed result.
func (runner *Runner) failTypedTool(ctx context.Context, writer *FrameWriter, attemptID, toolCallID int64, toolName, errorCode, errorDetail string) error {
	payload := map[string]any{"success": false, "errorCode": errorCode, "errorDetail": errorDetail}
	// The frozen wire contract binds payload.schema_kind to the tool
	// definition (runtime.proto ResultPayload): a structured failure of a
	// tool carries that tool's own result schema kind, never a generic one.
	return runner.commitTypedTool(ctx, writer, attemptID, toolCallID, typedResultSchemaKind(toolName), payload, 0)
}

// typedResultSchemaKind maps one supervisor_typed tool to its frozen
// result schema kind (success and failure payloads share the kind).
func typedResultSchemaKind(toolName string) string {
	return toolName + "_result_v1"
}

// commitTypedTool seals the typed result through CompleteToolCall and
// sends the committed ToolResult (evidence ids and artifact ref included)
// to the worker (ARCH-TOOL-003, RUNTIME-AGENT-008: the worker only ever
// consumes the committed payload).
func (runner *Runner) commitTypedTool(ctx context.Context, writer *FrameWriter, attemptID, toolCallID int64, schemaKind string, payload map[string]any, artifactID int64) error {
	canonical, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	outcome := runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED
	if success, _ := payload["success"].(bool); !success {
		outcome = runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_FAILED
	}
	reply, err := runner.toolCallChannel().Request(ctx, &runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg: &runtimev1.ControlEnvelope_CompleteToolCall{CompleteToolCall: &runtimev1.CompleteToolCall{
			AttemptId: attemptID, ToolCallId: toolCallID, Outcome: outcome,
			Payload: &runtimev1.ResultPayload{
				SchemaKind: schemaKind, CanonicalJson: canonical, ContentDigest: digest[:],
			},
			ArtifactId: artifactID, ErrorCode: payloadText(payload, "errorCode"), ErrorDetail: payloadText(payload, "errorDetail"),
		}},
	})
	if err != nil {
		return err
	}
	ack := reply.GetCompleteToolCallAck()
	if ack == nil || !ack.GetAccepted() {
		return fmt.Errorf("complete typed tool %d rejected: %s", toolCallID, ack.GetDetail())
	}
	committed := canonical
	if committedPayload := ack.GetCommittedPayload(); committedPayload != nil && len(committedPayload.GetCanonicalJson()) > 0 {
		committed = committedPayload.GetCanonicalJson()
	}
	toolResult := &workerv1.ToolResult{
		ToolCallId: toolCallID, Success: outcome == runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED,
		ResultJson: committed, ErrorCode: payloadText(payload, "errorCode"), ErrorDetail: payloadText(payload, "errorDetail"),
		EvidenceIds: ack.GetEvidenceIds(),
	}
	if ref := ack.GetArtifactRef(); ref != nil && ref.GetArtifactId() != 0 {
		toolResult.ArtifactRef = &workerv1.WorkerArtifactRef{
			ArtifactId: ref.GetArtifactId(), Role: ref.GetRole(), MediaType: ref.GetMediaType(),
			SizeBytes: ref.GetSizeBytes(), Sha256: ref.GetSha256(), BodyExpired: ref.GetBodyExpired(),
		}
	}
	return writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_ToolResult{ToolResult: toolResult}})
}

// commitLocalTool uploads a spilled workspace output when present and
// seals the tool call (ARCH-OUTPUT-001/005, ARCH-TOOL-003). Spilled bodies
// carry the bounded tail preview plus the artifact locator in the
// committed payload — the frozen wire oneof cannot inline both, so the
// supervisor derives the preview from the file it uploads
// (ARCH-OUTPUT-003: the model context gets the locator and bounded
// facts, never the full body).
func (runner *Runner) commitLocalTool(ctx context.Context, writer *FrameWriter, attemptID int64, local *workerv1.LocalToolCompleted) error {
	var artifactID int64
	preview := string(local.GetInlineOutput())
	if local.GetWorkspaceOutputPath() != "" {
		uploaded, err := runner.uploadWorkspaceFile(ctx, attemptID, local.GetToolCallId(), local.GetWorkspaceOutputPath())
		if err != nil {
			return err
		}
		artifactID = uploaded
		if tail, tailErr := fileTail(local.GetWorkspaceOutputPath(), tailPreviewBytes); tailErr == nil {
			preview = "…（完整输出已存入 Artifact）\n" + tail
		}
	}
	payload := map[string]any{"success": local.GetSuccess()}
	if local.GetSuccess() {
		payload["output"] = preview
	} else {
		payload["errorCode"] = local.GetErrorCode()
		payload["errorDetail"] = local.GetErrorDetail()
	}
	if artifactID != 0 {
		payload["truncated"] = true
		payload["artifact"] = map[string]any{"id": fmt.Sprint(artifactID), "mediaType": local.GetMediaType()}
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	outcome := runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED
	if !local.GetSuccess() {
		outcome = runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_FAILED
	}
	reply, err := runner.toolCallChannel().Request(ctx, &runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg: &runtimev1.ControlEnvelope_CompleteToolCall{CompleteToolCall: &runtimev1.CompleteToolCall{
			AttemptId: attemptID, ToolCallId: local.GetToolCallId(), Outcome: outcome,
			Payload: &runtimev1.ResultPayload{
				SchemaKind: "workspace_tool_result_v1", CanonicalJson: canonical, ContentDigest: digest[:],
			},
			ArtifactId: artifactID, ErrorCode: local.GetErrorCode(), ErrorDetail: local.GetErrorDetail(),
		}},
	})
	if err != nil {
		return err
	}
	ack := reply.GetCompleteToolCallAck()
	if ack == nil || !ack.GetAccepted() {
		return fmt.Errorf("complete tool call %d rejected: %s", local.GetToolCallId(), ack.GetDetail())
	}
	// ToolResult carries the committed model-visible preview, the artifact
	// ref and the deterministic evidence ids (ARCH-TOOL-003).
	toolResult := &workerv1.ToolResult{
		ToolCallId: local.GetToolCallId(), Success: local.GetSuccess(),
		ResultJson: canonical, ErrorCode: local.GetErrorCode(), ErrorDetail: local.GetErrorDetail(),
		EvidenceIds: ack.GetEvidenceIds(),
	}
	if artifactID != 0 {
		toolResult.ArtifactRef = &workerv1.WorkerArtifactRef{ArtifactId: artifactID}
	}
	if err := writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_ToolResult{ToolResult: toolResult}}); err != nil {
		return err
	}
	if local.GetWorkspaceOutputPath() != "" {
		return os.Remove(local.GetWorkspaceOutputPath())
	}
	return nil
}

// uploadWorkspaceFile streams one spilled workspace output into a
// tool_result Artifact (RUNTIME-UPLOAD-001..006, RUNTIME-ARTIFACT-001).
func (runner *Runner) uploadWorkspaceFile(ctx context.Context, attemptID, toolCallID int64, path string) (int64, error) {
	return runner.uploadWorkspaceFileAs(ctx, attemptID, toolCallID, path, "text/plain")
}

// artifactContext attaches the runtime long-term bearer to one
// ArtifactService RPC. The channel dial carries no per-RPC credentials
// (FetchCredentialGrant attaches the same bearer explicitly); every
// Upload/ReadText/GrepText call must present it or the server's upload
// fence refuses the stream (RUNTIME-GRANT-001).
func (runner *Runner) artifactContext(ctx context.Context) (context.Context, error) {
	bearer, err := runner.toolCallChannel().BearerToken()
	if err != nil {
		return nil, err
	}
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+bearer)), nil
}

// uploadWorkspaceFileAs streams one spilled workspace output with the
// caller's media type (the thanos spill is application/json).
func (runner *Runner) uploadWorkspaceFileAs(ctx context.Context, attemptID, toolCallID int64, path, mediaType string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	uploadID := fmt.Sprintf("tool-%d-%d", attemptID, toolCallID)
	uploadCtx, err := runner.artifactContext(ctx)
	if err != nil {
		return 0, err
	}
	stream, err := runner.Artifacts.Upload(uploadCtx)
	if err != nil {
		return 0, err
	}
	header := &runtimev1.ArtifactUploadHeader{
		UploadId: uploadID, AttemptId: attemptID,
		BootId: runner.Sink.BootID(), ConnectionEpoch: runner.Sink.Epoch(),
		OwnerType: "tool_call", OwnerId: toolCallID,
		Kind:          runtimev1.ArtifactKind_ARTIFACT_KIND_TOOL_RESULT,
		RetentionKind: runtimev1.RetentionKind_RETENTION_KIND_GENERATED,
		Sensitive:     false, SizeBytes: uint64(size), Sha256: hash.Sum(nil),
		MediaType: mediaType,
	}
	// Every send failure funnels into the authoritative disposition: the
	// server may reject the header, finish at the byte count, or close the
	// stream — the Result (or the status error) is what decides, never the
	// local send outcome (RUNTIME-UPLOAD-001).
	settle := func(sendErr error) (int64, error) {
		result, recvErr := stream.CloseAndRecv()
		if recvErr != nil {
			return 0, fmt.Errorf("upload stream failed: %w (send: %v)", recvErr, sendErr)
		}
		if !result.GetCommitted() {
			return 0, fmt.Errorf("artifact upload rejected: %s", result.GetRejectReason())
		}
		return result.GetArtifactId(), nil
	}
	if err := stream.Send(&runtimev1.ArtifactUploadFrame{Frame: &runtimev1.ArtifactUploadFrame_Header{Header: header}}); err != nil {
		return settle(err)
	}
	buffer := make([]byte, 64*1024)
	var offset uint64
	for {
		n, readErr := file.Read(buffer)
		if n > 0 {
			if err := stream.Send(&runtimev1.ArtifactUploadFrame{Frame: &runtimev1.ArtifactUploadFrame_Chunk{Chunk: &runtimev1.ArtifactUploadChunk{
				Offset: offset, Payload: buffer[:n],
			}}}); err != nil {
				return settle(err)
			}
			offset += uint64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, readErr
		}
	}
	if err := stream.Send(&runtimev1.ArtifactUploadFrame{Frame: &runtimev1.ArtifactUploadFrame_End{End: &runtimev1.ArtifactUploadEnd{UploadedSizeBytes: offset}}}); err != nil {
		return settle(err)
	}
	result, err := stream.CloseAndRecv()
	if err != nil {
		return 0, err
	}
	if !result.GetCommitted() {
		return 0, fmt.Errorf("artifact upload rejected: %s", result.GetRejectReason())
	}
	return result.GetArtifactId(), nil
}

// tailPreviewBytes bounds the spilled-result preview tail.
const tailPreviewBytes = 16 * 1024

// fileTail reads the bounded tail of one file (best effort; the committed
// payload falls back to the marker alone when the read fails).
func fileTail(path string, limit int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	var offset int64
	if info.Size() > limit {
		offset = info.Size() - limit
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	body, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func parseLocator(value string) (int64, error) {
	var id int64
	_, err := fmt.Sscanf(value, "%d", &id)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("artifactId 必须是十进制 locator")
	}
	return id, nil
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return "(无匹配)"
	}
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	return body
}

func payloadText(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}
