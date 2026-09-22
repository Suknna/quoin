package ops

// 回归：ops /metrics 必须暴露运行期计数（Fix：运行期 Metrics 的私有
// registry 未并入 ops 导出面时，事件投递后计数恒 0）。测试走真实链路：
// 真实 alertmanager 解析器 + 真实 SQLite 队列 + 真实转发循环 + 真实
// ops 导出面；仅两个按接口注入的出站边界（凭据快照、DeliverEvents 上游）
// 使用测试替身——被测的观测链路本身没有任何 mock。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/plugins"
	_ "github.com/Suknna/quoin/internal/plugins/builtin"
	"github.com/Suknna/quoin/internal/stele"
)

// acceptDeliverer 是出站 gRPC 上游的替身：每个事件都裁决 ACCEPTED。
type acceptDeliverer struct{}

func (acceptDeliverer) DeliverEvents(_ context.Context, events []*runtimev1.RelayEvent) (*runtimev1.DeliverEventsResponse, error) {
	results := make([]runtimev1.EventDeliveryStatus, len(events))
	for index := range results {
		results[index] = runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_ACCEPTED
	}
	return &runtimev1.DeliverEventsResponse{Results: results}, nil
}

// readyLookup 是凭据快照的替身：就绪并接受测试 bearer。
type readyLookup struct{}

func (readyLookup) Ready() bool { return true }

func (readyLookup) Credential(string, string) (int64, int64, uint64, bool) { return 1, 1, 1, true }

func scrapeMetrics(t *testing.T, server *sharedops.Server) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("ops /metrics code=%d", recorder.Code)
	}
	return recorder.Body.String()
}

func TestOpsMetricsCountRealDelivery(t *testing.T) {
	server, err := sharedops.New("stele", "127.0.0.1:0", sharedops.DependencyUnavailable)
	if err != nil {
		t.Fatal(err)
	}
	// 与生产装配一致：运行期 Metrics 的 registry 并入 ops 导出面。
	metrics := stele.NewMetrics()
	if err := server.RegisterComponentGatherer(metrics.Registry()); err != nil {
		t.Fatal(err)
	}

	queue, err := stele.OpenQueue(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	forwarder := stele.NewForwarder(queue, acceptDeliverer{}, metrics)
	go forwarder.Run(ctx)

	webhook := stele.NewWebhook(queue, readyLookup{}, plugins.Default(), metrics)
	receiver := httptest.NewServer(webhook.Handler())
	defer receiver.Close()

	// 投递前：接线后的族以预置零值可见（封闭标签契约不因接线破坏）。
	if body := scrapeMetrics(t, server); !strings.Contains(body, `stele_events_forwarded_total{delivery_status="accepted"} 0`) {
		t.Fatalf("pre-delivery exposition misses the closed zero series:\n%s", body)
	}

	// 真实入站：Alertmanager webhook 载荷经真实解析器入队（202 即 ACK）。
	payload := `{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"ops-metrics-regression"},"annotations":{"summary":"regression"},"startsAt":"2026-09-21T15:00:00Z","endsAt":"2026-09-21T15:05:00Z"}]}`
	request, err := http.NewRequest(http.MethodPost, receiver.URL+"/webhook/alertmanager", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test-bearer")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook intake code=%d, want 202", response.StatusCode)
	}

	// 投递后：真实 ops /metrics 必须计数（转发循环兜底周期 2s，这里轮询）。
	deadline := time.Now().Add(10 * time.Second)
	var body string
	for {
		body = scrapeMetrics(t, server)
		if strings.Contains(body, `stele_events_forwarded_total{delivery_status="accepted"} 1`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ops /metrics never counted the delivered event:\n%s", body)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// 覆盖语义而非重复导出：同名族只出现一组封闭序列（3 个状态各一条）。
	for _, status := range []string{"accepted", "rejected", "unavailable"} {
		if got := strings.Count(body, `stele_events_forwarded_total{delivery_status="`+status+`"}`); got != 1 {
			t.Fatalf("stele_events_forwarded_total{%s} exposed %d times, want exactly 1", status, got)
		}
	}
	// 入站裁决同样经真实链路计数并可见：一次 202 = 一次 accepted。
	if !strings.Contains(body, `stele_deliveries_total{delivery_status="accepted"} 1`) {
		t.Fatalf("ops /metrics does not expose the accepted intake verdict of the real 202 delivery")
	}
	// 未被运行期接线的目录族与基础导出面保持原样（合并不破坏目录契约）。
	if !strings.Contains(body, "stele_ready 0") {
		t.Fatalf("catalog-owned stele_ready gauge lost from the merged exposition")
	}
	if !strings.Contains(body, "stele_quoin_available 0") {
		t.Fatalf("catalog placeholder stele_quoin_available lost from the merged exposition")
	}
}
