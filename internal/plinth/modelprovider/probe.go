package modelprovider

// OpenAI-compatible model provider probe client (T08). Native HTTP only —
// no supplier SDK: the probe speaks the wire protocol (chat completions
// with streaming, tool calls, embeddings) and observes typed provider
// metadata (usage, request id). Every action maps to one Begin/Complete
// ModelCall ledger pair over the control stream.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config is the non-secret model provider revision projection.
type Config struct {
	Type                string `json:"type"`
	BaseURL             string `json:"baseUrl"`
	ChatModelID         string `json:"chatModelId"`
	EmbeddingModelID    string `json:"embeddingModelId"`
	ContextBudgetTokens int    `json:"contextBudgetTokens"`
	MaxOutputTokens     int    `json:"maxOutputTokens"`
}

// ProbeCapabilities is the typed qualification matrix (frozen action set).
type ProbeCapabilities struct {
	StreamingSupported   bool `json:"streamingSupported"`
	NativeToolCalling    bool `json:"nativeToolCallingSupported"`
	MultiToolCall        bool `json:"multiToolCallSupported"`
	CancellationObserved bool `json:"cancellationObserved"`
	UsageObserved        bool `json:"usageObserved"`
	RequestIDObserved    bool `json:"requestIdObserved"`
	EmbeddingSupported   bool `json:"embeddingSupported"`
	EmbeddingVectorDim   int  `json:"embeddingVectorDim,omitempty"`
}

// Fixed probe profiles (connection-probes.yaml request profiles).
var (
	chatPrompt = "Reply with the single word: ready"
	toolSchema = map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "probe_noop",
			"description": "A fixed no-side-effect probe tool",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"note": map[string]string{"type": "string"}},
			},
		},
	}
	cancelPrompt = "Count from 1 to 100 slowly"
)

type client struct {
	http    *http.Client
	baseURL string
	apiKey  string
}

func newClient(config Config, apiKey string) *client {
	return &client{
		http:    &http.Client{Timeout: 60 * time.Second},
		baseURL: strings.TrimSuffix(config.BaseURL, "/"),
		apiKey:  apiKey,
	}
}

func (probe *client) post(ctx context.Context, path string, body map[string]any, stream bool) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, probe.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+probe.apiKey)
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	return probe.http.Do(request)
}

// Action names for ledger call_seq allocation.
const (
	ActionChatStream = "chat-stream"
	ActionToolCall   = "native-tool-call"
	ActionParallel   = "parallel-tool-calls"
	ActionCancel     = "cancellation"
	ActionUsage      = "usage-and-request-id"
	ActionEmbedding  = "embedding"
)

// ActionResult is one action's typed observation.
type ActionResult struct {
	Action    string
	OK        bool
	Detail    string
	Usage     usage
	RequestID string
}

type usage struct {
	Input  int64 `json:"input_tokens"`
	Output int64 `json:"output_tokens"`
	Total  int64 `json:"total_tokens"`
}

// RunChatStream verifies stream_opened/multiple_chunks/terminal completion.
func RunChatStream(ctx context.Context, probe *client, config Config, modelID string) (ActionResult, []string, string, error) {
	response, err := probe.post(ctx, "/v1/chat/completions", map[string]any{
		"model": modelID, "stream": true,
		"messages": []map[string]string{{"role": "user", "content": chatPrompt}},
	}, true)
	result := ActionResult{Action: ActionChatStream}
	if err != nil {
		return result, nil, "", fmt.Errorf("chat stream 请求失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, nil, "", fmt.Errorf("chat stream HTTP %d", response.StatusCode)
	}
	result.RequestID = response.Header.Get("X-Request-Id")
	result.OK = true
	reader := bufio.NewReader(response.Body)
	var chunks []string
	var assembled string
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimPrefix(trimmed, "data:")
				payload = strings.TrimSpace(payload)
				if payload == "[DONE]" {
					return result, chunks, assembled, nil
				}
				chunks = append(chunks, payload)
				var delta struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(payload), &delta) == nil && len(delta.Choices) > 0 {
					assembled += delta.Choices[0].Delta.Content
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return result, chunks, assembled, errors.New("流在 [DONE] 前结束")
			}
			return result, chunks, assembled, readErr
		}
	}
}

// RunToolCall offers the fixed tool schema and requires a native call.
func RunToolCall(ctx context.Context, probe *client, modelID string, parallel bool) (ActionResult, string, error) {
	tools := []any{toolSchema}
	if parallel {
		second := toolSchema
		secondMap := map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "probe_noop_second",
				"description": "A second fixed no-side-effect probe tool",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"note": map[string]string{"type": "string"}},
				},
			},
		}
		second = secondMap
		tools = append(tools, second)
	}
	response, err := probe.post(ctx, "/v1/chat/completions", map[string]any{
		"model": modelID,
		"messages": []map[string]any{
			{"role": "user", "content": map[string]any{
				"type": "text",
				"text": func() string {
					if parallel {
						return "Call both probe tools in this turn."
					}
					return "Call the probe_noop tool."
				}(),
			}},
		},
		"tools": tools,
	}, false)
	result := ActionResult{Action: ActionToolCall}
	if parallel {
		result.Action = ActionParallel
	}
	if err != nil {
		return result, "", fmt.Errorf("tool call 请求失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, "", fmt.Errorf("tool call HTTP %d", response.StatusCode)
	}
	result.RequestID = response.Header.Get("X-Request-Id")
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var completion struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &completion); err != nil {
		return result, "", fmt.Errorf("tool call 响应不是合法 JSON: %w", err)
	}
	if completion.Usage != nil {
		result.Usage = usage{Input: completion.Usage.PromptTokens, Output: completion.Usage.CompletionTokens, Total: completion.Usage.TotalTokens}
	}
	calls := completion.Choices[0].Message.ToolCalls
	if len(calls) == 0 {
		return result, "", errors.New("未观察到原生 tool call")
	}
	if parallel && len(calls) < 2 {
		return result, "", fmt.Errorf("并行 tool call 数量是 %d，期望 ≥2", len(calls))
	}
	for _, call := range calls {
		if call.Function.Name == "" {
			return result, "", errors.New("tool call 缺少函数名")
		}
		if !json.Valid([]byte(call.Function.Arguments)) {
			return result, "", errors.New("tool call arguments 不是合法 JSON")
		}
	}
	result.OK = true
	return result, string(body), nil
}

// RunUsageAndRequestID completes one chat request and inspects typed
// provider metadata (usage fields + request id header/body).
func RunUsageAndRequestID(ctx context.Context, probe *client, modelID string) (ActionResult, string, error) {
	response, err := probe.post(ctx, "/v1/chat/completions", map[string]any{
		"model":    modelID,
		"messages": []map[string]string{{"role": "user", "content": chatPrompt}},
	}, false)
	result := ActionResult{Action: ActionUsage}
	if err != nil {
		return result, "", fmt.Errorf("usage 请求失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, "", fmt.Errorf("usage HTTP %d", response.StatusCode)
	}
	result.RequestID = response.Header.Get("X-Request-Id")
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var completion struct {
		ID    string `json:"id"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &completion); err != nil {
		return result, "", fmt.Errorf("usage 响应不是合法 JSON: %w", err)
	}
	if completion.Usage != nil && completion.Usage.TotalTokens > 0 {
		result.Usage = usage{Input: completion.Usage.PromptTokens, Output: completion.Usage.CompletionTokens, Total: completion.Usage.TotalTokens}
		result.OK = true
	} else {
		return result, "", errors.New("未观察到 usage")
	}
	if result.RequestID == "" {
		result.RequestID = completion.ID
	}
	return result, string(body), nil
}

// RunCancellation starts a long streamed response, persists the model call,
// cancels it and requires the provider stream to terminate.
func RunCancellation(ctx context.Context, probe *client, modelID string) (ActionResult, string, error) {
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	response, err := probe.post(callCtx, "/v1/chat/completions", map[string]any{
		"model": modelID, "stream": true,
		"messages": []map[string]string{{"role": "user", "content": cancelPrompt}},
	}, true)
	result := ActionResult{Action: ActionCancel}
	if err != nil {
		return result, "", fmt.Errorf("cancellation 请求失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, "", fmt.Errorf("cancellation HTTP %d", response.StatusCode)
	}
	result.RequestID = response.Header.Get("X-Request-Id")
	// Consume until at least one data chunk arrives, then cancel mid-stream.
	reader := bufio.NewReader(response.Body)
	sawChunk := false
	for !sawChunk {
		line, readErr := reader.ReadString('\n')
		if strings.HasPrefix(strings.TrimSpace(line), "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
			if payload != "" && payload != "[DONE]" {
				sawChunk = true
			}
		}
		if readErr != nil {
			return result, "", errors.New("取消前流已结束，无法观察取消传播")
		}
	}
	cancel()
	// After cancellation the body read must terminate (provider observed).
	_, postCancelErr := io.Copy(io.Discard, reader)
	if postCancelErr == nil {
		return result, "", errors.New("取消后流仍在正常结束路径，未观察到取消传播")
	}
	result.OK = true
	return result, "", nil
}

// RunEmbedding embeds the fixed sentence with the configured model.
func RunEmbedding(ctx context.Context, probe *client, modelID string) (ActionResult, [][]float64, error) {
	response, err := probe.post(ctx, "/v1/embeddings", map[string]any{
		"model": modelID,
		"input": []string{"The monitoring stack is healthy."},
	}, false)
	result := ActionResult{Action: ActionEmbedding}
	if err != nil {
		return result, nil, fmt.Errorf("embedding 请求失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, nil, fmt.Errorf("embedding HTTP %d", response.StatusCode)
	}
	result.RequestID = response.Header.Get("X-Request-Id")
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	var embedding struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage *struct {
			PromptTokens int64 `json:"prompt_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &embedding); err != nil {
		return result, nil, fmt.Errorf("embedding 响应不是合法 JSON: %w", err)
	}
	if len(embedding.Data) == 0 || len(embedding.Data[0].Embedding) == 0 {
		return result, nil, errors.New("embedding 向量为空")
	}
	if embedding.Usage != nil {
		result.Usage = usage{Input: embedding.Usage.PromptTokens, Total: embedding.Usage.TotalTokens}
	}
	result.OK = true
	return result, [][]float64{embedding.Data[0].Embedding}, nil
}

// DigestCanonical computes the canonical SHA-256 for ledger digests.
func DigestCanonical(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
