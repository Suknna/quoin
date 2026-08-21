// Package main is the deterministic OpenAI-compatible fixture for T08
// (test/fixtures/model-provider). It is a black-box probe target: real HTTP
// semantics (SSE streaming, tool calls, embeddings, request ids, usage),
// deterministic bodies, and a partial-stream-failure mode selectable per
// model id. It never asserts anything about the probe — it only answers.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	address := flag.String("address", "127.0.0.1:18443", "listen address")
	flag.Parse()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(writer http.ResponseWriter, request *http.Request) {
		if !authorize(writer, request) {
			return
		}
		writeJSON(writer, map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "fixture-chat-1", "object": "model", "owned_by": "fixture"},
				{"id": "fixture-embed-1", "object": "model", "owned_by": "fixture"},
			},
		})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(writer http.ResponseWriter, request *http.Request) {
		if !authorize(writer, request) {
			return
		}
		var body chatRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "invalid body", http.StatusBadRequest)
			return
		}
		writer.Header().Set("X-Request-Id", "req-fixture-"+fmt.Sprint(time.Now().UnixNano()))
		if body.Stream {
			serveStream(writer, body)
			return
		}
		serveCompletion(writer, body)
	})
	mux.HandleFunc("POST /v1/embeddings", func(writer http.ResponseWriter, request *http.Request) {
		if !authorize(writer, request) {
			return
		}
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "invalid body", http.StatusBadRequest)
			return
		}
		writer.Header().Set("X-Request-Id", "req-fixture-emb-"+fmt.Sprint(time.Now().UnixNano()))
		// 16-dimension deterministic vector derived from the input length.
		dimension := 16
		data := make([]map[string]any, 0, len(body.Input))
		for index, input := range body.Input {
			vector := make([]float64, dimension)
			for step := 0; step < dimension; step++ {
				vector[step] = float64((len(input)*13+step*7)%97) / 97.0
			}
			data = append(data, map[string]any{"object": "embedding", "index": index, "embedding": vector})
		}
		writeJSON(writer, map[string]any{
			"object": "list", "data": data,
			"model": body.Model,
			"usage": map[string]int{"prompt_tokens": 7, "total_tokens": 7},
		})
	})
	log.Printf("fixture model provider listening on %s", *address)
	log.Fatal(http.ListenAndServe(*address, mux))
}

func authorize(writer http.ResponseWriter, request *http.Request) bool {
	if request.Header.Get("Authorization") != "Bearer fixture-api-key-2026" {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(writer, `{"error":{"message":"bad key","type":"invalid_request_error"}}`, http.StatusUnauthorized)
		return false
	}
	return true
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

// chatRequest mirrors the deterministic chat request fields the fixture
// branches on (streaming agent mode included).
type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role      string `json:"role"`
		Content   any    `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tool_calls"`
		ToolCallID string `json:"tool_call_id"`
	} `json:"messages"`
	Tools []map[string]any `json:"tools"`
}

// isAgentFirstTurn reports whether this streaming request is the T10 agent's
// first call: the fixed initial-analysis user prompt plus the tool schema.
func (body chatRequest) isAgentFirstTurn() bool {
	if len(body.Tools) == 0 {
		return false
	}
	return strings.Contains(promptText(body.Messages), "请分析以下告警") && !body.hasToolResult()
}

// hasToolResult reports whether any message carries a committed tool result
// (the agent's second call carries the workspace tool result preview).
func (body chatRequest) hasToolResult() bool {
	for _, message := range body.Messages {
		if message.Role == "tool" && strings.Contains(fmt.Sprint(message.Content), `"success"`) {
			return true
		}
	}
	return false
}

// serveStream emits SSE chunks; the broken model id disconnects after two
// chunks (partial stream failure fixture). The agent's first call answers a
// streaming native tool call (bash); its continuation returns the final
// text diagnosis.
func serveStream(writer http.ResponseWriter, body chatRequest) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-store")
	flusher, _ := writer.(http.Flusher)
	chunk := func(payload string) {
		fmt.Fprintf(writer, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	model := body.Model
	if body.isAgentFirstTurn() {
		log.Printf("agent first turn: streaming native bash tool call")
		// One deterministic streaming tool call: bash. The split
		// arguments stream exercises the delta accumulation path. Chunks
		// are built with encoding/json so the stream is always valid JSON.
		toolCall := func(arguments any) map[string]any {
			return map[string]any{
				"id": "chat-agent", "object": "chat.completion.chunk", "model": model,
				"choices": []any{map[string]any{
					"index": 0, "finish_reason": nil,
					"delta": map[string]any{"tool_calls": []any{arguments}},
				}},
			}
		}
		chunk(mustJSONChunk(toolCall(map[string]any{
			"index": 0, "id": "call-agent-bash", "type": "function",
			"function": map[string]any{"name": "bash", "arguments": "{\"command\":\"echo agent-fixture-proof\"}"},
		})))
		time.Sleep(150 * time.Millisecond)
		chunk(mustJSONChunk(toolCall(map[string]any{
			"index": 0, "function": map[string]any{"arguments": ""},
		})))
		time.Sleep(150 * time.Millisecond)
		chunk(mustJSONChunk(map[string]any{
			"id": "chat-agent", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
		}))
		chunk("[DONE]")
		return
	}
	if body.hasToolResult() {
		log.Printf("agent continuation: streaming text diagnosis (%d words)", 10)
		words := []string{"初步诊断：", "该告警", "为", "agent-fixture-proof", "场景", "的可复现示例", "，", "建议按", "排查顺序", "确认。"}
		for _, word := range words {
			chunk(mustJSONChunk(map[string]any{
				"id": "chat-agent", "object": "chat.completion.chunk", "model": model,
				"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{"content": word}, "finish_reason": nil,
				}},
			}))
			time.Sleep(150 * time.Millisecond)
		}
		chunk(mustJSONChunk(map[string]any{
			"id": "chat-agent", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		}))
		chunk("[DONE]")
		return
	}
	log.Printf("default stream: %d probe words", 4)
	words := []string{"count", "ing", " slow", "ly"}
	for index, word := range words {
		chunk(mustJSONChunk(map[string]any{
			"id": "chat-fixture", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": word}, "finish_reason": nil,
			}},
		}))
		if model == "fixture-broken-stream" && index == 1 {
			// Partial stream failure: hang up before [DONE].
			if hijacker, ok := writer.(http.Hijacker); ok {
				if connection, _, err := hijacker.Hijack(); err == nil {
					_ = connection.Close()
					return
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	chunk(mustJSONChunk(map[string]any{
		"id": "chat-fixture", "object": "chat.completion.chunk", "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	}))
	chunk("[DONE]")
}

// mustJSONChunk renders one SSE chunk payload (marshal errors are
// impossible for these shapes; a panic would be loud and immediate).
func mustJSONChunk(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(body)
}

func jsonQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// serveCompletion answers tool calls deterministically by prompt content.
func serveCompletion(writer http.ResponseWriter, body chatRequest) {
	prompt := promptText(body.Messages)
	switch {
	case strings.Contains(prompt, "both probe tools"):
		writeJSON(writer, map[string]any{
			"id": "chat-fixture-parallel", "object": "chat.completion", "model": body.Model,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{
						{"id": "call-fixture-1", "type": "function", "function": map[string]any{"name": "probe_noop", "arguments": `{"note":"first"}`}},
						{"id": "call-fixture-2", "type": "function", "function": map[string]any{"name": "probe_noop_second", "arguments": `{"note":"second"}`}},
					},
				},
			}},
			"usage": map[string]int{"prompt_tokens": 18, "completion_tokens": 12, "total_tokens": 30},
		})
	case strings.Contains(prompt, "probe_noop"):
		writeJSON(writer, map[string]any{
			"id": "chat-fixture-tool", "object": "chat.completion", "model": body.Model,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{
						{"id": "call-fixture-1", "type": "function", "function": map[string]any{"name": "probe_noop", "arguments": `{"note":"ok"}`}},
					},
				},
			}},
			"usage": map[string]int{"prompt_tokens": 14, "completion_tokens": 8, "total_tokens": 22},
		})
	default:
		writeJSON(writer, map[string]any{
			"id": "chat-fixture", "object": "chat.completion", "model": body.Model,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ready"},
			}},
			"usage": map[string]int{"prompt_tokens": 12, "completion_tokens": 1, "total_tokens": 13},
		})
	}
}

func promptText(messages []struct {
	Role      string `json:"role"`
	Content   any    `json:"content"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tool_calls"`
	ToolCallID string `json:"tool_call_id"`
}) string {
	var text string
	for _, message := range messages {
		switch content := message.Content.(type) {
		case string:
			text += content + "\n"
		case map[string]any:
			if field, ok := content["text"].(string); ok {
				text += field + "\n"
			}
		}
	}
	return text
}
