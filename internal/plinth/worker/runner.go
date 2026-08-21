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
	"strings"
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
	Binding   plinthruntime.DispatchBinding
	Config    RunnerConfig
	// retries tracks the physical retry sequence per logical call (the
	// worker re-sends ChatModelRequest after a retryable failure).
	retries map[uint32]uint32
	// tools tracks the durable identity of each prepared tool call
	// (toolCallID -> tool name + execution mode).
	tools map[int64]toolMeta
	// lastModelFailure carries the most recent structured model-call
	// failure class so a worker exit without a result proposes the attempt
	// failure with the provider's real reason instead of a generic
	// protocol error (RUNTIME-TASK-009).
	lastModelFailure string
}

type toolMeta struct {
	name      string
	mode      string
	arguments map[string]any
	grants    []*runtimev1.ConnectionGrant
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
	// Every exit path stops and reaps the worker process: a protocol
	// error must never leak the sandboxed child (ARCH-WORKER-001: one
	// fresh workspace per attempt, destroyed with the process).
	defer func() {
		_ = process.Process.Kill()
		_, _ = process.Process.Wait()
	}()
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
	executor := &model.Executor{Sink: runner.Sink, Channel: runner.Channel, Client: runner.Client, Binding: runner.Binding}
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
				runner.lastModelFailure = failure.Reason
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
				runner.tools[authorization.ToolCallID] = toolMeta{name: matched.ToolName, mode: mode, arguments: arguments, grants: authorization.ConnectionGrants}
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
			ack, err := runner.commitResult(ctx, attemptID, proposal)
			switch {
			case err != nil:
				return err
			case ack != nil && !ack.GetAccepted():
				// Quoin adjudicated the attempt terminal through another
				// path (cancellation fence or loss convergence won the commit
				// race): the worker's result is a late result — audit only. The
				// worker is answered deterministically and the attempt is NOT
				// re-proposed (DATA-ATTEMPT-004).
				sharedops.LogEvent("plinth", "info", "worker.result_late", fmt.Sprintf("attempt=%d detail=%s", attemptID, ack.GetDetail()))
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

// commitResult proposes the worker's final domain output through the
// channel's reliable delivery and returns the adjudication ack
// (RUNTIME-TASK-008/012). The frozen dispatch binding identifies the
// attempt on Quoin even after same-boot reconnects; a rejected ack means
// the attempt was already terminal through another commit-order winner.
func (runner *Runner) commitResult(ctx context.Context, attemptID int64, proposal *workerv1.WorkerResultProposal) (*runtimev1.ResultAck, error) {
	ack, err := runner.Channel.ProposeResult(ctx, &runtimev1.ResultProposal{
		AttemptId:       attemptID,
		BootId:          runner.Binding.BootID,
		ConnectionEpoch: runner.Binding.Epoch,
		Outcome:         runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
		Payload: &runtimev1.ResultPayload{
			SchemaKind: proposal.GetSchemaKind(), CanonicalJson: proposal.GetCanonicalJson(),
			ContentDigest: proposal.GetContentDigest(), EvidenceIds: proposal.GetEvidenceIds(),
			ArtifactIds: proposal.GetArtifactIds(),
		},
	})
	if err != nil {
		return nil, err
	}
	return ack, nil
}

// proposeFailure registers a structured technical failure as the
// attempt's terminal result for reliable delivery: the frozen binding
// identifies the attempt, and a provider failure class that already
// sealed a model call maps to the attempt's termination reason instead of
// a generic protocol error (RUNTIME-TASK-009).
func (runner *Runner) proposeFailure(attemptID int64, reason, detail string) {
	if mapped := attemptTerminationFor(runner.lastModelFailure); mapped != "" {
		reason = mapped
	}
	canonical := []byte(`""`)
	digest := sha256.Sum256(canonical)
	runner.Channel.RegisterResult(&runtimev1.ResultProposal{
		AttemptId:         attemptID,
		BootId:            runner.Binding.BootID,
		ConnectionEpoch:   runner.Binding.Epoch,
		Outcome:           runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED,
		TerminationReason: terminationEnum(reason),
		Payload: &runtimev1.ResultPayload{
			SchemaKind: "initial_analysis_output_v1", CanonicalJson: canonical,
			ContentDigest: digest[:],
		},
	})
	sharedops.LogEvent("plinth", "info", "worker.proposed_failure", fmt.Sprintf("attempt=%d reason=%s detail=%s", attemptID, reason, detail))
}

// attemptTerminationFor maps one sealed model-call failure class onto the
// attempt's closed termination reasons (the SQL enum carries no
// transport_error; a transport-class provider failure is the provider
// being unavailable from the attempt's viewpoint).
func attemptTerminationFor(modelFailure string) string {
	switch modelFailure {
	case "timeout":
		return "timeout"
	case "rate_limited":
		return "rate_limited"
	case "invalid_response":
		return "invalid_response"
	case "context_overflow":
		return "context_too_large"
	case "transport_error", "provider_unavailable":
		return "provider_unavailable"
	default:
		return ""
	}
}

// terminationEnum resolves one stable termination-reason name to its wire
// enum (the value map keys are the UPPER_SNAKE enum names).
func terminationEnum(reason string) runtimev1.TerminationReason {
	if value, ok := runtimev1.TerminationReason_value["TERMINATION_REASON_"+strings.ToUpper(reason)]; ok && value != 0 {
		return runtimev1.TerminationReason(value)
	}
	return runtimev1.TerminationReason_TERMINATION_REASON_WORKER_PROTOCOL_ERROR
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
