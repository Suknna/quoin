package worker

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
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

// TestKubernetesReadInvalidOrUngrantableCannotFetchCredentials drives
// Runner.executeTool rather than a helper branch. The capability shipped its
// real executor, so the security boundary moved from "tool unknown" to the
// executor's own preflight ordering: an INVALID request and an UNGRANTED
// call must both seal their model-visible failure BEFORE any credential
// fetch. The client would happily answer with a reachable Kubernetes-looking
// endpoint, so any regression past the preflight return is observable as a
// fetch, not inferred from implementation structure.
func TestKubernetesReadInvalidOrUngrantableCannotFetchCredentials(t *testing.T) {
	assembleTestTypedExecutors(t)
	tests := []struct {
		name          string
		args          map[string]any
		grants        []*runtimev1.ConnectionGrant
		wantErrorCode string
	}{
		{
			name:          "invalid request without operation",
			args:          map[string]any{},
			grants:        []*runtimev1.ConnectionGrant{{GrantId: 71}},
			wantErrorCode: "invalid_arguments",
		},
		{
			name:          "valid request without frozen grant",
			args:          map[string]any{"operation": "get", "namespace": "default", "name": "api"},
			grants:        nil,
			wantErrorCode: "grant_missing",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			control := &fakeToolCallChannel{}
			client := &fetchCountingRuntimeClient{}
			meta := toolMeta{name: "kubernetes_read", mode: "TOOL_EXECUTION_MODE_SUPERVISOR_TYPED", arguments: testCase.args, grants: testCase.grants}
			runner := &Runner{Client: client, toolCalls: control, tools: map[int64]toolMeta{73: meta}}
			if err := runner.executeTool(context.Background(), NewFrameWriter(&bytes.Buffer{}), 41, 73, meta); err != nil {
				t.Fatal(err)
			}
			if client.fetches.Load() != 0 {
				t.Fatalf("FetchCredentialGrant calls=%d, want 0", client.fetches.Load())
			}
			if len(control.completes) != 1 || control.completes[0].GetErrorCode() != testCase.wantErrorCode {
				t.Fatalf("completions=%+v, want one %s result", control.completes, testCase.wantErrorCode)
			}
		})
	}
}
