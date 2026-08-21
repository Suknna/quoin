package model

// Adapter contract test: the Eino OpenAI-compatible adapter must surface
// streaming native tool calls (aggregated across deltas) and content
// deltas the way the executor consumes them, against a deterministic
// in-process SSE provider. This pins the behavior the fixture provider
// relies on end to end.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

func sseChunk(t *testing.T, writer http.ResponseWriter, payload string) {
	t.Helper()
	fmt.Fprintf(writer, "data: %s\n\n", payload)
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
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
	toolsJSON, err := json.Marshal([]*schema.ToolInfo{{
		Name: "bash", Desc: "run bash",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"command": {Type: schema.String, Required: true},
		}),
	}})
	if err != nil {
		t.Fatal(err)
	}
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
