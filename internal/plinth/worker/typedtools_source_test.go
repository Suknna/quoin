package worker

// Real invocation-path tests for the thanos_query v3 supervisor executor
// (ADR-0004): the model proposes query (+ optional sourceRef); the grant
// frozen in the authorization transaction binds the source connection. The
// preflight guards must seal their model-visible failure and RETURN — the
// missing-return regression crashed the supervisor on meta.grants[0] — and
// a valid source-mode call must reach the upstream exactly once.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"google.golang.org/grpc"
	plinthruntime "github.com/Suknna/quoin/internal/plinth/runtime"
)

// thanosGrantRuntimeClient answers FetchCredentialGrant with a Thanos secret
// whose baseUrl points at the test upstream, counting every fetch.
type thanosGrantRuntimeClient struct {
	fetches     atomic.Int64
	baseURL     string
	denyGrantID int64
}

func (*thanosGrantRuntimeClient) Register(context.Context, *runtimev1.RegisterRuntimeRequest, ...grpc.CallOption) (*runtimev1.RegisterRuntimeResponse, error) {
	return nil, fmt.Errorf("Register is not expected in typed tool execution")
}

func (*thanosGrantRuntimeClient) Connect(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[runtimev1.ControlEnvelope, runtimev1.ControlEnvelope], error) {
	return nil, fmt.Errorf("Connect is not expected in typed tool execution")
}

func (client *thanosGrantRuntimeClient) FetchCredentialGrant(_ context.Context, request *runtimev1.FetchCredentialGrantRequest, _ ...grpc.CallOption) (*runtimev1.FetchCredentialGrantResponse, error) {
	client.fetches.Add(1)
	if request.GetGrantId() == client.denyGrantID {
		return nil, fmt.Errorf("grant denied")
	}
	return &runtimev1.FetchCredentialGrantResponse{
		RevisionConfigJson: []byte(fmt.Sprintf(`{"type":"thanos","baseUrl":%q}`, client.baseURL)),
		Secret: &runtimev1.FetchCredentialGrantResponse_Thanos{Thanos: &runtimev1.ThanosCredentialSecret{
			Username: "fixture", Password: "fixture",
		}},
	}, nil
}

func TestThanosQuerySourceModeSucceedsWithoutResourceRef(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.URL.Path, "/api/v1/query") {
			http.NotFound(writer, request)
			return
		}
		fmt.Fprint(writer, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up"},"value":[1757700000,"1"]}]}}`)
	}))
	defer upstream.Close()

	control := &fakeToolCallChannel{}
	client := &thanosGrantRuntimeClient{baseURL: upstream.URL}
	meta := toolMeta{
		name: "thanos_query", mode: "TOOL_EXECUTION_MODE_SUPERVISOR_TYPED",
		arguments: map[string]any{"query": "up", "sourceRef": "prom-main"},
		grants:    []*runtimev1.ConnectionGrant{{GrantId: 71}},
	}
	runner := &Runner{
		Client: client, toolCalls: control,
		Binding: plinthruntime.DispatchBinding{BootID: "boot-source", Epoch: 3},
		Config:  RunnerConfig{WorkspaceRoot: t.TempDir()},
		tools:   map[int64]toolMeta{73: meta},
	}
	if err := runner.executeTool(context.Background(), NewFrameWriter(&bytes.Buffer{}), 41, 73, meta); err != nil {
		t.Fatal(err)
	}
	if client.fetches.Load() != 1 {
		t.Fatalf("FetchCredentialGrant calls=%d, want exactly 1", client.fetches.Load())
	}
	if len(control.completes) != 1 {
		t.Fatalf("completions=%d, want exactly one", len(control.completes))
	}
	result := control.completes[0].GetPayload()
	if result.GetSchemaKind() != "thanos_query_result_v1" {
		t.Fatalf("schema kind = %q, want thanos_query_result_v1", result.GetSchemaKind())
	}
	payload := string(result.GetCanonicalJson())
	if !strings.Contains(payload, `"success":true`) || !strings.Contains(payload, `"resultType":"vector"`) {
		t.Fatalf("payload=%s, want a successful vector query result", payload)
	}
}

// The missing-return regression: an ungrantable call must seal exactly one
// grant_missing completion without fetching credentials and without
// reaching meta.grants[0] (which panicked the supervisor).
func TestThanosQueryWithoutGrantSealsOnceAndNeverFetches(t *testing.T) {
	control := &fakeToolCallChannel{}
	client := &thanosGrantRuntimeClient{baseURL: "http://127.0.0.1:1", denyGrantID: -1}
	meta := toolMeta{
		name: "thanos_query", mode: "TOOL_EXECUTION_MODE_SUPERVISOR_TYPED",
		arguments: map[string]any{"query": "up"},
	}
	runner := &Runner{
		Client: client, toolCalls: control,
		Binding: plinthruntime.DispatchBinding{BootID: "boot-source", Epoch: 3},
		Config:  RunnerConfig{WorkspaceRoot: t.TempDir()},
		tools:   map[int64]toolMeta{73: meta},
	}
	if err := runner.executeTool(context.Background(), NewFrameWriter(&bytes.Buffer{}), 41, 73, meta); err != nil {
		t.Fatal(err)
	}
	if client.fetches.Load() != 0 {
		t.Fatalf("FetchCredentialGrant calls=%d, want 0", client.fetches.Load())
	}
	if len(control.completes) != 1 || control.completes[0].GetErrorCode() != "grant_missing" {
		t.Fatalf("completions=%+v, want exactly one grant_missing result", control.completes)
	}
}
