package app

// request_metrics.go: HTTP/gRPC 请求指标埋点与 relay 拦截器。CONTEXT「审计
// 与执行溯源」把匿名登录失败、CSRF/畸形请求、无效组件身份与 429 排除在
// SQLite 审计之外、以「有界指标」承接可见性——本文件是那四个预初始化
// family 的唯一生产者。route_group/rpc_group/method/grpc_status 词表全部来自
// contracts/metrics.yaml 的封闭标签集（openapi.yaml tags/methods、runtime.proto
// services 与 metrics 自有枚举的投影），测试对权威目录钉住不漂移。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// routeGroupOf 把一个 API 路径映射到 catalog 封闭的 route_group
// （= openapi.yaml 顶层 tags 词表）。映射的完备性由
// TestRouteGroupCoversAdmissionSurface 对整个 admission 声明面钉住。
func routeGroupOf(path string) string {
	rest := strings.TrimPrefix(path, "/api/v1/")
	switch {
	case rest == "alerts/events":
		return "realtime"
	case strings.HasPrefix(rest, "auth/"):
		return "auth"
	case rest == "admin/about":
		// openapi 把 /admin/about 归 adminops 而非 admin。
		return "adminops"
	case strings.HasPrefix(rest, "admin/"), rest == "audit-events":
		return "admin"
	case strings.HasPrefix(rest, "maintenance"):
		return "maintenance"
	case rest == "alerts", strings.HasPrefix(rest, "alerts/"), strings.HasPrefix(rest, "alert-intake-issues"):
		return "alerts"
	case strings.HasPrefix(rest, "investigations"), strings.HasPrefix(rest, "investigation-attachments"):
		return "investigations"
	case strings.HasPrefix(rest, "inspections"):
		return "inspections"
	case strings.HasPrefix(rest, "knowledge"):
		return "knowledge"
	case rest == "artifacts/retention-settings":
		return "adminops"
	case strings.HasPrefix(rest, "artifacts"), strings.HasPrefix(rest, "evidence"):
		// evidence 正文下载在 openapi 归 artifacts 标签，但 catalog 的封闭
		// route_group 词表没有该值——归入 files（同为文件传输面）。
		return "files"
	default:
		// adminops：连接、备份、告警源、业务视图、插件目录等管理操作面。
		return "adminops"
	}
}

// methodOf 把请求方法投影到 catalog 封闭的 method 词表（openapi.yaml 的
// methods：get/patch/post/put）。词表之外的方法（DELETE、OPTIONS、任意
// token）不属于声明面——准入层会拒绝它们——而封闭词表里没有容纳它们的
// 值，发明标签会破坏 OPS-METRIC 的封闭枚举纪律，所以这类请求不产生
// 观测（false）。
func methodOf(method string) (string, bool) {
	switch strings.ToLower(method) {
	case "get", "patch", "post", "put":
		return strings.ToLower(method), true
	default:
		return "", false
	}
}

// isStreamingPath 判定长生命周期流式路由：family 帮助文本明确「SSE 与升级
// WebSocket 不计入 duration」（请求计数照常）。
func isStreamingPath(path string) bool {
	return path == "/api/v1/alerts/events" || strings.HasSuffix(path, "/stream")
}

// statusRecorder 捕获响应状态码。语义与 net/http 对齐：
//   - 隐式 200：handler 从不写头也从未写 body 时按 200 计；
//   - 首个终结状态生效：第一次 >= 200 的 WriteHeader（或第一次隐式写）
//     固定观测值，之后多余的 WriteHeader 只透传不改写；
//   - 1xx 信息性响应（Early Hints、101 协议升级）只透传、不终结也不记
//     录——升级连接没有 >= 200 的终结状态，保持隐式 200（封闭词表没有
//     1xx 类）。
type statusRecorder struct {
	http.ResponseWriter
	status    int
	finalized bool
}

// Unwrap 让 http.ResponseController 穿透包装拿到底层连接能力（备份/产物
// 下载的 SetWriteDeadline 豁免、WebSocket 升级的 Hijack）。没有它，包装
// 会让这些能力静默降级为 ErrNotSupported——大文件传输将重新落回 30s
// WriteTimeout 被截断（HTTP-FILE-007 禁止静默截断）。
func (recorder *statusRecorder) Unwrap() http.ResponseWriter {
	return recorder.ResponseWriter
}

func (recorder *statusRecorder) WriteHeader(status int) {
	if status >= 200 && !recorder.finalized {
		recorder.status = status
		recorder.finalized = true
	}
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *statusRecorder) Write(body []byte) (int, error) {
	// 第一次写 body 隐式终结响应（200）；其后的 WriteHeader 是多余的。
	recorder.finalized = true
	return recorder.ResponseWriter.Write(body)
}

// Flush 转发 http.Flusher（SSE 依赖；包装不能打掉流式能力）。ResponseController
// 的路径由 Unwrap 承担，这里保留直接类型断言的兼容。
func (recorder *statusRecorder) Flush() {
	if flusher, ok := recorder.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func statusClassOf(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

// requestMetricsMiddleware 计数 /api/ 请求的 route_group/method/status_class
// 并观测非流式请求时长。匿名失败（登录失败、CSRF 拒绝、429）与已认证流量
// 同口径计数——它们是审计豁免类别的唯一量化载体。观测放在 defer 里：
// handler panic 时也必须完成计数（未写出任何字节即 panic 的请求按 5xx 计
// ——服务端故障不能被记成 2xx），随后原样重抛交给 net/http 的连接级恢复。
func requestMetricsMiddleware(metrics *sharedops.RequestMetrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		method, bounded := methodOf(request.Method)
		if !bounded || !strings.HasPrefix(request.URL.Path, "/api/") || metrics == nil {
			next.ServeHTTP(writer, request)
			return
		}
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
		defer func() {
			recovered := recover()
			class := statusClassOf(recorder.status)
			if recovered != nil && !recorder.finalized {
				// panic 且未写出终结状态：按 5xx 计（真实 writer 不会为它
				// 发出任何状态行，连接直接断开）。
				class = "5xx"
			}
			metrics.HTTPRequests.WithLabelValues(routeGroupOf(request.URL.Path), method, class).Inc()
			if !isStreamingPath(request.URL.Path) {
				metrics.HTTPDuration.WithLabelValues(routeGroupOf(request.URL.Path), method, class).Observe(time.Since(started).Seconds())
			}
			if recovered != nil {
				panic(recovered)
			}
		}()
		next.ServeHTTP(recorder, request)
	})
}

// rpcGroupOf 把 gRPC full method 映射到 catalog 封闭的 rpc_group（relay 上
// 只挂这三个服务；未知服务名按 runtime_control 计并留诊断，不发明词表）。
func rpcGroupOf(fullMethod string) string {
	switch {
	case strings.Contains(fullMethod, "ArtifactService"):
		return "artifact_service"
	case strings.Contains(fullMethod, "SteleRelay"):
		return "stele_relay"
	default:
		return "runtime_control"
	}
}

// grpcStatusOf 把 grpc code 映射到 catalog 封闭的 grpc_status 词表。权威
// 枚举（contracts/metrics.yaml label_sets.grpc_status，17 值）与 gRPC code
// 一一对应，包括 unauthenticated——组件身份失败有自带的词表值，不并入
// permission_denied。TestGRPCStatusMapping 对目录钉住完备性。
func grpcStatusOf(err error) string {
	if err == nil {
		return "ok"
	}
	switch status.Code(err) {
	case codes.OK:
		return "ok"
	case codes.Canceled:
		return "cancelled"
	case codes.InvalidArgument:
		return "invalid_argument"
	case codes.DeadlineExceeded:
		return "deadline_exceeded"
	case codes.NotFound:
		return "not_found"
	case codes.AlreadyExists:
		return "already_exists"
	case codes.PermissionDenied:
		return "permission_denied"
	case codes.Unauthenticated:
		return "unauthenticated"
	case codes.ResourceExhausted:
		return "resource_exhausted"
	case codes.FailedPrecondition:
		return "failed_precondition"
	case codes.Aborted:
		return "aborted"
	case codes.OutOfRange:
		return "out_of_range"
	case codes.Unimplemented:
		return "unimplemented"
	case codes.Internal:
		return "internal"
	case codes.Unavailable:
		return "unavailable"
	case codes.DataLoss:
		return "data_loss"
	default:
		return "unknown"
	}
}

// relayMetricsInterceptor 计数 relay 请求的 rpc_group/grpc_status 并观测
// 时长，同时承担 panic recovery：gRPC 没有内建恢复，handler panic 会击穿
// 整个控制面进程——恢复为 codes.Internal 并记诊断。诊断只携带方法定位符
// 与事实（CONTEXT「秘密与日志」的字段白名单）：recover 的值可能包含敏感
// 数据，绝不进入日志。
func relayMetricsInterceptor(metrics *sharedops.RequestMetrics) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (response any, err error) {
		started := time.Now()
		defer func() {
			if recover() != nil {
				sharedops.LogEvent("quoin", "error", "relay.handler_panic", fmt.Sprintf("method=%s", info.FullMethod))
				response, err = nil, status.Error(codes.Internal, "internal error")
			}
			group, code := rpcGroupOf(info.FullMethod), grpcStatusOf(err)
			metrics.GRPCRequests.WithLabelValues(group, code).Inc()
			metrics.GRPCDuration.WithLabelValues(group, code).Observe(time.Since(started).Seconds())
		}()
		return handler(ctx, request)
	}
}

// relayStreamMetricsInterceptor 是流式版本：计数按 RPC 终结时的最终状态
// （长控制流一条 series），同样承担 panic recovery（诊断同样不含 recover
// 值）。
func relayStreamMetricsInterceptor(metrics *sharedops.RequestMetrics) grpc.StreamServerInterceptor {
	return func(server any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		started := time.Now()
		defer func() {
			if recover() != nil {
				sharedops.LogEvent("quoin", "error", "relay.stream_panic", fmt.Sprintf("method=%s", info.FullMethod))
				err = status.Error(codes.Internal, "internal error")
			}
			group, code := rpcGroupOf(info.FullMethod), grpcStatusOf(err)
			metrics.GRPCRequests.WithLabelValues(group, code).Inc()
			metrics.GRPCDuration.WithLabelValues(group, code).Observe(time.Since(started).Seconds())
		}()
		return handler(server, stream)
	}
}
