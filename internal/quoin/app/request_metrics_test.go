package app

// 请求指标与 relay 拦截器测试：route_group 映射对 admission 声明面完备、
// HTTP 中间件计数标签正确、拦截器 panic recovery 返回 codes.Internal 且
// 计数照常、Unauthenticated 归 unauthenticated 的词表纪律。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/prometheus/client_golang/prometheus"
	prometheusdto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func testRequestMetrics(t *testing.T) *sharedops.RequestMetrics {
	t.Helper()
	server, err := sharedops.New("quoin", "127.0.0.1:0", sharedops.Ready)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := server.RequestMetrics()
	if err != nil {
		t.Fatal(err)
	}
	return metrics
}

func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	counter, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatal(err)
	}
	var model prometheusdto.Metric
	if err := counter.Write(&model); err != nil {
		t.Fatal(err)
	}
	return model.GetCounter().GetValue()
}

func histogramCount(t *testing.T, vec *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	observer, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatal(err)
	}
	var model prometheusdto.Metric
	if err := observer.(prometheus.Metric).Write(&model); err != nil {
		t.Fatal(err)
	}
	return model.GetHistogram().GetSampleCount()
}

// route_group 映射必须覆盖整个 admission 声明面且只产出 catalog 封闭词表
// 内的值（admin/adminops/alerts/auth/config/files/inspections/
// investigations/knowledge/maintenance/realtime）。
func TestRouteGroupCoversAdmissionSurface(t *testing.T) {
	closed := map[string]bool{
		"admin": true, "adminops": true, "alerts": true, "auth": true, "config": true,
		"files": true, "inspections": true, "investigations": true, "knowledge": true,
		"maintenance": true, "realtime": true,
	}
	table := accessDeclarationTable()
	if len(table) == 0 {
		t.Fatal("admission declaration table is empty")
	}
	for id, declaration := range table {
		group := routeGroupOf(declaration.Path)
		if !closed[group] {
			t.Fatalf("%s (%s) maps to off-catalog group %q", id, declaration.Path, group)
		}
	}
	// 钉住关键分组（防回归漂移）。
	for path, want := range map[string]string{
		"/api/v1/auth/login":                   "auth",
		"/api/v1/auth/oidc/callback":           "auth",
		"/api/v1/admin/users":                  "admin",
		"/api/v1/admin/about":                  "adminops",
		"/api/v1/audit-events":                 "admin",
		"/api/v1/alerts":                       "alerts",
		"/api/v1/alerts/events":                "realtime",
		"/api/v1/alert-intake-issues":          "alerts",
		"/api/v1/investigations":               "investigations",
		"/api/v1/investigation-attachments":    "investigations",
		"/api/v1/inspections/runs":             "inspections",
		"/api/v1/knowledge/candidates":         "knowledge",
		"/api/v1/artifacts/1":                  "files",
		"/api/v1/artifacts/retention-settings": "adminops",
		"/api/v1/evidence/9":                   "files",
		"/api/v1/connections":                  "adminops",
		"/api/v1/backups":                      "adminops",
		"/api/v1/maintenance":                  "maintenance",
	} {
		if got := routeGroupOf(path); got != want {
			t.Fatalf("routeGroupOf(%s)=%s, want %s", path, got, want)
		}
	}
}

func TestHTTPMetricsMiddlewareCountsRequests(t *testing.T) {
	metrics := testRequestMetrics(t)
	handler := requestMetricsMiddleware(metrics, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/auth/login" {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/alerts", nil))
	if got := counterValue(t, metrics.HTTPRequests, "auth", "post", "4xx"); got != 1 {
		t.Fatalf("login 4xx count=%v, want 1", got)
	}
	if got := counterValue(t, metrics.HTTPRequests, "alerts", "get", "2xx"); got != 1 {
		t.Fatalf("alerts 2xx count=%v, want 1", got)
	}
	if got := histogramCount(t, metrics.HTTPDuration, "auth", "post", "4xx"); got != 1 {
		t.Fatalf("login duration samples=%d, want 1", got)
	}
	// 流式路由计数但不出 duration 样本。
	streamHandler := requestMetricsMiddleware(metrics, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(2 * time.Millisecond)
	}))
	streamHandler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/alerts/events", nil))
	if got := counterValue(t, metrics.HTTPRequests, "realtime", "get", "2xx"); got != 1 {
		t.Fatalf("sse count=%v, want 1", got)
	}
	if got := histogramCount(t, metrics.HTTPDuration, "realtime", "get", "2xx"); got != 0 {
		t.Fatalf("sse must not produce duration samples, got %d", got)
	}
}

func TestUnaryRelayInterceptorRecoversPanicAndCounts(t *testing.T) {
	metrics := testRequestMetrics(t)
	interceptor := relayMetricsInterceptor(metrics)
	info := &grpc.UnaryServerInfo{FullMethod: "/runtime.v1.RuntimeControl/FetchCredentialGrant"}
	_, err := interceptor(context.Background(), nil, info, func(ctx context.Context, request any) (any, error) {
		panic("boom")
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("panic must surface as codes.Internal, got %v", err)
	}
	if got := counterValue(t, metrics.GRPCRequests, "runtime_control", "internal"); got != 1 {
		t.Fatalf("panic count=%v, want 1 (recovery still counts)", got)
	}
	// 正常路径与状态映射。
	_, err = interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/runtime.v1.SteleRelay/Connect"}, func(ctx context.Context, request any) (any, error) {
		return nil, status.Error(codes.Unauthenticated, "stele client identity required")
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("handler error must pass through, got %v", err)
	}
	if got := counterValue(t, metrics.GRPCRequests, "stele_relay", "unauthenticated"); got != 1 {
		t.Fatalf("unauthenticated must count as unauthenticated, got %v", got)
	}
	if _, err = interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/runtime.v1.ArtifactService/Upload"}, func(ctx context.Context, request any) (any, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := counterValue(t, metrics.GRPCRequests, "artifact_service", "ok"); got != 1 {
		t.Fatalf("ok count=%v, want 1", got)
	}
}

func TestStreamRelayInterceptorRecoversPanicAndCounts(t *testing.T) {
	metrics := testRequestMetrics(t)
	interceptor := relayStreamMetricsInterceptor(metrics)
	info := &grpc.StreamServerInfo{FullMethod: "/runtime.v1.SteleRelay/Connect"}
	err := interceptor(nil, nil, info, func(server any, stream grpc.ServerStream) error {
		panic("stream boom")
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("stream panic must surface as codes.Internal, got %v", err)
	}
	if got := counterValue(t, metrics.GRPCRequests, "stele_relay", "internal"); got != 1 {
		t.Fatalf("stream panic count=%v, want 1", got)
	}
}

func TestGRPCStatusMapping(t *testing.T) {
	if got := grpcStatusOf(nil); got != "ok" {
		t.Fatalf("nil=%s", got)
	}
	if got := grpcStatusOf(status.Error(codes.Unauthenticated, "x")); got != "unauthenticated" {
		t.Fatalf("unauthenticated=%s, want unauthenticated", got)
	}
	if got := grpcStatusOf(errors.New("plain")); got != "unknown" {
		t.Fatalf("plain error=%s, want unknown", got)
	}
	if got := grpcStatusOf(status.Error(codes.Canceled, "x")); got != "cancelled" {
		t.Fatalf("canceled=%s", got)
	}
}
