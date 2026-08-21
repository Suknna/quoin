package worker

// Attempt runner (ARCH-WORKER-001/002/006): the supervisor spawns one
// fresh worker process per attempt, drives the framed stdio protocol and
// bridges it to the Quoin control stream. The supervisor holds all
// credentials; the worker only ever sees the frozen non-secret input, its
// workspace and the fixed tool schema.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	workerv1 "github.com/Suknna/quoin/internal/gen/proto/plinth/worker/v1"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/plinth/model"
	plinthruntime "github.com/Suknna/quoin/internal/plinth/runtime"
)

// RunnerConfig carries the supervisor-side inputs for one attempt.
type RunnerConfig struct {
	WorkspaceRoot string
	ModelContract model.Contract
}

// Runner executes one dispatched initial-analysis attempt.
type Runner struct {
	Sink      *plinthruntime.FrameSink
	Channel   *plinthruntime.Channel
	Client    runtimev1.RuntimeControlClient
	Artifacts runtimev1.ArtifactServiceClient
	Config    RunnerConfig
	// retries tracks the physical retry sequence per logical call (the
	// worker re-sends ChatModelRequest after a retryable failure).
	retries map[uint32]uint32
	// tools tracks the durable identity of each prepared tool call
	// (toolCallID -> tool name + execution mode).
	tools map[int64]toolMeta
}

type toolMeta struct {
	name      string
	mode      string
	arguments map[string]any
}

// retrySeqOf returns the current physical retry index of one logical call.
func (runner *Runner) retrySeqOf(callSeq uint32) uint32 {
	if runner.retries == nil {
		runner.retries = map[uint32]uint32{}
	}
	return runner.retries[callSeq]
}

// advanceRetry records that one retryable failure was reported for the
// logical call; the worker re-sends the same call with the next retry_seq.
func (runner *Runner) advanceRetry(callSeq uint32) {
	if runner.retries == nil {
		runner.retries = map[uint32]uint32{}
	}
	runner.retries[callSeq]++
}

// Run drives the worker process to a terminal result proposal.
func (runner *Runner) Run(parent context.Context, attemptID int64, dispatch *runtimev1.DispatchAttempt) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	runner.Channel.RegisterTask(attemptID, cancel)
	workspaceDir := filepath.Join(runner.Config.WorkspaceRoot, fmt.Sprintf("attempt-%d", attemptID))
	if err := os.MkdirAll(workspaceDir, 0o700); err != nil {
		runner.proposeFailure(attemptID, "sandbox_unavailable", "workspace create failed: "+err.Error())
		return
	}
	defer func() {
		// Attempt terminal: the one-shot workspace is destroyed
		// (ARCH-WORKER-001).
		if err := os.RemoveAll(workspaceDir); err != nil {
			sharedops.LogEvent("plinth", "warn", "worker.workspace_cleanup", err.Error())
		}
	}()
	if err := runner.runWorker(ctx, attemptID, dispatch, workspaceDir); err != nil {
		// The worker already proposed a result on the normal paths; a
		// protocol-level exit maps to a structured technical failure.
		sharedops.LogEvent("plinth", "error", "worker.run_failed", fmt.Sprintf("attempt=%d %v", attemptID, err))
		runner.proposeFailure(attemptID, "worker_protocol_error", err.Error())
	}
}

// runWorker spawns the fresh worker process and bridges its frames.
func (runner *Runner) runWorker(ctx context.Context, attemptID int64, dispatch *runtimev1.DispatchAttempt, workspaceDir string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	process := exec.CommandContext(ctx, executable, "worker",
		"--workspace", workspaceDir,
		"--supervisor-pid", fmt.Sprint(os.Getpid()),
	)
	// The worker environment is cleared: only stdio survives
	// (ARCH-WORKER-002/007).
	process.Env = nil
	stdin, err := process.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := process.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := process.StderrPipe()
	if err != nil {
		return err
	}
	if err := process.Start(); err != nil {
		return fmt.Errorf("spawn worker: %w", err)
	}
	sharedops.LogEvent("plinth", "info", "worker.started", fmt.Sprintf("attempt=%d pid=%d", attemptID, process.Process.Pid))
	go drainStderr(stderr, attemptID)
	writer := NewFrameWriter(stdin)
	reader := NewFrameReader(stdout)
	// StartAttempt is the supervisor's first frame (ARCH-WORKER-006).
	input := dispatch.GetInput()
	if input == nil {
		process.Process.Kill()
		return errors.New("dispatch carries no input snapshot")
	}
	start := &workerv1.StartAttempt{
		Mode:       workerv1.WorkMode_WORK_MODE_INITIAL_ANALYSIS,
		SchemaKind: input.GetSchemaKind(), CanonicalJson: input.GetCanonicalJson(),
		ContentDigest: input.GetContentDigest(), AgentVersion: input.GetAgentVersion(),
	}
	for _, ref := range input.GetArtifactRefs() {
		start.ArtifactRefs = append(start.ArtifactRefs, &workerv1.WorkerArtifactRef{
			ArtifactId: ref.GetArtifactId(), Role: ref.GetRole(), MediaType: ref.GetMediaType(),
			SizeBytes: ref.GetSizeBytes(), Sha256: ref.GetSha256(), BodyExpired: ref.GetBodyExpired(),
		})
	}
	toolsDigest, err := ProviderToolsDigest()
	if err != nil {
		process.Process.Kill()
		return err
	}
	decoded, err := hex.DecodeString(toolsDigest)
	if err != nil {
		process.Process.Kill()
		return err
	}
	start.ToolSchemaDigest = decoded
	if err := writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_StartAttempt{StartAttempt: start}}); err != nil {
		process.Process.Kill()
		return err
	}
	// The supervisor reads the sandbox self-check ack before anything else.
	first, err := reader.Read()
	if err != nil {
		process.Process.Kill()
		return err
	}
	ack := first.GetStartAttemptAck()
	if ack == nil {
		process.Process.Kill()
		return errors.New("worker first reply is not StartAttemptAck")
	}
	if !ack.GetAccepted() {
		process.Process.Kill()
		return fmt.Errorf("sandbox_unavailable: %s", ack.GetDetail())
	}
	// The sandbox proved itself: accept the attempt as Running.
	if err := runner.Sink.Send(&runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg:           &runtimev1.ControlEnvelope_AttemptAccept{AttemptAccept: &runtimev1.AttemptAccept{AttemptId: attemptID}},
	}); err != nil {
		process.Process.Kill()
		return err
	}
	executor := &model.Executor{Sink: runner.Sink, Channel: runner.Channel, Client: runner.Client}
	// Bridge loop: the worker proposes; the supervisor persists via Quoin
	// and answers; the loop ends at WorkerResultProposal or cancel.
	resultProposed := false
	defer func() {
		if !resultProposed {
			// Worker exited without proposing: the supervisor closes the
			// attempt as a structured technical failure.
			select {
			case <-ctx.Done():
				return
			default:
			}
			runner.proposeFailure(attemptID, "worker_protocol_error", "worker exited without a result")
		}
	}()
	for {
		envelope, err := reader.Read()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			return fmt.Errorf("worker frame: %w", err)
		}
		switch message := envelope.Msg.(type) {
		case *workerv1.WorkerEnvelope_ChatModelRequest:
			request := message.ChatModelRequest
			started := time.Now()
			chunkSeq := uint64(0)
			executor.DeltaHook = func(hookCtx context.Context, delta string) error {
				chunkSeq++
				sendErr := writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_ChatModelChunk{
					ChatModelChunk: &workerv1.ChatModelChunk{ModelCallId: 0, ChunkSeq: chunkSeq, Text: delta},
				}})
				if sendErr == nil {
					// Transient observer delta on the control stream (never
					// authoritative, RUNTIME-AGENT: token delta).
					_ = runner.Sink.Send(&runtimev1.ControlEnvelope{
						CorrelationId: uint64(attemptID),
						Msg: &runtimev1.ControlEnvelope_ModelTokenDelta{ModelTokenDelta: &runtimev1.ModelTokenDelta{
							AttemptId: attemptID, DeltaSeq: chunkSeq, Text: delta,
						}},
					})
				}
				return sendErr
			}
			callID, assistantText, proposed, authorizations, responseDigest, failure, execErr := executor.Execute(ctx, attemptID,
				request.GetCallSeq(), runner.retrySeqOf(request.GetCallSeq()), request.GetMessagesJson(), request.GetToolsJson(),
				mapInputItems(request.GetInputItems()), request.GetEvictedTurnCount(), runner.Config.ModelContract)
			_ = started
			executor.DeltaHook = nil
			if execErr != nil && failure == nil {
				return fmt.Errorf("model executor: %w", execErr)
			}
			if failure != nil {
				if failure.Retryable {
					runner.advanceRetry(request.GetCallSeq())
				}
				if err := writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_ChatModelFailed{
					ChatModelFailed: &workerv1.ChatModelFailed{
						ModelCallId: failure.ModelCallID, Reason: failure.Reason,
						Detail: failure.Detail, Retryable: failure.Retryable,
						ProviderRequestId: failure.ProviderRequestID,
					},
				}}); err != nil {
					return err
				}
				continue
			}
			if err := writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_ChatModelStarted{
				ChatModelStarted: &workerv1.ChatModelStarted{ModelCallId: callID, CallSeq: request.GetCallSeq(), RetrySeq: runner.retrySeqOf(request.GetCallSeq())},
			}}); err != nil {
				return err
			}
			// Chunks already flowed during the stream; the completion
			// carries the durable payload with the ledger tool ids and the
			// raw response digest for the worker's next-turn lineage.
			completed := &workerv1.ChatModelCompleted{
				ModelCallId: callID, AssistantText: assistantText, ResponseDigest: responseDigest,
				Usage: &workerv1.ModelUsage{},
			}
			for _, authorization := range authorizations {
				var matched *model.ProposedTool
				for index := range proposed {
					if proposed[index].ProviderIndex == authorization.ProviderIndex {
						matched = &proposed[index]
						break
					}
				}
				if matched == nil {
					return fmt.Errorf("authorization index %d has no proposed tool", authorization.ProviderIndex)
				}
				mode := ExecutionModeFor(matched.ToolName)
				if runner.tools == nil {
					runner.tools = map[int64]toolMeta{}
				}
				var arguments map[string]any
				_ = json.Unmarshal(matched.ArgumentsJSON, &arguments)
				runner.tools[authorization.ToolCallID] = toolMeta{name: matched.ToolName, mode: mode, arguments: arguments}
				completed.ToolCalls = append(completed.ToolCalls, &workerv1.PreparedToolCall{
					ToolCallId: authorization.ToolCallID, ProviderIndex: authorization.ProviderIndex,
					ProviderToolCallId: matched.ProviderToolCallID,
					ToolName:           matched.ToolName, ArgumentsJson: matched.ArgumentsJSON,
					ArgumentsDigest: []byte(matched.ArgumentsDigest),
					ExecutionMode:   runtimev1.ToolExecutionMode(runtimev1.ToolExecutionMode_value[mode]),
					FailureMode:     runtimev1.ToolFailureMode(runtimev1.ToolFailureMode_value[authorization.FailureMode]),
				})
			}
			if err := writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_ChatModelCompleted{
				ChatModelCompleted: completed,
			}}); err != nil {
				return err
			}
		case *workerv1.WorkerEnvelope_ExecuteToolCall:
			execute := message.ExecuteToolCall
			meta, known := runner.tools[execute.GetToolCallId()]
			if !known {
				return fmt.Errorf("tool call %d has no prepared metadata", execute.GetToolCallId())
			}
			if err := runner.executeTool(ctx, writer, attemptID, execute.GetToolCallId(), meta); err != nil {
				return err
			}
		case *workerv1.WorkerEnvelope_LocalToolCompleted:
			local := message.LocalToolCompleted
			if err := runner.commitLocalTool(ctx, writer, attemptID, local); err != nil {
				return err
			}
		case *workerv1.WorkerEnvelope_WorkerResultProposal:
			proposal := message.WorkerResultProposal
			if err := runner.commitResult(ctx, attemptID, proposal); err != nil {
				return err
			}
			resultProposed = true
			if err := writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_WorkerResultAck{
				WorkerResultAck: &workerv1.WorkerResultAck{Accepted: true},
			}}); err != nil {
				return err
			}
			return nil
		case *workerv1.WorkerEnvelope_WorkerCancelled:
			_ = runner.Sink.Send(&runtimev1.ControlEnvelope{
				CorrelationId: uint64(attemptID),
				Msg:           &runtimev1.ControlEnvelope_CancelAck{CancelAck: &runtimev1.CancelAck{AttemptId: attemptID}},
			})
			return nil
		case *workerv1.WorkerEnvelope_WorkerLog:
			sharedops.LogEvent("plinth", "info", "worker.log", message.WorkerLog.GetMessage())
		default:
			return fmt.Errorf("worker sent an unexpected frame")
		}
	}
}

// executeTool persists pending->running and answers ToolCallStarted
// (ARCH-TOOL-002); supervisor_typed tools (artifact_read/artifact_grep)
// execute on the supervisor through the Attempt-scoped ArtifactService and
// converge on ToolResult directly (ARCH-OUTPUT-004).
func (runner *Runner) executeTool(ctx context.Context, writer *FrameWriter, attemptID, toolCallID int64, meta toolMeta) error {
	reply, err := runner.Channel.Request(ctx, &runtimev1.ControlEnvelope{
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

// executeTypedTool runs one supervisor_typed tool (artifact_read or
// artifact_grep) and seals it through CompleteToolCall; the committed
// ToolResult returns to the worker immediately (ARCH-TOOL-002/003).
func (runner *Runner) executeTypedTool(ctx context.Context, writer *FrameWriter, attemptID, toolCallID int64, toolName string) error {
	args, err := runner.toolArguments(ctx, attemptID, toolCallID)
	if err != nil {
		return err
	}
	artifactIDText, _ := args["artifactId"].(string)
	artifactID, err := parseLocator(artifactIDText)
	if err != nil {
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, "invalid_arguments", err.Error())
	}
	var payload map[string]any
	schemaKind := toolName + "_result_v1"
	switch toolName {
	case "artifact_read":
		startLine := int64(1)
		maxLines := int64(2000)
		if offset, ok := args["offset"].(float64); ok && offset >= 1 {
			startLine = int64(offset)
		}
		if limit, ok := args["limit"].(float64); ok && limit >= 1 && limit <= 2000 {
			maxLines = int64(limit)
		}
		response, rpcErr := runner.Artifacts.ReadText(ctx, &runtimev1.ArtifactReadTextRequest{
			AttemptId: attemptID, ArtifactId: artifactID, BootId: runner.Sink.BootID(),
			ConnectionEpoch: runner.Sink.Epoch(), StartLine: uint64(startLine), MaxLines: uint32(maxLines),
		})
		if rpcErr != nil {
			return runner.failTypedTool(ctx, writer, attemptID, toolCallID, "artifact_read_failed", rpcErr.Error())
		}
		payload = map[string]any{
			"success": true, "output": string(response.GetContent()),
			"startLine": response.GetStartLine(), "nextLine": response.GetNextLine(), "eof": response.GetEof(),
			"totalBytes": response.GetTotalSizeBytes(), "totalLines": response.GetTotalLines(),
			"artifact": map[string]any{"id": fmt.Sprint(response.GetArtifactId()), "mediaType": response.GetMediaType()},
		}
	case "artifact_grep":
		pattern, _ := args["pattern"].(string)
		if pattern == "" {
			return runner.failTypedTool(ctx, writer, attemptID, toolCallID, "invalid_arguments", "pattern 必须是非空字符串")
		}
		response, rpcErr := runner.Artifacts.GrepText(ctx, &runtimev1.ArtifactGrepTextRequest{
			AttemptId: attemptID, ArtifactId: artifactID, BootId: runner.Sink.BootID(),
			ConnectionEpoch: runner.Sink.Epoch(), Re2Pattern: pattern,
			MaxMatches: 200, ContextLines: 5,
		})
		if rpcErr != nil {
			return runner.failTypedTool(ctx, writer, attemptID, toolCallID, "artifact_grep_failed", rpcErr.Error())
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
	default:
		return runner.failTypedTool(ctx, writer, attemptID, toolCallID, "unknown_tool", "supervisor tool "+toolName+" is not in the fixed catalog")
	}
	return runner.commitTypedTool(ctx, writer, attemptID, toolCallID, schemaKind, payload)
}

// failTypedTool seals a failed supervisor_typed tool with the
// return_to_model failure shape and relays the committed result.
func (runner *Runner) failTypedTool(ctx context.Context, writer *FrameWriter, attemptID, toolCallID int64, errorCode, errorDetail string) error {
	payload := map[string]any{"success": false, "errorCode": errorCode, "errorDetail": errorDetail}
	return runner.commitTypedTool(ctx, writer, attemptID, toolCallID, "typed_tool_result_v1", payload)
}

// commitTypedTool seals the typed result through CompleteToolCall and
// sends the committed ToolResult to the worker.
func (runner *Runner) commitTypedTool(ctx context.Context, writer *FrameWriter, attemptID, toolCallID int64, schemaKind string, payload map[string]any) error {
	canonical, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	outcome := runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED
	if success, _ := payload["success"].(bool); !success {
		outcome = runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_FAILED
	}
	reply, err := runner.Channel.Request(ctx, &runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg: &runtimev1.ControlEnvelope_CompleteToolCall{CompleteToolCall: &runtimev1.CompleteToolCall{
			AttemptId: attemptID, ToolCallId: toolCallID, Outcome: outcome,
			Payload: &runtimev1.ResultPayload{
				SchemaKind: schemaKind, CanonicalJson: canonical, ContentDigest: digest[:],
			},
			ErrorCode: payloadText(payload, "errorCode"), ErrorDetail: payloadText(payload, "errorDetail"),
		}},
	})
	if err != nil {
		return err
	}
	ack := reply.GetCompleteToolCallAck()
	if ack == nil || !ack.GetAccepted() {
		return fmt.Errorf("complete typed tool %d rejected: %s", toolCallID, ack.GetDetail())
	}
	return writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_ToolResult{
		ToolResult: &workerv1.ToolResult{
			ToolCallId: toolCallID, Success: outcome == runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED,
			ResultJson: canonical, ErrorCode: payloadText(payload, "errorCode"), ErrorDetail: payloadText(payload, "errorDetail"),
		},
	}})
}

// commitLocalTool uploads a spilled workspace output when present and
// seals the tool call (ARCH-OUTPUT-001/005, ARCH-TOOL-003).
func (runner *Runner) commitLocalTool(ctx context.Context, writer *FrameWriter, attemptID int64, local *workerv1.LocalToolCompleted) error {
	var artifactID int64
	if local.GetWorkspaceOutputPath() != "" {
		uploaded, err := runner.uploadWorkspaceFile(ctx, attemptID, local.GetToolCallId(), local.GetWorkspaceOutputPath())
		if err != nil {
			return err
		}
		artifactID = uploaded
	}
	payload := map[string]any{"success": local.GetSuccess()}
	if local.GetSuccess() {
		payload["output"] = string(local.GetInlineOutput())
	} else {
		payload["errorCode"] = local.GetErrorCode()
		payload["errorDetail"] = local.GetErrorDetail()
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
	reply, err := runner.Channel.Request(ctx, &runtimev1.ControlEnvelope{
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
	// ToolResult carries the committed model-visible preview and the
	// artifact ref (ARCH-TOOL-003).
	toolResult := &workerv1.ToolResult{
		ToolCallId: local.GetToolCallId(), Success: local.GetSuccess(),
		ResultJson: canonical, ErrorCode: local.GetErrorCode(), ErrorDetail: local.GetErrorDetail(),
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
	stream, err := runner.Artifacts.Upload(ctx)
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
		MediaType: "text/plain",
	}
	if err := stream.Send(&runtimev1.ArtifactUploadFrame{Frame: &runtimev1.ArtifactUploadFrame_Header{Header: header}}); err != nil {
		return 0, err
	}
	buffer := make([]byte, 64*1024)
	var offset uint64
	for {
		n, readErr := file.Read(buffer)
		if n > 0 {
			if err := stream.Send(&runtimev1.ArtifactUploadFrame{Frame: &runtimev1.ArtifactUploadFrame_Chunk{Chunk: &runtimev1.ArtifactUploadChunk{
				Offset: offset, Payload: buffer[:n],
			}}}); err != nil {
				return 0, err
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
		return 0, err
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

// commitResult proposes the worker's final domain output to Quoin and
// relays the adjudication (RUNTIME-TASK-008/012).
func (runner *Runner) commitResult(ctx context.Context, attemptID int64, proposal *workerv1.WorkerResultProposal) error {
	reply, err := runner.Channel.Request(ctx, &runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg: &runtimev1.ControlEnvelope_ResultProposal{ResultProposal: &runtimev1.ResultProposal{
			AttemptId: attemptID, BootId: runner.Sink.BootID(), ConnectionEpoch: runner.Sink.Epoch(),
			Outcome: runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
			Payload: &runtimev1.ResultPayload{
				SchemaKind: proposal.GetSchemaKind(), CanonicalJson: proposal.GetCanonicalJson(),
				ContentDigest: proposal.GetContentDigest(), EvidenceIds: proposal.GetEvidenceIds(),
				ArtifactIds: proposal.GetArtifactIds(),
			},
		}},
	})
	if err != nil {
		return err
	}
	ack := reply.GetResultAck()
	if ack == nil {
		return errors.New("result ack missing")
	}
	if !ack.GetAccepted() {
		return fmt.Errorf("result rejected: %s", ack.GetDetail())
	}
	return nil
}

// proposeFailure closes the attempt as a structured technical failure.
func (runner *Runner) proposeFailure(attemptID int64, reason, detail string) {
	_ = runner.Sink.Send(&runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg: &runtimev1.ControlEnvelope_ResultProposal{ResultProposal: &runtimev1.ResultProposal{
			AttemptId: attemptID, BootId: runner.Sink.BootID(), ConnectionEpoch: runner.Sink.Epoch(),
			Outcome:           runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED,
			TerminationReason: runtimev1.TerminationReason(runtimev1.TerminationReason_value[reason]),
			Payload: &runtimev1.ResultPayload{
				SchemaKind: "initial_analysis_output_v1", CanonicalJson: []byte(`""`),
			},
		}},
	})
	sharedops.LogEvent("plinth", "info", "worker.proposed_failure", fmt.Sprintf("attempt=%d reason=%s detail=%s", attemptID, reason, detail))
}

// drainStderr forwards bounded worker diagnostics.
func drainStderr(stderr io.Reader, attemptID int64) {
	buffer := make([]byte, 4096)
	for {
		n, err := stderr.Read(buffer)
		if n > 0 {
			sharedops.LogEvent("plinth", "info", "worker.stderr", fmt.Sprintf("attempt=%d %s", attemptID, string(buffer[:n])))
		}
		if err != nil {
			return
		}
	}
}

// mapInputItems converts the worker wire lineage to the runtime wire shape
// (the enums are shared; only the Go types differ).
func mapInputItems(items []*workerv1.WorkerModelInputItem) []*runtimev1.ModelInputItem {
	var mapped []*runtimev1.ModelInputItem
	for _, item := range items {
		mapped = append(mapped, &runtimev1.ModelInputItem{
			Sequence: item.GetSequence(), ItemKind: item.GetItemKind(),
			ItemId: item.GetItemId(), ContentDigest: item.GetContentDigest(),
			Role: item.GetRole(),
		})
	}
	return mapped
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
