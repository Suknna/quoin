package model

// Adapter contract test: the Eino OpenAI-compatible adapter must surface
// streaming native tool calls (aggregated across deltas) and content
// deltas the way the executor consumes them, against a deterministic
// in-process SSE provider. This pins the behavior the fixture provider
// relies on end to end.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/schema"
)

func sseChunk(t *testing.T, writer http.ResponseWriter, payload string) {
	t.Helper()
	fmt.Fprintf(writer, "data: %s\n\n", payload)
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

// TestAdapterSendsCanonicalOpenAITools exercises the same executor adapter used
// by inspection and investigation workers. The canonical catalog is already in
// OpenAI's nested tools format, so decoding it as Eino ToolInfo would silently
// produce zero-value names and descriptions on the provider wire.
func TestAdapterSendsCanonicalOpenAITools(t *testing.T) {
	for _, agentVersion := range []string{"initial-analysis-v1", "investigation-v1"} {
		t.Run(agentVersion, func(t *testing.T) {
			toolsJSON, err := attempt.CanonicalToolsJSON(agentVersion)
			if err != nil {
				t.Fatal(err)
			}
			if agentVersion == "investigation-v1" {
				var canonical []struct {
					Function struct {
						Name       string         `json:"name"`
						Parameters map[string]any `json:"parameters"`
					} `json:"function"`
				}
				if err := json.Unmarshal(toolsJSON, &canonical); err != nil {
					t.Fatal(err)
				}
				for _, tool := range canonical {
					if tool.Function.Name == "quoin_browser" && tool.Function.Parameters["type"] != nil {
						t.Fatalf("canonical quoin_browser schema unexpectedly changed: %#v", tool.Function.Parameters)
					}
				}
			}
			provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				defer request.Body.Close()
				var body struct {
					Tools []struct {
						Type     string `json:"type"`
						Function struct {
							Name        string         `json:"name"`
							Description string         `json:"description"`
							Parameters  map[string]any `json:"parameters"`
						} `json:"function"`
					} `json:"tools"`
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
					http.Error(writer, "bad request", http.StatusBadRequest)
					return
				}
				if len(body.Tools) == 0 {
					t.Error("provider request has no tools")
				}
				for index, tool := range body.Tools {
					if tool.Type != "function" || tool.Function.Name == "" || tool.Function.Description == "" || tool.Function.Parameters == nil {
						t.Errorf("tools[%d]=%+v: expected nested OpenAI function name, description, and parameters", index, tool)
						continue
					}
					// OpenAI-compatible providers, including DeepSeek, require every
					// function parameter root to declare object. The investigation
					// browser tool is a closed oneOf union, so it exercises the
					// adapter's non-trivial JSON Schema path rather than a flat tool.
					if tool.Function.Parameters["type"] != "object" {
						t.Errorf("tools[%d] %q parameters root type=%#v, want object: %#v", index, tool.Function.Name, tool.Function.Parameters["type"], tool.Function.Parameters)
					}
					if agentVersion == "investigation-v1" && tool.Function.Name == "quoin_browser" {
						if _, ok := tool.Function.Parameters["oneOf"]; !ok {
							t.Errorf("quoin_browser lost its closed oneOf validation on the provider wire: %#v", tool.Function.Parameters)
						}
					}
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"id":"completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer provider.Close()

			contract := Contract{ModelID: "m", BaseURL: provider.URL, APIKey: "k", ContextBudget: 4096, MaxOutput: 1024}
			adapter, _, err := newAdapter(context.Background(), toolsJSON, contract.APIKey, contract)
			if err != nil {
				t.Fatal(err)
			}
			executor := &Executor{}
			text, _, _, finish, _, _, err := executor.callProvider(context.Background(), adapter, []*schema.Message{schema.SystemMessage("s"), schema.UserMessage("u")}, contract)
			if err != nil {
				t.Fatal(err)
			}
			if text != "ok" || finish != "stop" {
				t.Fatalf("response text=%q finish=%q", text, finish)
			}
		})
	}
}

func TestClassifyProviderErrorDoesNotTreatGeneric400AsContextOverflow(t *testing.T) {
	for _, test := range []struct {
		name string
		err  *openai.APIError
		want string
	}{
		{name: "tool validation", err: &openai.APIError{HTTPStatusCode: http.StatusBadRequest, Message: "Invalid tools[0].function.name empty string"}, want: "invalid_response"},
		{name: "context length", err: &openai.APIError{HTTPStatusCode: http.StatusBadRequest, Type: "context_length_exceeded", Message: "maximum context length exceeded"}, want: "context_overflow"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, retryable := classifyProviderError(test.err)
			if got != test.want || retryable {
				t.Fatalf("classifyProviderError() = (%q, %t), want (%q, false)", got, retryable, test.want)
			}
		})
	}
}

// TestStreamingDeltaHookFailureFencesRetry uses the real SSE adapter path. A
// hook may have exposed the delta before returning an error, so its failure
// must retain the partial text and mark the physical call non-retryable.
func TestStreamingDeltaHookFailureFencesRetry(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		sseChunk(t, writer, `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"already visible"},"finish_reason":null}]}`)
		// The callback error stops the executor before the provider completes.
		fmt.Fprint(writer, "data: [DONE]\\n\\n")
	}))
	defer provider.Close()

	contract := Contract{ModelID: "m", BaseURL: strings.TrimSuffix(provider.URL, "/") + "/v1", APIKey: "k", ContextBudget: 4096, MaxOutput: 1024, Streaming: true}
	toolsJSON := []byte(`[{"type":"function","function":{"name":"noop","description":"test tool","parameters":{"type":"object"}}}]`)
	adapter, _, err := newAdapter(context.Background(), toolsJSON, contract.APIKey, contract)
	if err != nil {
		t.Fatal(err)
	}
	hookErr := errors.New("worker delta delivery failed after exposure")
	executor := &Executor{DeltaHook: func(context.Context, string) error { return hookErr }}
	_, _, _, _, chunkSeen, partialText, err := executor.callProvider(context.Background(), adapter, []*schema.Message{schema.UserMessage("u")}, contract)
	if !errors.Is(err, hookErr) || !chunkSeen || partialText != "already visible" {
		t.Fatalf("callback failure = err=%v chunkSeen=%t partialText=%q", err, chunkSeen, partialText)
	}
	_, retryable := classifyStreamError(err, chunkSeen)
	if retryable {
		t.Fatal("an exposed delta callback failure must not be retryable")
	}
}

func TestAdapterStreamingToolCallAndContent(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/v1/chat/completions") {
			http.Error(writer, "wrong path", http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("X-Request-Id", "req-stream-test")
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		toolTurn := true
		for _, message := range body.Messages {
			if message.Role == "tool" {
				toolTurn = false
			}
		}
		if toolTurn {
			sseChunk(t, writer, `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"echo proof"}}]},"finish_reason":null}]}`)
			sseChunk(t, writer, `{"id":"c2","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"}"}}]},"finish_reason":null}]}`)
			sseChunk(t, writer, `{"id":"c3","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
			fmt.Fprint(writer, "data: [DONE]\n\n")
			return
		}
		sseChunk(t, writer, `{"id":"c4","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"初步"},"finish_reason":null}]}`)
		sseChunk(t, writer, `{"id":"c5","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"诊断"},"finish_reason":null}]}`)
		sseChunk(t, writer, `{"id":"c6","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer provider.Close()

	contract := Contract{ModelID: "m", BaseURL: strings.TrimSuffix(provider.URL, "/") + "/v1", APIKey: "k", ContextBudget: 4096, MaxOutput: 1024, Streaming: true}
	toolsJSON := []byte(`[{"type":"function","function":{"name":"bash","description":"run bash","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}]`)
	adapter, _, err := newAdapter(context.Background(), toolsJSON, contract.APIKey, contract)
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{DeltaHook: func(ctx context.Context, delta string) error { return nil }}
	text, toolCalls, _, finish, _, _, err := executor.callProvider(context.Background(), adapter,
		[]*schema.Message{schema.SystemMessage("s"), schema.UserMessage("u")}, contract)
	if err != nil {
		t.Fatal(err)
	}
	_ = text
	if finish != "tool_calls" {
		t.Fatalf("finish=%q", finish)
	}
	// A pure tool turn carries no visible content (chunkSeen tracks only
	// content deltas; it drives the retryability rule).
	if len(toolCalls) != 1 {
		t.Fatalf("tool calls=%+v (streaming tool call deltas must aggregate)", toolCalls)
	}
	if toolCalls[0].Function.Name != "bash" {
		t.Fatalf("tool=%+v", toolCalls[0])
	}
	args := strings.TrimSpace(toolCalls[0].Function.Arguments)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("aggregated arguments %q are not valid JSON: %v", args, err)
	}
	if parsed["command"] != "echo proof" {
		t.Fatalf("arguments=%q", args)
	}
	// The continuation turn accumulates the visible content deltas.
	text2, toolCalls2, _, finish2, _, _, err := executor.callProvider(context.Background(), adapter,
		[]*schema.Message{schema.SystemMessage("s"), {Role: schema.Tool, Content: `{"success":true}`}}, contract)
	if err != nil {
		t.Fatal(err)
	}
	if text2 != "初步诊断" || len(toolCalls2) != 0 || finish2 != "stop" {
		t.Fatalf("continuation text=%q tools=%+v finish=%q", text2, toolCalls2, finish2)
	}
	_ = time.Now
}
