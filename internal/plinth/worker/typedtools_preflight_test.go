package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	plinthruntime "github.com/Suknna/quoin/internal/plinth/runtime"
	"google.golang.org/grpc"
)

// fakeToolCallChannel exercises the same BeginToolCall -> CompleteToolCall
// control request sequence used by a live Runner, while retaining every
// request for the no-side-effect assertion below.
type fakeToolCallChannel struct {
	begins    []*runtimev1.BeginToolCall
	completes []*runtimev1.CompleteToolCall
}

func (channel *fakeToolCallChannel) Request(_ context.Context, envelope *runtimev1.ControlEnvelope) (*runtimev1.ControlEnvelope, error) {
	if begin := envelope.GetBeginToolCall(); begin != nil {
		channel.begins = append(channel.begins, begin)
		return &runtimev1.ControlEnvelope{Msg: &runtimev1.ControlEnvelope_BeginToolCallAck{
			BeginToolCallAck: &runtimev1.BeginToolCallAck{Accepted: true},
		}}, nil
	}
	if complete := envelope.GetCompleteToolCall(); complete != nil {
		channel.completes = append(channel.completes, complete)
		return &runtimev1.ControlEnvelope{Msg: &runtimev1.ControlEnvelope_CompleteToolCallAck{
			CompleteToolCallAck: &runtimev1.CompleteToolCallAck{Accepted: true, CommittedPayload: complete.GetPayload()},
		}}, nil
	}
	return nil, fmt.Errorf("unexpected control request %T", envelope.Msg)
}

func (*fakeToolCallChannel) BearerToken() (string, error) { return "test-runtime-token", nil }

// fetchCountingRuntimeClient provides a valid Kubernetes response if the
// execution ever reaches FetchCredentialGrant. A preflight must never do so.
type fetchCountingRuntimeClient struct {
	fetches     atomic.Int64
	endpoint    string
	lastBoot    string
	lastEpoch   uint64
	denyGrantID int64
}

func (*fetchCountingRuntimeClient) Register(context.Context, *runtimev1.RegisterRuntimeRequest, ...grpc.CallOption) (*runtimev1.RegisterRuntimeResponse, error) {
	return nil, fmt.Errorf("Register is not expected in typed tool execution")
}

func (*fetchCountingRuntimeClient) Connect(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[runtimev1.ControlEnvelope, runtimev1.ControlEnvelope], error) {
	return nil, fmt.Errorf("Connect is not expected in typed tool execution")
}

func (client *fetchCountingRuntimeClient) FetchCredentialGrant(_ context.Context, request *runtimev1.FetchCredentialGrantRequest, _ ...grpc.CallOption) (*runtimev1.FetchCredentialGrantResponse, error) {
	client.fetches.Add(1)
	if request.GetGrantId() == client.denyGrantID {
		return nil, fmt.Errorf("grant denied")
	}
	client.lastBoot = request.GetBootId()
	client.lastEpoch = request.GetConnectionEpoch()
	return &runtimev1.FetchCredentialGrantResponse{
		RevisionConfigJson: []byte(`{"defaultNamespace":"default"}`),
		Secret: &runtimev1.FetchCredentialGrantResponse_Kubernetes{Kubernetes: &runtimev1.KubernetesCredentialSecret{
			Kubeconfig: fmt.Sprintf(`apiVersion: v1
clusters:
- name: fixture
  cluster:
    server: %s
    insecure-skip-tls-verify: true
contexts:
- name: fixture
  context:
    cluster: fixture
    user: fixture
current-context: fixture
users:
- name: fixture
  user:
    token: fixture
`, client.endpoint),
		}},
	}, nil
}

// TestKubernetesReadPreflightIsModelVisibleAndCannotReachCredentialsOrAPI
// drives Runner.executeTool rather than a helper branch. It deliberately
// supplies a frozen grant and a reachable Kubernetes-looking HTTP endpoint:
// any regression past the preflight return is observable as a fetch or HTTP
// request, not inferred from implementation structure.
func TestKubernetesReadFetchesUsingFrozenDispatchBinding(t *testing.T) {
	kubernetesAPI := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api" {
			t.Fatalf("unexpected Kubernetes path %s", request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{"versions":["v1"]}`))
	}))
	defer kubernetesAPI.Close()
	control := &fakeToolCallChannel{}
	client := &fetchCountingRuntimeClient{endpoint: kubernetesAPI.URL}
	meta := toolMeta{name: "kubernetes_read", mode: "TOOL_EXECUTION_MODE_SUPERVISOR_TYPED", arguments: map[string]any{"businessSystem": "payments", "operation": "discovery"}, grants: []*runtimev1.ConnectionGrant{{GrantId: 71}}}
	runner := &Runner{
		Client: client, toolCalls: control,
		Binding: plinthruntime.DispatchBinding{BootID: "frozen-boot", Epoch: 7},
		tools:   map[int64]toolMeta{73: meta},
	}
	var frames bytes.Buffer
	if err := runner.executeTool(context.Background(), NewFrameWriter(&frames), 41, 73, meta); err != nil {
		t.Fatal(err)
	}
	if client.fetches.Load() != 1 || client.lastBoot != "frozen-boot" || client.lastEpoch != 7 {
		t.Fatalf("FetchCredentialGrant binding=%q/%d calls=%d, want frozen-boot/7 and one call", client.lastBoot, client.lastEpoch, client.fetches.Load())
	}
}

func TestKubernetesReadPreflightIsModelVisibleAndCannotReachCredentialsOrAPI(t *testing.T) {
	var kubernetesRequests atomic.Int64
	kubernetesAPI := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		kubernetesRequests.Add(1)
	}))
	defer kubernetesAPI.Close()

	cases := []struct {
		name   string
		code   string
		detail string
	}{
		{name: "target not found", code: "target_not_found", detail: "未找到该业务系统，请提供业务系统 key 或准确名称。"},
		{name: "target ambiguous", code: "target_ambiguous", detail: "该业务系统名称对应多个对象，请提供业务系统 key。"},
		{name: "no mapping", code: "no_mapping", detail: "该业务系统尚未绑定可用的 Kubernetes 连接。"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			control := &fakeToolCallChannel{}
			client := &fetchCountingRuntimeClient{endpoint: kubernetesAPI.URL}
			runner := &Runner{
				Client:    client,
				toolCalls: control,
			}
			meta := toolMeta{
				name: "kubernetes_read", mode: "TOOL_EXECUTION_MODE_SUPERVISOR_TYPED",
				arguments: map[string]any{"businessSystem": "payments", "operation": "discovery"},
				// A non-empty grant prevents a test from passing merely because the
				// normal executor would reject an empty grant before attempting it.
				grants:          []*runtimev1.ConnectionGrant{{GrantId: 71}},
				preflightCode:   test.code,
				preflightDetail: test.detail,
			}
			var frames bytes.Buffer
			if err := runner.executeTool(context.Background(), NewFrameWriter(&frames), 41, 73, meta); err != nil {
				t.Fatal(err)
			}

			if len(control.begins) != 1 || control.begins[0].GetAttemptId() != 41 || control.begins[0].GetToolCallId() != 73 {
				t.Fatalf("BeginToolCall requests=%+v", control.begins)
			}
			if len(control.completes) != 1 {
				t.Fatalf("CompleteToolCall requests=%+v", control.completes)
			}
			complete := control.completes[0]
			if complete.GetOutcome() != runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_FAILED || complete.GetErrorCode() != test.code || complete.GetErrorDetail() != test.detail {
				t.Fatalf("model-visible preflight completion=%+v", complete)
			}
			if client.fetches.Load() != 0 {
				t.Fatalf("FetchCredentialGrant calls=%d, want 0", client.fetches.Load())
			}
			if kubernetesRequests.Load() != 0 {
				t.Fatalf("Kubernetes HTTP requests=%d, want 0", kubernetesRequests.Load())
			}

			reader := NewFrameReader(bytes.NewReader(frames.Bytes()))
			started, err := reader.Read()
			if err != nil {
				t.Fatal(err)
			}
			if got := started.GetToolCallStarted(); got == nil || got.GetToolCallId() != 73 || got.GetExecutionMode() != runtimev1.ToolExecutionMode_TOOL_EXECUTION_MODE_SUPERVISOR_TYPED {
				t.Fatalf("ToolCallStarted=%+v", started)
			}
			result, err := reader.Read()
			if err != nil {
				t.Fatal(err)
			}
			if got := result.GetToolResult(); got == nil || got.GetSuccess() || got.GetErrorCode() != test.code || got.GetErrorDetail() != test.detail {
				t.Fatalf("ToolResult=%+v", result)
			}
		})
	}
}

func TestKubernetesReadContinuesAfterOneGrantFails(t *testing.T) {
	var requests atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		_, _ = writer.Write([]byte(`{"versions":["v1"]}`))
	}))
	defer api.Close()
	control := &fakeToolCallChannel{}
	client := &fetchCountingRuntimeClient{endpoint: api.URL, denyGrantID: 1}
	meta := toolMeta{name: "kubernetes_read", mode: "TOOL_EXECUTION_MODE_SUPERVISOR_TYPED", arguments: map[string]any{"businessSystem": "payments", "operation": "discovery"}, grants: []*runtimev1.ConnectionGrant{{GrantId: 1}, {GrantId: 2}}}
	runner := &Runner{Client: client, toolCalls: control, Binding: plinthruntime.DispatchBinding{BootID: "frozen", Epoch: 4}, tools: map[int64]toolMeta{73: meta}}
	var frames bytes.Buffer
	if err := runner.executeTool(context.Background(), NewFrameWriter(&frames), 41, 73, meta); err != nil {
		t.Fatal(err)
	}
	if client.fetches.Load() != 2 || requests.Load() != 1 {
		t.Fatalf("fetches=%d requests=%d, want 2/1", client.fetches.Load(), requests.Load())
	}
	if len(control.completes) != 1 {
		t.Fatalf("completions=%d", len(control.completes))
	}
	var payload struct {
		Success   bool   `json:"success"`
		ErrorCode string `json:"errorCode"`
		Results   []struct {
			Success bool `json:"success"`
		} `json:"results"`
	}
	if err := json.Unmarshal(control.completes[0].GetPayload().GetCanonicalJson(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Success || payload.ErrorCode != "partial_failure" || len(payload.Results) != 2 || payload.Results[0].Success || !payload.Results[1].Success {
		t.Fatalf("payload=%s", control.completes[0].GetPayload().GetCanonicalJson())
	}
}
