// Package model is the supervisor-side ChatModel executor (ARCH-AGENT-001/
// 005): it consumes the worker's canonical Eino messages/tools JSON, runs
// the provider call through the frozen Eino OpenAI-compatible adapter with
// the attempt-scoped API key injected in memory only, streams visible
// deltas back to the worker, and seals the physical Model Call through the
// Begin/Complete ledger on the control stream. Hidden reasoning never
// leaves the adapter boundary.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/plinth/agent"
	"github.com/Suknna/quoin/internal/plinth/runtime"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/schema"
	"google.golang.org/grpc/metadata"
)

// Contract is the frozen chat contract of one attempt (ARCH-AGENT-003).
type Contract struct {
	ModelID       string
	BaseURL       string
	APIKey        string
	ContextBudget int
	MaxOutput     int
	Streaming     bool
}

// ProposedTool is one provider tool call to seal (mirrors the Quoin-side
// attempt.ProposedTool; a unit test pins the canonical digests equal).
type ProposedTool struct {
	ProviderIndex      uint32
	ProviderToolCallID string
	ToolName           string
	ArgumentsJSON      []byte
	ArgumentsDigest    string
}

// Executor drives one physical model call over the live control stream.
type Executor struct {
	Sink    *runtime.FrameSink
	Channel *runtime.Channel
	Client  runtimev1.RuntimeControlClient
	// DeltaHook receives visible stream deltas (wired by the runner to the
	// worker's ChatModelChunk and the control-stream ModelTokenDelta).
	DeltaHook func(ctx context.Context, delta string) error
}

// Failure is a structured model-call failure for the worker.
type Failure struct {
	Reason            string // stable machine reason for the worker log
	Detail            string
	Retryable         bool
	ProviderRequestID string
	ModelCallID       int64 // 0 when no ledger row exists
}

// Authorization is the durable identity of one pending tool call the
// Quoin ledger assigned (CompleteModelCallAck, ARCH-TOOL-001).
type Authorization struct {
	ToolCallID         int64
	ProviderIndex      uint32
	ProviderToolCallID string
	FailureMode        string
}

// Execute runs one logical chat call: BeginModelCall → provider stream →
// CompleteModelCall. It returns the durable call id, the visible assistant
// text, the proposed tool calls and their ledger authorizations on
// success; on failure it returns the structured Failure for
// ChatModelFailed.
func (executor *Executor) Execute(ctx context.Context, attemptID int64, callSeq, retrySeq uint32, messagesJSON, toolsJSON []byte, items []*runtimev1.ModelInputItem, evicted uint32, contract Contract) (int64, string, []ProposedTool, []Authorization, []byte, *Failure, error) {
	// All control-stream digests travel as RAW 32-byte SHA-256 values for
	// agent attempts (the Quoin side hex-encodes once; the probe slice's
	// hex-ASCII convention never reaches this path).
	promptDigest := sha256.Sum256([]byte(agent.SystemPrompt))
	toolDigest := sha256.Sum256(toolsJSON)
	inputDigest := inputItemsDigest(items)
	renderedRaw, err := renderedDigestRaw(messagesJSON, toolsJSON, contract)
	if err != nil {
		return 0, "", nil, nil, nil, &Failure{Reason: "invalid_response", Detail: err.Error()}, nil
	}
	begin := &runtimev1.BeginModelCall{
		AttemptId: attemptID, CallSeq: callSeq, RetrySeq: retrySeq,
		ModelId: contract.ModelID, Operation: runtimev1.ModelOperation_MODEL_OPERATION_CHAT,
		PromptDigest: promptDigest[:], ToolSchemaDigest: toolDigest[:],
		InputDigest: inputDigest, RenderedRequestDigest: renderedRaw,
		InputItems:          items,
		ContextBudgetTokens: uint64(contract.ContextBudget), MaxOutputTokens: uint64(contract.MaxOutput),
		EvictedTurnCount: evicted,
	}
	reply, err := executor.roundTrip(ctx, &runtimev1.ControlEnvelope{
		Msg: &runtimev1.ControlEnvelope_BeginModelCall{BeginModelCall: begin},
	})
	if err != nil {
		return 0, "", nil, nil, nil, &Failure{Reason: "transport_error", Detail: err.Error(), Retryable: true}, nil
	}
	ack := reply.GetBeginModelCallAck()
	if ack == nil || !ack.GetAccepted() {
		detail := "begin rejected"
		if ack != nil {
			detail = ack.GetDetail()
		}
		return 0, "", nil, nil, nil, &Failure{Reason: "invalid_response", Detail: detail}, nil
	}
	callID := ack.GetModelCallId()
	fail := func(reason, detail string, retryable bool, requestID string) (int64, string, []ProposedTool, []Authorization, []byte, *Failure, error) {
		// Seal the failed physical call so the audit row exists
		// (ARCH-AGENT-005).
		_ = executor.completeFailure(ctx, attemptID, callID, reason, requestID)
		return callID, "", nil, nil, nil, &Failure{Reason: reason, Detail: detail, Retryable: retryable, ProviderRequestID: requestID, ModelCallID: callID}, nil
	}
	grant := ack.GetModelProviderGrant()
	if grant == nil {
		return fail("provider_unavailable", "begin ack lacks the model provider grant", false, "")
	}
	secret, err := executor.fetchGrant(ctx, grant.GetGrantId(), attemptID)
	if err != nil {
		return fail("provider_unavailable", "credential grant denied: "+err.Error(), false, "")
	}
	var messages []*schema.Message
	if err := json.Unmarshal(messagesJSON, &messages); err != nil {
		return fail("invalid_response", "messages_json unparseable: "+err.Error(), false, "")
	}
	chatModel, capture, err := newAdapter(ctx, toolsJSON, secret, contract)
	if err != nil {
		return fail("invalid_response", err.Error(), false, "")
	}
	started := time.Now()
	assistantText, toolCalls, usage, finishReason, chunkSeen, callErr := executor.callProvider(ctx, chatModel, messages, contract)
	if callErr != nil {
		reason, retryable := classifyProviderError(callErr)
		if chunkSeen {
			retryable = false
		}
		return fail(reason, callErr.Error(), retryable, capture.RequestID())
	}
	latency := time.Since(started).Milliseconds()
	proposed := make([]ProposedTool, 0, len(toolCalls))
	for index, call := range toolCalls {
		arguments := []byte(call.Function.Arguments)
		if arguments == nil {
			arguments = []byte("{}")
		}
		proposed = append(proposed, ProposedTool{
			ProviderIndex: uint32(index), ProviderToolCallID: call.ID,
			ToolName: call.Function.Name, ArgumentsJSON: arguments,
			ArgumentsDigest: sha256Hex(arguments),
		})
	}
	responseDigest, err := canonicalResponseDigest(assistantText, proposed)
	if err != nil {
		return fail("invalid_response", err.Error(), false, capture.RequestID())
	}
	responseDigestRaw, err := hex.DecodeString(responseDigest)
	if err != nil {
		return fail("invalid_response", "response digest decode: "+err.Error(), false, capture.RequestID())
	}
	completion := &runtimev1.CompleteModelCall{
		AttemptId: attemptID, ModelCallId: callID,
		Outcome:           runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_SUCCEEDED,
		ProviderRequestId: capture.RequestID(), LatencyMs: uint64(latency),
		InputTokens: uint64(usage.PromptTokens), OutputTokens: uint64(usage.CompletionTokens),
		TotalTokens: uint64(usage.TotalTokens), FinishReason: finishReason,
		AssistantText: assistantText, ResponseDigest: responseDigestRaw, ResponseComplete: true,
	}
	for _, tool := range proposed {
		argumentsRaw, decodeErr := hex.DecodeString(tool.ArgumentsDigest)
		if decodeErr != nil {
			return fail("invalid_response", "arguments digest decode: "+decodeErr.Error(), false, capture.RequestID())
		}
		completion.ToolCalls = append(completion.ToolCalls, &runtimev1.ProposedToolCall{
			ProviderIndex: tool.ProviderIndex, ProviderToolCallId: tool.ProviderToolCallID,
			ToolName: tool.ToolName, ArgumentsJson: tool.ArgumentsJSON,
			ArgumentsDigest: argumentsRaw,
		})
	}
	reply, err = executor.roundTrip(ctx, &runtimev1.ControlEnvelope{
		Msg: &runtimev1.ControlEnvelope_CompleteModelCall{CompleteModelCall: completion},
	})
	if err != nil {
		return fail("transport_error", "complete round trip failed: "+err.Error(), false, capture.RequestID())
	}
	completeAck := reply.GetCompleteModelCallAck()
	if completeAck == nil || !completeAck.GetAccepted() {
		detail := "complete rejected"
		if completeAck != nil {
			detail = completeAck.GetDetail()
		}
		return fail("invalid_response", detail, false, capture.RequestID())
	}
	var authorizations []Authorization
	for _, wire := range completeAck.GetToolCalls() {
		authorizations = append(authorizations, Authorization{
			ToolCallID: wire.GetToolCallId(), ProviderIndex: wire.GetProviderIndex(),
			ProviderToolCallID: wire.GetProviderToolCallId(), FailureMode: wire.GetFailureMode().String(),
		})
	}
	return callID, assistantText, proposed, authorizations, responseDigestRaw, nil, nil
}

// completeFailure seals a failed physical call row (best effort; the
// worker already has the failure on its way).
func (executor *Executor) completeFailure(ctx context.Context, attemptID, callID int64, reason, requestID string) error {
	if callID == 0 {
		return nil
	}
	failureReason := runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_INVALID_RESPONSE
	switch reason {
	case "timeout":
		failureReason = runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_TIMEOUT
	case "rate_limited":
		failureReason = runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_RATE_LIMITED
	case "provider_unavailable":
		failureReason = runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_PROVIDER_UNAVAILABLE
	case "context_overflow":
		failureReason = runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_CONTEXT_OVERFLOW
	case "transport_error":
		failureReason = runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_TRANSPORT_ERROR
	}
	_, err := executor.roundTrip(ctx, &runtimev1.ControlEnvelope{
		Msg: &runtimev1.ControlEnvelope_CompleteModelCall{CompleteModelCall: &runtimev1.CompleteModelCall{
			AttemptId: attemptID, ModelCallId: callID,
			Outcome:           runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED,
			FailureReason:     failureReason,
			ProviderRequestId: requestID,
		}},
	})
	return err
}

// callProvider runs the adapter and relays visible stream deltas.
func (executor *Executor) callProvider(ctx context.Context, chatModel *openai.ChatModel, messages []*schema.Message, contract Contract) (text string, toolCalls []schema.ToolCall, usage *schema.TokenUsage, finishReason string, chunkSeen bool, err error) {
	if !contract.Streaming {
		message, err := chatModel.Generate(ctx, messages)
		if err != nil {
			return "", nil, nil, "", false, err
		}
		usage := &schema.TokenUsage{}
		if message.ResponseMeta != nil && message.ResponseMeta.Usage != nil {
			usage = message.ResponseMeta.Usage
		}
		finish := ""
		if message.ResponseMeta != nil {
			finish = message.ResponseMeta.FinishReason
		}
		return message.Content, message.ToolCalls, usage, finish, false, nil
	}
	stream, err := chatModel.Stream(ctx, messages)
	if err != nil {
		return "", nil, nil, "", false, err
	}
	defer stream.Close()
	var final *schema.Message
	var accumulated string
	// Streaming tool calls arrive as per-index fragments across messages
	// (the final finish chunk carries none); aggregate by provider index
	// before sealing.
	type toolAccumulator struct {
		id   string
		name string
		args strings.Builder
	}
	acc := map[int]*toolAccumulator{}
	for {
		message, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return "", nil, nil, "", chunkSeen, recvErr
		}
		final = message
		// Each delta carries its own content slice; the durable text is
		// the concatenation of the visible deltas (the final message only
		// carries the last delta plus finish_reason/usage).
		if message.Content != "" {
			accumulated += message.Content
			if executor.DeltaHook != nil {
				if err := executor.DeltaHook(ctx, message.Content); err != nil {
					return "", nil, nil, "", chunkSeen, err
				}
			}
			chunkSeen = true
		}
		for _, call := range message.ToolCalls {
			index := 0
			if call.Index != nil {
				index = *call.Index
			}
			entry := acc[index]
			if entry == nil {
				entry = &toolAccumulator{}
				acc[index] = entry
			}
			if call.ID != "" {
				entry.id = call.ID
			}
			if call.Function.Name != "" {
				entry.name = call.Function.Name
			}
			entry.args.WriteString(call.Function.Arguments)
		}
	}
	if final == nil {
		return "", nil, nil, "", chunkSeen, errors.New("provider stream ended without a final message")
	}
	usage = &schema.TokenUsage{}
	if final.ResponseMeta != nil && final.ResponseMeta.Usage != nil {
		usage = final.ResponseMeta.Usage
	}
	finish := ""
	if final.ResponseMeta != nil {
		finish = final.ResponseMeta.FinishReason
	}
	merged := make([]schema.ToolCall, 0, len(acc))
	for index := 0; index < len(acc); index++ {
		entry := acc[index]
		if entry == nil {
			continue
		}
		merged = append(merged, schema.ToolCall{
			Index: intPointer(index), ID: entry.id, Type: "function",
			Function: schema.FunctionCall{Name: entry.name, Arguments: entry.args.String()},
		})
	}
	return accumulated, merged, usage, finish, chunkSeen, nil
}

// classifyProviderError maps adapter errors onto the frozen failure
// reasons (ARCH-AGENT-004: retry only before any visible chunk).
func classifyProviderError(err error) (reason string, retryable bool) {
	var apiErr *openai.APIError
	switch {
	case errors.As(err, &apiErr):
		switch apiErr.HTTPStatusCode {
		case 408:
			return "timeout", true
		case 429:
			return "rate_limited", true
		case 400:
			return "context_overflow", false
		default:
			return "provider_unavailable", false
		}
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", true
	case errors.Is(err, context.Canceled):
		return "cancelled", false
	default:
		return "transport_error", true
	}
}

// fetchGrant resolves the attempt-scoped credential over the authenticated
// channel (RUNTIME-GRANT-001).
func (executor *Executor) fetchGrant(ctx context.Context, grantID, attemptID int64) (string, error) {
	bearer, err := executor.Channel.BearerToken()
	if err != nil {
		return "", err
	}
	grantCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	reply, err := executor.Client.FetchCredentialGrant(metadata.NewOutgoingContext(grantCtx, metadata.Pairs("authorization", "Bearer "+bearer)), &runtimev1.FetchCredentialGrantRequest{
		GrantId: grantID, AttemptId: attemptID, BootId: executor.Sink.BootID(), ConnectionEpoch: executor.Sink.Epoch(),
	})
	if err != nil {
		return "", err
	}
	secret := reply.GetModelProvider()
	if secret == nil {
		return "", errors.New("grant carries no model provider secret")
	}
	return secret.GetApiKey(), nil
}

// roundTrip sends one envelope and waits for the correlated reply.
func (executor *Executor) roundTrip(ctx context.Context, envelope *runtimev1.ControlEnvelope) (*runtimev1.ControlEnvelope, error) {
	return executor.Channel.Request(ctx, envelope)
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// inputItemsDigest is the ordered item digest concatenation hash (raw
// 32-byte SHA-256, BeginModelCall contract).
func inputItemsDigest(items []*runtimev1.ModelInputItem) []byte {
	hash := sha256.New()
	for _, item := range items {
		hash.Write(item.GetContentDigest())
	}
	return hash.Sum(nil)
}

// renderedDigestRaw is the canonical provider request digest: the exact
// request parameters the adapter receives (model, messages, tools,
// max_tokens, temperature 0, stream flag), returned as raw 32 bytes.
func renderedDigestRaw(messagesJSON, toolsJSON []byte, contract Contract) ([]byte, error) {
	var messages, tools any
	if err := json.Unmarshal(messagesJSON, &messages); err != nil {
		return nil, fmt.Errorf("messages_json unparseable: %w", err)
	}
	if err := json.Unmarshal(toolsJSON, &tools); err != nil {
		return nil, fmt.Errorf("tools_json unparseable: %w", err)
	}
	canonical := map[string]any{
		"model": contract.ModelID, "messages": messages, "tools": tools,
		"max_tokens": contract.MaxOutput, "temperature": 0, "stream": contract.Streaming,
	}
	body, err := json.Marshal(canonical)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	return sum[:], nil
}

// newAdapter builds the Eino OpenAI-compatible adapter with the injected
// secret and a request-id capturing transport (never persisted).
func newAdapter(ctx context.Context, toolsJSON []byte, apiKey string, contract Contract) (*openai.ChatModel, *captureTransport, error) {
	capture := &captureTransport{base: http.DefaultTransport}
	client := &http.Client{Transport: capture, Timeout: 10 * time.Minute}
	// The go-openai adapter appends "/chat/completions" to BaseURL; the
	// stored connection config carries the bare provider root (the probe
	// posts to /v1/chat/completions explicitly), so normalize here.
	baseURL := strings.TrimSuffix(contract.BaseURL, "/")
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL += "/v1"
	}
	config := &openai.ChatModelConfig{
		APIKey: apiKey, BaseURL: baseURL, Model: contract.ModelID,
		MaxTokens: &contract.MaxOutput, Temperature: temperaturePointer(0),
		HTTPClient: client,
	}
	chatModel, err := openai.NewChatModel(ctx, config)
	if err != nil {
		return nil, nil, fmt.Errorf("openai adapter: %w", err)
	}
	var tools []*schema.ToolInfo
	if err := json.Unmarshal(toolsJSON, &tools); err != nil {
		return nil, nil, fmt.Errorf("tools_json unparseable: %w", err)
	}
	if err := chatModel.BindTools(tools); err != nil {
		return nil, nil, fmt.Errorf("bind tools: %w", err)
	}
	return chatModel, capture, nil
}

func temperaturePointer(value float32) *float32 { return &value }

func intPointer(value int) *int { return &value }

// captureTransport records the provider request id header.
type captureTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	id   string
}

func (transport *captureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err == nil {
		transport.mu.Lock()
		if id := response.Header.Get("X-Request-Id"); id != "" {
			transport.id = id
		} else if id := response.Header.Get("x-request-id"); id != "" {
			transport.id = id
		}
		transport.mu.Unlock()
	}
	return response, err
}

// RequestID returns the captured provider request id.
func (transport *captureTransport) RequestID() string {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.id
}

// canonicalResponseDigest mirrors the Quoin-side canonical chat response
// (attempt.CanonicalChatResponseJSON); a unit test pins both equal.
func canonicalResponseDigest(assistantText string, tools []ProposedTool) (string, error) {
	type canonicalTool struct {
		ProviderToolCallID string `json:"providerToolCallId"`
		ToolName           string `json:"toolName"`
		Arguments          any    `json:"arguments"`
	}
	canonical := struct {
		AssistantText string          `json:"assistantText"`
		ToolCalls     []canonicalTool `json:"toolCalls"`
	}{AssistantText: assistantText, ToolCalls: []canonicalTool{}}
	for _, tool := range tools {
		var arguments any
		if err := json.Unmarshal(tool.ArgumentsJSON, &arguments); err != nil {
			return "", fmt.Errorf("tool %s arguments unparseable: %w", tool.ToolName, err)
		}
		canonical.ToolCalls = append(canonical.ToolCalls, canonicalTool{
			ProviderToolCallID: tool.ProviderToolCallID, ToolName: tool.ToolName, Arguments: arguments,
		})
	}
	body, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return sha256Hex(body), nil
}
