package fixtures

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProviderLeg is one black-box observation against the deterministic
// OpenAI-compatible fixture. The fixture only answers; this probe is the
// party that observes and records (ARCH-VALIDATION-003,
// VERIFY-EXTERNAL-002: Release Qualification model traffic goes through
// the deterministic fixture only).
type ProviderLeg struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// ProviderFixtureAPIKey is the frozen bearer credential the fixture
// answers; it is fixture-public test material, never a product or
// production credential.
const ProviderFixtureAPIKey = "fixture-api-key-2026"

// ProviderFixtureDefaultModels are the frozen model ids.
var ProviderFixtureDefaultModels = []string{"fixture-chat-1", "fixture-chat-thanos", "fixture-embed-1"}

// ProbeProviderFixture drives the frozen fixture contract over real
// HTTP: model listing, bearer rejection, deterministic completions with
// usage and request ids, native and parallel tool calls, SSE stream
// order and cancellation, and embedding dimensions/determinism.
func ProbeProviderFixture(ctx context.Context, baseURL string) []ProviderLeg {
	probe := &providerProbe{base: strings.TrimSuffix(baseURL, "/"), client: &http.Client{Timeout: 30 * time.Second}}
	return []ProviderLeg{
		probe.modelsList(ctx),
		probe.bearerRejected(ctx),
		probe.completionDefault(ctx),
		probe.requestIDsDistinct(ctx),
		probe.toolCallNative(ctx),
		probe.toolCallsParallel(ctx),
		probe.streamOrder(ctx),
		probe.streamCancel(ctx),
		probe.embeddingDimensions(ctx),
		probe.embeddingDeterministic(ctx),
	}
}

type providerProbe struct {
	base   string
	client *http.Client
}

func (probe *providerProbe) post(ctx context.Context, path string, payload any, headers map[string]string) (int, http.Header, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, probe.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+ProviderFixtureAPIKey)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := probe.client.Do(request)
	if err != nil {
		return 0, nil, nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	return response.StatusCode, response.Header, responseBody, err
}

func leg(name string, passed bool, format string, arguments ...any) ProviderLeg {
	return ProviderLeg{Name: name, Passed: passed, Detail: fmt.Sprintf(format, arguments...)}
}

func (probe *providerProbe) modelsList(ctx context.Context) ProviderLeg {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probe.base+"/v1/models", nil)
	if err != nil {
		return leg("models-list", false, "request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+ProviderFixtureAPIKey)
	response, err := probe.client.Do(request)
	if err != nil {
		return leg("models-list", false, "transport: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return leg("models-list", false, "body: %v", err)
	}
	observed := make([]string, 0, len(listing.Data))
	for _, model := range listing.Data {
		observed = append(observed, model.ID)
	}
	return leg("models-list", strings.Join(observed, ",") == strings.Join(ProviderFixtureDefaultModels, ","), "models=%v", observed)
}

func (probe *providerProbe) bearerRejected(ctx context.Context) ProviderLeg {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probe.base+"/v1/models", nil)
	if err != nil {
		return leg("bearer-rejected", false, "request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer wrong-key")
	response, err := probe.client.Do(request)
	if err != nil {
		return leg("bearer-rejected", false, "transport: %v", err)
	}
	defer response.Body.Close()
	io.Copy(io.Discard, response.Body)
	rejected := response.StatusCode == http.StatusUnauthorized && response.Header.Get("WWW-Authenticate") == "Bearer"
	return leg("bearer-rejected", rejected, "status=%d wwwAuthenticate=%q", response.StatusCode, response.Header.Get("WWW-Authenticate"))
}

type completionDocument struct {
	ID      string `json:"id"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func chatPayload(prompt string) map[string]any {
	return map[string]any{
		"model":    "fixture-chat-1",
		"messages": []map[string]any{{"role": "user", "content": prompt}},
	}
}

func (probe *providerProbe) completionDefault(ctx context.Context) ProviderLeg {
	status, _, body, err := probe.post(ctx, "/v1/chat/completions", chatPayload("hello fixture"), nil)
	if err != nil {
		return leg("completion-default", false, "transport: %v", err)
	}
	var completion completionDocument
	if err := json.Unmarshal(body, &completion); err != nil {
		return leg("completion-default", false, "body: %v", err)
	}
	passed := status == http.StatusOK &&
		len(completion.Choices) == 1 &&
		completion.Choices[0].Message.Content == "ready" &&
		completion.Choices[0].FinishReason == "stop" &&
		completion.Usage.TotalTokens > 0
	return leg("completion-default", passed, "status=%d content=%q usage=%+v", status, completion.Choices[0].Message.Content, completion.Usage)
}

func (probe *providerProbe) requestIDsDistinct(ctx context.Context) ProviderLeg {
	_, first, _, err := probe.post(ctx, "/v1/chat/completions", chatPayload("id probe one"), nil)
	if err != nil {
		return leg("request-ids-distinct", false, "transport: %v", err)
	}
	_, second, _, err := probe.post(ctx, "/v1/chat/completions", chatPayload("id probe two"), nil)
	if err != nil {
		return leg("request-ids-distinct", false, "transport: %v", err)
	}
	passed := first.Get("X-Request-Id") != "" && first.Get("X-Request-Id") != second.Get("X-Request-Id")
	return leg("request-ids-distinct", passed, "first=%q second=%q", first.Get("X-Request-Id"), second.Get("X-Request-Id"))
}

func (probe *providerProbe) toolCallNative(ctx context.Context) ProviderLeg {
	status, _, body, err := probe.post(ctx, "/v1/chat/completions", chatPayload("please run probe_noop"), nil)
	if err != nil {
		return leg("tool-call-native", false, "transport: %v", err)
	}
	var completion completionDocument
	if err := json.Unmarshal(body, &completion); err != nil {
		return leg("tool-call-native", false, "body: %v", err)
	}
	calls := completion.Choices[0].Message.ToolCalls
	passed := status == http.StatusOK && len(calls) == 1 && calls[0].Function.Name == "probe_noop" && calls[0].ID != "" && completion.Choices[0].FinishReason == "tool_calls"
	return leg("tool-call-native", passed, "calls=%d finish=%q", len(calls), completion.Choices[0].FinishReason)
}

func (probe *providerProbe) toolCallsParallel(ctx context.Context) ProviderLeg {
	status, _, body, err := probe.post(ctx, "/v1/chat/completions", chatPayload("run both probe tools"), nil)
	if err != nil {
		return leg("tool-calls-parallel", false, "transport: %v", err)
	}
	var completion completionDocument
	if err := json.Unmarshal(body, &completion); err != nil {
		return leg("tool-calls-parallel", false, "body: %v", err)
	}
	calls := completion.Choices[0].Message.ToolCalls
	passed := status == http.StatusOK && len(calls) == 2 && calls[0].ID != calls[1].ID
	return leg("tool-calls-parallel", passed, "calls=%d", len(calls))
}

// streamOrder opens a real SSE stream and records the frame order: at
// least one content delta, exactly one terminal finish frame, and
// data: [DONE] as the final frame.
func (probe *providerProbe) streamOrder(ctx context.Context) ProviderLeg {
	payload := chatPayload("stream me a deterministic answer")
	payload["stream"] = true
	status, _, body, err := probe.post(ctx, "/v1/chat/completions", payload, nil)
	if err != nil {
		return leg("stream-order", false, "transport: %v", err)
	}
	frames := []string{}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "data: ") {
			frames = append(frames, strings.TrimPrefix(line, "data: "))
		}
	}
	contentFrames, finishFrames, doneTerminal := 0, 0, false
	for index, frame := range frames {
		if frame == "[DONE]" {
			if index == len(frames)-1 {
				doneTerminal = true
			}
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
			return leg("stream-order", false, "frame %d: %v", index, err)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if chunk.Choices[0].Delta.Content != "" {
			contentFrames++
		}
		if chunk.Choices[0].FinishReason != nil {
			finishFrames++
		}
	}
	passed := status == http.StatusOK && contentFrames >= 1 && finishFrames == 1 && doneTerminal
	return leg("stream-order", passed, "status=%d contentFrames=%d finishFrames=%d doneTerminal=%t", status, contentFrames, finishFrames, doneTerminal)
}

// streamCancel proves the cancellable-delay path: a stream with a
// server-side delay must be cancellable from the client before any
// frame arrives. The fixture must be running with a completion delay
// for this leg; a fixture without delay answers instantly and the leg
// reports that distinction rather than failing.
func (probe *providerProbe) streamCancel(ctx context.Context) ProviderLeg {
	payload := chatPayload("cancel me mid delay")
	payload["stream"] = true
	cancelCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	_, _, _, err := probe.post(cancelCtx, "/v1/chat/completions", payload, nil)
	if err == nil {
		return leg("stream-cancel", false, "stream answered inside the cancellation window; fixture delay missing")
	}
	passed := cancelCtx.Err() == context.DeadlineExceeded || strings.Contains(err.Error(), "context deadline exceeded")
	return leg("stream-cancel", passed, "client aborted before completion: %v", err)
}

func (probe *providerProbe) embeddingDimensions(ctx context.Context) ProviderLeg {
	dimensionOf := func(model string) int {
		status, _, body, err := probe.post(ctx, "/v1/embeddings", map[string]any{"model": model, "input": []string{"维度探针"}}, nil)
		if err != nil || status != http.StatusOK {
			return -1
		}
		var listing struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &listing); err != nil || len(listing.Data) == 0 {
			return -1
		}
		return len(listing.Data[0].Embedding)
	}
	standard, wide := dimensionOf("fixture-embed-1"), dimensionOf("fixture-embed-wide")
	passed := standard == 16 && wide == 32
	return leg("embedding-dimensions", passed, "standard=%d wide=%d", standard, wide)
}

func (probe *providerProbe) embeddingDeterministic(ctx context.Context) ProviderLeg {
	embed := func() []float64 {
		status, _, body, err := probe.post(ctx, "/v1/embeddings", map[string]any{"model": "fixture-embed-1", "input": []string{"确定性探针 identical input"}}, nil)
		if err != nil || status != http.StatusOK {
			return nil
		}
		var listing struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &listing) != nil || len(listing.Data) == 0 {
			return nil
		}
		return listing.Data[0].Embedding
	}
	first, second := embed(), embed()
	passed := first != nil && len(first) == len(second)
	if passed {
		for index := range first {
			if first[index] != second[index] {
				passed = false
				break
			}
		}
	}
	return leg("embedding-deterministic", passed, "identical=%t dimensions=%d", passed, len(first))
}
