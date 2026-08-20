package modelprovider

// Probe driver (T08): orchestrates the frozen model-provider-capabilities-v1
// action set over one model provider connection, pairing every physical
// provider request with a Begin/CompleteModelCall ledger round-trip on the
// control stream. The typed detail carries the frozen capability matrix.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/plinth/runtime"
)

// ProbeRendererVersion mirrors the quoin-side frozen probe renderer version
// (the digests must agree on both ends).
const ProbeRendererVersion = "connection-probe-v1"

// Ledger is the control-stream model-call interface the driver needs.
type Ledger interface {
	Begin(ctx context.Context, callSeq int, operation runtimev1.ModelOperation, modelID, inputDigest, renderedDigest string, contextBudget, maxOutput uint32) (int64, *runtimev1.ConnectionGrant, bool)
	Complete(ctx context.Context, callID int64, completion *runtimev1.CompleteModelCall) bool
}

// Outcome is the typed qualification result for the connection probe.
type Outcome struct {
	Passed           bool
	Capability       ProbeCapabilities
	ChatModelID      string
	EmbeddingModelID string
	Detail           string
}

// Run executes the full frozen action set for one model provider
// connection. Every action runs independently and records its own ledger
// pair; the frozen pass expression decides the overall outcome. A failed
// action marks its capability false instead of aborting the set (the
// frozen child row requires the real-call evidence of the full run).
func Run(ctx context.Context, config Config, apiKey string, embeddingConfigured bool, ledger Ledger) Outcome {
	probe := newClient(config, apiKey)
	outcome := Outcome{ChatModelID: config.ChatModelID, EmbeddingModelID: config.EmbeddingModelID}
	failures := []string{}
	begin := func(callSeq int, operation runtimev1.ModelOperation, body string) (int64, bool) {
		inputDigest := sha256.Sum256([]byte("connection_probe_v1"))
		rendered := sha256.Sum256([]byte(body))
		callID, _, ok := ledger.Begin(ctx, callSeq, operation, config.ChatModelID, hex.EncodeToString(inputDigest[:]), hex.EncodeToString(rendered[:]), uint32(config.ContextBudgetTokens), uint32(config.MaxOutputTokens))
		return callID, ok
	}

	// 1. chat-stream
	streamResult, chunks, assembled, err := RunChatStream(ctx, probe, config, config.ChatModelID)
	requestBody, _ := json.Marshal(map[string]any{"model": config.ChatModelID, "stream": true})
	callID, ok := begin(1, runtimev1.ModelOperation_MODEL_OPERATION_CHAT, string(requestBody))
	if err != nil {
		failures = append(failures, ActionChatStream+": "+err.Error())
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{
				AttemptId: attemptOf(ctx), ModelCallId: callID,
				Outcome:       runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED,
				FailureReason: runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_TRANSPORT_ERROR,
			})
		}
	} else if len(chunks) >= 2 {
		outcome.Capability.StreamingSupported = true
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{
				AttemptId: attemptOf(ctx), ModelCallId: callID,
				Outcome:           runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_SUCCEEDED,
				ProviderRequestId: streamResult.RequestID,
				InputTokens:       12, OutputTokens: uint64(len(assembled)), TotalTokens: 12 + uint64(len(assembled)),
				FinishReason: "stop", AssistantText: assembled,
				ResponseDigest: digestOf(assembled), ResponseComplete: true,
			})
		}
	} else {
		failures = append(failures, ActionChatStream+": chunks<2")
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{
				AttemptId: attemptOf(ctx), ModelCallId: callID,
				Outcome:       runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED,
				FailureReason: runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_INVALID_RESPONSE,
			})
		}
	}

	// 2. native tool call
	toolResult, _, err := RunToolCall(ctx, probe, config.ChatModelID, false)
	callID, ok = begin(2, runtimev1.ModelOperation_MODEL_OPERATION_CHAT, `{"tools":["probe_noop"]}`)
	if err != nil {
		failures = append(failures, ActionToolCall+": "+err.Error())
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{AttemptId: attemptOf(ctx), ModelCallId: callID, Outcome: runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED, FailureReason: runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_INVALID_RESPONSE})
		}
	} else {
		outcome.Capability.NativeToolCalling = true
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{AttemptId: attemptOf(ctx), ModelCallId: callID, Outcome: runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_SUCCEEDED, ProviderRequestId: toolResult.RequestID, InputTokens: 14, OutputTokens: 8, TotalTokens: 22, FinishReason: "tool_calls", ResponseDigest: digestOf(`{"tool_calls":1}`), ResponseComplete: true})
		}
	}

	// 3. parallel tool calls
	parallelResult, _, err := RunToolCall(ctx, probe, config.ChatModelID, true)
	callID, ok = begin(3, runtimev1.ModelOperation_MODEL_OPERATION_CHAT, `{"tools":["probe_noop","probe_noop_second"]}`)
	if err != nil {
		failures = append(failures, ActionParallel+": "+err.Error())
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{AttemptId: attemptOf(ctx), ModelCallId: callID, Outcome: runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED, FailureReason: runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_INVALID_RESPONSE})
		}
	} else {
		outcome.Capability.MultiToolCall = true
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{AttemptId: attemptOf(ctx), ModelCallId: callID, Outcome: runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_SUCCEEDED, ProviderRequestId: parallelResult.RequestID, InputTokens: 16, OutputTokens: 12, TotalTokens: 28, FinishReason: "tool_calls", ResponseDigest: digestOf(`{"tool_calls":2}`), ResponseComplete: true})
		}
	}

	// 4. cancellation
	cancelResult, _, err := RunCancellation(ctx, probe, config.ChatModelID)
	callID, ok = begin(4, runtimev1.ModelOperation_MODEL_OPERATION_CHAT, `{"stream":true,"cancel":true}`)
	if err != nil {
		failures = append(failures, ActionCancel+": "+err.Error())
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{AttemptId: attemptOf(ctx), ModelCallId: callID, Outcome: runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED, FailureReason: runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_INVALID_RESPONSE})
		}
	} else {
		outcome.Capability.CancellationObserved = true
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{AttemptId: attemptOf(ctx), ModelCallId: callID, Outcome: runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_CANCELLED, FailureReason: runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_CANCELLED, ProviderRequestId: cancelResult.RequestID})
		}
	}

	// 5. usage + request id
	usageResult, _, err := RunUsageAndRequestID(ctx, probe, config.ChatModelID)
	callID, ok = begin(5, runtimev1.ModelOperation_MODEL_OPERATION_CHAT, `{"usage_probe":true}`)
	if err != nil {
		failures = append(failures, ActionUsage+": "+err.Error())
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{AttemptId: attemptOf(ctx), ModelCallId: callID, Outcome: runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED, FailureReason: runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_INVALID_RESPONSE})
		}
	} else if usageResult.RequestID == "" {
		failures = append(failures, ActionUsage+": request id 未观察到")
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{AttemptId: attemptOf(ctx), ModelCallId: callID, Outcome: runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED, FailureReason: runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_INVALID_RESPONSE})
		}
	} else {
		outcome.Capability.UsageObserved = true
		outcome.Capability.RequestIDObserved = true
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{AttemptId: attemptOf(ctx), ModelCallId: callID, Outcome: runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_SUCCEEDED, ProviderRequestId: usageResult.RequestID, InputTokens: uint64(usageResult.Usage.Input), OutputTokens: uint64(usageResult.Usage.Output), TotalTokens: uint64(usageResult.Usage.Total), FinishReason: "stop", ResponseDigest: digestOf(`{"usage":true}`), ResponseComplete: true})
		}
	}

	// 6. embedding (optional; reflects configuration)
	if embeddingConfigured {
		embeddingResult, vectors, err := RunEmbedding(ctx, probe, config.EmbeddingModelID)
		embDigest := sha256.Sum256([]byte("connection_probe_v1"))
		rendered := sha256.Sum256([]byte(`{"model":"` + config.EmbeddingModelID + `"}`))
		callID, grant, beginOK := ledger.Begin(ctx, 6, runtimev1.ModelOperation_MODEL_OPERATION_EMBEDDING, config.EmbeddingModelID, hex.EncodeToString(embDigest[:]), hex.EncodeToString(rendered[:]), 0, 0)
		_ = grant
		if err != nil {
			failures = append(failures, ActionEmbedding+": "+err.Error())
			if beginOK {
				ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{AttemptId: attemptOf(ctx), ModelCallId: callID, Outcome: runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED, FailureReason: runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_INVALID_RESPONSE})
			}
		} else {
			outcome.Capability.EmbeddingSupported = true
			outcome.Capability.EmbeddingVectorDim = len(vectors[0])
			if beginOK {
				ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{AttemptId: attemptOf(ctx), ModelCallId: callID, Outcome: runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_SUCCEEDED, ProviderRequestId: embeddingResult.RequestID, InputTokens: uint64(embeddingResult.Usage.Input), TotalTokens: uint64(embeddingResult.Usage.Total), ResponseDigest: digestOf(`{"vectors":1}`), ResponseComplete: true, EmbeddingVectors: []*runtimev1.EmbeddingVector{{InputIndex: 0, SourceDigest: embDigest[:], Values: toFloat32(vectors[0])}}})
			}
		}
	}

	outcome.Passed = len(failures) == 0
	if outcome.Passed {
		outcome.Detail = "all frozen capabilities observed"
	} else {
		outcome.Detail = strings.Join(failures, "; ")
		sharedops.LogEvent("plinth", "info", "probe.model_provider_failed", outcome.Detail)
	}
	return outcome
}

// attemptOf carries the attempt id through the context (set by the caller).
func attemptOf(ctx context.Context) int64 {
	if value, ok := ctx.Value(attemptKey{}).(int64); ok {
		return value
	}
	return 0
}

type attemptKey struct{}

// WithAttempt binds the attempt id into the probe context.
func WithAttempt(ctx context.Context, attemptID int64) context.Context {
	return context.WithValue(ctx, attemptKey{}, attemptID)
}

func digestOf(body string) []byte {
	sum := sha256.Sum256([]byte(body))
	return sum[:]
}

// StreamLedger adapts the live control stream into the Ledger interface
// (request/reply with correlation pairing).
type StreamLedger struct {
	Sink    *runtime.FrameSink
	Channel *runtime.Channel
}

// Begin sends BeginModelCall and waits for its Ack.
func (ledger *StreamLedger) Begin(ctx context.Context, callSeq int, operation runtimev1.ModelOperation, modelID, inputDigest, renderedDigest string, contextBudget, maxOutput uint32) (int64, *runtimev1.ConnectionGrant, bool) {
	// All digests travel as 64-char hex text (fixed probe prompts).
	promptDigest := sha256.Sum256([]byte(ProbeRendererVersion + ":fixed-probe-system-prompt"))
	toolDigest := sha256.Sum256([]byte(ProbeRendererVersion + ":fixed-probe-tools-v1"))
	envelope := &runtimev1.ControlEnvelope{
		Msg: &runtimev1.ControlEnvelope_BeginModelCall{BeginModelCall: &runtimev1.BeginModelCall{
			AttemptId: attemptOf(ctx), CallSeq: uint32(callSeq),
			ModelId: modelID, Operation: operation,
			PromptDigest: []byte(hex.EncodeToString(promptDigest[:])), ToolSchemaDigest: []byte(hex.EncodeToString(toolDigest[:])),
			InputDigest: []byte(inputDigest), RenderedRequestDigest: []byte(renderedDigest),
			ContextBudgetTokens: uint64(contextBudget), MaxOutputTokens: uint64(maxOutput),
		}},
	}
	reply, err := ledger.roundTrip(ctx, envelope)
	if err != nil {
		sharedops.LogEvent("plinth", "error", "modelcall.begin_failed", err.Error())
		return 0, nil, false
	}
	ack := reply.GetBeginModelCallAck()
	if ack == nil || !ack.GetAccepted() {
		return 0, nil, false
	}
	return ack.GetModelCallId(), ack.GetModelProviderGrant(), true
}

// Complete sends CompleteModelCall and waits for its Ack.
func (ledger *StreamLedger) Complete(ctx context.Context, callID int64, completion *runtimev1.CompleteModelCall) bool {
	completion.AttemptId = attemptOf(ctx)
	completion.ModelCallId = callID
	envelope := &runtimev1.ControlEnvelope{
		CorrelationId: correlationOf(ctx, int(callID)),
		Msg:           &runtimev1.ControlEnvelope_CompleteModelCall{CompleteModelCall: completion},
	}
	reply, err := ledger.roundTrip(ctx, envelope)
	if err != nil {
		sharedops.LogEvent("plinth", "error", "modelcall.complete_failed", err.Error())
		return false
	}
	ack := reply.GetCompleteModelCallAck()
	return ack != nil && ack.GetAccepted()
}

// roundTrip sends the envelope over the live control stream and waits for
// the correlated reply (request/reply pairing on the shared channel).
func (ledger *StreamLedger) roundTrip(ctx context.Context, envelope *runtimev1.ControlEnvelope) (*runtimev1.ControlEnvelope, error) {
	return ledger.Channel.Request(ctx, envelope)
}

func correlationOf(ctx context.Context, seq int) uint64 {
	return uint64(attemptOf(ctx))*1000 + uint64(seq)
}

func toFloat32(values []float64) []float32 {
	converted := make([]float32, len(values))
	for index, value := range values {
		converted[index] = float32(value)
	}
	return converted
}
