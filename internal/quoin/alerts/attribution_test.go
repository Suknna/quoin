package alerts

// Attribution tests cover the business-view-owned first-observation decision.
// A view participates in alert attribution only through an explicit
// alertSourceKeys scope naming the delivering Alertmanager source, and only
// when every exact label condition is present. Later view edits must never
// rewrite a frozen decision, and the legacy business_system field stays
// history-only: new occurrences never grow one.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func deliverWebhookFrom(t *testing.T, service *Service, relayID string, sourceID, credentialID int64, labels map[string]string, startsAt string) DeliveryResult {
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
	result, err := service.Deliver(context.Background(), relayID, sourceID, credentialID, 1, body, time.Now().UTC())
	if err != nil {
		t.Fatalf("deliver %s: %v", relayID, err)
	}
	if !result.Accepted {
		t.Fatalf("deliver %s rejected: %+v", relayID, result)
	}
	return result
}

func occurrenceBusinessID(t *testing.T, service *Service, occurrenceID int64) *int64 {
	t.Helper()
	var value *int64
	if err := service.db.QueryRow(`SELECT business_system_id FROM alert_occurrences WHERE id=?`, occurrenceID).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
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

type viewAttributionRow struct {
	Status           string
	AttributedViewID *int64
	CandidatesJSON   string
	ReasonJSON       string
}

func occurrenceViewAttribution(t *testing.T, service *Service, occurrenceID int64) viewAttributionRow {
	t.Helper()
	var row viewAttributionRow
	if err := service.db.QueryRow(`SELECT status,attributed_view_id,candidates_json,reason_json FROM alert_occurrence_view_attributions WHERE occurrence_id=?`, occurrenceID).
		Scan(&row.Status, &row.AttributedViewID, &row.CandidatesJSON, &row.ReasonJSON); err != nil {
		t.Fatal(err)
	}
	return row
}

func candidatesContain(t *testing.T, candidatesJSON string, wantKey string) bool {
	t.Helper()
	var candidates []struct {
		ViewKey     string `json:"viewKey"`
		DisplayName string `json:"displayName"`
	}
	if err := json.Unmarshal([]byte(candidatesJSON), &candidates); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.ViewKey == wantKey {
			if candidate.DisplayName == "" {
				t.Fatalf("candidate %s must freeze a display name", wantKey)
			}
			return true
		}
	}
	return false
}

func TestViewAttributionUniqueMatchFreezesSnapshot(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")
	seedAttributionView(t, service, "payments-prod", "支付生产", []string{"am-prod"}, map[string]string{"service": "payments", "env": "production"})

	result := deliverWebhookFrom(t, service, "view-unique", sourceID, credentialID, map[string]string{
		"alertname": "Latency", "service": "payments", "env": "production",
	}, "2026-09-01T10:00:00Z")
	occurrenceID := result.Occurrences[0].ID

	// 旧 business_system 字段保持历史专用：新归属绝不写它。
	if id := occurrenceBusinessID(t, service, occurrenceID); id != nil {
		t.Fatalf("new model must never write business_system_id, got %d", *id)
	}
	row := occurrenceViewAttribution(t, service, occurrenceID)
	if row.Status != "attributed" || row.AttributedViewID == nil || *row.AttributedViewID != 1 {
		t.Fatalf("unique match must attribute to view 1, got %+v", row)
	}
	if !candidatesContain(t, row.CandidatesJSON, "payments-prod") {
		t.Fatalf("attributed candidate snapshot must carry the view identity: %s", row.CandidatesJSON)
	}
	if row.ReasonJSON != `{"code":"exactly_one_matching_view"}` {
		t.Fatalf("unexpected reason %s", row.ReasonJSON)
	}
	detail, err := service.GetOccurrence(ctx, occurrenceID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.ViewAttribution == nil || detail.ViewAttribution.Status != "attributed" || detail.ViewAttribution.ViewKey != "payments-prod" || detail.ViewAttribution.ViewName != "支付生产" {
		t.Fatalf("detail must surface the real view attribution: %+v", detail.ViewAttribution)
	}

	// 冻结名字：视图改名/退役后，列表与详情的 key/name 仍来自首收快照，
	// 绝不漂移到当前业务视图行。
	if _, err := service.db.Exec(`UPDATE business_views SET display_name='改名后' WHERE view_key='payments-prod'`); err != nil {
		t.Fatal(err)
	}
	renamedDetail, err := service.GetOccurrence(ctx, occurrenceID)
	if err != nil {
		t.Fatal(err)
	}
	if renamedDetail.ViewAttribution == nil || renamedDetail.ViewAttribution.ViewKey != "payments-prod" || renamedDetail.ViewAttribution.ViewName != "支付生产" {
		t.Fatalf("frozen identity must not follow the renamed view: %+v", renamedDetail.ViewAttribution)
	}
	snapshot, err := service.AlertSnapshot(ctx, "Firing", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range snapshot.Items {
		if item.ID == fmt.Sprintf("%d", occurrenceID) && (item.ViewAttribution == nil || item.ViewAttribution.ViewName != "支付生产") {
			t.Fatalf("list must surface the frozen snapshot name, got %+v", item.ViewAttribution)
		}
	}
}

// 归属证据冻结由 schema 触发器强制：任何 UPDATE/DELETE 都必须被 SQL 拒绝，
// 且 INSERT 必须闭合到同一 Delivery 的真实条目。
func TestViewAttributionRowsAreFrozenBySchema(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")
	seedAttributionView(t, service, "payments-prod", "支付生产", []string{"am-prod"}, map[string]string{"service": "payments"})
	result := deliverWebhookFrom(t, service, "frozen-row", sourceID, credentialID, map[string]string{
		"alertname": "Latency", "service": "payments",
	}, "2026-09-01T10:06:00Z")
	occurrenceID := result.Occurrences[0].ID

	if _, err := service.db.Exec(`UPDATE alert_occurrence_view_attributions SET status='unattributed', attributed_view_id=NULL, candidates_json='[]' WHERE occurrence_id=?`, occurrenceID); err == nil {
		t.Fatal("attribution UPDATE must be rejected by the frozen trigger")
	}
	if _, err := service.db.Exec(`DELETE FROM alert_occurrence_view_attributions WHERE occurrence_id=?`, occurrenceID); err == nil {
		t.Fatal("attribution DELETE must be rejected by the frozen trigger")
	}
	if _, err := service.db.Exec(`INSERT INTO alert_occurrence_view_attributions(occurrence_id,status,attributed_view_id,candidates_json,reason_json,evaluated_from_delivery_id,evaluated_from_delivery_item_id,created_at) VALUES(?,'unattributed',NULL,'[]','{"code":"label_mismatch"}',1,999,'2026-09-01T10:06:00Z')`, occurrenceID); err == nil {
		t.Fatal("attribution INSERT must close to its own delivery item")
	}
	// 视图稳定 key 不可改写、行不可删除（退役不复用）。
	if _, err := service.db.Exec(`UPDATE business_views SET view_key='renamed' WHERE view_key='payments-prod'`); err == nil {
		t.Fatal("business view key must be immutable")
	}
	if _, err := service.db.Exec(`DELETE FROM business_views WHERE view_key='payments-prod'`); err == nil {
		t.Fatal("business views must never be deleted")
	}
	if row := occurrenceViewAttribution(t, service, occurrenceID); row.Status != "attributed" {
		t.Fatalf("rejected writes must leave the frozen row intact: %+v", row)
	}
}

func TestViewAttributionRequiresExplicitSourceAndLabels(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")
	seedSource(t, service, ctx, "am-edge")
	// 普通巡检视图：无 alertSourceKeys，绝不参与告警归属。
	seedAttributionView(t, service, "plain-view", "普通视图", nil, map[string]string{"service": "payments"})
	// 只声明其它 AM 源的视图。
	seedAttributionView(t, service, "edge-view", "边缘视图", []string{"am-edge"}, map[string]string{"service": "payments"})

	labels := map[string]string{"alertname": "Latency", "service": "payments"}
	plain := deliverWebhookFrom(t, service, "plain-view-delivery", sourceID, credentialID, labels, "2026-09-01T10:01:00Z")
	if id := occurrenceBusinessID(t, service, plain.Occurrences[0].ID); id != nil {
		t.Fatalf("view without alertSourceKeys must not attribute, got %d", *id)
	}
	if row := occurrenceViewAttribution(t, service, plain.Occurrences[0].ID); row.Status != "unattributed" || row.ReasonJSON != `{"code":"source_mismatch"}` {
		t.Fatalf("unscoped view mismatch diagnostics=%+v", row)
	}
	if detail, err := service.GetOccurrence(ctx, plain.Occurrences[0].ID); err != nil || detail.ViewAttribution == nil || detail.ViewAttribution.Status != "unattributed" {
		t.Fatalf("unattributed detail must still carry view attribution diagnostics: %+v %v", detail.ViewAttribution, err)
	}

	edge := deliverWebhookFrom(t, service, "edge-view-delivery", sourceID, credentialID, labels, "2026-09-01T10:02:00Z")
	if row := occurrenceViewAttribution(t, service, edge.Occurrences[0].ID); row.Status != "unattributed" || row.ReasonJSON != `{"code":"source_mismatch"}` {
		t.Fatalf("other-source view mismatch diagnostics=%+v", row)
	}

	// 标签不满足：源匹配但条件不命中，仍不归属。
	seedAttributionView(t, service, "payments-prod", "支付生产", []string{"am-prod"}, map[string]string{"service": "payments", "env": "production"})
	mismatch := deliverWebhookFrom(t, service, "label-mismatch", sourceID, credentialID, labels, "2026-09-01T10:03:00Z")
	if row := occurrenceViewAttribution(t, service, mismatch.Occurrences[0].ID); row.Status != "unattributed" || row.ReasonJSON != `{"code":"label_mismatch"}` {
		t.Fatalf("label mismatch diagnostics=%+v", row)
	}
}

func TestViewAttributionEmptyConditionsNeverSwallowAll(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")
	// 历史直插兜底行：无标签条件却声明了来源。空标签条件绝不构成吞掉一切的
	// 兜底匹配；写路径已在 businessview 校验层拒绝该形状。
	seedAttributionView(t, service, "catch-all", "兜底视图", []string{"am-prod"}, nil)

	result := deliverWebhookFrom(t, service, "catch-all-delivery", sourceID, credentialID, map[string]string{
		"alertname": "Anything",
	}, "2026-09-01T10:04:00Z")
	if row := occurrenceViewAttribution(t, service, result.Occurrences[0].ID); row.Status != "unattributed" || row.ReasonJSON != `{"code":"label_mismatch"}` {
		t.Fatalf("empty label conditions must never match everything: %+v", row)
	}
}

func TestViewAttributionAmbiguousFreezesCandidateSnapshots(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")
	seedAttributionView(t, service, "payments-prod", "支付生产", []string{"am-prod"}, map[string]string{"service": "payments"})
	seedAttributionView(t, service, "payments-canary", "支付金丝雀", []string{"am-prod"}, map[string]string{"service": "payments"})

	result := deliverWebhookFrom(t, service, "view-ambiguous", sourceID, credentialID, map[string]string{
		"alertname": "Latency", "service": "payments",
	}, "2026-09-01T10:05:00Z")
	occurrenceID := result.Occurrences[0].ID
	if id := occurrenceBusinessID(t, service, occurrenceID); id != nil {
		t.Fatalf("ambiguous match must not assign anything, got %d", *id)
	}
	row := occurrenceViewAttribution(t, service, occurrenceID)
	if row.Status != "ambiguous" || row.AttributedViewID != nil {
		t.Fatalf("ambiguous decision shape=%+v", row)
	}
	if row.ReasonJSON != `{"code":"multiple_matching_views"}` {
		t.Fatalf("unexpected reason %s", row.ReasonJSON)
	}
	for _, key := range []string{"payments-prod", "payments-canary"} {
		if !candidatesContain(t, row.CandidatesJSON, key) {
			t.Fatalf("ambiguous candidates must freeze every matching view snapshot (%s): %s", key, row.CandidatesJSON)
		}
	}

	// 冻结语义：视图后续改名/改条件不得改写已冻结的歧义证据。
	if _, err := service.db.Exec(`UPDATE business_views SET display_name='改名后', label_conditions_json='{"service":"other"}', row_version=row_version+1 WHERE view_key='payments-canary'`); err != nil {
		t.Fatal(err)
	}
	after := occurrenceViewAttribution(t, service, occurrenceID)
	if after != row {
		t.Fatalf("frozen ambiguous evidence changed: before=%+v after=%+v", row, after)
	}
}

func TestViewAttributionIsWriteOnceAtCreation(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")

	early := deliverWebhookFrom(t, service, "view-early", sourceID, credentialID, map[string]string{
		"alertname": "Early", "service": "payments",
	}, "2026-09-01T11:00:00Z")
	if row := occurrenceViewAttribution(t, service, early.Occurrences[0].ID); row.Status != "unattributed" {
		t.Fatalf("no view exists yet; must be unattributed: %+v", row)
	}

	seedAttributionView(t, service, "payments-prod", "支付生产", []string{"am-prod"}, map[string]string{"service": "payments"})

	// 同一 occurrence 的重复投递不得重新归属（write-once）。
	repeat, err := service.Deliver(context.Background(), "view-early-repeat", sourceID, credentialID, 1, mustBody(t, map[string]string{
		"alertname": "Early", "service": "payments",
	}, "2026-09-01T11:00:00Z"), time.Now().UTC())
	if err != nil || !repeat.Accepted {
		t.Fatalf("repeat delivery: %+v %v", repeat, err)
	}
	if row := occurrenceViewAttribution(t, service, early.Occurrences[0].ID); row.Status != "unattributed" {
		t.Fatalf("repeat delivery must not rewrite historical attribution: %+v", row)
	}

	// 新 occurrence 按当前视图归属。
	later := deliverWebhookFrom(t, service, "view-later", sourceID, credentialID, map[string]string{
		"alertname": "Early", "service": "payments",
	}, "2026-09-01T11:30:00Z")
	if row := occurrenceViewAttribution(t, service, later.Occurrences[0].ID); row.Status != "attributed" {
		t.Fatalf("new occurrence after view creation must attribute: %+v", row)
	}
}

func TestSnapshotViewFilterAndDetailKey(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "am-prod")
	seedAttributionView(t, service, "payments-prod", "支付生产", []string{"am-prod"}, map[string]string{"service": "payments"})
	seedAttributionView(t, service, "billing-prod", "计费生产", []string{"am-prod"}, map[string]string{"service": "billing"})

	deliver := func(relayID, service_ string) int64 {
		t.Helper()
		result := deliverWebhookFrom(t, service, relayID, sourceID, credentialID, map[string]string{
			"alertname": relayID, "service": service_,
		}, fmt.Sprintf("2026-09-01T12:%02d:00Z", len(relayID)))
		return result.Occurrences[0].ID
	}
	deliver("pay-1", "payments")
	deliver("bill-1", "billing")
	unattributed := deliver("none-1", "unknown")
	// 平台内部故障只在无过滤读取中并列出现；归属过滤绝不拼接平台行。
	if _, err := service.db.Exec(`INSERT INTO platform_faults(component,reason,state,first_seen_at,last_seen_at) VALUES('plinth','runtime_control_stream_disconnected','Firing','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	unfiltered, err := service.AlertSnapshot(ctx, "Firing", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered.Items) != 4 {
		t.Fatalf("unfiltered snapshot must list three occurrences plus the platform fault, got %d", len(unfiltered.Items))
	}
	for _, item := range unfiltered.Items {
		switch item.Labels["alertname"] {
		case "pay-1":
			if item.ViewAttribution == nil || item.ViewAttribution.ViewKey != "payments-prod" || item.ViewAttribution.ViewName != "支付生产" {
				t.Fatalf("pay-1 view attribution wrong: %+v", item.ViewAttribution)
			}
		case "bill-1":
			if item.ViewAttribution == nil || item.ViewAttribution.ViewKey != "billing-prod" {
				t.Fatalf("bill-1 view attribution wrong: %+v", item.ViewAttribution)
			}
		case "none-1":
			if item.ViewAttribution == nil || item.ViewAttribution.Status != "unattributed" {
				t.Fatalf("none-1 must stay unattributed: %+v", item.ViewAttribution)
			}
		}
	}

	filtered, err := service.AlertSnapshot(ctx, "Firing", "", "payments-prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].Labels["alertname"] != "pay-1" {
		t.Fatalf("view filter must return exactly pay-1 without platform faults, got %+v", filtered.Items)
	}

	// 旧 businessSystem 过滤保持兼容：无历史行时为合法空结果。
	legacy, err := service.AlertSnapshot(ctx, "Firing", "payments", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy.Items) != 0 {
		t.Fatalf("legacy business filter must remain usable, got %+v", legacy.Items)
	}

	unknown, err := service.AlertSnapshot(ctx, "Firing", "", "ghost")
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
	if detail.ViewAttribution == nil || detail.ViewAttribution.ViewKey != "" {
		t.Fatalf("unattributed detail must not fabricate a view, got %+v", detail.ViewAttribution)
	}
}
