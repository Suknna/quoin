package supervisor

// 独立计划采集范围强制测试：经真实 httptest 指标端点验证冻结条件被注入
// 表达式的每一个向量选择器（含聚合/函数/子查询的内部读取路径），矛盾不
// 放宽、重复不叠加、integration 显式放行、空/未知范围与缺条件一律在触达
// 上游之前 fail closed。断言一律用官方 promql/parser 解析实际出站 query，
// 不做子串猜测。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// scopedCollectionCapture 承载一次采集的出站事实：上游收到的查询参数、
// 请求次数（fail closed 场景必须为零）与采集错误。
type scopedCollectionCapture struct {
	mu         sync.Mutex
	query      url.Values
	hits       int
	collectErr error
}

// runScopedCollection 用真实 httptest 端点执行一次 metricsCollector 采集，
// 返回捕获的出站查询参数。服务器返回最小成功 Prometheus envelope。
func runScopedCollection(t *testing.T, request plugins.CollectRequest) *scopedCollectionCapture {
	t.Helper()
	capture := &scopedCollectionCapture{}
	server := httptestSuccessServer(t, capture)
	settings := fmt.Sprintf(`{"type":"prometheus","baseUrl":%q,"authType":"none"}`, server.URL)
	call, err := newMetricsCall(testPluginRegistry(), plugins.PrometheusID, json.RawMessage(settings), "", "", "")
	if err != nil {
		t.Fatalf("newMetricsCall failed: %v", err)
	}
	_, collectErr := (&metricsCollector{}).Collect(context.Background(), call, request)
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.collectErr = collectErr
	return capture
}

// httptestSuccessServer 记录每次请求的查询参数并返回空向量结果。
func httptestSuccessServer(t *testing.T, capture *scopedCollectionCapture) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.mu.Lock()
		capture.query = r.URL.Query()
		capture.hits++
		capture.mu.Unlock()
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// selectorMatchers 解析出站 PromQL 并返回每个向量选择器的 matcher 集合
// （源顺序）。解析失败即测试失败：出站查询必须是合法封闭语法的 PromQL。
func selectorMatchers(t *testing.T, query string) [][]*labels.Matcher {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("outbound query %q is not parsable PromQL: %v", query, err)
	}
	var selectors [][]*labels.Matcher
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		if vector, ok := node.(*parser.VectorSelector); ok {
			selectors = append(selectors, vector.LabelMatchers)
		}
		return nil
	})
	return selectors
}

// exactMatcherCount 统计一个选择器上 label=value 的精确 matcher 数。
func exactMatcherCount(matchers []*labels.Matcher, name, value string) int {
	count := 0
	for _, matcher := range matchers {
		if matcher.Type == labels.MatchEqual && matcher.Name == name && matcher.Value == value {
			count++
		}
	}
	return count
}

// assertEverySelectorCarries 断言出站查询的每个选择器都精确匹配全部条件，
// 并返回选择器数量供调用方断言注入覆盖面。
func assertEverySelectorCarries(t *testing.T, capture *scopedCollectionCapture, conditions map[string]string) int {
	t.Helper()
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.query.Get("query") == "" {
		t.Fatalf("upstream never received a query (hits=%d)", capture.hits)
	}
	selectors := selectorMatchers(t, capture.query.Get("query"))
	if len(selectors) == 0 {
		t.Fatalf("outbound query %q has no vector selectors to scope", capture.query.Get("query"))
	}
	for i, matchers := range selectors {
		for name, value := range conditions {
			if got := exactMatcherCount(matchers, name, value); got != 1 {
				t.Fatalf("selector %d of %q carries %d exact %s=%q matchers, want exactly 1", i, capture.query.Get("query"), got, name, value)
			}
		}
	}
	return len(selectors)
}

var scopeTestConditions = map[string]string{"job": "mall-mysql-exporter", "env": "prod"}

// businessView 即时采集：聚合、二值运算与既有 matcher 的选择器全部被注入
// 冻结条件，原有 matcher 原样保留。
func TestBusinessViewScopeInjectedIntoEverySelector(t *testing.T) {
	capture := runScopedCollection(t, plugins.CollectRequest{
		TemplateID: "promql_instant", TemplateVersion: "1",
		Params:     json.RawMessage(`{"expression":"sum by (job) (rate(process_cpu_seconds_total[5m])) + max_over_time(up{mode=\"idle\"}[10m])"}`),
		EvidenceAt: "2026-09-13T00:00:00Z",
		Scope:      plugins.CollectScope{Kind: plugins.ScopeBusinessView},
		Targets:    []plugins.CollectTarget{{LabelConditions: scopeTestConditions}},
	})
	if capture.collectErr != nil {
		t.Fatalf("scoped collection failed: %v", capture.collectErr)
	}
	count := assertEverySelectorCarries(t, capture, scopeTestConditions)
	if count != 2 {
		t.Fatalf("expected 2 vector selectors, got %d", count)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	wantOriginal := []int{0, 1} // 选择器0无 mode 条件；选择器1自带 mode="idle"
	for i, matchers := range selectorMatchers(t, capture.query.Get("query")) {
		if got := exactMatcherCount(matchers, "mode", "idle"); got != wantOriginal[i] {
			t.Fatalf("selector %d original matcher contract broken: mode=idle count %d, want %d (%v)", i, got, wantOriginal[i], matchers)
		}
	}
}

// 聚合+子查询的内部读取路径必须被收窄：选择器注入发生在数据读取之前，
// 结果后过滤无法给出同等保证。
func TestScopeInjectionBoundsAggregationAndSubquery(t *testing.T) {
	capture := runScopedCollection(t, plugins.CollectRequest{
		TemplateID: "promql_instant", TemplateVersion: "1",
		Params:     json.RawMessage(`{"expression":"max_over_time(sum(up) by (job)[5m:1m])"}`),
		EvidenceAt: "2026-09-13T00:00:00Z",
		Scope:      plugins.CollectScope{Kind: plugins.ScopeBusinessView},
		Targets:    []plugins.CollectTarget{{LabelConditions: map[string]string{"job": "mall-mysql-exporter"}}},
	})
	if capture.collectErr != nil {
		t.Fatalf("scoped collection failed: %v", capture.collectErr)
	}
	if count := assertEverySelectorCarries(t, capture, map[string]string{"job": "mall-mysql-exporter"}); count != 1 {
		t.Fatalf("expected 1 vector selector, got %d", count)
	}
}

// 冻结表达式自带的矛盾条件与视图条件取交集（空结果），绝不删除或改写
// 表达式自带 matcher 来放宽范围。
func TestScopeContradictionDoesNotLoosen(t *testing.T) {
	capture := runScopedCollection(t, plugins.CollectRequest{
		TemplateID: "promql_instant", TemplateVersion: "1",
		Params:     json.RawMessage(`{"expression":"up{job=\"redis\"}"}`),
		EvidenceAt: "2026-09-13T00:00:00Z",
		Scope:      plugins.CollectScope{Kind: plugins.ScopeBusinessView},
		Targets:    []plugins.CollectTarget{{LabelConditions: map[string]string{"job": "mall-mysql-exporter"}}},
	})
	if capture.collectErr != nil {
		t.Fatalf("scoped collection failed: %v", capture.collectErr)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	selectors := selectorMatchers(t, capture.query.Get("query"))
	if len(selectors) != 1 {
		t.Fatalf("expected 1 selector, got %d", len(selectors))
	}
	if exactMatcherCount(selectors[0], "job", "redis") != 1 || exactMatcherCount(selectors[0], "job", "mall-mysql-exporter") != 1 {
		t.Fatalf("contradiction must keep both exact matchers (intersection), got %v", selectors[0])
	}
}

// 选择器已携带与条件一致的精确 matcher 时不重复叠加。
func TestScopeDuplicateExactMatcherNotDuplicated(t *testing.T) {
	capture := runScopedCollection(t, plugins.CollectRequest{
		TemplateID: "promql_instant", TemplateVersion: "1",
		Params:     json.RawMessage(`{"expression":"up{job=\"mall-mysql-exporter\"}"}`),
		EvidenceAt: "2026-09-13T00:00:00Z",
		Scope:      plugins.CollectScope{Kind: plugins.ScopeBusinessView},
		Targets:    []plugins.CollectTarget{{LabelConditions: map[string]string{"job": "mall-mysql-exporter"}}},
	})
	if capture.collectErr != nil {
		t.Fatalf("scoped collection failed: %v", capture.collectErr)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	selectors := selectorMatchers(t, capture.query.Get("query"))
	if len(selectors) != 1 || exactMatcherCount(selectors[0], "job", "mall-mysql-exporter") != 1 {
		t.Fatalf("identical exact matcher must not be duplicated, got %v", selectors)
	}
}

// objects 范围只消费冻结的身份条件 map：CanonicalIdentity 即使是不可解析
// 的串也绝不参与推导（实际来源身份是 unit-separator 编码，禁止猜测解析）。
func TestObjectsScopeUsesFrozenIdentityMapOnly(t *testing.T) {
	capture := runScopedCollection(t, plugins.CollectRequest{
		TemplateID: "promql_instant", TemplateVersion: "1",
		Params:     json.RawMessage(`{"expression":"up"}`),
		EvidenceAt: "2026-09-13T00:00:00Z",
		Scope:      plugins.CollectScope{Kind: plugins.ScopeObjects},
		Targets: []plugins.CollectTarget{{
			ObjectType:        "target",
			CanonicalIdentity: "instance=10.43.211.32:9104\x1fjob=mall-mysql-exporter",
			LabelConditions:   map[string]string{"job": "mall-mysql-exporter", "instance": "10.43.211.32:9104"},
		}},
	})
	if capture.collectErr != nil {
		t.Fatalf("scoped collection failed: %v", capture.collectErr)
	}
	assertEverySelectorCarries(t, capture, map[string]string{"job": "mall-mysql-exporter", "instance": "10.43.211.32:9104"})
}

// 显式 integration 是唯一无条件采集：表达式原样出站，一个 matcher 都不添。
func TestIntegrationScopeRunsExpressionAsIs(t *testing.T) {
	capture := runScopedCollection(t, plugins.CollectRequest{
		TemplateID: "promql_instant", TemplateVersion: "1",
		Params:     json.RawMessage(`{"expression":"up{job=\"web\"}"}`),
		EvidenceAt: "2026-09-13T00:00:00Z",
		Scope:      plugins.CollectScope{Kind: plugins.ScopeIntegration},
	})
	if capture.collectErr != nil {
		t.Fatalf("integration collection failed: %v", capture.collectErr)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	// parser 会把 metric name 物化为 __name__ matcher；integration 的最强
	// 断言是出站查询串与冻结表达式逐字一致。
	if capture.query.Get("query") != `up{job="web"}` {
		t.Fatalf("integration expression must leave the query untouched, got %q", capture.query.Get("query"))
	}
}

// 范围模板同样被收窄：出站 query 携带条件 matcher，窗口参数保持不变。
func TestRangeTemplateScopeEnforced(t *testing.T) {
	capture := runScopedCollection(t, plugins.CollectRequest{
		TemplateID: "promql_range", TemplateVersion: "1",
		Params:     json.RawMessage(`{"expression":"up","rangeSeconds":600,"stepSeconds":60}`),
		EvidenceAt: "2026-09-13T00:00:00Z",
		Scope:      plugins.CollectScope{Kind: plugins.ScopeBusinessView},
		Targets:    []plugins.CollectTarget{{LabelConditions: map[string]string{"job": "mall-mysql-exporter"}}},
	})
	if capture.collectErr != nil {
		t.Fatalf("scoped range collection failed: %v", capture.collectErr)
	}
	assertEverySelectorCarries(t, capture, map[string]string{"job": "mall-mysql-exporter"})
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.query.Get("step") != "60" {
		t.Fatalf("range window must be preserved, got step=%q", capture.query.Get("step"))
	}
}

// fail closed 矩阵：空/未知范围、缺目标、缺条件、坏表达式都在触达上游前
// 被拒绝——任何一条都不允许退化为无范围全源查询。
func TestScopeFailClosedBeforeUpstream(t *testing.T) {
	scenarios := []struct {
		name    string
		request plugins.CollectRequest
	}{
		{
			name:    "missing scope kind",
			request: plugins.CollectRequest{TemplateID: "promql_instant", TemplateVersion: "1", Params: json.RawMessage(`{"expression":"up"}`)},
		},
		{
			name:    "unknown scope kind",
			request: plugins.CollectRequest{TemplateID: "promql_instant", TemplateVersion: "1", Params: json.RawMessage(`{"expression":"up"}`), Scope: plugins.CollectScope{Kind: plugins.CollectScopeKind("tenant")}},
		},
		{
			name:    "businessView without target",
			request: plugins.CollectRequest{TemplateID: "promql_instant", TemplateVersion: "1", Params: json.RawMessage(`{"expression":"up"}`), Scope: plugins.CollectScope{Kind: plugins.ScopeBusinessView}},
		},
		{
			name:    "businessView without conditions",
			request: plugins.CollectRequest{TemplateID: "promql_instant", TemplateVersion: "1", Params: json.RawMessage(`{"expression":"up"}`), Scope: plugins.CollectScope{Kind: plugins.ScopeBusinessView}, Targets: []plugins.CollectTarget{{}}},
		},
		{
			name:    "objects without conditions",
			request: plugins.CollectRequest{TemplateID: "promql_instant", TemplateVersion: "1", Params: json.RawMessage(`{"expression":"up"}`), Scope: plugins.CollectScope{Kind: plugins.ScopeObjects}, Targets: []plugins.CollectTarget{{ObjectType: "target", CanonicalIdentity: "anything"}}},
		},
		{
			name: "multiple targets merge ambiguity",
			request: plugins.CollectRequest{TemplateID: "promql_instant", TemplateVersion: "1", Params: json.RawMessage(`{"expression":"up"}`), Scope: plugins.CollectScope{Kind: plugins.ScopeBusinessView}, Targets: []plugins.CollectTarget{
				{LabelConditions: map[string]string{"job": "mall-mysql-exporter"}},
				{LabelConditions: map[string]string{"job": "other-exporter"}},
			}},
		},
		{
			name:    "invalid label name",
			request: plugins.CollectRequest{TemplateID: "promql_instant", TemplateVersion: "1", Params: json.RawMessage(`{"expression":"up"}`), Scope: plugins.CollectScope{Kind: plugins.ScopeBusinessView}, Targets: []plugins.CollectTarget{{LabelConditions: map[string]string{"bad label": "v"}}}},
		},
		{
			name:    "unparsable expression",
			request: plugins.CollectRequest{TemplateID: "promql_instant", TemplateVersion: "1", Params: json.RawMessage(`{"expression":"up{"}`), Scope: plugins.CollectScope{Kind: plugins.ScopeBusinessView}, Targets: []plugins.CollectTarget{{LabelConditions: scopeTestConditions}}},
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			capture := &scopedCollectionCapture{}
			server := httptestSuccessServer(t, capture)
			settings := fmt.Sprintf(`{"type":"prometheus","baseUrl":%q,"authType":"none"}`, server.URL)
			call, err := newMetricsCall(testPluginRegistry(), plugins.PrometheusID, json.RawMessage(settings), "", "", "")
			if err != nil {
				t.Fatalf("newMetricsCall failed: %v", err)
			}
			if _, collectErr := (&metricsCollector{}).Collect(context.Background(), call, scenario.request); collectErr == nil {
				t.Fatal("unscoped or malformed collection must fail closed")
			}
			capture.mu.Lock()
			defer capture.mu.Unlock()
			if capture.hits != 0 {
				t.Fatalf("fail-closed collection must never reach the upstream, hits=%d", capture.hits)
			}
		})
	}
}
