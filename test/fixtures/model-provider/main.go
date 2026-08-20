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
		var body struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
			Tools []map[string]any `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "invalid body", http.StatusBadRequest)
			return
		}
		writer.Header().Set("X-Request-Id", "req-fixture-"+fmt.Sprint(time.Now().UnixNano()))
		if body.Stream {
			serveStream(writer, body.Model)
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

// serveStream emits SSE chunks; the broken model id disconnects after two
// chunks (partial stream failure fixture).
func serveStream(writer http.ResponseWriter, model string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-store")
	flusher, _ := writer.(http.Flusher)
	chunk := func(payload string) {
		fmt.Fprintf(writer, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	words := []string{"count", "ing", " slow", "ly"}
	for index, word := range words {
		chunk(fmt.Sprintf(`{"id":"chat-fixture","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, model, word))
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
	chunk(`{"id":"chat-fixture","object":"chat.completion.chunk","model":` + jsonQuote(model) + `,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	chunk("[DONE]")
}

func jsonQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// serveCompletion answers tool calls deterministically by prompt content.
func serveCompletion(writer http.ResponseWriter, body struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
	Tools []map[string]any `json:"tools"`
}) {
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
	Role    string `json:"role"`
	Content any    `json:"content"`
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
