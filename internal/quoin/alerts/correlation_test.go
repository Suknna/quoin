package alerts

// ADR-0012 intake 流水线测试：Normalize（统一语义列冻结）、Enrich（富化求值
// 与冻结）、Correlate（多命中全记录 + 冻结快照）与 normalizer_missing 接入
// 问题。匹配语义沿用 ADR-0008：视图必须显式声明交付告警源且非空精确标签
// 条件全部命中，空标签条件绝不构成兜底匹配；后续视图编辑绝不改写已冻结
// 证据。

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	// 与 cmd/quoin 相同的装配：blank-import 内置插件注册表，使 alertmanager
	// 协议在进程默认注册表中携带 AlertNormalizer（ADR-0012）。
	_ "github.com/Suknna/quoin/internal/plugins/builtin"
)

func deliverWebhookFrom(t *testing.T, service *Service, eventID string, sourceID, credentialID int64, labels map[string]string, startsAt string) DeliveryResult {
	t.Helper()
	result, err := service.Deliver(context.Background(), eventID, sourceID, credentialID, 1, mustBody(t, labels, startsAt), time.Now().UTC())
	if err != nil {
		t.Fatalf("deliver %s: %v", eventID, err)
	}
	if !result.Accepted {
		t.Fatalf("deliver %s rejected: %+v", eventID, result)
	}
	return result
}

func mustBody(t *testing.T, labels map[string]string, startsAt string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"status": "firing",
		"alerts": []map[string]any{{
			"status":   "firing",
			"labels":   labels,
			"startsAt": startsAt,
			"endsAt":   "0001-01-01T00:00:00Z",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// seedAttributionView inserts a business view directly: the alerts package
// only reads business_views, ownership of the write path lives in the
// businessview module (validated there, including alertSourceKeys rules).
func seedAttributionView(t *testing.T, service *Service, viewKey, displayName string, sourceKeys []string, conditions map[string]string) int64 {
	t.Helper()
	if sourceKeys == nil {
		sourceKeys = []string{}
	}
	if conditions == nil {
		conditions = map[string]string{}
	}
	sourceKeysJSON, err := json.Marshal(sourceKeys)
	if err != nil {
		t.Fatal(err)
	}
	conditionsJSON, err := json.Marshal(conditions)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := service.db.Exec(`INSERT INTO business_views(view_key,display_name,description,connection_id,label_conditions_json,alert_source_keys_json,row_version,created_at,updated_at) VALUES(?,?,'',NULL,?,?,1,?,?)`,
		viewKey, displayName, string(conditionsJSON), string(sourceKeysJSON), now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// seedEnrichmentRule 直插富化规则：写路径归 businessview 模块（校验在那里
// 覆盖），这里只构造求值输入。
func seedEnrichmentRule(t *testing.T, service *Service, ruleKey string, sourceKeys []string, conditions, outputs map[string]string, priority int) {
	t.Helper()
	if sourceKeys == nil {
		sourceKeys = []string{}
	}
	if conditions == nil {
		conditions = map[string]string{}
	}
	sourceKeysJSON, _ := json.Marshal(sourceKeys)
	conditionsJSON, _ := json.Marshal(conditions)
	outputsJSON, _ := json.Marshal(outputs)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := service.db.Exec(`INSERT INTO enrichment_rules(rule_key,display_name,label_conditions_json,alert_source_keys_json,outputs_json,priority,row_version,created_at,updated_at) VALUES(?,?,?,?,?,?,1,?,?)`,
		ruleKey, ruleKey, string(conditionsJSON), string(sourceKeysJSON), string(outputsJSON), priority, now, now); err != nil {
		t.Fatal(err)
	}
}

type correlationRow struct {
	ViewKey     string
	DisplayName string
}

func occurrenceCorrelations(t *testing.T, service *Service, occurrenceID int64) []correlationRow {
	t.Helper()
	rows, err := service.db.Query(`SELECT view_key, display_name FROM alert_occurrence_correlations WHERE occurrence_id=? ORDER BY matched_at, id`, occurrenceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := []correlationRow{}
	for rows.Next() {
		var row correlationRow
		if err := rows.Scan(&row.ViewKey, &row.DisplayName); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	return result
}

func occurrenceEnrichment(t *testing.T, service *Service, occurrenceID int64) (fields map[string]string, ruleKeys []string) {
	t.Helper()
	var documentJSON string
	if err := service.db.QueryRow(`SELECT enrichment_json FROM alert_enrichments WHERE occurrence_id=?`, occurrenceID).Scan(&documentJSON); err != nil {
		t.Fatal(err)
	}
	var document struct {
		Fields map[string]string `json:"fields"`
		Rules  []struct {
			Key string `json:"key"`
		} `json:"rules"`
	}
	if err := json.Unmarshal([]byte(documentJSON), &document); err != nil {
		t.Fatal(err)
	}
	for _, rule := range document.Rules {
		ruleKeys = append(ruleKeys, rule.Key)
	}
	return document.Fields, ruleKeys
}

// Normalize：alertmanager normalizer 把 severity/alertname/annotations/instance
// 投影进统一语义列并冻结。
func TestNormalizeFreezesUnifiedSemantics(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")

	result, err := service.Deliver(context.Background(), "norm-1", sourceID, credentialID, 1,
		webhookBody("firing", map[string]string{
			"alertname": "HighLatency", "severity": "critical", "instance": "db-1:9100", "job": "mysql",
		}, "2026-09-01T10:00:00Z", `,"annotations":{"summary":"high"}`), time.Now().UTC())
	if err != nil || !result.Accepted {
		t.Fatalf("deliver norm-1: %v %+v", err, result)
	}
	occurrenceID := result.Occurrences[0].ID

	var severity, title, resource string
	if err := service.db.QueryRow(`SELECT severity, title, resource FROM alert_occurrences WHERE id=?`, occurrenceID).Scan(&severity, &title, &resource); err != nil {
		t.Fatal(err)
	}
	if severity != "critical" || title != "HighLatency" || resource != "db-1:9100" {
		t.Fatalf("unified semantics not frozen: severity=%q title=%q resource=%q", severity, title, resource)
	}
	summary, err := service.GetOccurrence(ctx, occurrenceID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Severity != "critical" || summary.Title != "HighLatency" || summary.Resource != "db-1:9100" {
		t.Fatalf("summary projection wrong: %+v", summary)
	}
	if summary.Annotations["summary"] != "high" {
		t.Fatalf("annotations must come from the frozen canonical map: %+v", summary.Annotations)
	}
}

// Normalize 的缺省路径：webhook 未携带 severity/annotations 时按 normalizer
// 映射降为 info，并冻结空注释——不是接入问题（normalizer 存在且解析成功）。
func TestNormalizeDegradesUnmappedSeverityToInfo(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")

	result := deliverWebhookFrom(t, service, "norm-info", sourceID, credentialID, map[string]string{
		"alertname": "CPU", "severity": "none",
	}, "2026-09-01T10:01:00Z")
	occurrenceID := result.Occurrences[0].ID
	var severity, annotations string
	if err := service.db.QueryRow(`SELECT severity, annotations_canonical FROM alert_occurrences WHERE id=?`, occurrenceID).Scan(&severity, &annotations); err != nil {
		t.Fatal(err)
	}
	if severity != "info" || annotations != "{}" {
		t.Fatalf("unmapped severity must degrade to info with empty annotations, got %q %q", severity, annotations)
	}
	var issues int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM alert_intake_issues WHERE kind='normalizer_missing'`).Scan(&issues); err != nil || issues != 0 {
		t.Fatalf("existing normalizer must not raise normalizer_missing: %d %v", issues, err)
	}
}

// 无 normalizer 的协议（未来协议）：语义全部缺省冻结（单元面）——
// normalizeDelivery 对未知 kind 不得解析出 normalizer，semanticsFor 退化为
// info/”/'{}'/”；越界 index 同样防御性降级。
func TestNormalizeMissingProtocolDegradesToDefaults(t *testing.T) {
	normalization := normalizeDelivery("unknown-protocol", mustBody(t, map[string]string{"alertname": "CPU"}, "2026-09-01T10:00:00Z"))
	if normalization.ok {
		t.Fatal("unknown protocol must not resolve a normalizer")
	}
	severity, title, annotations, resource := normalization.semanticsFor(0)
	if severity != "info" || title != "" || annotations != "{}" || resource != "" {
		t.Fatalf("degraded semantics wrong: %q %q %q %q", severity, title, annotations, resource)
	}
	// normalizer 输出与 payload 条目数不一致的防御分支：越界降级。
	normalization = deliveryNormalization{ok: true, normalized: nil}
	if _, _, annotations, _ := normalization.semanticsFor(3); annotations != "{}" {
		t.Fatalf("out-of-range alert must degrade, got %q", annotations)
	}
	// 词表外 severity 的防御性收敛由 semanticsFor 保证（normalizer 契约之外）。
}

// normalizer_missing 接入问题聚合：来源级问题闭合到已处理 Delivery，重复
// 记录按既有聚合计数推进（trg_alert_intake_issues_repeat_update 冻结校验）。
// v1 schema 的 protocol CHECK 只允许 alertmanager（注册表已装配 normalizer），
// 端到端缺失路径在未来协议放开后自然覆盖；这里直接驱动写入器验证新 kind 的
// schema 闭合约束与聚合推进（每 Delivery 至多一条无条目事件，跨 Delivery 聚合）。
func TestRecordNormalizerMissingIssueAggregates(t *testing.T) {
	service, database, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-future")

	// 两次真实交付提供闭合所需的已处理 Delivery 行。
	firstDelivery := deliverWebhookFrom(t, service, "norm-agg-1", sourceID, credentialID, map[string]string{
		"alertname": "CPU",
	}, "2026-09-01T10:12:00Z")
	secondDelivery := deliverWebhookFrom(t, service, "norm-agg-2", sourceID, credentialID, map[string]string{
		"alertname": "CPU",
	}, "2026-09-01T10:13:00Z")

	now := time.Now().UTC().Format(time.RFC3339Nano)
	first, err := service.recordNormalizerMissingIssue(ctx, database.SQL, sourceID, firstDelivery.DeliveryID, "future-protocol", now)
	if err != nil {
		t.Fatal(err)
	}
	if first.Kind != "normalizer_missing" || first.OccurrenceCount != 1 {
		t.Fatalf("first record wrong: %+v", first)
	}
	repeat, err := service.recordNormalizerMissingIssue(ctx, database.SQL, sourceID, secondDelivery.DeliveryID, "future-protocol", now)
	if err != nil {
		t.Fatal(err)
	}
	if repeat.OccurrenceCount != 2 {
		t.Fatalf("repeat must advance the aggregate, got %+v", repeat)
	}
	var count, rowVersion int64
	if err := database.SQL.QueryRow(`SELECT occurrence_count, row_version FROM alert_intake_issues WHERE kind='normalizer_missing'`).Scan(&count, &rowVersion); err != nil || count != 2 || rowVersion != 2 {
		t.Fatalf("aggregate row wrong: count=%d rowVersion=%d err=%v", count, rowVersion, err)
	}
	// 闭合触发器：不指向该源真实 Delivery 的 INSERT 必须被拒绝。
	if _, err := database.SQL.Exec(`INSERT INTO alert_intake_issues(source_id, delivery_id, kind, issue_key, detail_json, first_seen_at, last_seen_at, created_at) VALUES(?,?, 'normalizer_missing', ?, '{}', ?, ?, ?)`,
		sourceID, firstDelivery.DeliveryID+999, first.IssueKey, now, now, now); err == nil {
		t.Fatal("normalizer_missing must close to a real processed delivery of the same source")
	}
}

// Enrich：priority 升序叠加，后命中不覆盖已写字段；无命中也冻结空文档；
// 停用规则不参与；来源不匹配不参与。
func TestEnrichmentOverlaysByPriorityWithoutOverwrite(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")
	seedEnrichmentRule(t, service, "global-default", nil, nil, map[string]string{"team": "unknown", "env": "global"}, 200)
	seedEnrichmentRule(t, service, "db-team", []string{"am-prod"}, map[string]string{"service": "mysql"}, map[string]string{"team": "db", "service": "mysql"}, 100)
	// 停用规则与其它来源规则绝不参与。
	seedEnrichmentRule(t, service, "disabled-rule", nil, nil, map[string]string{"team": "nope"}, 50)
	if _, err := service.db.Exec(`UPDATE enrichment_rules SET enabled=0, row_version=row_version+1 WHERE rule_key='disabled-rule'`); err != nil {
		t.Fatal(err)
	}

	result := deliverWebhookFrom(t, service, "enrich-1", sourceID, credentialID, map[string]string{
		"alertname": "MysqlDown", "service": "mysql",
	}, "2026-09-01T10:03:00Z")
	occurrenceID := result.Occurrences[0].ID

	fields, ruleKeys := occurrenceEnrichment(t, service, occurrenceID)
	// db-team (priority 100) 先写 team=db；global-default (200) 的 team=unknown
	// 不覆盖，但其独有 env=global 叠加。service 字段两规则都声明同值。
	if fields["team"] != "db" || fields["env"] != "global" {
		t.Fatalf("overlay wrong: %+v", fields)
	}
	if len(ruleKeys) != 2 || ruleKeys[0] != "db-team" || ruleKeys[1] != "global-default" {
		t.Fatalf("rule trace must follow evaluation order: %v", ruleKeys)
	}

	// 无命中：fields 为空对象但行存在（区分未求值）。
	other := deliverWebhookFrom(t, service, "enrich-2", sourceID, credentialID, map[string]string{
		"alertname": "Other",
	}, "2026-09-01T10:04:00Z")
	fields, ruleKeys = occurrenceEnrichment(t, service, other.Occurrences[0].ID)
	// global-default 无条件命中一切 → other 也被全局规则富化；改用不含全局
	// 规则的对照在下一用例验证空冻结。
	if len(fields) == 0 {
		t.Fatalf("global default rule must match everything: %+v", fields)
	}
	if len(ruleKeys) != 1 || ruleKeys[0] != "global-default" {
		t.Fatalf("only the global rule matches: %v", ruleKeys)
	}

	summary, err := service.GetOccurrence(ctx, occurrenceID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Enrichment == nil || summary.Enrichment.Fields["team"] != "db" {
		t.Fatalf("detail enrichment projection wrong: %+v", summary.Enrichment)
	}
	if len(summary.Enrichment.RuleKeys) != 2 || summary.Enrichment.RuleKeys[0] != "db-team" {
		t.Fatalf("detail ruleKeys trace wrong: %v", summary.Enrichment.RuleKeys)
	}
}

// Enrich 空冻结：无任何启用规则时 alert_enrichments 仍写一行 fields={}。
func TestEnrichmentFreezesEmptyDocumentWhenNoRuleMatches(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")

	result := deliverWebhookFrom(t, service, "enrich-empty", sourceID, credentialID, map[string]string{
		"alertname": "Lonely",
	}, "2026-09-01T10:05:00Z")
	occurrenceID := result.Occurrences[0].ID
	fields, ruleKeys := occurrenceEnrichment(t, service, occurrenceID)
	if len(fields) != 0 || len(ruleKeys) != 0 {
		t.Fatalf("no rules must freeze an empty document: %+v %v", fields, ruleKeys)
	}
	var rows int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM alert_enrichments WHERE occurrence_id=?`, occurrenceID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("empty enrichment must still be frozen: %d %v", rows, err)
	}
	summary, err := service.GetOccurrence(ctx, occurrenceID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Enrichment == nil || len(summary.Enrichment.Fields) != 0 {
		t.Fatalf("empty enrichment must surface as empty fields: %+v", summary.Enrichment)
	}
}

// Enrich 冻结不可改写：schema 触发器拒绝 UPDATE/DELETE。
func TestEnrichmentRowsAreFrozenBySchema(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")
	result := deliverWebhookFrom(t, service, "enrich-frozen", sourceID, credentialID, map[string]string{
		"alertname": "CPU",
	}, "2026-09-01T10:06:00Z")
	occurrenceID := result.Occurrences[0].ID
	if _, err := service.db.Exec(`UPDATE alert_enrichments SET enrichment_json='{"fields":{"team":"hack"},"rules":[]}' WHERE occurrence_id=?`, occurrenceID); err == nil {
		t.Fatal("enrichment UPDATE must be rejected")
	}
	if _, err := service.db.Exec(`DELETE FROM alert_enrichments WHERE occurrence_id=?`, occurrenceID); err == nil {
		t.Fatal("enrichment DELETE must be rejected")
	}
}

// Correlate：多命中全记录，每命中视图一行冻结快照；视图改名后不漂移。
func TestCorrelationRecordsEveryMatchingView(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")
	seedAttributionView(t, service, "payments-prod", "支付生产", []string{"am-prod"}, map[string]string{"service": "payments"})
	seedAttributionView(t, service, "payments-canary", "支付金丝雀", []string{"am-prod"}, map[string]string{"service": "payments"})

	result := deliverWebhookFrom(t, service, "corr-multi", sourceID, credentialID, map[string]string{
		"alertname": "Latency", "service": "payments",
	}, "2026-09-01T10:07:00Z")
	occurrenceID := result.Occurrences[0].ID
	correlations := occurrenceCorrelations(t, service, occurrenceID)
	if len(correlations) != 2 {
		t.Fatalf("both matching views must be recorded, got %+v", correlations)
	}
	keys := map[string]string{}
	for _, row := range correlations {
		keys[row.ViewKey] = row.DisplayName
	}
	if keys["payments-prod"] != "支付生产" || keys["payments-canary"] != "支付金丝雀" {
		t.Fatalf("frozen display names wrong: %+v", keys)
	}

	// 冻结语义：视图改名/退役不改写已冻结关联。
	if _, err := service.db.Exec(`UPDATE business_views SET display_name='改名后' WHERE view_key='payments-prod'`); err != nil {
		t.Fatal(err)
	}
	after := occurrenceCorrelations(t, service, occurrenceID)
	if len(after) != 2 || after[0].DisplayName != "支付生产" {
		t.Fatalf("frozen correlation drifted after rename: %+v", after)
	}

	summary, err := service.GetOccurrence(ctx, occurrenceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Correlations) != 2 || summary.Correlations[0].ViewKey != "payments-prod" || summary.Correlations[0].DisplayName != "支付生产" {
		t.Fatalf("detail correlations wrong: %+v", summary.Correlations)
	}
}

// Correlate 匹配前提：显式来源声明 + 非空精确标签条件全部命中；无标签
// 条件的视图即使声明来源也不构成兜底匹配。
func TestCorrelationRequiresExplicitSourceAndLabels(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")
	seedSource(t, service, ctx, "am-edge")
	seedAttributionView(t, service, "plain-view", "普通视图", nil, map[string]string{"service": "payments"})
	seedAttributionView(t, service, "edge-view", "边缘视图", []string{"am-edge"}, map[string]string{"service": "payments"})
	seedAttributionView(t, service, "catch-all", "兜底视图", []string{"am-prod"}, nil)

	labels := map[string]string{"alertname": "Latency", "service": "payments"}
	for eventID, startsAt := range map[string]string{
		"corr-plain": "2026-09-01T10:08:00Z", "corr-catchall": "2026-09-01T10:09:00Z",
	} {
		result := deliverWebhookFrom(t, service, eventID, sourceID, credentialID, labels, startsAt)
		if correlations := occurrenceCorrelations(t, service, result.Occurrences[0].ID); len(correlations) != 0 {
			t.Fatalf("%s: unscoped/other-source/empty-condition views must not correlate, got %+v", eventID, correlations)
		}
	}

	// 标签不满足：源匹配但条件不命中，仍不关联。
	seedAttributionView(t, service, "payments-prod", "支付生产", []string{"am-prod"}, map[string]string{"service": "payments", "env": "production"})
	mismatch := deliverWebhookFrom(t, service, "corr-mismatch", sourceID, credentialID, labels, "2026-09-01T10:10:00Z")
	if correlations := occurrenceCorrelations(t, service, mismatch.Occurrences[0].ID); len(correlations) != 0 {
		t.Fatalf("label mismatch must not correlate: %+v", correlations)
	}
}

// Correlate write-once：同一 occurrence 的重复交付不追加关联；首观测后新建
// 的视图不回写历史 occurrence；新 occurrence 按当前视图求值。
func TestCorrelationIsWriteOnceAtCreation(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")

	early := deliverWebhookFrom(t, service, "corr-early", sourceID, credentialID, map[string]string{
		"alertname": "Early", "service": "payments",
	}, "2026-09-01T11:00:00Z")
	if correlations := occurrenceCorrelations(t, service, early.Occurrences[0].ID); len(correlations) != 0 {
		t.Fatalf("no view exists yet; must have no correlations: %+v", correlations)
	}

	seedAttributionView(t, service, "payments-prod", "支付生产", []string{"am-prod"}, map[string]string{"service": "payments"})

	// 同一 occurrence 的重复投递不得追加关联（write-once）。
	repeat, err := service.Deliver(context.Background(), "corr-early-repeat", sourceID, credentialID, 1, mustBody(t, map[string]string{
		"alertname": "Early", "service": "payments",
	}, "2026-09-01T11:00:00Z"), time.Now().UTC())
	if err != nil || !repeat.Accepted {
		t.Fatalf("repeat delivery: %+v %v", repeat, err)
	}
	if correlations := occurrenceCorrelations(t, service, early.Occurrences[0].ID); len(correlations) != 0 {
		t.Fatalf("repeat delivery must not add correlations: %+v", correlations)
	}

	// 新 occurrence 按当前视图关联。
	later := deliverWebhookFrom(t, service, "corr-later", sourceID, credentialID, map[string]string{
		"alertname": "Early", "service": "payments",
	}, "2026-09-01T11:30:00Z")
	if correlations := occurrenceCorrelations(t, service, later.Occurrences[0].ID); len(correlations) != 1 || correlations[0].ViewKey != "payments-prod" {
		t.Fatalf("new occurrence must correlate with current views: %+v", correlations)
	}
}

// Correlate 冻结由 schema 触发器强制：任何 UPDATE/DELETE 都被拒绝。
func TestCorrelationRowsAreFrozenBySchema(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")
	seedAttributionView(t, service, "payments-prod", "支付生产", []string{"am-prod"}, map[string]string{"service": "payments"})
	result := deliverWebhookFrom(t, service, "corr-frozen", sourceID, credentialID, map[string]string{
		"alertname": "Latency", "service": "payments",
	}, "2026-09-01T10:11:00Z")
	occurrenceID := result.Occurrences[0].ID

	if _, err := service.db.Exec(`UPDATE alert_occurrence_correlations SET display_name='hack' WHERE occurrence_id=?`, occurrenceID); err == nil {
		t.Fatal("correlation UPDATE must be rejected by the frozen trigger")
	}
	if _, err := service.db.Exec(`DELETE FROM alert_occurrence_correlations WHERE occurrence_id=?`, occurrenceID); err == nil {
		t.Fatal("correlation DELETE must be rejected by the frozen trigger")
	}
}

// 快照 viewKey 过滤走 correlations 命中；平台故障只在无过滤读取中并列。
func TestSnapshotViewFilterAndDetailKey(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")
	seedAttributionView(t, service, "payments-prod", "支付生产", []string{"am-prod"}, map[string]string{"service": "payments"})
	seedAttributionView(t, service, "billing-prod", "计费生产", []string{"am-prod"}, map[string]string{"service": "billing"})

	deliver := func(eventID, service_ string) int64 {
		t.Helper()
		result := deliverWebhookFrom(t, service, eventID, sourceID, credentialID, map[string]string{
			"alertname": eventID, "service": service_,
		}, fmt.Sprintf("2026-09-01T12:%02d:00Z", len(eventID)))
		return result.Occurrences[0].ID
	}
	deliver("pay-1", "payments")
	deliver("bill-1", "billing")
	unattributed := deliver("none-1", "unknown")
	if _, err := service.db.Exec(`INSERT INTO platform_faults(component,reason,severity,title,annotations_canonical,state,first_seen_at,last_seen_at) VALUES('plinth','runtime_control_stream_disconnected','critical','Platform component unavailable','{"description":"runtime_control_stream_disconnected","summary":"内部组件 plinth 不可用"}','Firing','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	unfiltered, err := service.AlertSnapshot(ctx, "Firing", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered.Items) != 4 {
		t.Fatalf("unfiltered snapshot must list three occurrences plus the platform fault, got %d", len(unfiltered.Items))
	}
	for _, item := range unfiltered.Items {
		if item.Source == "platform" {
			if item.Severity != "critical" || item.Title != "Platform component unavailable" || item.Annotations["summary"] != "内部组件 plinth 不可用" {
				t.Fatalf("platform fault must read its frozen semantic columns: %+v", item)
			}
			continue
		}
		switch item.Labels["alertname"] {
		case "pay-1":
			if len(item.Correlations) != 1 || item.Correlations[0].ViewKey != "payments-prod" || item.Correlations[0].DisplayName != "支付生产" {
				t.Fatalf("pay-1 correlations wrong: %+v", item.Correlations)
			}
		case "bill-1":
			if len(item.Correlations) != 1 || item.Correlations[0].ViewKey != "billing-prod" {
				t.Fatalf("bill-1 correlations wrong: %+v", item.Correlations)
			}
		case "none-1":
			if len(item.Correlations) != 0 {
				t.Fatalf("none-1 must stay uncorrelated: %+v", item.Correlations)
			}
		}
	}

	filtered, err := service.AlertSnapshot(ctx, "Firing", "payments-prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].Labels["alertname"] != "pay-1" {
		t.Fatalf("view filter must return exactly pay-1 without platform faults, got %+v", filtered.Items)
	}

	unknown, err := service.AlertSnapshot(ctx, "Firing", "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown.Items) != 0 {
		t.Fatalf("unknown view key must filter to empty, got %d", len(unknown.Items))
	}

	detail, err := service.GetOccurrence(ctx, unattributed)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Correlations) != 0 {
		t.Fatalf("unattributed detail must not fabricate a view, got %+v", detail.Correlations)
	}
}
