package builtin

// The Prometheus-compatible metrics plugins (ADR-0004, reworked by
// ADR-0011): prometheus and thanos share one typed tool set —
//
//   - thanos_query: the model-visible read-only PromQL instant query tool
//     (quoin_routed; the frozen result contract thanos_query_result_v1 is
//     unchanged);
//   - metrics_probe / metrics_discover / metrics_collect: internal tools
//     Quoin's schedulers call directly (connection probes, bounded
//     observation discovery, deterministic inspection collection). They
//     never render into model catalogs.
//
// Handlers build protocol requests (method + relative path) and interpret
// raw responses; the Stele gateway resolves the connection endpoint,
// injects credentials, rate limits and transports. Auth/TLS fields in
// connection settings are gateway concerns and are ignored here.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/Suknna/quoin/internal/plugins"
)

// queryTimeout bounds one external query call (per-call deadlines, no
// product-level totals).
const queryTimeout = 30 * time.Second

// Spill thresholds: the first bound reached sends the complete raw body into
// a tool_result Artifact and keeps only a bounded preview in the model
// context.
const (
	spillBytes   = 50 * 1024
	spillLines   = 2000
	previewBytes = 16 * 1024
)

// metricsConfigSchema is the real instance-settings schema of the metrics
// plugins: exactly the typed connection revision fields the gateway resolves
// (endpoint, TLS, auth mode). baseUrl is the only mandatory field; `type` is
// constrained per plugin when present.
func metricsConfigSchema(connectionType string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"type":          map[string]any{"type": "string", "enum": []any{connectionType}},
			"baseUrl":       map[string]any{"type": "string", "minLength": 1},
			"tlsCaPem":      map[string]any{"type": "string"},
			"tlsServerName": map[string]any{"type": "string"},
			"tlsSkipVerify": map[string]any{"type": "boolean"},
			"authType":      map[string]any{"type": "string", "enum": []any{"none", "basic", "bearer"}},
			"username":      map[string]any{"type": "string"},
		},
		"required": []any{"type", "baseUrl"},
	}
}

// ---------------------------------------------------------------------------
// thanos_query — the model-visible PromQL instant query tool
// ---------------------------------------------------------------------------

// queryArgs is the typed argument set of the shared PromQL query tool. The
// frozen v3 contract made sourceRef the only optional locator; the v4 bump
// records the execution-location move to quoin_routed (ADR-0011).
type queryArgs struct {
	Query     string `json:"query" doc:"要执行的 PromQL 即时查询表达式"`
	SourceRef string `json:"sourceRef,omitempty" doc:"仅在来源有歧义时显式命名来源连接"`
}

// queryArtifactRef is the long-body spill locator inside the sealed result.
type queryArtifactRef struct {
	ID         string `json:"id"`
	MediaType  string `json:"mediaType"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"sizeBytes"`
	TotalLines int64  `json:"totalLines"`
}

// queryResult is the canonical thanos_query_result_v1 payload. Structured
// failures travel inside the same shape (success=false).
type queryResult struct {
	Success     bool              `json:"success"`
	StartedAt   string            `json:"startedAt"`
	FinishedAt  string            `json:"finishedAt"`
	ErrorCode   string            `json:"errorCode,omitempty"`
	ErrorDetail string            `json:"errorDetail,omitempty"`
	Status      string            `json:"status,omitempty"`
	ResultType  string            `json:"resultType,omitempty"`
	SampleCount int               `json:"sampleCount,omitempty"`
	Truncated   bool              `json:"truncated"`
	TotalBytes  int64             `json:"totalBytes"`
	TotalLines  int64             `json:"totalLines"`
	Output      string            `json:"output"`
	Artifact    *queryArtifactRef `json:"artifact,omitempty"`
}

// queryTool is the shared compiled PromQL query tool: the complete frozen
// contract (version, execution location, result schema, model-facing
// description) owned by the plugin, not by any core table.
var queryTool = plugins.Tool[queryArgs, queryResult]{
	Name: "thanos_query", Version: "4", FailureMode: plugins.FailureReturnToModel, ResultKind: "thanos_query_result_v1",
	ProducesEvidence: true, RequiresConnectionGrant: true, Timeout: queryTimeout, RateLimitPerMinute: 60,
	Description: "执行只读 PromQL 即时查询。必须提供 query；sourceRef 可选，仅在来源有歧义时显式命名来源连接，Quoin 按冻结授权解析连接、范围与必需 labels，结果作为不可变 Evidence 封存。",
	Handler:     runQueryTool,
}

func runQueryTool(t *plugins.ToolContext, args queryArgs) (queryResult, error) {
	startedAt := time.Now().UTC()
	fail := func(code, detail string) (queryResult, error) {
		return queryResult{
			Success: false, StartedAt: startedAt.Format(time.RFC3339Nano),
			FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
			ErrorCode:  code, ErrorDetail: detail,
		}, nil
	}
	if strings.TrimSpace(args.Query) == "" {
		return fail("invalid_arguments", "query 必须是非空字符串")
	}
	body, callErr := platformGet(t, "/api/v1/query", url.Values{"query": {args.Query}}, queryTimeout)
	if callErr != nil {
		return fail(gatewayFailureCode(callErr), "查询请求失败: "+callErr.Error())
	}
	if body.StatusCode != 200 {
		return fail("thanos_http_error", fmt.Sprintf("查询端点返回 HTTP %d", body.StatusCode)+suffixDetail(boundedHead(body.Body, 4096)))
	}
	status, resultType, sampleCount, parseErr := summarizeResponse(body.Body)
	if parseErr != nil {
		return fail("thanos_invalid_response", "响应不是合法查询结果: "+parseErr.Error())
	}
	if status != "success" {
		return fail("thanos_query_error", "查询失败"+suffixDetail(boundedHead(body.Body, 4096)))
	}
	totalBytes := int64(len(body.Body))
	totalLines := int64(bytes.Count(body.Body, []byte("\n")))
	if totalBytes > 0 && body.Body[totalBytes-1] != '\n' {
		totalLines++
	}
	spilled := totalBytes > spillBytes || totalLines > spillLines
	var artifact *queryArtifactRef
	if spilled && t.Spill != nil {
		artifactID, err := t.Spill(t.Context, body.Body, "application/json")
		if err != nil {
			// Upload failure fails the tool call; a bare preview is never
			// reported as the complete result.
			return fail("artifact_commit_failed", "长输出 Artifact 提交失败: "+err.Error())
		}
		artifact = &queryArtifactRef{
			ID: strconv.FormatInt(artifactID, 10), MediaType: "application/json",
			SHA256: sha256Hex(body.Body), SizeBytes: totalBytes, TotalLines: totalLines,
		}
	}
	output := boundedHead(body.Body, spillBytes)
	if spilled {
		output = "…（完整输出已存入 Artifact）\n" + boundedHead(body.Body, previewBytes)
	}
	return queryResult{
		Success: true, Status: status, ResultType: resultType, SampleCount: sampleCount,
		StartedAt: startedAt.Format(time.RFC3339Nano), FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Truncated: spilled, TotalBytes: totalBytes, TotalLines: totalLines, Output: output,
		Artifact: artifact,
	}, nil
}

// gatewayFailureCode maps gateway-level failures onto the frozen tool error
// vocabulary (fail closed: credential problems are grant_missing, quotas are
// gateway_rate_limited).
func gatewayFailureCode(err error) string {
	switch {
	case errors.Is(err, plugins.ErrCredentialUnavailable):
		return "grant_missing"
	case errors.Is(err, plugins.ErrPlatformRateLimited):
		return "gateway_rate_limited"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, plugins.ErrPlatformUnreachable):
		return "thanos_unavailable"
	default:
		return "thanos_unavailable"
	}
}

// platformGet runs one GET against the resolved connection through the
// gateway caller, classifying transport failures onto the sentinel classes.
func platformGet(t *plugins.ToolContext, path string, query url.Values, timeout time.Duration) (*plugins.PlatformResponse, error) {
	ctx := t.Context
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	response, err := t.Platform.Call(ctx, plugins.PlatformRequest{
		Method: "GET", Path: path, Query: query, Timeout: timeout,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("查询超时: %w", context.DeadlineExceeded)
		}
		return nil, err
	}
	return response, nil
}

// summarizeResponse extracts status/resultType/sampleCount without
// materializing the result array (a huge matrix is walked token-wise).
func summarizeResponse(body []byte) (status, resultType string, sampleCount int, err error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", "", 0, errors.New("顶层不是 JSON 对象")
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return "", "", 0, err
		}
		name, _ := key.(string)
		switch name {
		case "status":
			if err := decoder.Decode(&status); err != nil {
				return "", "", 0, err
			}
		case "data":
			dataToken, err := decoder.Token()
			if err != nil || dataToken != json.Delim('{') {
				return "", "", 0, errors.New("data 不是 JSON 对象")
			}
			for decoder.More() {
				dataKey, err := decoder.Token()
				if err != nil {
					return "", "", 0, err
				}
				dataName, _ := dataKey.(string)
				switch dataName {
				case "resultType":
					if err := decoder.Decode(&resultType); err != nil {
						return "", "", 0, err
					}
				case "result":
					resultToken, err := decoder.Token()
					if err != nil || resultToken != json.Delim('[') {
						return "", "", 0, errors.New("result 不是 JSON 数组")
					}
					sampleCount, err = countArrayElements(decoder)
					if err != nil {
						return "", "", 0, err
					}
				default:
					if err := skipValue(decoder); err != nil {
						return "", "", 0, err
					}
				}
			}
			if _, err := decoder.Token(); err != nil { // close of data object
				return "", "", 0, err
			}
		default:
			if err := skipValue(decoder); err != nil {
				return "", "", 0, err
			}
		}
	}
	return status, resultType, sampleCount, nil
}

func countArrayElements(decoder *json.Decoder) (int, error) {
	count := 0
	for decoder.More() {
		if err := skipValue(decoder); err != nil {
			return count, err
		}
		count++
	}
	if _, err := decoder.Token(); err != nil { // closing ']'
		return count, err
	}
	return count, nil
}

func skipValue(decoder *json.Decoder) error {
	depth := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
			if depth == 0 {
				return nil
			}
		default:
			if depth == 0 {
				return nil
			}
		}
	}
}

func boundedHead(body []byte, limit int) string {
	if int64(len(body)) > int64(limit) {
		return string(body[:limit]) + "…"
	}
	return string(body)
}

func suffixDetail(detail string) string {
	if strings.TrimSpace(detail) == "" {
		return ""
	}
	return "：" + strings.TrimSpace(detail)
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, len(sum)*2)
	for _, value := range sum {
		out = append(out, hexDigits[value>>4], hexDigits[value&0x0f])
	}
	return string(out)
}

// ---------------------------------------------------------------------------
// metrics_probe — internal connection probe (vector(1) contract)
// ---------------------------------------------------------------------------

// metricsProbeResult is the canonical probe observation.
type metricsProbeResult struct {
	Reachable    bool   `json:"reachable"`
	LatencyMS    int64  `json:"latencyMs"`
	Kind         string `json:"kind"`
	Query        string `json:"query"`
	ResponseType string `json:"responseType,omitempty"`
	SampleCount  int    `json:"sampleCount,omitempty"`
	SampleValue  string `json:"sampleValue,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

// probeTool executes the frozen thanos-query-v1 action set: vector(1) must
// return exactly one sample of value "1".
var probeTool = plugins.Tool[struct{}, metricsProbeResult]{
	Name: "metrics_probe", Version: "1", FailureMode: plugins.FailureReturnToModel, ResultKind: "metrics_probe_result_v1",
	Internal: true, Timeout: 15 * time.Second,
	Description: "内部工具：对指标连接执行 vector(1) 探测，校验查询端点连通与响应契约。",
	Handler: func(t *plugins.ToolContext, _ struct{}) (metricsProbeResult, error) {
		started := time.Now().UTC()
		result := metricsProbeResult{Kind: t.Conn.Type, Query: "vector(1)"}
		body, err := platformGet(t, "/api/v1/query", url.Values{"query": {"vector(1)"}}, 15*time.Second)
		result.LatencyMS = time.Since(started).Milliseconds()
		if err != nil {
			result.Detail = "查询请求失败: " + err.Error()
			return result, nil
		}
		if body.StatusCode != 200 {
			result.Detail = fmt.Sprintf("查询端点返回 HTTP %d", body.StatusCode)
			return result, nil
		}
		var parsed struct {
			Status string `json:"status"`
			Data   struct {
				ResultType string `json:"resultType"`
				Result     []struct {
					Value [2]any `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body.Body, &parsed); err != nil {
			result.Detail = "响应不是合法 JSON: " + err.Error()
			return result, nil
		}
		if parsed.Status != "success" {
			result.Detail = "查询状态为 " + parsed.Status
			return result, nil
		}
		result.Reachable = true
		result.ResponseType = parsed.Data.ResultType
		result.SampleCount = len(parsed.Data.Result)
		if len(parsed.Data.Result) == 1 {
			if sample, ok := parsed.Data.Result[0].Value[1].(string); ok {
				result.SampleValue = sample
			}
		}
		if parsed.Data.ResultType != "vector" {
			result.Detail = fmt.Sprintf("resultType 是 %s，期望 vector", parsed.Data.ResultType)
		} else if len(parsed.Data.Result) != 1 {
			result.Detail = fmt.Sprintf("样本数是 %d，期望 1", len(parsed.Data.Result))
		} else if result.SampleValue != "1" {
			result.Detail = fmt.Sprintf("样本值是 %s，期望 1", result.SampleValue)
		}
		return result, nil
	},
}

// ---------------------------------------------------------------------------
// metrics_discover — internal bounded discovery pass
// ---------------------------------------------------------------------------

type discoverArgs struct {
	ObjectType string `json:"objectType"`
	Limit      int    `json:"limit,omitempty"`
}

// metricsDiscoverObjects is the shared bounded-discovery declaration.
func metricsDiscoverObjects() []plugins.DiscoverObject {
	return []plugins.DiscoverObject{
		{ObjectType: "target", IdentityLabels: []string{"job", "instance"}, Query: "up", Limit: 500},
	}
}

var discoverTool = plugins.Tool[discoverArgs, plugins.DiscoverResult]{
	Name: "metrics_discover", Version: "1", FailureMode: plugins.FailureFailAttempt, ResultKind: "metrics_discover_result_v1",
	Internal: true, Timeout: queryTimeout,
	Description: "内部工具：执行声明式有界发现（up 序列），产出目标对象与完整性标记。",
	Handler: func(t *plugins.ToolContext, args discoverArgs) (plugins.DiscoverResult, error) {
		declaredObjects := metricsDiscoverObjects()
		var declared *plugins.DiscoverObject
		for index := range declaredObjects {
			if declaredObjects[index].ObjectType == args.ObjectType {
				declared = &declaredObjects[index]
				break
			}
		}
		if declared == nil {
			return plugins.DiscoverResult{}, fmt.Errorf("unknown discovery object %q", args.ObjectType)
		}
		limit := declared.Limit
		if args.Limit > 0 && args.Limit < limit {
			limit = args.Limit
		}
		body, err := platformGet(t, "/api/v1/query", url.Values{"query": {declared.Query}}, queryTimeout)
		if err != nil {
			return plugins.DiscoverResult{}, err
		}
		if body.StatusCode != 200 {
			return plugins.DiscoverResult{}, fmt.Errorf("查询端点返回 HTTP %d", body.StatusCode)
		}
		var envelope struct {
			Status   string   `json:"status"`
			Warnings []string `json:"warnings"`
			Data     struct {
				Result []struct {
					Metric map[string]string `json:"metric"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body.Body, &envelope); err != nil {
			return plugins.DiscoverResult{}, fmt.Errorf("invalid Prometheus response: %w", err)
		}
		if envelope.Status != "success" {
			return plugins.DiscoverResult{}, fmt.Errorf("查询状态为 %s", envelope.Status)
		}
		warnings := envelope.Warnings
		objects := make([]plugins.DiscoveredObject, 0, len(envelope.Data.Result))
		for i, item := range envelope.Data.Result {
			if len(objects) == limit {
				// The budget is a hard fact of the pass: truncation is
				// reported as incompleteness so the control plane never
				// projects a whole scope from a truncated response.
				warnings = append(warnings, fmt.Sprintf("discovery truncated at %d objects", limit))
				break
			}
			identity := make(map[string]string, len(declared.IdentityLabels))
			for _, label := range declared.IdentityLabels {
				value, present := item.Metric[label]
				if !present || value == "" {
					return plugins.DiscoverResult{}, fmt.Errorf("series %d lacks identity label %q", i, label)
				}
				identity[label] = value
			}
			objects = append(objects, plugins.DiscoveredObject{
				ObjectType:        declared.ObjectType,
				CanonicalIdentity: canonicalTargetIdentity(identity),
				DisplayName:       item.Metric["job"] + "/" + item.Metric["instance"],
			})
		}
		return plugins.DiscoverResult{Objects: objects, Incomplete: len(warnings) != 0}, nil
	},
}

// canonicalTargetIdentity is the source identity string for one target. The
// control plane's identity_key encoding stays the equality authority; this
// canonical identity carries the same label facts.
func canonicalTargetIdentity(identity map[string]string) string {
	parts := make([]string, 0, len(identity))
	for _, name := range []string{"job", "instance"} {
		if value, ok := identity[name]; ok {
			parts = append(parts, name+"="+value)
		}
	}
	return strings.Join(parts, ",")
}

// ---------------------------------------------------------------------------
// metrics_collect — internal deterministic collection
// ---------------------------------------------------------------------------

type collectArgs struct {
	TemplateID      string                  `json:"templateId"`
	TemplateVersion string                  `json:"templateVersion"`
	Params          json.RawMessage         `json:"params"`
	EvidenceAt      string                  `json:"evidenceAt,omitempty"`
	ScopeKind       string                  `json:"scopeKind"`
	Targets         []plugins.CollectTarget `json:"targets"`
}

var collectTool = plugins.Tool[collectArgs, plugins.CollectResult]{
	Name: "metrics_collect", Version: "1", FailureMode: plugins.FailureFailAttempt, ResultKind: "metrics_collect_result_v1",
	Internal: true, Timeout: queryTimeout,
	// 显式 Schema：targets 是冻结的 CollectTarget 结构数组（struct 切片在
	// 派生词表之外），内部工具不进模型目录，泛型 array 语义足够。
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"templateId":      map[string]any{"type": "string"},
			"templateVersion": map[string]any{"type": "string"},
			"params":          map[string]any{"type": "object"},
			"evidenceAt":      map[string]any{"type": "string"},
			"scopeKind":       map[string]any{"type": "string"},
			"targets":         map[string]any{"type": "array"},
		},
		"required": []any{"templateId", "templateVersion", "params", "scopeKind", "targets"},
	},
	Description: "内部工具：执行冻结模板的确定性 PromQL 采集（含范围强制），产出逐检查证据与完整性标记。",
	Handler: func(t *plugins.ToolContext, args collectArgs) (plugins.CollectResult, error) {
		if args.TemplateVersion == "" {
			return plugins.CollectResult{}, fmt.Errorf("collector requires a frozen template version")
		}
		mode, expression, rangeSeconds, stepSeconds, shapeErr := pluginTemplateQuery(args.TemplateID, mapAnyFromRaw(args.Params))
		if shapeErr != nil {
			return plugins.CollectResult{}, shapeErr
		}
		// 范围强制在查询执行之前：businessView/objects 的冻结条件被注入表达
		// 式的每个向量选择器（含聚合/函数/子查询内部），保证聚合与速率窗口
		// 读取的原始序列本身已被收窄——结果后过滤做不到这一点。
		conditions, scopeErr := scopeLabelConditions(args.ScopeKind, args.Targets)
		if scopeErr != nil {
			return plugins.CollectResult{}, scopeErr
		}
		if len(conditions) > 0 {
			expression, scopeErr = scopeEnforcedExpression(expression, conditions)
			if scopeErr != nil {
				return plugins.CollectResult{}, scopeErr
			}
		}
		values := url.Values{"query": {expression}}
		path := "/api/v1/query"
		if mode == "range" {
			path = "/api/v1/query_range"
			now := time.Now().UTC()
			values.Set("start", strconv.FormatFloat(float64(now.Add(-time.Duration(*rangeSeconds)*time.Second).UnixNano())/1e9, 'f', -1, 64))
			values.Set("end", strconv.FormatFloat(float64(now.UnixNano())/1e9, 'f', -1, 64))
			values.Set("step", strconv.FormatInt(*stepSeconds, 10))
		}
		body, err := platformGet(t, path, values, queryTimeout)
		if err != nil {
			return plugins.CollectResult{}, err
		}
		if body.StatusCode != 200 {
			return plugins.CollectResult{}, fmt.Errorf("查询端点返回 HTTP %d", body.StatusCode)
		}
		var envelope struct {
			Status   string          `json:"status"`
			Warnings []string        `json:"warnings"`
			Error    string          `json:"error"`
			Data     json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body.Body, &envelope); err != nil {
			return plugins.CollectResult{}, fmt.Errorf("PromQL response is not valid JSON: %w", err)
		}
		if envelope.Status != "success" {
			return plugins.CollectResult{}, fmt.Errorf("查询状态 %q: %s", envelope.Status, envelope.Error)
		}
		if len(envelope.Data) == 0 {
			return plugins.CollectResult{}, fmt.Errorf("PromQL response carried no data projection")
		}
		checkID := args.TemplateID
		if len(args.Targets) != 0 && args.Targets[0].CanonicalIdentity != "" {
			checkID = args.Targets[0].CanonicalIdentity
		}
		return plugins.CollectResult{
			Checks: []plugins.CheckObservation{{
				CheckID: checkID, Succeeded: true,
				EvidenceJSON: []byte(fmt.Sprintf(`{"result":%s}`, envelope.Data)),
			}},
			// 上游 warnings（截断/局部响应）标记整趟不完整，绝不伪装完整结果。
			Incomplete: len(envelope.Warnings) != 0,
		}, nil
	},
}

func mapAnyFromRaw(raw json.RawMessage) map[string]any {
	decoded := map[string]any{}
	_ = json.Unmarshal(raw, &decoded)
	return decoded
}

// pluginTemplateQuery 把冻结模板/参数解析为类型化查询；未知模板或参数形状
// 不匹配是确定性错误。
func pluginTemplateQuery(templateID string, params map[string]any) (mode, expression string, rangeSeconds, stepSeconds *int64, err error) {
	switch templateID {
	case "promql_instant":
		raw, ok := params["expression"].(string)
		if !ok || raw == "" || len(params) != 1 {
			return "", "", nil, nil, fmt.Errorf("promql_instant requires exactly one non-empty expression")
		}
		return "instant", raw, nil, nil, nil
	case "promql_range":
		raw, ok := params["expression"].(string)
		if !ok || raw == "" {
			return "", "", nil, nil, fmt.Errorf("promql_range requires an expression")
		}
		r, rOK := params["rangeSeconds"].(float64)
		s, sOK := params["stepSeconds"].(float64)
		if !rOK || !sOK || r < 1 || s < 1 || r != float64(int64(r)) || s != float64(int64(s)) || len(params) != 3 {
			return "", "", nil, nil, fmt.Errorf("promql_range requires positive integer rangeSeconds/stepSeconds and an expression")
		}
		rI, sI := int64(r), int64(s)
		return "range", raw, &rI, &sI, nil
	}
	return "", "", nil, nil, fmt.Errorf("unknown inspection template %q", templateID)
}

// promQLScopeParse 用与控制面声明校验相同的锁定上游解析器选项（关闭全部
// 实验特性）解析表达式；上游 Parser 实例持有 lexer/错误缓冲等可变状态、
// 不并发安全，因此每次解析新建实例，绝不共享。
func promQLScopeParse(expression string) (parser.Expr, error) {
	return parser.NewParser(parser.Options{}).ParseExpr(expression)
}

// scopeLabelConditions 从冻结参数解析本次采集必须注入的精确 label 条件。
// conditions 唯一来源是目标上的冻结 map（businessView：视图条件；objects：
// 来源行身份事实）；绝不从 CanonicalIdentity 猜测解析。每个采集子 Attempt
// 恰好一个目标：多目标意味着条件来源不唯一，直接拒绝而不合并。
func scopeLabelConditions(scopeKind string, targets []plugins.CollectTarget) (map[string]string, error) {
	switch plugins.CollectScopeKind(scopeKind) {
	case plugins.ScopeIntegration:
		return nil, nil
	case plugins.ScopeBusinessView, plugins.ScopeObjects:
		if len(targets) != 1 {
			return nil, fmt.Errorf("scope %q requires exactly one frozen target, got %d", scopeKind, len(targets))
		}
		conditions := targets[0].LabelConditions
		if err := requireLabelConditions(scopeKind, conditions); err != nil {
			return nil, err
		}
		return conditions, nil
	default:
		return nil, fmt.Errorf("collection request carries unsupported scope kind %q", scopeKind)
	}
}

// requireLabelConditions 拒绝空条件、空键/空值与非法 label 名：精确条件是
// 唯一收窄事实，任何缺失或不可能匹配的条件都意味着无法证明采集被收窄。
func requireLabelConditions(scopeKind string, conditions map[string]string) error {
	if len(conditions) == 0 {
		return fmt.Errorf("scope %q requires non-empty frozen label conditions", scopeKind)
	}
	for name, value := range conditions {
		if name == "" || value == "" {
			return fmt.Errorf("scope %q carries an empty label condition", scopeKind)
		}
		// 锁定经典 label 名词表（model.LabelNameRE）：UTF-8 scheme 对条件名
		// 几乎全放行，收窄事实必须落在可静态复核的封闭词表内。
		if !model.LabelNameRE.MatchString(name) {
			return fmt.Errorf("scope %q carries invalid label name %q", scopeKind, name)
		}
	}
	return nil
}

// scopeEnforcedExpression 用官方 PromQL AST 把冻结的精确条件注入表达式中
// 每一个向量选择器，再整体重渲染并重解析闭环验证。选择器已带的条件按集合
// 交语义保留（矛盾即空结果，绝不放宽）；渲染/验证失败与解析失败一样是
// 确定性拒绝。零选择器表达式（如 vector(1)）没有任何来源序列读路径，无
// 需注入。
func scopeEnforcedExpression(expression string, conditions map[string]string) (string, error) {
	names := make([]string, 0, len(conditions))
	for name, value := range conditions {
		if name == "" || value == "" {
			return "", fmt.Errorf("scope condition %q is empty", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	required := make([]*labels.Matcher, 0, len(names))
	for _, name := range names {
		matcher, err := labels.NewMatcher(labels.MatchEqual, name, conditions[name])
		if err != nil {
			return "", err
		}
		required = append(required, matcher)
	}
	expr, err := promQLScopeParse(expression)
	if err != nil {
		return "", fmt.Errorf("scope enforcement rejected unparsable expression: %w", err)
	}
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		if vector, ok := node.(*parser.VectorSelector); ok {
			vector.LabelMatchers = appendExactMatchers(vector.LabelMatchers, required)
		}
		return nil
	})
	rendered := expr.String()
	if err := verifyScopeEnforced(rendered, required); err != nil {
		return "", err
	}
	return rendered, nil
}

// appendExactMatchers 追加缺失的精确 matcher；同 label 同值的既有精确
// matcher 不重复追加。异值矛盾按 PromQL 交集语义自然收窄为空集，绝不删除
// 或改写表达式自带条件。
func appendExactMatchers(existing, required []*labels.Matcher) []*labels.Matcher {
	out := existing
	for _, matcher := range required {
		duplicate := false
		for _, have := range existing {
			if have.Type == labels.MatchEqual && have.Name == matcher.Name && have.Value == matcher.Value {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, matcher)
		}
	}
	return out
}

// verifyScopeEnforced 重解析渲染结果并断言每个向量选择器都携带全部精确
// 条件——渲染闭环是收窄承诺的最终防线，任何选择器缺条件都拒绝本趟采集。
func verifyScopeEnforced(rendered string, required []*labels.Matcher) error {
	expr, err := promQLScopeParse(rendered)
	if err != nil {
		return fmt.Errorf("scope enforcement produced an unparsable expression: %w", err)
	}
	var missing error
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		vector, ok := node.(*parser.VectorSelector)
		if !ok || missing != nil {
			return nil
		}
		for _, matcher := range required {
			present := false
			for _, have := range vector.LabelMatchers {
				if have.Type == labels.MatchEqual && have.Name == matcher.Name && have.Value == matcher.Value {
					present = true
					break
				}
			}
			if !present {
				missing = fmt.Errorf("rendered selector %q lost scope condition %s=%q", vector.String(), matcher.Name, matcher.Value)
				return nil
			}
		}
		return nil
	})
	return missing
}

// ---------------------------------------------------------------------------
// Plugin registration (blank-import assembly)
// ---------------------------------------------------------------------------

// metricsTools is the shared tool set both metrics plugins contribute: the
// model-visible PromQL query tool plus the internal probe/discover/collect
// tools Quoin's schedulers invoke. Identical manifests deduplicate onto one
// registry entry with both plugins as provenance.
func metricsTools() []plugins.ToolEntry {
	return []plugins.ToolEntry{
		queryTool.Entry(""),
		probeTool.Entry(""),
		discoverTool.Entry(""),
		collectTool.Entry(""),
	}
}

// metricsToolProvider stamps the owning plugin onto the shared entries.
type metricsToolProvider struct{ id string }

func (p metricsToolProvider) Tools() []plugins.ToolEntry {
	entries := metricsTools()
	for i := range entries {
		entries[i].Owner = p.id
	}
	return entries
}

// promQLInspectionTemplates is the shared deterministic PromQL collection
// catalog; the (pluginID, templateID, version) triple is the identity the
// inspection plans bind.
func promQLInspectionTemplates() []plugins.InspectionTemplate {
	return []plugins.InspectionTemplate{
		{ID: "promql_instant", Version: "1", Title: "PromQL 即时查询", Description: "以 evidence_at 为观测点执行一次即时向量查询"},
		{ID: "promql_range", Version: "1", Title: "PromQL 范围查询", Description: "以 evidence_at 为终点执行一次范围查询并保存实际窗口"},
	}
}

func init() {
	plugins.Register(plugins.Plugin{
		ID:          plugins.PrometheusID,
		Version:     "1",
		DisplayName: "Prometheus",
		Description: "连接 Prometheus 实例：作为指标查询的来源接入，提供与 Thanos 同契约的只读 PromQL 模型工具；授权按实际来源连接解析。",
		// 有界观测声明与真实注册的内部发现/采集工具同源落地；执行经
		// Quoin 调度、Stele 网关（ADR-0011）。
		DiscoverObjects:     metricsDiscoverObjects(),
		InspectionTemplates: promQLInspectionTemplates(),
		ConfigSchema:        metricsConfigSchema("prometheus"),
		DefaultEnabled:      true,
		ConnectionKind:      "prometheus",
		Tools:               metricsToolProvider{id: plugins.PrometheusID},
	})
	plugins.Register(plugins.Plugin{
		ID:                  plugins.ThanosID,
		Version:             "1",
		DisplayName:         "Thanos",
		Description:         "连接 Prometheus 兼容的全局查询层：提供资源范围内只读 PromQL 模型工具与来源接入。",
		DiscoverObjects:     metricsDiscoverObjects(),
		InspectionTemplates: promQLInspectionTemplates(),
		ConfigSchema:        metricsConfigSchema("thanos"),
		DefaultEnabled:      true,
		ConnectionKind:      "thanos",
		Tools:               metricsToolProvider{id: plugins.ThanosID},
	})
}
