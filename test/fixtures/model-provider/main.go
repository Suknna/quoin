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
	"regexp"
	"strings"
	"time"
)

func main() {
	address := flag.String("address", "127.0.0.1:18443", "listen address")
	completionDelay := flag.Duration("completion-delay", 0, "deterministic cancellable delay before each chat response")
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
				{"id": "fixture-chat-thanos", "object": "model", "owned_by": "fixture"},
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
			serveStream(writer, request, body, *completionDelay)
			return
		}
		serveCompletion(writer, request, body, *completionDelay)
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

// thanosArtifactID extracts the committed artifact locator from the
// thanos_query tool result the fixture itself drives (the model context
// receives the artifact summary inside the result payload).
func thanosArtifactID(body chatRequest) string {
	for _, message := range body.Messages {
		if message.Role != "tool" {
			continue
		}
		match := thanosArtifactPattern.FindStringSubmatch(fmt.Sprint(message.Content))
		if len(match) == 2 {
			return match[1]
		}
	}
	return ""
}

var thanosArtifactPattern = regexp.MustCompile(`"artifact"\s*:\s*\{"id"\s*:\s*"(\d+)"`)

// attachmentLocatorPattern extracts the artifactId from the frozen
// attachment locator block the investigation input renders for user
// messages ([附件 N] name（artifactId=X，Y 字节）).
var attachmentLocatorPattern = regexp.MustCompile(`artifactId=(\d+)`)

// artifactReadTarget resolves the artifactId from the LAST user turn's
// locator block (the current turn's attachments; earlier turns' blocks
// stay in the conversation but are not re-read).
func artifactReadTarget(body chatRequest) string {
	for index := len(body.Messages) - 1; index >= 0; index-- {
		message := body.Messages[index]
		if message.Role != "user" {
			continue
		}
		match := attachmentLocatorPattern.FindStringSubmatch(fmt.Sprint(message.Content))
		if len(match) == 2 {
			return match[1]
		}
		return ""
	}
	return ""
}

// attachmentEcho projects a bounded slice of the artifact_read tool
// result the attachment branch itself drove (the committed output text).
func attachmentEcho(body chatRequest) string {
	for _, message := range body.Messages {
		if message.Role != "tool" {
			continue
		}
		match := artifactOutputPattern.FindStringSubmatch(fmt.Sprint(message.Content))
		if len(match) == 2 {
			content := match[1]
			if len(content) > 24 {
				content = content[:24]
			}
			return content
		}
	}
	return "（未读到内容）"
}

var artifactOutputPattern = regexp.MustCompile(`"output"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// browserSessionTarget extracts only the opaque session id from an already
// committed quoin_browser result. T22 never derives a session from a URL,
// DOM, or profile path.
var browserSessionPattern = regexp.MustCompile(`"sessionId"\s*:\s*"([^"]+)"`)

func browserSessionTarget(body chatRequest) string {
	for _, message := range body.Messages {
		if message.Role != "tool" {
			continue
		}
		match := browserSessionPattern.FindStringSubmatch(fmt.Sprint(message.Content))
		if len(match) == 2 {
			return match[1]
		}
	}
	return ""
}

// toolMessageCount counts the committed tool-result messages the agent
// already carries (one per executed tool call).
func toolMessageCount(body chatRequest) int {
	count := 0
	for _, message := range body.Messages {
		if message.Role == "tool" {
			count++
		}
	}
	return count
}

// toolCallChunk renders one streaming tool-call delta chunk (an empty name
// carries the trailing arguments fragment like the T10 fixture).
func toolCallChunk(model, callID, toolName, arguments string) map[string]any {
	return toolCallChunkIndexed(model, callID, 0, toolName, arguments)
}

// toolCallChunkIndexed renders one tool-call delta with its provider
// index (parallel calls in one assistant turn carry distinct indices).
func toolCallChunkIndexed(model, callID string, index int, toolName, arguments string) map[string]any {
	return map[string]any{
		"id": "chat-agent", "object": "chat.completion.chunk", "model": model,
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": nil,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": index, "id": callID, "type": "function",
				"function": map[string]any{"name": toolName, "arguments": arguments},
			}}},
		}},
	}
}

// serveStream emits SSE chunks; the broken model id disconnects after two
// chunks (partial stream failure fixture). The agent's first call answers a
// streaming native tool call (bash); its continuation returns the final
// text diagnosis.
func serveStream(writer http.ResponseWriter, request *http.Request, body chatRequest, delay time.Duration) {
	if delay > 0 {
		log.Printf("stream delayed: model=%s duration=%s", body.Model, delay)
		select {
		case <-time.After(delay):
		case <-request.Context().Done():
			log.Printf("stream cancelled: model=%s", body.Model)
			return
		}
	}
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
	prompt := promptText(body.Messages)
	if strings.Contains(prompt, "Quoin 知识提炼助手") || strings.Contains(prompt, "T28 导入原文") || strings.Contains(prompt, "T28_CANCEL_SLOW") {
		if strings.Contains(prompt, "T28_CANCEL_SLOW") {
			select {
			case <-time.After(5 * time.Second):
			case <-request.Context().Done():
				return
			}
		}
		// T28: extraction returns the closed proposal consumed by the dedicated
		// worker mode; it deliberately does not enter the generic tool loop.
		content := `{"items":[{"title":"连接池超时处置","body":"先检查连接池上限与等待超时，再观察连接数和错误率。","scope":{}}]}`
		chunk(mustJSONChunk(map[string]any{"id": "chat-knowledge", "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": content}, "finish_reason": nil}}}))
		chunk(mustJSONChunk(map[string]any{"id": "chat-knowledge", "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
		chunk("[DONE]")
		return
	}
	// T13 investigation fixtures branch on the frozen investigation system
	// contract: a deterministic Chinese answer (the acceptance asserts the
	// multi-byte deltas survive the byte-level SSE framing exactly) and a
	// partial-then-hangup branch that drives the attempt failure and the
	// error frame.
	if strings.Contains(prompt, "只读运维调查代理") {
		// T22 is the real Exploration path: an Investigation model turn opens
		// an existing read-only Browser Identity, then closes that same opaque
		// session after receiving the committed result. It is deliberately not a
		// manual-login operation.
		if strings.Contains(prompt, "T22Browser") {
			key := ""
			for _, field := range strings.Fields(prompt) {
				if strings.HasPrefix(field, "t22-browser-") {
					key = field
					break
				}
			}
			if !body.hasToolResult() && key != "" {
				log.Printf("investigation (T22Browser): opening exploration for %s", key)
				arguments := fmt.Sprintf(`{"action":"open","businessSystemKey":%q}`, key)
				// Emit actual argument fragments, not a complete arguments JSON in
				// one delta. The OpenAI-compatible adapter surfaces native function
				// calls only after it joins the streaming fragments (the production
				// protocol form exercised by model/adapter_test.go).
				split := len(arguments) / 2
				chunk(mustJSONChunk(toolCallChunk(body.Model, "call-t22-open", "quoin_browser", arguments[:split])))
				chunk(mustJSONChunk(toolCallChunk(body.Model, "call-t22-open", "", arguments[split:])))
				chunk(mustJSONChunk(map[string]any{"id": "chat-t22", "object": "chat.completion.chunk", "model": body.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}}))
				chunk("[DONE]")
				return
			}
			if session := browserSessionTarget(body); session != "" && toolMessageCount(body) == 1 {
				log.Printf("investigation (T22Browser): closing exploration session")
				arguments := fmt.Sprintf(`{"action":"close_session","sessionId":%q}`, session)
				split := len(arguments) / 2
				chunk(mustJSONChunk(toolCallChunk(body.Model, "call-t22-close", "quoin_browser", arguments[:split])))
				chunk(mustJSONChunk(toolCallChunk(body.Model, "call-t22-close", "", arguments[split:])))
				chunk(mustJSONChunk(map[string]any{"id": "chat-t22", "object": "chat.completion.chunk", "model": body.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}}))
				chunk("[DONE]")
				return
			}
			if toolMessageCount(body) >= 2 {
				chunk(mustJSONChunk(map[string]any{"id": "chat-t22", "object": "chat.completion.chunk", "model": body.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "T22 浏览器探索已完成（fixture-proof-t22）。"}, "finish_reason": nil}}}))
				chunk(mustJSONChunk(map[string]any{"id": "chat-t22", "object": "chat.completion.chunk", "model": body.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
				chunk("[DONE]")
				return
			}
		}
		// T14 attachment branch: the first turn sees the frozen attachment
		// locator block; it reads the artifact body through the granted
		// artifact_read tool and echoes a bounded slice in the final answer.
		if strings.Contains(prompt, "T14Attach") || artifactReadTarget(body) != "" {
			if artifactID := artifactReadTarget(body); artifactID != "" && !body.hasToolResult() {
				log.Printf("investigation (T14Attach): streaming artifact_read on attachment %s", artifactID)
				chunk(mustJSONChunk(toolCallChunk(body.Model, "call-t14-read", "artifact_read", fmt.Sprintf(`{"artifactId":"%s"}`, artifactID))))
				time.Sleep(150 * time.Millisecond)
				chunk(mustJSONChunk(toolCallChunk(body.Model, "call-t14-read", "", "")))
				time.Sleep(150 * time.Millisecond)
				chunk(mustJSONChunk(map[string]any{
					"id": "chat-t14", "object": "chat.completion.chunk", "model": body.Model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
				}))
				chunk("[DONE]")
				return
			}
			if body.hasToolResult() {
				log.Printf("investigation (T14Attach): streaming the attachment echo answer")
				words := []string{"附件已读取：", attachmentEcho(body), "（attachment-proof-t14）", "内容完整可追溯。"}
				for _, word := range words {
					chunk(mustJSONChunk(map[string]any{
						"id": "chat-t14", "object": "chat.completion.chunk", "model": body.Model,
						"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": word}, "finish_reason": nil}},
					}))
					time.Sleep(120 * time.Millisecond)
				}
				chunk(mustJSONChunk(map[string]any{
					"id": "chat-t14", "object": "chat.completion.chunk", "model": body.Model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				}))
				chunk("[DONE]")
				return
			}
		}
		// T14 tool-chain branch: write + long bash output (spills into a
		// tool_result Artifact) + thanos_query in one turn, then the final
		// answer (the acceptance asserts the persisted tool-call timeline,
		// the spilled artifact and the sealed evidence).
		if strings.Contains(prompt, "T14Tools") && !body.hasToolResult() {
			log.Printf("investigation (T14Tools): streaming write + long bash + thanos_query")
			for index, call := range []struct{ id, name, args string }{
				{"call-t14-write", "write", `{"path":"t14.txt","content":"T14-WRITE-PROOF\n"}`},
				{"call-t14-bash", "bash", `{"command":"seq 1 30000"}`},
				{"call-t14-thanos", "thanos_query", `{"query":"big"}`},
			} {
				chunk(mustJSONChunk(toolCallChunkIndexed(body.Model, call.id, index, call.name, call.args)))
				time.Sleep(150 * time.Millisecond)
				chunk(mustJSONChunk(toolCallChunkIndexed(body.Model, call.id, index, "", "")))
				time.Sleep(150 * time.Millisecond)
			}
			chunk(mustJSONChunk(map[string]any{
				"id": "chat-t14", "object": "chat.completion.chunk", "model": body.Model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
			}))
			chunk("[DONE]")
			return
		}
		if strings.Contains(prompt, "T14Tools") && body.hasToolResult() {
			log.Printf("investigation (T14Tools): streaming the tool-chain conclusion")
			words := []string{"工具链执行完成（t14-tools-proof）：", "工作区写入/长输出产物", "与 Thanos 只读查询", "均已封存为证据。"}
			for _, word := range words {
				chunk(mustJSONChunk(map[string]any{
					"id": "chat-t14", "object": "chat.completion.chunk", "model": body.Model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": word}, "finish_reason": nil}},
				}))
				time.Sleep(120 * time.Millisecond)
			}
			chunk(mustJSONChunk(map[string]any{
				"id": "chat-t14", "object": "chat.completion.chunk", "model": body.Model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			}))
			chunk("[DONE]")
			return
		}
		// T14 sandbox adversarial branch: one turn of hostile bash probes
		// (cross-process /proc, state paths, external network, write outside
		// the workspace) plus a benign /proc read for contrast; the committed
		// tool results carry the denial evidence the acceptance asserts.
		if strings.Contains(prompt, "T14Sandbox") && !body.hasToolResult() {
			log.Printf("investigation (T14Sandbox): streaming the adversarial bash suite")
			for index, call := range []struct{ id, name, args string }{
				{"call-t14-proc", "bash", `{"command":"cat /proc/1/environ"}`},
				{"call-t14-self", "bash", `{"command":"head -3 /proc/self/status"}`},
				{"call-t14-net", "bash", `{"command":"exec 3<>/dev/tcp/127.0.0.1/9090 && echo connected"}`},
				{"call-t14-write", "bash", `{"command":"touch /tmp/t14-escape-proof && echo wrote"}`},
			} {
				chunk(mustJSONChunk(toolCallChunkIndexed(body.Model, call.id, index, call.name, call.args)))
				time.Sleep(150 * time.Millisecond)
				chunk(mustJSONChunk(toolCallChunkIndexed(body.Model, call.id, index, "", "")))
				time.Sleep(150 * time.Millisecond)
			}
			chunk(mustJSONChunk(map[string]any{
				"id": "chat-t14", "object": "chat.completion.chunk", "model": body.Model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
			}))
			chunk("[DONE]")
			return
		}
		if strings.Contains(prompt, "T14Sandbox") && body.hasToolResult() {
			log.Printf("investigation (T14Sandbox): streaming the adversarial conclusion")
			words := []string{"沙箱对抗执行完成（t14-sandbox-proof）：", "跨进程/网络/越界写入", "均被拒绝并已留痕。"}
			for _, word := range words {
				chunk(mustJSONChunk(map[string]any{
					"id": "chat-t14", "object": "chat.completion.chunk", "model": body.Model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": word}, "finish_reason": nil}},
				}))
				time.Sleep(120 * time.Millisecond)
			}
			chunk(mustJSONChunk(map[string]any{
				"id": "chat-t14", "object": "chat.completion.chunk", "model": body.Model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			}))
			chunk("[DONE]")
			return
		}
		if strings.Contains(prompt, "T13Broken") {
			log.Printf("investigation (T13Broken): partial tokens then hang up")
			for _, word := range []string{"开始", "分析"} {
				chunk(mustJSONChunk(map[string]any{
					"id": "chat-t13", "object": "chat.completion.chunk", "model": model,
					"choices": []any{map[string]any{
						"index": 0, "delta": map[string]any{"content": word}, "finish_reason": nil,
					}},
				}))
				time.Sleep(120 * time.Millisecond)
			}
			if hijacker, ok := writer.(http.Hijacker); ok {
				if connection, _, err := hijacker.Hijack(); err == nil {
					_ = connection.Close()
					return
				}
			}
			return
		}
		log.Printf("investigation: streaming deterministic Chinese answer (%d words)", 15)
		words := []string{"调查结论：", "该", "告警", "最可能", "是", "短时", "资源", "波动", "引起", "，", "建议", "观察", "后续", "指标", "（fixture-proof-t13）"}
		delay := 80 * time.Millisecond
		// The UI-CHAT-003 turn (capacity/dependency wording) streams slowly
		// so the e2e reader detach lands mid-stream deterministically.
		if strings.Contains(prompt, "容量、依赖") {
			delay = 400 * time.Millisecond
		}
		for _, word := range words {
			chunk(mustJSONChunk(map[string]any{
				"id": "chat-t13", "object": "chat.completion.chunk", "model": model,
				"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{"content": word}, "finish_reason": nil,
				}},
			}))
			time.Sleep(delay)
		}
		chunk(mustJSONChunk(map[string]any{
			"id": "chat-t13", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		}))
		chunk("[DONE]")
		return
	}
	// T12 recovery fixtures branch on the alert name carried inside the
	// agent's frozen occurrence context: a long slow text stream (cancel /
	// reconnect / lease windows) and a partial stream that hangs up before
	// [DONE] (partial-token-then-failure).
	if body.isAgentFirstTurn() && strings.Contains(prompt, "T12Partial") {
		log.Printf("agent first turn (T12Partial): partial tokens then hang up")
		for _, word := range []string{"初步诊断：", "中断前"} {
			chunk(mustJSONChunk(map[string]any{
				"id": "chat-t12", "object": "chat.completion.chunk", "model": model,
				"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{"content": word}, "finish_reason": nil,
				}},
			}))
			time.Sleep(150 * time.Millisecond)
		}
		if hijacker, ok := writer.(http.Hijacker); ok {
			if connection, _, err := hijacker.Hijack(); err == nil {
				_ = connection.Close()
				return
			}
		}
		return
	}
	if body.isAgentFirstTurn() && strings.Contains(prompt, "T12Slow") {
		log.Printf("agent first turn (T12Slow): slow text stream (%d words)", 36)
		words := make([]string, 0, 36)
		words = append(words, "初步诊断：", "该告警", "在")
		for index := 0; index < 30; index++ {
			words = append(words, fmt.Sprintf("第%d段", index+1))
		}
		words = append(words, "排查", "完成。")
		for _, word := range words {
			chunk(mustJSONChunk(map[string]any{
				"id": "chat-t12", "object": "chat.completion.chunk", "model": model,
				"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{"content": word}, "finish_reason": nil,
				}},
			}))
			time.Sleep(300 * time.Millisecond)
		}
		chunk(mustJSONChunk(map[string]any{
			"id": "chat-t12", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		}))
		chunk("[DONE]")
		return
	}
	if body.isAgentFirstTurn() && model == "fixture-chat-thanos" {
		log.Printf("agent first turn (thanos): streaming native thanos_query tool call")
		chunk(mustJSONChunk(toolCallChunk(model, "call-agent-thanos", "thanos_query", `{"query":"big"}`)))
		time.Sleep(150 * time.Millisecond)
		chunk(mustJSONChunk(toolCallChunk(model, "call-agent-thanos", "", "")))
		time.Sleep(150 * time.Millisecond)
		chunk(mustJSONChunk(map[string]any{
			"id": "chat-agent", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
		}))
		chunk("[DONE]")
		return
	}
	if artifactID := thanosArtifactID(body); artifactID != "" && model == "fixture-chat-thanos" && toolMessageCount(body) == 1 {
		log.Printf("agent continuation (thanos): streaming artifact_read on the spilled result")
		chunk(mustJSONChunk(toolCallChunk(model, "call-agent-read", "artifact_read", fmt.Sprintf(`{"artifactId":"%s"}`, artifactID))))
		time.Sleep(150 * time.Millisecond)
		chunk(mustJSONChunk(toolCallChunk(model, "call-agent-read", "", "")))
		time.Sleep(150 * time.Millisecond)
		chunk(mustJSONChunk(map[string]any{
			"id": "chat-agent", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
		}))
		chunk("[DONE]")
		return
	}
	if model == "fixture-chat-thanos" {
		// A committed tool result reaches the model context verbatim; the
		// bounded diagnostic log pins the exact sealed payload the agent
		// branch sees (success preview or structured failure).
		if body.hasToolResult() {
			for _, message := range body.Messages {
				if message.Role == "tool" {
					content := fmt.Sprint(message.Content)
					if len(content) > 400 {
						content = content[:400]
					}
					log.Printf("tool result payload: %s", content)
				}
			}
		}
		log.Printf("agent continuation (thanos): streaming text diagnosis (%d words)", 10)
		words := []string{"初步诊断：", "该告警", "通过", "thanos-proof", "只读查询", "确认了", "指标现状", "，", "建议按", "排查顺序继续。"}
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
func serveCompletion(writer http.ResponseWriter, request *http.Request, body chatRequest, delay time.Duration) {
	if delay > 0 {
		log.Printf("completion delayed: model=%s duration=%s", body.Model, delay)
		select {
		case <-time.After(delay):
		case <-request.Context().Done():
			log.Printf("completion cancelled: model=%s", body.Model)
			return
		}
	}
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
