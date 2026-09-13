package modelprovider

// T29 batch embedding executor tests: the frozen input contract, the one
// ledger pair per batch, dimension enforcement and the closed result shape.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
)

type recordingLedger struct {
	mu        sync.Mutex
	begins    int
	budgets   [][2]uint32
	completes []*runtimev1.CompleteModelCall
}

func (ledger *recordingLedger) Begin(ctx context.Context, callSeq int, operation runtimev1.ModelOperation, modelID, inputDigest, renderedDigest string, contextBudget, maxOutput uint32) (int64, *runtimev1.ConnectionGrant, bool) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ledger.begins++
	ledger.budgets = append(ledger.budgets, [2]uint32{contextBudget, maxOutput})
	return int64(ledger.begins), &runtimev1.ConnectionGrant{Purpose: "embedding"}, true
}

func (ledger *recordingLedger) Complete(ctx context.Context, callID int64, completion *runtimev1.CompleteModelCall) bool {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ledger.completes = append(ledger.completes, completion)
	return true
}

func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/embeddings" {
			http.Error(writer, "unexpected path", http.StatusNotFound)
			return
		}
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "bad body", http.StatusBadRequest)
			return
		}
		writer.Header().Set("X-Request-Id", "req-batch-1")
		dimension := fixtureDimension(body.Model)
		data := make([]map[string]any, 0, len(body.Input))
		for index := range body.Input {
			data = append(data, map[string]any{"object": "embedding", "index": index, "embedding": rampVector(dimension)})
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"object": "list", "data": data, "model": body.Model, "usage": map[string]int{"prompt_tokens": 3, "total_tokens": 3}})
	}))
	t.Cleanup(server.Close)
	return server
}

// TestRunToolCallUsesStringContent pins the OpenAI-compatible chat wire shape
// required by DeepSeek: a tool-only turn is ordinary text, not a multimodal
// content-part object. The provider then has a chance to demonstrate a native
// tool call instead of rejecting the request before capability evaluation.
func TestRunToolCallUsesStringContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			http.Error(writer, "unexpected path", http.StatusNotFound)
			return
		}
		var body struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "bad body", http.StatusBadRequest)
			return
		}
		if len(body.Messages) != 1 || body.Messages[0].Role != "user" || !json.Valid(body.Messages[0].Content) || body.Messages[0].Content[0] != '"' {
			http.Error(writer, "tool prompt content must be a JSON string", http.StatusBadRequest)
			return
		}
		if len(body.Tools) != 1 {
			http.Error(writer, "expected one tool", http.StatusBadRequest)
			return
		}
		writer.Header().Set("X-Request-Id", "req-tool-1")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"id": "call-1", "type": "function", "function": map[string]string{"name": "probe_noop", "arguments": `{}`},
							},
						},
					},
				},
			},
		})
	}))
	t.Cleanup(server.Close)

	result, _, err := RunToolCall(context.Background(), newClient(Config{BaseURL: server.URL}, "test-key"), "fixture-chat", false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.RequestID != "req-tool-1" {
		t.Fatalf("tool capability observation wrong: %+v", result)
	}
}

// TestRunNormalizesOmittedBudgets exercises the real probe entrypoint rather
// than hand-seeded ledger rows. A revision without metadata must record every
// chat ModelCall with valid bounds, so Quoin accepts the real-call facts before
// it evaluates the probe's typed child.
func TestRunNormalizesOmittedBudgets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			http.Error(writer, "unexpected path", http.StatusNotFound)
			return
		}
		var body struct {
			Stream   bool              `json:"stream"`
			Tools    []json.RawMessage `json:"tools"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "bad body", http.StatusBadRequest)
			return
		}
		writer.Header().Set("X-Request-Id", "req-probe")
		if body.Stream {
			if len(body.Messages) == 1 && body.Messages[0].Content == cancelPrompt {
				_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"1\"}}]}\n\n"))
				if flusher, ok := writer.(http.Flusher); ok {
					flusher.Flush()
				}
				<-request.Context().Done()
				return
			}
			_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"rea\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"dy\"}}]}\n\ndata: [DONE]\n\n"))
			return
		}
		if len(body.Tools) > 0 {
			calls := []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]string{"name": "probe_noop", "arguments": `{}`}}}
			if len(body.Tools) == 2 {
				calls = append(calls, map[string]any{"id": "call-2", "type": "function", "function": map[string]string{"name": "probe_noop_second", "arguments": `{}`}})
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": calls}}}, "usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "req-usage", "usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
	}))
	t.Cleanup(server.Close)
	ledger := &recordingLedger{}
	result := Run(WithAttempt(context.Background(), 17), Config{BaseURL: server.URL, ChatModelID: "fixture-chat"}, "test-key", false, ledger)
	if !result.Passed {
		t.Fatalf("probe with supported fixture must pass: %s", result.Detail)
	}
	if len(ledger.budgets) != 5 {
		t.Fatalf("chat probe ledger calls=%d, want 5", len(ledger.budgets))
	}
	for index, budget := range ledger.budgets {
		if budget != [2]uint32{DefaultProbeContextBudgetTokens, DefaultProbeMaxOutputTokens} {
			t.Fatalf("ledger call %d budget=%v, want default valid bounds", index+1, budget)
		}
	}
}

func TestRunBatchRebuildProducesClosedResultWithOneLedgerPair(t *testing.T) {
	server := fixtureServer(t)
	ledger := &recordingLedger{}
	input := BatchInput{
		SchemaKind: "embedding_v1", AttemptID: 7, GenerationID: 3, Mode: "rebuild",
		ModelContract: ModelContract{EmbeddingModelID: "fixture-embed-1", VectorDim: 16},
		Inputs: []TextInput{
			{VersionID: 11, Text: "数据库连接池耗尽导致超时", Digest: TextDigestOf("数据库连接池耗尽导致超时")},
			{VersionID: 12, Text: "node disk pressure", Digest: TextDigestOf("node disk pressure")},
		},
	}
	canonical, digest, err := RunBatch(context.Background(), Config{BaseURL: server.URL}, "fixture-api-key-2026", input, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 32 {
		t.Fatalf("digest length = %d", len(digest))
	}
	if ledger.begins != 1 || len(ledger.completes) != 1 {
		t.Fatalf("ledger pairs = %d/%d, want exactly one begin/complete", ledger.begins, len(ledger.completes))
	}
	if ledger.completes[0].Outcome != runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_SUCCEEDED {
		t.Fatalf("ledger outcome = %v", ledger.completes[0].Outcome)
	}
	if len(ledger.completes[0].EmbeddingVectors) != 2 {
		t.Fatalf("ledger vectors = %d, want 2", len(ledger.completes[0].EmbeddingVectors))
	}
	var outcome BatchOutcome
	if err := json.Unmarshal(canonical, &outcome); err != nil {
		t.Fatal(err)
	}
	if outcome.SchemaKind != ResultSchemaKind || outcome.AttemptID != 7 || outcome.GenerationID != 3 || len(outcome.Vectors) != 2 {
		t.Fatalf("outcome envelope wrong: %+v", outcome)
	}
	for index, vector := range outcome.Vectors {
		if vector.VersionID != input.Inputs[index].VersionID || vector.Digest != input.Inputs[index].Digest || len(vector.Values) != 16 {
			t.Fatalf("vector %d wrong: %+v", index, vector)
		}
	}
}

func TestRunBatchRejectsDimensionDrift(t *testing.T) {
	server := fixtureServer(t)
	ledger := &recordingLedger{}
	input := BatchInput{
		SchemaKind: "embedding_v1", AttemptID: 8, GenerationID: 4, Mode: "rebuild",
		ModelContract: ModelContract{EmbeddingModelID: "fixture-embed-wide", VectorDim: 8}, // wide serves 32
		Inputs:        []TextInput{{VersionID: 21, Text: "drift", Digest: TextDigestOf("drift")}},
	}
	_, _, err := RunBatch(context.Background(), Config{BaseURL: server.URL}, "fixture-api-key-2026", input, ledger)
	if err == nil || !strings.Contains(err.Error(), "dimension") {
		t.Fatalf("dimension drift accepted: %v", err)
	}
	if len(ledger.completes) != 1 || ledger.completes[0].Outcome != runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED {
		t.Fatal("dimension drift must seal the physical call failed")
	}
}

func TestRunBatchQueryModeCarriesQueryDigest(t *testing.T) {
	server := fixtureServer(t)
	ledger := &recordingLedger{}
	query := TextInput{Text: "连接池怎么处理", Digest: TextDigestOf("连接池怎么处理")}
	input := BatchInput{
		SchemaKind: "embedding_v1", AttemptID: 9, GenerationID: 5, Mode: "query",
		ModelContract: ModelContract{EmbeddingModelID: "fixture-embed-1", VectorDim: 16},
		Query:         &query,
	}
	canonical, _, err := RunBatch(context.Background(), Config{BaseURL: server.URL}, "fixture-api-key-2026", input, ledger)
	if err != nil {
		t.Fatal(err)
	}
	var outcome BatchOutcome
	if err := json.Unmarshal(canonical, &outcome); err != nil {
		t.Fatal(err)
	}
	if outcome.SchemaKind != QuerySchemaKind || outcome.Digest != query.Digest || len(outcome.Values) != 16 || len(outcome.Vectors) != 0 {
		t.Fatalf("query outcome wrong: %+v", outcome)
	}
}

func TestParseBatchInputEnforcesClosedContract(t *testing.T) {
	valid := `{"schemaKind":"embedding_v1","attemptId":1,"generationId":1,"mode":"rebuild","modelContract":{"embeddingModelId":"m","vectorDim":16},"inputs":[{"versionId":1,"text":"a","digest":"d"}]}`
	if _, err := ParseBatchInput([]byte(valid), 1); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if _, err := ParseBatchInput([]byte(`{"schemaKind":"embedding_v1","attemptId":2,"mode":"rebuild"}`), 1); err == nil {
		t.Fatal("identity mismatch accepted")
	}
	if _, err := ParseBatchInput([]byte(`{"schemaKind":"other_v1","attemptId":1,"mode":"rebuild","modelContract":{"embeddingModelId":"m"},"inputs":[{"text":"a"}]}`), 1); err == nil {
		t.Fatal("unknown schema kind accepted")
	}
	if _, err := ParseBatchInput([]byte(valid+`{"trailing":true}`), 1); err == nil {
		t.Fatal("trailing data accepted")
	}
	if _, err := ParseBatchInput([]byte(strings.Replace(valid, `"mode":"rebuild"`, `"mode":"query"`, 1)), 1); err == nil {
		t.Fatal("query mode without query text accepted")
	}
}

// TextDigestOf is the canonical SHA-256 hex of the frozen text.
func TextDigestOf(text string) string {
	return DigestCanonical(text)
}

// fixtureDimension mirrors the acceptance fixture's frozen per-model
// dimensions inside this test binary.
func fixtureDimension(model string) int {
	if model == "fixture-embed-wide" {
		return 32
	}
	return 16
}

// offsetVector is a distinct deterministic vector (constant components).
func offsetVector(dimension int, value float64) []float64 {
	vector := make([]float64, dimension)
	for index := range vector {
		vector[index] = value
	}
	return vector
}

// rampVector is a deterministic unit-length test vector.
func rampVector(dimension int) []float64 {
	vector := make([]float64, dimension)
	for index := range vector {
		vector[index] = float64(index%7+1) / 7.0
	}
	return vector
}

func TestRunBatchRejectsReorderedOrMissingIndexBindings(t *testing.T) {
	// A compliant-but-reordered provider body must not silently swap two
	// texts' vectors: the explicit `index` field owns the binding.
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Request-Id", "req-shuffle")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"object": "embedding", "index": 1, "embedding": offsetVector(16, 0.5)},
				{"object": "embedding", "index": 0, "embedding": rampVector(16)},
			},
			"usage": map[string]int{"prompt_tokens": 2, "total_tokens": 2},
		})
	}))
	t.Cleanup(server.Close)
	ledger := &recordingLedger{}
	input := BatchInput{
		SchemaKind: "embedding_v1", AttemptID: 11, GenerationID: 6, Mode: "rebuild",
		ModelContract: ModelContract{EmbeddingModelID: "fixture-embed-1", VectorDim: 16},
		Inputs: []TextInput{
			{VersionID: 31, Text: "文本甲", Digest: TextDigestOf("文本甲")},
			{VersionID: 32, Text: "文本乙", Digest: TextDigestOf("文本乙")},
		},
	}
	canonical, _, err := RunBatch(context.Background(), Config{BaseURL: server.URL}, "k", input, ledger)
	if err != nil {
		t.Fatalf("reordered body rejected: %v", err)
	}
	var outcome BatchOutcome
	if jsonErr := json.Unmarshal(canonical, &outcome); jsonErr != nil {
		t.Fatal(jsonErr)
	}
	if outcome.Vectors[0].VersionID != 31 || outcome.Vectors[1].VersionID != 32 {
		t.Fatalf("index binding lost: %+v", outcome.Vectors)
	}
	// Value binding: input 0 carries the ramp vector, input 1 the offset
	// one — a position-only implementation fails here.
	if outcome.Vectors[0].Values[0] != float32(1.0/7.0) {
		t.Fatalf("input 0 vector mis-bound: %v", outcome.Vectors[0].Values[:2])
	}
	if outcome.Vectors[1].Values[0] != 0.5 {
		t.Fatalf("input 1 vector mis-bound: %v", outcome.Vectors[1].Values[:2])
	}

	// A missing or duplicate index is a hard rejection, never a silent gap.
	missing := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"object": "embedding", "index": 1, "embedding": rampVector(16)},
				{"object": "embedding", "index": 1, "embedding": rampVector(16)},
			},
		})
	}))
	t.Cleanup(missing.Close)
	if _, _, err := RunBatch(context.Background(), Config{BaseURL: missing.URL}, "k", input, ledger); err == nil {
		t.Fatal("duplicate index accepted")
	}
}

func TestRunBatchRejectsMissingIndexField(t *testing.T) {
	// An OpenAI-compatible body omitting `index` must not bind to 0.
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"object": "embedding", "embedding": rampVector(16)}},
		})
	}))
	t.Cleanup(server.Close)
	ledger := &recordingLedger{}
	input := BatchInput{
		SchemaKind: "embedding_v1", AttemptID: 12, GenerationID: 7, Mode: "query",
		ModelContract: ModelContract{EmbeddingModelID: "fixture-embed-1", VectorDim: 16},
		Query:         &TextInput{Text: "缺省 index", Digest: TextDigestOf("缺省 index")},
	}
	if _, _, err := RunBatch(context.Background(), Config{BaseURL: server.URL}, "k", input, ledger); err == nil {
		t.Fatal("missing index field accepted")
	}
}
