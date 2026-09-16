package supervisor

// 独立计划插件采集（ADR-0004）：supervisor 消费冻结的
// inspection_plugin_execution_v1 输入（插件/模板/参数与本次接入授权），
// 按模板确定性执行类型化采集，提出 inspection_plugin_result_v1。执行只依据
// 冻结输入，不做运行时 descriptor 查询；模板实现与 Quoin 侧模板目录一一对应。

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	"github.com/Suknna/quoin/internal/plinth/runtime"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"google.golang.org/grpc/metadata"
)

const (
	pluginExecutionSchemaKind = "inspection_plugin_execution_v1"
	pluginResultSchemaKind    = "inspection_plugin_result_v1"
)

// inspectionPluginTarget 是冻结派发输入中的单个目标定位。LabelConditions 是
// 控制面在 Run 冻结时抽取的精确 label=value 事实（businessView：视图条件；
// objects：从观测来源行抽取的身份 label 事实）；执行侧绝不解析
// CanonicalIdentity 自行推导条件，条件缺失即 fail closed。
type inspectionPluginTarget struct {
	ObjectType      string            `json:"objectType"`
	IdentityKey     string            `json:"identityKey"`
	LabelConditions map[string]string `json:"labelConditions,omitempty"`
}

type inspectionPluginInput struct {
	SchemaKind      string                  `json:"schemaKind"`
	AttemptID       int64                   `json:"attemptId"`
	InspectionRunID int64                   `json:"inspectionRunId"`
	CheckKey        string                  `json:"checkKey"`
	PluginID        string                  `json:"pluginId"`
	TemplateID      string                  `json:"templateId"`
	TemplateVersion string                  `json:"templateVersion"`
	Params          map[string]any          `json:"params"`
	EvidenceAt      string                  `json:"evidenceAt"`
	GrantID         int64                   `json:"grantId"`
	Target          *inspectionPluginTarget `json:"target,omitempty"`
	// ScopeKind 是控制面冻结的显式范围词表（integration/businessView/objects）。
	// 空值与未知值一律拒绝：旧 businessView 快照没有条件，绝不能因缺字段而
	// 被当作无范围采集执行。
	ScopeKind string `json:"scopeKind"`
}

// runInspectionPluginCollection executes one frozen plugin collection check.
// Supported templates this build: promql_instant / promql_range（与 Quoin 侧
// inspection 模板目录一一对应；执行通过既有 Thanos 类型化凭据边界）。
func (supervisor *Supervisor) runInspectionPluginCollection(parent context.Context, sink *runtime.FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, binding runtime.DispatchBinding, stopTask func(int64) bool) {
	attemptID := dispatch.GetAttemptId()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	supervisor.Channel.RegisterTask(attemptID, cancel)
	defer stopTask(attemptID)
	if err := sink.Send(&runtimev1.ControlEnvelope{CorrelationId: uint64(attemptID), Msg: &runtimev1.ControlEnvelope_AttemptAccept{AttemptAccept: &runtimev1.AttemptAccept{AttemptId: attemptID}}}); err != nil {
		return
	}
	var input inspectionPluginInput
	if dispatch.GetInput() == nil || json.Unmarshal(dispatch.GetInput().GetCanonicalJson(), &input) != nil || input.SchemaKind != pluginExecutionSchemaKind || input.AttemptID != attemptID || input.InspectionRunID != dispatch.GetScopeId() || input.CheckKey == "" || input.PluginID == "" || input.TemplateID == "" {
		supervisor.proposeInspectionPlugin(sink, attemptID, binding, input, "error", nil, nil, "query_failed")
		return
	}
	// 范围词表与条件完整性在取凭据之前 fail closed：显式 integration 才允许
	// 无条件执行；businessView/objects 必须冻结非空精确条件；空值与未知值
	// 一律拒绝，绝不把缺字段的旧快照降级成无范围全源采集。
	if scopeErr := validateInspectionScope(input); scopeErr != nil {
		supervisor.proposeInspectionPlugin(sink, attemptID, binding, input, "error", nil, []string{scopeErr.Error()}, "query_failed")
		return
	}
	// 模板权威来自共享注册表描述：冻结的 (plugin, template, version) 必须
	// 仍然是已声明目录的一员；参数形状校验紧随其后。
	if !supervisor.templateDeclared(input.PluginID, input.TemplateID, input.TemplateVersion) {
		supervisor.proposeInspectionPlugin(sink, attemptID, binding, input, "error", nil, []string{"template is not declared by the plugin"}, "query_failed")
		return
	}
	grant, ok := supervisor.primaryGrant(dispatch.GetInput(), "config_thanos_query")
	if !ok || grant.GetGrantId() != input.GrantID {
		supervisor.proposeInspectionPlugin(sink, attemptID, binding, input, "error", nil, []string{"missing config metrics grant"}, "query_failed")
		return
	}
	bearer, err := supervisor.Channel.BearerToken()
	if err != nil {
		supervisor.proposeInspectionPlugin(sink, attemptID, binding, input, "error", nil, []string{"runtime credential unavailable"}, "query_failed")
		return
	}
	grantCtx, grantCancel := context.WithTimeout(ctx, 15*time.Second)
	payload, err := client.FetchCredentialGrant(metadata.NewOutgoingContext(grantCtx, metadata.Pairs("authorization", "Bearer "+bearer)), &runtimev1.FetchCredentialGrantRequest{GrantId: grant.GetGrantId(), AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch})
	grantCancel()
	if err != nil || payload.GetThanos() == nil {
		supervisor.proposeInspectionPlugin(sink, attemptID, binding, input, "error", nil, []string{"credential grant unavailable"}, "query_failed")
		return
	}
	// 采集经注册表 Collector 绑定执行：参数/窗口/目标全部来自冻结输入，
	// 适配器通过合同秘密边界取用凭据，绝不接触传输信封。
	bundle, bound := supervisor.pluginRegistry().Bundle(input.PluginID)
	if !bound || bundle.Collector == nil {
		supervisor.proposeInspectionPlugin(sink, attemptID, binding, input, "error", nil, []string{fmt.Sprintf("plugin %q has no bound collector in this process", input.PluginID)}, "plugin_unavailable")
		return
	}
	paramsJSON, marshalErr := json.Marshal(input.Params)
	if marshalErr != nil {
		supervisor.proposeInspectionPlugin(sink, attemptID, binding, input, "error", nil, []string{"frozen params are not canonical JSON"}, "query_failed")
		return
	}
	call, callErr := newMetricsCall(supervisor.pluginRegistry(), input.PluginID, json.RawMessage(payload.GetRevisionConfigJson()),
		payload.GetThanos().GetUsername(), payload.GetThanos().GetPassword(), payload.GetThanos().GetBearerToken())
	if callErr != nil {
		supervisor.proposeInspectionPlugin(sink, attemptID, binding, input, "error", nil, []string{callErr.Error()}, "query_failed")
		return
	}
	targets := []plugins.CollectTarget{}
	if input.Target != nil {
		targets = append(targets, plugins.CollectTarget{
			ObjectType:        input.Target.ObjectType,
			CanonicalIdentity: input.Target.IdentityKey,
			LabelConditions:   input.Target.LabelConditions,
		})
	}
	collectCtx, collectCancel := context.WithTimeout(ctx, 60*time.Second)
	result, collectErr := bundle.Collector.Collect(collectCtx, call, plugins.CollectRequest{
		TemplateID: input.TemplateID, TemplateVersion: input.TemplateVersion,
		Params: paramsJSON, EvidenceAt: input.EvidenceAt,
		Scope: plugins.CollectScope{Kind: plugins.CollectScopeKind(input.ScopeKind)}, Targets: targets,
	})
	collectCancel()
	if collectErr != nil {
		supervisor.proposeInspectionPlugin(sink, attemptID, binding, input, "error", nil, []string{collectErr.Error()}, "query_failed")
		return
	}
	if result.Incomplete || len(result.Checks) == 0 {
		// 截断/局部响应不得伪装完整结果。
		supervisor.proposeInspectionPlugin(sink, attemptID, binding, input, "gap", nil, []string{"collection pass incomplete"}, "partial_response")
		return
	}
	var evidence struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(result.Checks[0].EvidenceJSON, &evidence); err != nil || len(evidence.Result) == 0 {
		supervisor.proposeInspectionPlugin(sink, attemptID, binding, input, "error", nil, []string{"collector evidence carried no result projection"}, "query_failed")
		return
	}
	supervisor.proposeInspectionPlugin(sink, attemptID, binding, input, "success", evidence.Result, nil, "")
}

// validateInspectionScope 校验冻结输入的显式范围词表与条件完整性。只有
// integration 允许无条件执行（用户自写表达式的采集边界是接入本身）；
// businessView/objects 是真实收窄承诺，必须携带控制面冻结的非空精确
// label 条件。空值与未知值一律拒绝——缺字段绝不等价于更宽的授权。
func validateInspectionScope(input inspectionPluginInput) error {
	switch plugins.CollectScopeKind(input.ScopeKind) {
	case plugins.ScopeIntegration:
		return nil
	case plugins.ScopeBusinessView, plugins.ScopeObjects:
		if input.Target == nil {
			return fmt.Errorf("scope %q requires a frozen target with label conditions", input.ScopeKind)
		}
		return requireLabelConditions(input.ScopeKind, input.Target.LabelConditions)
	default:
		return fmt.Errorf("frozen inspection input carries unsupported scope kind %q", input.ScopeKind)
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

// templateDeclared 校验冻结模板身份是否仍在插件声明目录中。
func (supervisor *Supervisor) templateDeclared(pluginID, templateID, templateVersion string) bool {
	descriptor, ok := supervisor.pluginRegistry().Descriptor(pluginID)
	if !ok {
		return false
	}
	for _, template := range descriptor.InspectionTemplates {
		if template.ID == templateID && template.Version == templateVersion {
			return true
		}
	}
	return false
}

// metricsCollector is the PromQL-driving collector adapter bound for the
// Prometheus-compatible plugins. It executes the frozen template/params
// through the existing controlled metrics transport and seals the closed
// Prometheus projection as per-check evidence.
type metricsCollector struct{}

func (c *metricsCollector) Collect(ctx context.Context, call *plugins.Call, request plugins.CollectRequest) (*plugins.CollectResult, error) {
	if request.TemplateVersion == "" {
		return nil, fmt.Errorf("collector requires a frozen template version")
	}
	mode, expression, rangeSeconds, stepSeconds, shapeErr := pluginTemplateQuery(request.TemplateID, mapAnyFromRaw(request.Params))
	if shapeErr != nil {
		return nil, shapeErr
	}
	// 范围强制在查询执行之前：businessView/objects 的冻结条件被注入表达式
	// 的每个向量选择器（含聚合/函数/子查询内部），保证聚合与速率窗口读取的
	// 原始序列本身已被收窄——结果后过滤做不到这一点。integration 是显式的
	// 无条件采集；空/未知范围 kind 在这里同样是硬错误。
	conditions, scopeErr := scopeLabelConditions(request)
	if scopeErr != nil {
		return nil, scopeErr
	}
	if len(conditions) > 0 {
		expression, scopeErr = scopeEnforcedExpression(expression, conditions)
		if scopeErr != nil {
			return nil, scopeErr
		}
	}
	config, err := metricsSettings(call)
	if err != nil {
		return nil, err
	}
	secret, err := metricsSecret(ctx, call)
	if err != nil {
		return nil, err
	}
	raw, warnings, err := plinthconnections.RunPromQL(ctx, config, secret, mode, expression, rangeSeconds, stepSeconds)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if unmarshalErr := json.Unmarshal(raw, &envelope); unmarshalErr != nil || len(envelope.Data) == 0 {
		return nil, fmt.Errorf("PromQL response carried no data projection")
	}
	checkID := request.TemplateID
	if len(request.Targets) != 0 && request.Targets[0].CanonicalIdentity != "" {
		checkID = request.Targets[0].CanonicalIdentity
	}
	result := &plugins.CollectResult{
		Checks: []plugins.CheckObservation{{
			CheckID: checkID, Succeeded: true,
			EvidenceJSON: []byte(fmt.Sprintf(`{"result":%s}`, envelope.Data)),
		}},
	}
	// 上游 warnings（截断/局部响应）标记整趟不完整，绝不伪装完整结果。
	result.Incomplete = len(warnings) != 0
	return result, nil
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

// scopeLabelConditions 从冻结请求解析本次采集必须注入的精确 label 条件。
// conditions 唯一来源是目标上的冻结 map（businessView：视图条件；objects：
// 来源行身份事实）；绝不从 CanonicalIdentity 猜测解析。每个采集子 Attempt
// 恰好一个目标：多目标意味着条件来源不唯一（同 label 异值会互相覆盖而扩
// 大范围），直接拒绝而不合并。
func scopeLabelConditions(request plugins.CollectRequest) (map[string]string, error) {
	switch request.Scope.Kind {
	case plugins.ScopeIntegration:
		return nil, nil
	case plugins.ScopeBusinessView, plugins.ScopeObjects:
		if len(request.Targets) != 1 {
			return nil, fmt.Errorf("scope %q requires exactly one frozen target, got %d", request.Scope.Kind, len(request.Targets))
		}
		conditions := request.Targets[0].LabelConditions
		if err := requireLabelConditions(string(request.Scope.Kind), conditions); err != nil {
			return nil, err
		}
		return conditions, nil
	default:
		return nil, fmt.Errorf("collection request carries unsupported scope kind %q", request.Scope.Kind)
	}
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

func (supervisor *Supervisor) proposeInspectionPlugin(sink *runtime.FrameSink, attemptID int64, binding runtime.DispatchBinding, input inspectionPluginInput, outcome string, result json.RawMessage, messages []string, gapReason string) {
	var gap any
	if gapReason != "" {
		gap = gapReason
	}
	// 范围模板的窗口是冻结输入的派生事实（evidence_at + rangeSeconds/stepSeconds），
	// 与执行结果无关；失败/gap 同样携带，保证控制面可复核请求的查询窗口。
	var window any
	if input.TemplateID == "promql_range" && input.EvidenceAt != "" {
		if endAt, err := time.Parse(time.RFC3339Nano, input.EvidenceAt); err == nil {
			if r, rOK := input.Params["rangeSeconds"].(float64); rOK {
				if s, sOK := input.Params["stepSeconds"].(float64); sOK {
					window = map[string]any{
						"startAt":     endAt.Add(-time.Duration(int64(r)) * time.Second).UTC().Format(time.RFC3339Nano),
						"endAt":       endAt.UTC().Format(time.RFC3339Nano),
						"stepSeconds": int64(s),
					}
				}
			}
		}
	}
	if result == nil {
		result = json.RawMessage("null")
	}
	// 真实采集消息按结果类型分流：error 的诊断进 errors；gap 的观察事实（如
	// 截断/局部响应说明）进 warnings——控制面把它们冻结进检查结果元数据，分
	// 析清单据此保持缺口可见。绝不丢弃为空数组。
	warnings := []string{}
	errorMessages := []string{}
	if outcome == "error" {
		errorMessages = messages
	} else if len(messages) != 0 {
		warnings = messages
	}
	canonical, err := json.Marshal(map[string]any{
		"schemaKind": pluginResultSchemaKind, "attemptId": attemptID, "inspectionRunId": input.InspectionRunID,
		"checkKey": input.CheckKey, "outcome": outcome,
		"observedAt": time.Now().UTC().Format(time.RFC3339Nano), "executionWindow": window, "result": result,
		"warnings": warnings, "errors": errorMessages, "gapReason": gap,
	})
	if err != nil {
		return
	}
	digest := sha256.Sum256(canonical)
	_ = sink.Send(&runtimev1.ControlEnvelope{CorrelationId: uint64(attemptID), Msg: &runtimev1.ControlEnvelope_ResultProposal{ResultProposal: &runtimev1.ResultProposal{AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch, Outcome: outcomeFor(map[string]string{"success": "passed", "gap": "failed", "error": "failed"}[outcome]), Payload: &runtimev1.ResultPayload{SchemaKind: pluginResultSchemaKind, CanonicalJson: canonical, ContentDigest: digest[:]}}}})
}
