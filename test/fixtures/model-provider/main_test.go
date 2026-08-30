package main

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
)

func fixtureChatRequest(t *testing.T, body string) chatRequest {
	t.Helper()
	var request chatRequest
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatal(err)
	}
	return request
}

// streamedToolArguments reassembles the protocol-level function argument
// fragments, exactly as the OpenAI stream adapter does before authorizing a
// ToolCall. It deliberately inspects only function metadata, never prompts.
func streamedToolArguments(t *testing.T, stream string) string {
	t.Helper()
	var arguments strings.Builder
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var event struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		for _, choice := range event.Choices {
			for _, call := range choice.Delta.ToolCalls {
				arguments.WriteString(call.Function.Arguments)
			}
		}
	}
	return arguments.String()
}

// T22's model fixture is intentionally wired through quoin_browser rather than
// a manual-login or profile-management path. This static test keeps the opt-in
// real-runtime acceptance route honest when Docker/Chromium are unavailable.
func TestT22FixtureOpensAndClosesQuoinBrowserSession(t *testing.T) {
	open := fixtureChatRequest(t, `{"model":"fixture-chat-1","stream":true,"messages":[{"role":"system","content":"只读运维调查代理"},{"role":"user","content":"T22Browser t22-browser-payments"}]}`)
	openResponse := httptest.NewRecorder()
	serveStream(openResponse, httptest.NewRequest("POST", "/v1/chat/completions", nil), open, 0)
	openBody := openResponse.Body.String()
	if !strings.Contains(openBody, `"name":"quoin_browser"`) || streamedToolArguments(t, openBody) != `{"action":"open","businessSystemKey":"t22-browser-payments"}` {
		t.Fatalf("T22 first turn did not open the keyed quoin_browser path")
	}

	close := fixtureChatRequest(t, `{"model":"fixture-chat-1","stream":true,"messages":[{"role":"system","content":"只读运维调查代理"},{"role":"user","content":"T22Browser t22-browser-payments"},{"role":"tool","content":"{\"success\":true,\"sessionId\":\"42\"}"}]}`)
	closeResponse := httptest.NewRecorder()
	serveStream(closeResponse, httptest.NewRequest("POST", "/v1/chat/completions", nil), close, 0)
	closeBody := closeResponse.Body.String()
	if !strings.Contains(closeBody, `"name":"quoin_browser"`) || streamedToolArguments(t, closeBody) != `{"action":"close_session","sessionId":"42"}` {
		t.Fatalf("T22 second turn did not close the committed opaque browser session")
	}
}

// The T29 drift acceptance switches embedding models and must observe a
// different validated dimension through the real generation path; the
// semantic channel needs token overlap (not input length) to rank honestly.
func TestT29FixtureEmbeddingDimensionsAndTokenSimilarity(t *testing.T) {
	if dimension := embeddingDimension("fixture-embed-1"); dimension != 16 {
		t.Fatalf("fixture-embed-1 dimension = %d, want 16", dimension)
	}
	if dimension := embeddingDimension("fixture-embed-wide"); dimension != 32 {
		t.Fatalf("fixture-embed-wide dimension = %d, want 32", dimension)
	}
	baseline := embedText("数据库连接池耗尽导致超时", 16)
	if len(baseline) != 16 {
		t.Fatalf("baseline dimension = %d, want 16", len(baseline))
	}
	similar := embedText("数据库连接池需要等待超时回收", 16)
	unrelated := embedText("Kubernetes node disk pressure eviction", 16)
	if cosine(baseline, similar) <= cosine(baseline, unrelated) {
		t.Fatalf("shared CJK vocabulary must outrank unrelated ASCII text: %v vs %v", cosine(baseline, similar), cosine(baseline, unrelated))
	}
	if length := len(embedText("x", 32)); length != 32 {
		t.Fatalf("wide model dimension = %d, want 32", length)
	}
}

func cosine(a, b []float64) float64 {
	dot, na, nb := 0.0, 0.0, 0.0
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
