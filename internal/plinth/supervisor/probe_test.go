package supervisor

// model_provider 资格探测的 supervisor 路径测试：AttemptAccept →
// FetchCredentialGrant(model_probe_chat) → modelprovider.Run 六项冻结动作
// （真实 HTTP 打向 httptest provider，ledger 经 Channel.Request 回环 ack）
// → 封闭 connection_probe_model_provider_v1 的 passed 提案；metrics grant
// 与缺失 grant 的确定性失败形状。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/plinth/runtime"
	"google.golang.org/grpc"
)

// probeStream 记录出向帧（Channel 的测试 sendStream）。
type probeStream struct {
	mu   sync.Mutex
	sent []*runtimev1.ControlEnvelope
}

func (stream *probeStream) Send(envelope *runtimev1.ControlEnvelope) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.sent = append(stream.sent, envelope)
	return nil
}

func (stream *probeStream) snapshot() []*runtimev1.ControlEnvelope {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return append([]*runtimev1.ControlEnvelope(nil), stream.sent...)
}

// grantStubClient 只实现 FetchCredentialGrant（Connect 在本路径不会被调）。
type grantStubClient struct {
	response *runtimev1.FetchCredentialGrantResponse
	err      error
}

func (stub *grantStubClient) Connect(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[runtimev1.ControlEnvelope, runtimev1.ControlEnvelope], error) {
	return nil, errors.New("Connect is not used by the probe path")
}

func (stub *grantStubClient) FetchCredentialGrant(context.Context, *runtimev1.FetchCredentialGrantRequest, ...grpc.CallOption) (*runtimev1.FetchCredentialGrantResponse, error) {
	return stub.response, stub.err
}

// modelCallResponder 监听出向帧并回注 Begin/CompleteModelCall 的 ack
// （等效 Quoin 侧 runtime_modelcall 的 ledger 裁决）。
func modelCallResponder(channel *runtime.Channel, sink *runtime.FrameSink, stream *probeStream, stop <-chan struct{}) {
	answered := 0
	for {
		select {
		case <-stop:
			return
		default:
		}
		frames := stream.snapshot()
		for ; answered < len(frames); answered++ {
			envelope := frames[answered]
			correlation := envelope.GetCorrelationId()
			switch envelope.GetMsg().(type) {
			case *runtimev1.ControlEnvelope_BeginModelCall:
				channel.DispatchServerFrameForTest(sink, nil, &runtimev1.ControlEnvelope{
					CorrelationId: correlation,
					Msg: &runtimev1.ControlEnvelope_BeginModelCallAck{BeginModelCallAck: &runtimev1.BeginModelCallAck{
						Accepted: true, ModelCallId: int64(answered + 1),
					}},
				})
			case *runtimev1.ControlEnvelope_CompleteModelCall:
				channel.DispatchServerFrameForTest(sink, nil, &runtimev1.ControlEnvelope{
					CorrelationId: correlation,
					Msg: &runtimev1.ControlEnvelope_CompleteModelCallAck{CompleteModelCallAck: &runtimev1.CompleteModelCallAck{
						Accepted: true,
					}},
				})
			}
		}
		time.Sleep(time.Millisecond)
	}
}

// fakeProvider 实现资格探测六项动作的最小 OpenAI 兼容端点。
func fakeProvider(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Request-Id", "req-probe-1")
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		switch {
		case request.URL.Path == "/v1/embeddings":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3,0.4]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`))
		case request.URL.Path == "/v1/chat/completions" && body["stream"] == true:
			writer.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := writer.(http.Flusher)
			fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n")
			flusher.Flush()
			fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n")
			flusher.Flush()
			// 取消动作会在两块之间取消 ctx；慢 [DONE] 让 RunCancellation
			// 的取消路径与 RunChatStream 的正常收尾都成立。
			select {
			case <-request.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			fmt.Fprint(writer, "data: [DONE]\n\n")
			flusher.Flush()
		case request.URL.Path == "/v1/chat/completions" && body["tools"] != nil:
			writer.Header().Set("Content-Type", "application/json")
			calls := `[{"id":"c1","type":"function","function":{"name":"probe_noop","arguments":"{}"}}]`
			if tools, _ := body["tools"].([]any); len(tools) > 1 {
				calls = `[{"id":"c1","type":"function","function":{"name":"probe_noop","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"probe_noop_second","arguments":"{}"}}]`
			}
			_, _ = writer.Write([]byte(`{"choices":[{"message":{"tool_calls":` + calls + `},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":6,"total_tokens":16}}`))
		case request.URL.Path == "/v1/chat/completions":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"id":"req-probe-1","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
}

// awaitProbeFrame 等待满足条件的出向帧。
func awaitProbeFrame(t *testing.T, stream *probeStream, match func(*runtimev1.ControlEnvelope) bool) *runtimev1.ControlEnvelope {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, envelope := range stream.snapshot() {
			if match(envelope) {
				return envelope
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("expected outbound frame was not sent")
	return nil
}

func newProbeSupervisor(t *testing.T) (*Supervisor, *runtime.Channel, *probeStream) {
	t.Helper()
	channel, err := runtime.NewChannel(runtime.ChannelConfig{Slot: "plinth"})
	if err != nil {
		t.Fatal(err)
	}
	stream := &probeStream{}
	channel.SetSendStreamForTest(stream)
	supervisor := &Supervisor{Channel: channel}
	return supervisor, channel, stream
}

func probeDispatch(grantPurpose string) *runtimev1.DispatchAttempt {
	return &runtimev1.DispatchAttempt{
		AttemptId:   42,
		AttemptType: runtimev1.AttemptType_ATTEMPT_TYPE_CONNECTION_PROBE,
		ScopeType:   runtimev1.ScopeType_SCOPE_TYPE_CONNECTION, ScopeId: 7,
		Input: &runtimev1.AttemptInputSnapshot{
			SchemaKind: "connection_probe_v1", CanonicalJson: []byte(`{"connectionName":"main-model"}`),
			ConnectionGrants: []*runtimev1.ConnectionGrant{{GrantId: 91, ConnectionRevisionId: 3, CredentialGenerationId: 4, Purpose: grantPurpose}},
		},
	}
}

func TestProbeModelProviderPassesAgainstLiveProvider(t *testing.T) {
	provider := fakeProvider(t)
	defer provider.Close()
	supervisor, channel, stream := newProbeSupervisor(t)
	stop := make(chan struct{})
	defer close(stop)
	go modelCallResponder(channel, runtime.NewFrameSinkForTest(channel), stream, stop)
	client := &grantStubClient{response: &runtimev1.FetchCredentialGrantResponse{
		ConnectionType:     "model_provider",
		RevisionConfigJson: []byte(fmt.Sprintf(`{"type":"openai","baseUrl":%q,"chatModelId":"chat-1","embeddingModelId":"emb-1"}`, provider.URL)),
		Secret:             &runtimev1.FetchCredentialGrantResponse_ModelProvider{ModelProvider: &runtimev1.ModelProviderCredentialSecret{ApiKey: "test-key"}},
	}}
	sink := runtime.NewFrameSinkForTest(channel)
	supervisor.HandleDispatchAttempt(context.Background(), sink, client, probeDispatch("model_probe_chat"), runtime.DispatchBinding{BootID: "probe-boot", Epoch: 3}, func(id int64) bool { channel.FinishTask(id); return true })

	// Accept 先行，结果提案随后封闭 passed。
	awaitProbeFrame(t, stream, func(envelope *runtimev1.ControlEnvelope) bool {
		return envelope.GetAttemptAccept() != nil && envelope.GetAttemptAccept().GetAttemptId() == 42
	})
	proposal := awaitProbeFrame(t, stream, func(envelope *runtimev1.ControlEnvelope) bool {
		return envelope.GetResultProposal() != nil
	}).GetResultProposal()
	if proposal.GetOutcome() != runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		var detail map[string]any
		_ = json.Unmarshal(proposal.GetPayload().GetCanonicalJson(), &detail)
		t.Fatalf("outcome=%v detail=%v", proposal.GetOutcome(), detail)
	}
	if proposal.GetPayload().GetSchemaKind() != "connection_probe_model_provider_v1" {
		t.Fatalf("schema=%q", proposal.GetPayload().GetSchemaKind())
	}
	if proposal.GetBootId() != "probe-boot" || proposal.GetConnectionEpoch() != 3 {
		t.Fatalf("proposal fence=(%s,%d), want the frozen dispatch binding", proposal.GetBootId(), proposal.GetConnectionEpoch())
	}
	var payload struct {
		Outcome string          `json:"outcome"`
		Detail  json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(proposal.GetPayload().GetCanonicalJson(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Outcome != "passed" {
		t.Fatalf("payload outcome=%q", payload.Outcome)
	}
	var typed map[string]any
	if err := json.Unmarshal(payload.Detail, &typed); err != nil {
		t.Fatal(err)
	}
	for _, capability := range []string{"streamingSupported", "nativeToolCallingSupported", "multiToolCallSupported", "cancellationObserved", "usageObserved", "requestIdObserved", "embeddingSupported"} {
		if typed[capability] != true {
			t.Fatalf("capability %s=%v, want true (detail=%v)", capability, typed[capability], typed)
		}
	}
	if typed["embeddingVectorDim"] != float64(4) {
		t.Fatalf("embeddingVectorDim=%v", typed["embeddingVectorDim"])
	}
}

func TestProbeRejectsNonModelProviderGrant(t *testing.T) {
	supervisor, channel, stream := newProbeSupervisor(t)
	// metrics grant（prometheus_probe）已由 Quoin 本地执行，派发到这里的
	// 按确定性失败提案收口，而不是跑去执行。
	client := &grantStubClient{response: &runtimev1.FetchCredentialGrantResponse{ConnectionType: "prometheus"}}
	sink := runtime.NewFrameSinkForTest(channel)
	supervisor.HandleDispatchAttempt(context.Background(), sink, client, probeDispatch("prometheus_probe"), runtime.DispatchBinding{BootID: "probe-boot", Epoch: 1}, func(id int64) bool { channel.FinishTask(id); return true })
	proposal := awaitProbeFrame(t, stream, func(envelope *runtimev1.ControlEnvelope) bool {
		return envelope.GetResultProposal() != nil
	}).GetResultProposal()
	if proposal.GetOutcome() != runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED {
		t.Fatalf("outcome=%v, want failed", proposal.GetOutcome())
	}
	if !strings.Contains(string(proposal.GetPayload().GetCanonicalJson()), "model_probe_chat") {
		t.Fatalf("failure detail must explain the missing model_probe_chat grant: %s", proposal.GetPayload().GetCanonicalJson())
	}
}

func TestProbeWithoutGrantProposesFailure(t *testing.T) {
	supervisor, channel, stream := newProbeSupervisor(t)
	dispatch := probeDispatch("model_probe_chat")
	dispatch.Input.ConnectionGrants = nil
	sink := runtime.NewFrameSinkForTest(channel)
	supervisor.HandleDispatchAttempt(context.Background(), sink, &grantStubClient{}, dispatch, runtime.DispatchBinding{BootID: "probe-boot", Epoch: 1}, func(id int64) bool { channel.FinishTask(id); return true })
	proposal := awaitProbeFrame(t, stream, func(envelope *runtimev1.ControlEnvelope) bool {
		return envelope.GetResultProposal() != nil
	}).GetResultProposal()
	if proposal.GetOutcome() != runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED {
		t.Fatalf("outcome=%v, want failed", proposal.GetOutcome())
	}
}
