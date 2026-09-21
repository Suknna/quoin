// Tool execution on the supervisor (ARCH-WORKER-003, ARCH-TOOL-002/003;
// ADR-0011): the pending->running begin fence 与授权裁决不变,执行则分成
// 两条路径——worker_local 工具仍由 worker 沙箱自执行(LocalToolCompleted
// -> commitLocalTool -> CompleteToolCall),quoin_routed 工具 Plinth 只转发
// 不执行:BeginToolCallAck accepted 后等待 Quoin 推送的 ExternalToolResult
// 帧并原样组装 worker 的 ToolResult(Quoin 已自行封存,Plinth 不再发送
// CompleteToolCall)。
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	workerv1 "github.com/Suknna/quoin/internal/gen/proto/plinth/worker/v1"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	plinthruntime "github.com/Suknna/quoin/internal/plinth/runtime"
)

// executeTool persists pending->running and answers ToolCallStarted
// (ARCH-TOOL-002)。worker_local 工具到此返回,由 worker 在沙箱内执行;
// quoin_routed 工具(artifact_read/artifact_grep/thanos_query 等)在
// accepted 之后阻塞等待 Quoin 的封存结果(ADR-0011)。
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
	// 词表外的模式在发帧前 fail fast:继续下去 worker 只会等一个永不到来的
	// ToolResult(ADR-0011 之后合法值只有 worker_local/quoin_routed)。
	modeValue, known := runtimev1.ToolExecutionMode_value[meta.mode]
	if !known || modeValue == 0 {
		return fmt.Errorf("tool call %d carries unsupported execution mode %q", toolCallID, meta.mode)
	}
	mode := runtimev1.ToolExecutionMode(modeValue)
	if err := writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_ToolCallStarted{
		ToolCallStarted: &workerv1.ToolCallStarted{ToolCallId: toolCallID, ExecutionMode: mode},
	}}); err != nil {
		return err
	}
	switch meta.mode {
	case "TOOL_EXECUTION_MODE_WORKER_LOCAL":
		return nil
	case "TOOL_EXECUTION_MODE_QUOIN_ROUTED":
		return runner.awaitQuoinRoutedTool(ctx, writer, attemptID, toolCallID)
	default:
		return fmt.Errorf("tool call %d carries unsupported execution mode %q", toolCallID, meta.mode)
	}
}

// awaitQuoinRoutedTool 等待 Quoin 对一个 QUOIN_ROUTED 工具调用的封存结果
// (ExternalToolResult,ADR-0011)并组装 worker 的 ToolResult 帧:
//   - SUCCEEDED:result_json 即 payload.canonical_json(已封存的 committed
//     payload),携带 artifact 引用与确定性 evidence ids;
//   - FAILED/CANCELLED:组装失败形态(result_json 优先用 Quoin 的 payload,
//     缺失时本地合成 return_to_model 失败形状),worker 侧已有处理;
//   - 等待超时(ExternalResultTimeout):Quoin 已封存但结果帧在首发与重连
//     补发后仍未送达——合成确定性失败结果让 agent 循环继续,而不是无限
//     阻塞 attempt;账实以 Quoin ledger 为准,本帧只是模型可见的收敛形状。
//
// CompleteToolCall 不由 Plinth 发送——Quoin 已是这类工具的权威封存方。
// 等待期间 attempt 取消或 worker 退出时,ctx 结束会清理 waiter 并放弃
// 结果(Quoin 侧封存事实不受影响)。
func (runner *Runner) awaitQuoinRoutedTool(ctx context.Context, writer *FrameWriter, attemptID, toolCallID int64) error {
	result, err := runner.externalResultSource().AwaitExternalToolResult(ctx, toolCallID)
	if err != nil {
		if !errors.Is(err, plinthruntime.ErrExternalToolResultTimeout) {
			return err
		}
		toolResult := &workerv1.ToolResult{
			ToolCallId: toolCallID, Success: false,
			ErrorCode:   "result_delivery_timeout",
			ErrorDetail: "Quoin 已封存该调用,但结果帧在等待上限内未送达(首发与重连补发均未覆盖);真实封存结果以 Quoin 时间线为准",
		}
		toolResult.ResultJson, _ = json.Marshal(map[string]any{
			"success": false, "errorCode": toolResult.ErrorCode, "errorDetail": toolResult.ErrorDetail,
		})
		return writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_ToolResult{ToolResult: toolResult}})
	}
	toolResult := &workerv1.ToolResult{
		ToolCallId: toolCallID,
		Success:    result.GetOutcome() == runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED,
		ErrorCode:  result.GetErrorCode(), ErrorDetail: result.GetErrorDetail(),
		EvidenceIds: result.GetEvidenceIds(),
	}
	if payload := result.GetPayload(); payload != nil && len(payload.GetCanonicalJson()) > 0 {
		toolResult.ResultJson = payload.GetCanonicalJson()
	} else if !toolResult.Success {
		// 失败帧可能不带 payload:合成模型可见的失败形状,保证 tool 消息
		// 恒为合法 JSON 对象(与冻结的 return_to_model 契约一致)。
		toolResult.ResultJson, _ = json.Marshal(map[string]any{
			"success": false, "errorCode": result.GetErrorCode(), "errorDetail": result.GetErrorDetail(),
		})
	}
	if ref := result.GetArtifactRef(); ref != nil && ref.GetArtifactId() != 0 {
		// artifact 引用按现有 ToolResult 帧的 artifact 字段语义映射。
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

// artifactContext is the ArtifactService RPC context. The channel dial
// itself is mTLS-authenticated (ADR-0009); Upload/ReadText/GrepText carry
// the connection identity and no per-RPC bearer exists.
func (runner *Runner) artifactContext(ctx context.Context) context.Context {
	return ctx
}

// uploadWorkspaceFileAs streams one spilled workspace output with the
// caller's media type.
func (runner *Runner) uploadWorkspaceFileAs(ctx context.Context, attemptID, toolCallID int64, path, mediaType string) (int64, error) {
	if runner.uploadWorkspaceFileForTest != nil {
		return runner.uploadWorkspaceFileForTest(ctx, attemptID, toolCallID, path, mediaType)
	}
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
	uploadCtx := runner.artifactContext(ctx)
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
