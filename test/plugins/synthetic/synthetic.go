// Package synthetic is a compiled test-only full-line plugin (issue #110
// acceptance): an alert source, a trusted HTTP connection kind, one
// grant-scoped observation tool, one inspection template and its internal
// collection tool. Its plugin ID is intentionally distinct from its wire
// kind, and it uses a stable upstream externalId rather than Alertmanager's
// label fingerprint. Hosts assemble it through their registry just like
// production plugins; no Quoin domain branch names this source, tool or
// check — every authorization and collection fact below is plugin
// DECLARATION consumed by the generic attempt/inspection machinery.
package synthetic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/plugins"
)

const Kind = "synthetic-hook"

// ConnectionKindValue is the trusted HTTP connection kind every instance of
// this plugin binds (ADR-0014 registry-declared class).
const ConnectionKindValue = "synthetic-hook"

// The frozen tool and collection identities (declaration-owned, no core
// table or switch knows them).
const (
	QueryToolName           = "synthetic_query"
	QueryToolVersion        = "1"
	QueryResultSchemaKind   = "synthetic_query_result_v1"
	QueryGrantPurpose       = "synthetic_query"
	SourceItemRole          = "synthetic_source"
	CollectToolName         = "synthetic_collect"
	TemplateID              = "synthetic_check"
	TemplateVersion         = "1"
	CollectionGrantPurpose  = "config_synthetic_check"
	CollectResultSchemaKind = "synthetic_collect_result_v1"
)

// SourceSettingsSchema is the closed instance-settings contract of the
// synthetic event source (ADR-0014 story 2): one optional ignoredAlertnames
// array. VerifyAndParse drops matching alerts before enqueueing, so two
// instances of the same kind with distinct settings normalize the same
// webhook differently — the per-instance independence proof.
var SourceSettingsSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"ignoredAlertnames": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string", "maxLength": 200},
			"maxItems":    50,
			"description": "按 alertname 丢弃的告警名单（非秘密过滤配置）",
		},
	},
}

// sourceSettings is the typed view of SourceSettingsSchema instances.
type sourceSettings struct {
	IgnoredAlertnames []string `json:"ignoredAlertnames"`
}

func Plugin() plugins.Plugin {
	return plugins.Plugin{
		ID: "synthetic-plugin", Version: "2", DisplayName: "Synthetic webhook",
		EventSource: source{}, EventTypes: []string{"alerts.batch"},
		EventSourceConfigSchema: SourceSettingsSchema,
		AlertNormalizer:         normalizer{}, AlertIdentity: plugins.AlertIdentityExternal,
		// Trusted HTTP connection kind (ADR-0014): bounded probe contract,
		// closed auth-mode subset; credential material never appears here.
		ConnectionKind:      ConnectionKindValue,
		ConnectionTransport: plugins.ConnectionTransportHTTP,
		ConnectionAuthModes: []string{plugins.AuthModeNone},
		ConnectionProbePath: "/healthz",
		DefaultEnabled:      true,
		Tools:               syntheticToolProvider{},
		InspectionTemplates: []plugins.InspectionTemplate{
			{
				ID: TemplateID, Version: TemplateVersion,
				Title: "合成检查", Description: "以 evidence_at 为观测点对合成来源执行一次只读回显查询",
				// The collection grant purpose is a template declaration: the
				// inspection plan-run freezes grants under it and the
				// credential fulfiller re-validates exactly the declared
				// purposes (ADR-0014).
				GrantPurpose:    CollectionGrantPurpose,
				CollectToolName: CollectToolName,
				ResultKind:      "json",
				ParamsSchema: map[string]any{
					"type": "object", "additionalProperties": false,
					"required":   []string{"expression"},
					"properties": map[string]any{"expression": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}},
				},
			},
		},
	}
}

type source struct{}

func (source) Kind() string { return Kind }

func (source) VerifyAndParse(_ context.Context, req plugins.InboundRequest) ([]plugins.Event, error) {
	batch, err := contract.ParseAlertmanagerWebhook(req.Body)
	if err != nil {
		return nil, err
	}
	// Per-instance settings (ADR-0014 story 2): the gateway pins exactly the
	// matched source's document after bearer authentication; absent or empty
	// settings mean no filtering. Invalid settings cannot occur — Quoin
	// validated the document against SourceSettingsSchema before it ever
	// reached the snapshot.
	var settings sourceSettings
	if len(req.Settings) > 0 {
		if err := json.Unmarshal(req.Settings, &settings); err != nil {
			return nil, fmt.Errorf("synthetic source settings are not valid JSON: %w", err)
		}
	}
	ignored := make(map[string]bool, len(settings.IgnoredAlertnames))
	for _, name := range settings.IgnoredAlertnames {
		ignored[name] = true
	}
	kept := batch.Alerts[:0]
	for _, alert := range batch.Alerts {
		if ignored[alert.Labels["alertname"]] {
			continue
		}
		kept = append(kept, alert)
	}
	batch.Alerts = kept
	if len(batch.Alerts) == 0 {
		return nil, errors.New("synthetic alert batch is empty after instance settings filtering")
	}
	// Malformed individual items are Quoin intake issues, not a reason to
	// discard other valid members of this accepted delivery at the gateway.
	encoded, err := json.Marshal(batch)
	if err != nil {
		return nil, err
	}
	return []plugins.Event{{Type: "alerts.batch", Payload: encoded}}, nil
}

type normalizer struct{}

func (normalizer) NormalizeAlert(body []byte) ([]plugins.NormalizedAlert, error) {
	batch, err := contract.ParseAlertmanagerWebhook(body)
	if err != nil {
		return nil, err
	}
	result := make([]plugins.NormalizedAlert, 0, len(batch.Alerts))
	for _, item := range batch.Alerts {
		result = append(result, plugins.NormalizedAlert{
			Severity:    plugins.SeverityHigh,
			Title:       "Synthetic source",
			Resource:    item.Labels["instance"],
			Annotations: item.Annotations,
		})
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// synthetic_query — the grant-scoped model-visible observation tool
// ---------------------------------------------------------------------------

// queryArgs is the typed argument set of the synthetic query tool. The
// declared GrantPlan makes sourceRef the optional disambiguation argument
// and freezes the resolved execution arguments.
type queryArgs struct {
	Query     string `json:"query" doc:"合成查询表达式（回显于结果）"`
	SourceRef string `json:"sourceRef,omitempty" doc:"仅在来源有歧义时显式命名来源连接"`
}

// queryArtifactRef is the long-body spill locator inside the sealed result.
type queryArtifactRef struct {
	ID        string `json:"id"`
	MediaType string `json:"mediaType,omitempty"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

// queryResult is the canonical synthetic_query_result_v1 payload. Structured
// failures travel inside the same shape (success=false).
type queryResult struct {
	Success     bool              `json:"success"`
	StartedAt   string            `json:"startedAt"`
	FinishedAt  string            `json:"finishedAt"`
	Query       string            `json:"query"`
	Output      string            `json:"output,omitempty"`
	Truncated   bool              `json:"truncated,omitempty"`
	Artifact    *queryArtifactRef `json:"artifact,omitempty"`
	ErrorCode   string            `json:"errorCode,omitempty"`
	ErrorDetail string            `json:"errorDetail,omitempty"`
}

// queryTool is the compiled grant-scoped observation tool: the complete
// frozen contract (version, execution location, result schema, model-facing
// description, authorization plan and evidence projection) owned by this
// plugin — the attempt core resolves and validates it generically.
var queryTool = plugins.Tool[queryArgs, queryResult]{
	Name: QueryToolName, Version: QueryToolVersion, FailureMode: plugins.FailureReturnToModel, ResultKind: QueryResultSchemaKind,
	ProducesEvidence: true, Timeout: 15 * time.Second, RateLimitPerMinute: 60,
	Grant: &plugins.GrantPlan{
		Purpose:                  QueryGrantPurpose,
		SourceItemRole:           SourceItemRole,
		SourceRefArgument:        "sourceRef",
		FreezeExecutionArguments: true,
	},
	EvidenceProjector: queryEvidence,
	Description:       "合成插件只读查询工具：对已授权的合成来源执行一次回显查询；sourceRef 仅在来源有歧义时显式命名来源连接，结果作为不可变 Evidence 封存。",
	Handler:           runQueryTool,
}

func runQueryTool(t *plugins.ToolContext, args queryArgs) (queryResult, error) {
	startedAt := time.Now().UTC()
	fail := func(code, detail string) (queryResult, error) {
		return queryResult{
			Success: false, StartedAt: startedAt.Format(time.RFC3339Nano),
			FinishedAt: time.Now().UTC().Format(time.RFC3339Nano), Query: args.Query,
			ErrorCode: code, ErrorDetail: detail,
		}, nil
	}
	if strings.TrimSpace(args.Query) == "" {
		return fail("invalid_arguments", "query 必须是非空字符串")
	}
	response, err := t.Platform.Call(t.Context, plugins.PlatformRequest{
		Method: "GET", Path: "/query", Query: url.Values{"query": {args.Query}}, Timeout: 15 * time.Second,
	})
	if err != nil {
		return fail("synthetic_unreachable", "查询请求失败: "+err.Error())
	}
	if response.StatusCode != 200 {
		return fail("synthetic_http_error", fmt.Sprintf("查询端点返回 HTTP %d", response.StatusCode))
	}
	body := response.Body
	totalBytes := int64(len(body))
	result := queryResult{
		Success: true, StartedAt: startedAt.Format(time.RFC3339Nano),
		FinishedAt: time.Now().UTC().Format(time.RFC3339Nano), Query: args.Query,
	}
	// The same bounded spill convention as the metrics tools: long outputs
	// commit an Artifact and keep only a bounded preview in the model
	// context.
	if totalBytes > 8*1024 && t.Spill != nil {
		artifactID, err := t.Spill(t.Context, body, "text/plain")
		if err != nil {
			return fail("artifact_commit_failed", "长输出 Artifact 提交失败: "+err.Error())
		}
		sum := sha256.Sum256(body)
		result.Truncated = true
		result.Artifact = &queryArtifactRef{
			ID: strconv.FormatInt(artifactID, 10), MediaType: "text/plain",
			SHA256: hex.EncodeToString(sum[:]), SizeBytes: totalBytes,
		}
		result.Output = "…（完整输出已存入 Artifact）\n" + limitedHead(body, 1024)
		return result, nil
	}
	result.Output = string(body)
	return result, nil
}

// queryEvidence derives the deterministic evidence projection of one
// succeeded synthetic_query result (declared beside the tool contract).
func queryEvidence(argumentsJSON, payloadJSON []byte, artifactID int64) (plugins.EvidenceProjection, error) {
	var result queryResult
	if err := json.Unmarshal(payloadJSON, &result); err != nil {
		return plugins.EvidenceProjection{}, fmt.Errorf("synthetic_query result unparseable: %w", err)
	}
	if !result.Success {
		return plugins.EvidenceProjection{}, errors.New("synthetic_query evidence requires a succeeded payload")
	}
	if result.FinishedAt == "" || result.Output == "" {
		return plugins.EvidenceProjection{}, errors.New("synthetic_query success result requires finishedAt and output")
	}
	if result.Truncated != (result.Artifact != nil) {
		return plugins.EvidenceProjection{}, errors.New("synthetic_query truncated flag must pair with the artifact locator")
	}
	projection := plugins.EvidenceProjection{
		ParamsJSON: argumentsJSON, ObservedAt: result.FinishedAt, Integrity: "complete",
	}
	if result.Truncated {
		if artifactID <= 0 {
			return plugins.EvidenceProjection{}, errors.New("spilled synthetic_query result lacks the committed artifact")
		}
		// The payload's artifact locator must close onto the Artifact the
		// Tool Call completion commits: a locator for any other artifact is
		// a protocol conflict, never a second authority.
		if result.Artifact == nil || result.Artifact.ID != strconv.FormatInt(artifactID, 10) {
			return plugins.EvidenceProjection{}, errors.New("synthetic_query artifact locator does not match the committed artifact")
		}
		projection.ArtifactID = artifactID
	} else {
		projection.ResultJSON = payloadJSON
	}
	return projection, nil
}

func limitedHead(body []byte, limit int) string {
	if len(body) > limit {
		return string(body[:limit]) + "…"
	}
	return string(body)
}

// ---------------------------------------------------------------------------
// synthetic_collect — internal deterministic collection (inspection template)
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
	Name: CollectToolName, Version: "1", FailureMode: plugins.FailureFailAttempt, ResultKind: CollectResultSchemaKind,
	Internal: true, Timeout: 15 * time.Second,
	// 显式 Schema：targets 是冻结的 CollectTarget 结构数组（struct 切片在派生
	// 词表之外），内部工具不进模型目录，泛型 array 语义足够。
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
	Description: "内部工具：执行冻结合成模板的确定性 HTTP 采集，产出逐检查证据与完整性标记。",
	Handler: func(t *plugins.ToolContext, args collectArgs) (plugins.CollectResult, error) {
		if args.TemplateID != TemplateID || args.TemplateVersion != TemplateVersion {
			return plugins.CollectResult{}, fmt.Errorf("unknown synthetic template %s/%s", args.TemplateID, args.TemplateVersion)
		}
		var params struct {
			Expression string `json:"expression"`
		}
		if err := json.Unmarshal(args.Params, &params); err != nil || strings.TrimSpace(params.Expression) == "" {
			return plugins.CollectResult{}, fmt.Errorf("synthetic_check requires a non-empty expression parameter")
		}
		response, err := t.Platform.Call(t.Context, plugins.PlatformRequest{
			Method: "GET", Path: "/collect", Query: url.Values{"expression": {params.Expression}}, Timeout: 15 * time.Second,
		})
		if err != nil {
			return plugins.CollectResult{}, fmt.Errorf("collect request failed: %w", err)
		}
		if response.StatusCode != 200 {
			return plugins.CollectResult{}, fmt.Errorf("collect endpoint returned HTTP %d", response.StatusCode)
		}
		var payload struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(response.Body, &payload); err != nil {
			return plugins.CollectResult{}, fmt.Errorf("collect response is not valid JSON: %w", err)
		}
		return plugins.CollectResult{
			Checks: []plugins.CheckObservation{{
				CheckID: TemplateID, Succeeded: true,
				EvidenceJSON: []byte(fmt.Sprintf(`{"result":{"expression":%q,"value":%q,"evidenceAt":%q}}`, params.Expression, payload.Value, args.EvidenceAt)),
			}},
			Incomplete: false,
		}, nil
	},
}

// ---------------------------------------------------------------------------
// Tool assembly (explicit host registry registration)
// ---------------------------------------------------------------------------

// syntheticToolProvider stamps the owning plugin onto the entries. Hosts
// register the plugin through their assembly; the attempt core derives the
// frozen catalogs, grant authorizations and evidence projections from these
// declarations alone.
type syntheticToolProvider struct{}

func (syntheticToolProvider) Tools() []plugins.ToolEntry {
	return []plugins.ToolEntry{
		queryTool.Entry("synthetic-plugin"),
		collectTool.Entry("synthetic-plugin"),
	}
}
