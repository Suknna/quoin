package worker

// Worker process entry (ARCH-WORKER-001/002/006): one fresh process per
// attempt. stdin/stdout carry the framed protobuf protocol; stderr carries
// bounded non-secret diagnostics. The worker builds Eino message/tool
// semantics, drives the sequential loop and proposes the final domain
// output; every model/tool execution happens on the supervisor.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	workerv1 "github.com/Suknna/quoin/internal/gen/proto/plinth/worker/v1"
	"github.com/Suknna/quoin/internal/plinth/agent"
	"github.com/cloudwego/eino/schema"
)

type Config struct {
	WorkspaceDir   string
	SupervisorPID  int
	ReadOnlyPaths  []string
	ExecutablePath string
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
}

// OutputSchemaKind is the frozen result payload schema identifier for a
// successful initial analysis (mirrors the Quoin-side constant).
const OutputSchemaKind = "initial_analysis_output_v1"

// InvestigationOutputSchemaKind is the frozen result payload schema
// identifier for a successful investigation turn (mirrors the Quoin-side
// investigation.OutputSchemaKind).
const InvestigationOutputSchemaKind = "investigation_output_v1"

// maxModelRetries bounds the worker-side physical retry budget per logical
// call (ARCH-AGENT-004: only transport-class failures with no visible
// chunk may retry).
const maxModelRetries = 3

// attemptMode pins the frozen per-attempt-type contract: input schema,
// agent generation, system prompt, message assembly and result payload
// schema (DATA-ATTEMPT-001: attempt_type + "_v1").
type attemptMode struct {
	schemaKind       string
	agentVersion     string
	outputSchemaKind string
	prompt           string
	buildMessages    func(canonical []byte) ([]*schema.Message, error)
}

var initialAnalysisMode = attemptMode{
	schemaKind:       "initial_analysis_v1",
	agentVersion:     WorkerAgentVersion,
	outputSchemaKind: OutputSchemaKind,
	prompt:           agent.SystemPrompt,
	buildMessages: func(canonical []byte) ([]*schema.Message, error) {
		input, err := agent.ParseInput(canonical)
		if err != nil {
			return nil, err
		}
		return agent.BuildInitialMessages(input)
	},
}

var investigationMode = attemptMode{
	schemaKind:       "investigation_v1",
	agentVersion:     WorkerInvestigationAgentVersion,
	outputSchemaKind: InvestigationOutputSchemaKind,
	prompt:           agent.InvestigationSystemPrompt,
	buildMessages: func(canonical []byte) ([]*schema.Message, error) {
		input, err := agent.ParseInvestigationInput(canonical)
		if err != nil {
			return nil, err
		}
		return agent.BuildInvestigationMessages(input)
	},
}

// Run drives one attempt to a terminal outcome. Exit code 0 means the
// attempt concluded normally (result proposed or cancelled); exit code 1
// means a protocol/sandbox failure the supervisor maps to a structured
// technical failure.
func Run(ctx context.Context, config Config) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := os.MkdirAll(config.WorkspaceDir, 0o700); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if config.ExecutablePath == "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		config.ExecutablePath = executable
	}
	// Fail-closed sandbox before any attempt input is processed
	// (ARCH-WORKER-007/008). Landlock and the seccomp filter restrict the
	// CALLING THREAD only, and the Go runtime has already spawned extra
	// threads (sysmon) that never receive the restriction; a goroutine
	// migrating to one of them would fork unsandboxed children — observed
	// as a nondeterministic /tmp write escape. Pinning the run loop to the
	// thread that establishes the sandbox makes every child fork
	// (bash/grep) deterministically inherit the restriction; the worker is
	// a one-shot process, so the lock lives with it.
	runtime.LockOSThread()
	if err := Establish(Sandbox{
		WorkspaceDir: config.WorkspaceDir, SupervisorPID: config.SupervisorPID,
		ReadOnlyPaths: config.ReadOnlyPaths, ExecutablePath: config.ExecutablePath,
	}); err != nil {
		fmt.Fprintf(config.Stderr, "sandbox_unavailable: %v\n", err)
		return err
	}
	reader := NewFrameReader(config.Stdin)
	writer := NewFrameWriter(config.Stdout)
	start, err := reader.Read()
	if err != nil {
		return fmt.Errorf("%w: first frame: %v", ErrProtocol, err)
	}
	startAttempt := start.GetStartAttempt()
	if startAttempt == nil {
		return fmt.Errorf("%w: first frame must be StartAttempt", ErrProtocol)
	}
	attemptID := start.GetAttemptId()
	mode, err := verifyStart(startAttempt)
	if err != nil {
		_ = writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_StartAttemptAck{
			StartAttemptAck: &workerv1.StartAttemptAck{Accepted: false, Detail: err.Error()},
		}})
		return err
	}
	if err := writer.Send(&workerv1.WorkerEnvelope{AttemptId: attemptID, Msg: &workerv1.WorkerEnvelope_StartAttemptAck{
		StartAttemptAck: &workerv1.StartAttemptAck{Accepted: true},
	}}); err != nil {
		return err
	}
	return runLoop(ctx, config, reader, writer, attemptID, startAttempt, mode)
}

// verifyStart enforces the frozen input contract (ARCH-WORKER-006) and
// resolves the attempt mode.
func verifyStart(start *workerv1.StartAttempt) (attemptMode, error) {
	var mode attemptMode
	switch start.GetSchemaKind() {
	case initialAnalysisMode.schemaKind:
		mode = initialAnalysisMode
	case investigationMode.schemaKind:
		mode = investigationMode
	default:
		return attemptMode{}, fmt.Errorf("unsupported schema_kind %q", start.GetSchemaKind())
	}
	if start.GetAgentVersion() != mode.agentVersion {
		return attemptMode{}, fmt.Errorf("agent version mismatch: worker %s, dispatch %s", mode.agentVersion, start.GetAgentVersion())
	}
	sum := sha256.Sum256(start.GetCanonicalJson())
	if hex.EncodeToString(sum[:]) != hex.EncodeToString(start.GetContentDigest()) {
		return attemptMode{}, errors.New("input content digest mismatch")
	}
	return mode, nil
}

func runLoop(ctx context.Context, config Config, reader *FrameReader, writer *FrameWriter, attemptID int64, start *workerv1.StartAttempt, mode attemptMode) error {
	messages, err := mode.buildMessages(start.GetCanonicalJson())
	if err != nil {
		fmt.Fprintf(config.Stderr, "input_rejected: %v\n", err)
		return err
	}
	toolsJSON, err := ProviderToolsJSON(start.GetAgentVersion())
	if err != nil {
		return err
	}
	toolsDigest := sha256.Sum256(toolsJSON)
	callSeq := uint32(0)
	inputItems := initialInputItems(mode.prompt, start)
	// Deterministic Evidence and Artifact ids accumulate from the sealed
	// Tool Results (Quoin commits both before ToolResult reaches the
	// worker); the final proposal references them explicitly
	// (DATA-ANALYSIS-002).
	var evidenceIDs []int64
	var artifactIDs []int64
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		callSeq++
		// Physical retry loop inside one logical call (ARCH-AGENT-004).
		var (
			modelCallID    int64
			assistantText  string
			responseDigest []byte
			prepared       []agent.PreparedCall
		)
		retrySeq := uint32(0)
		for {
		resendRequest:
			messagesJSON, err := marshalMessages(messages)
			if err != nil {
				return err
			}
			messagesDigest := sha256.Sum256(messagesJSON)
			request := &workerv1.WorkerEnvelope{
				AttemptId: attemptID,
				Msg: &workerv1.WorkerEnvelope_ChatModelRequest{ChatModelRequest: &workerv1.ChatModelRequest{
					CallSeq:      callSeq,
					MessagesJson: messagesJSON, ToolsJson: toolsJSON,
					MessagesDigest: messagesDigest[:], ToolsDigest: toolsDigest[:],
					InputItems: wireInputItems(inputItems), EvictedTurnCount: 0,
				}},
			}
			if err := writer.Send(request); err != nil {
				return err
			}
			// Chunks may arrive before ChatModelStarted (the supervisor
			// streams as soon as the ledger row exists); they are transient
			// and never carry durable identity, so skip until Started/Failed.
			for {
				envelope, err := reader.Read()
				if err != nil {
					return err
				}
				if envelope.GetAttemptId() != attemptID {
					return fmt.Errorf("%w: attempt id mismatch", ErrProtocol)
				}
				switch message := envelope.Msg.(type) {
				case *workerv1.WorkerEnvelope_ChatModelStarted:
					modelCallID = message.ChatModelStarted.GetModelCallId()
					goto startedReceived
				case *workerv1.WorkerEnvelope_ChatModelChunk:
					// Transient display delta; skip.
				case *workerv1.WorkerEnvelope_ChatModelFailed:
					if message.ChatModelFailed.GetRetryable() && retrySeq+1 < maxModelRetries {
						retrySeq++
						fmt.Fprintf(config.Stderr, "model_retry: %s\n", message.ChatModelFailed.GetReason())
						goto resendRequest
					}
					fmt.Fprintf(config.Stderr, "model_failed: %s %s\n", message.ChatModelFailed.GetReason(), message.ChatModelFailed.GetDetail())
					return fmt.Errorf("model call failed: %s", message.ChatModelFailed.GetReason())
				case *workerv1.WorkerEnvelope_CancelWorker:
					return acknowledgeCancel(writer, attemptID)
				default:
					return fmt.Errorf("%w: expected ChatModelStarted/Failed", ErrProtocol)
				}
			}
		startedReceived:
			// Collect the remaining chunks until completion (stream deltas
			// are transient; the durable text arrives in ChatModelCompleted).
			for {
				envelope, err := reader.Read()
				if err != nil {
					return err
				}
				switch message := envelope.Msg.(type) {
				case *workerv1.WorkerEnvelope_ChatModelChunk:
					// Transient display delta; the worker does not persist it.
				case *workerv1.WorkerEnvelope_ChatModelCompleted:
					completed := message.ChatModelCompleted
					assistantText = completed.GetAssistantText()
					responseDigest = completed.GetResponseDigest()
					for _, call := range completed.GetToolCalls() {
						prepared = append(prepared, agent.PreparedCall{
							ToolCallID: call.GetToolCallId(), ProviderIndex: call.GetProviderIndex(),
							ProviderToolCallID: call.GetProviderToolCallId(), ToolName: call.GetToolName(),
							ArgumentsJSON: call.GetArgumentsJson(),
							ExecutionMode: executionModeName(call.GetExecutionMode()),
							FailureMode:   failureModeName(call.GetFailureMode()),
						})
					}
					goto completed
				case *workerv1.WorkerEnvelope_CancelWorker:
					return acknowledgeCancel(writer, attemptID)
				default:
					return fmt.Errorf("%w: unexpected frame during stream", ErrProtocol)
				}
			}
		completed:
			break
		}
		// Final answer: no tool calls → propose the immutable output.
		if len(prepared) == 0 {
			canonical, err := json.Marshal(assistantText)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(canonical)
			if err := writer.Send(&workerv1.WorkerEnvelope{
				AttemptId: attemptID,
				Msg: &workerv1.WorkerEnvelope_WorkerResultProposal{WorkerResultProposal: &workerv1.WorkerResultProposal{
					SchemaKind: mode.outputSchemaKind, CanonicalJson: canonical, ContentDigest: digest[:],
					EvidenceIds: evidenceIDs, ArtifactIds: artifactIDs,
				}},
			}); err != nil {
				return err
			}
			ack, err := reader.Read()
			if err != nil {
				return err
			}
			if ack.GetWorkerResultAck() == nil || !ack.GetWorkerResultAck().GetAccepted() {
				return fmt.Errorf("%w: result rejected: %s", ErrProtocol, ack.GetWorkerResultAck().GetDetail())
			}
			return nil
		}
		// Reconstruct the assistant tool-call message (ARCH-AGENT-007) and
		// execute the prepared calls strictly in order (ARCH-TOOL-002).
		messages = append(messages, agent.AssistantToolCallMessage(assistantText, prepared))
		inputItems = append(inputItems, priorCallItem(modelCallID, responseDigest))
		for _, call := range prepared {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			if err := writer.Send(&workerv1.WorkerEnvelope{
				AttemptId: attemptID,
				Msg:       &workerv1.WorkerEnvelope_ExecuteToolCall{ExecuteToolCall: &workerv1.ExecuteToolCall{ToolCallId: call.ToolCallID}},
			}); err != nil {
				return err
			}
			envelope, err := reader.Read()
			if err != nil {
				return err
			}
			var started *workerv1.ToolCallStarted
			switch message := envelope.Msg.(type) {
			case *workerv1.WorkerEnvelope_ToolCallStarted:
				started = message.ToolCallStarted
			case *workerv1.WorkerEnvelope_CancelWorker:
				return acknowledgeCancel(writer, attemptID)
			default:
				return fmt.Errorf("%w: expected ToolCallStarted", ErrProtocol)
			}
			if started.GetToolCallId() != call.ToolCallID {
				return fmt.Errorf("%w: ToolCallStarted id mismatch", ErrProtocol)
			}
			if started.GetExecutionMode().String() == "TOOL_EXECUTION_MODE_WORKER_LOCAL" {
				// Execute locally and report the raw outcome; the
				// supervisor commits it and answers ToolResult.
				var arguments map[string]any
				if err := json.Unmarshal(call.ArgumentsJSON, &arguments); err != nil {
					return err
				}
				result := ExecuteLocal(ctx, call.ToolName, arguments, config.WorkspaceDir)
				local := &workerv1.LocalToolCompleted{
					ToolCallId: call.ToolCallID, Success: result.Success,
					MediaType: result.MediaType, ErrorCode: result.ErrorCode, ErrorDetail: result.ErrorDetail,
				}
				if result.WorkspacePath != "" {
					local.Body = &workerv1.LocalToolCompleted_WorkspaceOutputPath{WorkspaceOutputPath: result.WorkspacePath}
				} else if result.Success {
					local.Body = &workerv1.LocalToolCompleted_InlineOutput{InlineOutput: []byte(result.Preview)}
				}
				if err := writer.Send(&workerv1.WorkerEnvelope{
					AttemptId: attemptID,
					Msg:       &workerv1.WorkerEnvelope_LocalToolCompleted{LocalToolCompleted: local},
				}); err != nil {
					return err
				}
			}
			// Both execution modes converge on ToolResult (supervisor_typed
			// tools are executed by the supervisor after ToolCallStarted).
			envelope, err = reader.Read()
			if err != nil {
				return err
			}
			var toolResult *workerv1.ToolResult
			switch message := envelope.Msg.(type) {
			case *workerv1.WorkerEnvelope_ToolResult:
				toolResult = message.ToolResult
			case *workerv1.WorkerEnvelope_CancelWorker:
				return acknowledgeCancel(writer, attemptID)
			default:
				return fmt.Errorf("%w: expected ToolResult", ErrProtocol)
			}
			if toolResult.GetToolCallId() != call.ToolCallID {
				return fmt.Errorf("%w: ToolResult id mismatch", ErrProtocol)
			}
			resultDigest := sha256.Sum256(toolResult.GetResultJson())
			messages = append(messages, agent.ToolResultMessage(call.ProviderToolCallID, call.ToolName, toolResult.GetResultJson()))
			inputItems = append(inputItems, toolResultItem(call.ToolCallID, resultDigest[:]))
			// Quoin committed the Evidence and the Artifact before the
			// ToolResult arrived (ARCH-TOOL-003); the final output must
			// reference exactly those sealed objects.
			evidenceIDs = append(evidenceIDs, toolResult.GetEvidenceIds()...)
			if artifactRef := toolResult.GetArtifactRef(); artifactRef != nil && artifactRef.GetArtifactId() != 0 {
				artifactIDs = append(artifactIDs, artifactRef.GetArtifactId())
			}
		}
	}
}

// acknowledgeCancel confirms the local stop (ARCH-WORKER-006 cancel path).
func acknowledgeCancel(writer *FrameWriter, attemptID int64) error {
	return writer.Send(&workerv1.WorkerEnvelope{
		AttemptId: attemptID,
		Msg:       &workerv1.WorkerEnvelope_WorkerCancelled{WorkerCancelled: &workerv1.WorkerCancelled{Detail: "worker stopped"}},
	})
}

// marshalMessages renders Eino messages to the canonical wire JSON.
func marshalMessages(messages []*schema.Message) ([]byte, error) {
	return json.Marshal(messages)
}

func executionModeName(mode interface{ String() string }) string {
	return mode.String()
}

func failureModeName(mode interface{ String() string }) string {
	return mode.String()
}
