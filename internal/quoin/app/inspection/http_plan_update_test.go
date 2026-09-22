package appinspection

// 计划更新路由的 HTTP 契约回归：实机 fix8 编辑页（/inspections/plans/:key/edit）
// 原样加载后直接保存被 422 "请求字段不满足要求" 拒绝。根因是 PUT 体匿名内嵌
// 未导出的 planRequest：huma 只把导出的匿名内嵌字段摊平进请求 schema
// （schema.go getFields 跳过未导出字段），PUT schema 因此只剩
// expectedRowVersion，全部计划字段都成 "unexpected property"。修复为导出的
// PlanRequest 后，本测试按 PlanEditor 的 planFormOf+save 逐字段重建真实页面
// 载荷（templateVersion/cron 空→null、语义字段取存储原值、
// expectedRowVersion=GET 返回值）验证：原样保存成功、rowVersion 递增、
// 语义字段不丢、路径与体标识不一致仍被确定性拒绝。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const unchangedUpdateBody = `{
	"clientCommandId": "cmd-update-unchanged",
	"planKey": "mall-shop-paas-acceptance",
	"displayName": "商城中间件可达性验收",
	"enabled": true,
	"connectionName": "mall-shop-prometheus",
	"pluginId": "prometheus",
	"templateId": "promql_instant",
	"templateVersion": null,
	"params": {"expression": "redis_up{system_id=\"mall-shop\"} or mysql_up{system_id=\"mall-shop\"}"},
	"scope": {"kind": "integration"},
	"checkDescription": "检查本次采样中 Redis 与 MySQL 是否可由各自 exporter 连接。预期各一条序列，1 表示连接成功，0 表示连接失败，缺失表示证据不足。不能等同于商城业务交易成功。",
	"metricUnit": "1=连接成功，0=连接失败",
	"reportInstructions": "分别列出Redis和MySQL的实际值、证据时间及结论，不将历史恢复与当前故障混淆。",
	"cron": null,
	"timezone": "Asia/Shanghai",
	"expectedRowVersion": 1
}`

func TestUpdatePlanUnchangedSaveSucceeds(t *testing.T) {
	_, db, api := newPlanScopeHarness(t)
	now := "2026-09-21T16:26:00Z"
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'fixture',1,'" + now + "','" + now + "')",
		"INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,zeroblob(32),1,'test','" + now + "','" + now + "','2099-01-01T00:00:00Z','2099-01-01T00:00:00Z')",
		"INSERT INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,zeroblob(12),zeroblob(16),'" + now + "')",
		"INSERT INTO connections(id,name,type,enabled,row_version,created_at) VALUES(1,'mall-shop-prometheus','prometheus',1,1,'" + now + "')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	create := `{
		"clientCommandId": "cmd-create-real-plan",
		"planKey": "mall-shop-paas-acceptance",
		"displayName": "商城中间件可达性验收",
		"enabled": true,
		"connectionName": "mall-shop-prometheus",
		"pluginId": "prometheus",
		"templateId": "promql_instant",
		"templateVersion": null,
		"params": {"expression": "redis_up{system_id=\"mall-shop\"} or mysql_up{system_id=\"mall-shop\"}"},
		"scope": {"kind": "integration"},
		"checkDescription": "检查本次采样中 Redis 与 MySQL 是否可由各自 exporter 连接。预期各一条序列，1 表示连接成功，0 表示连接失败，缺失表示证据不足。不能等同于商城业务交易成功。",
		"metricUnit": "1=连接成功，0=连接失败",
		"reportInstructions": "分别列出Redis和MySQL的实际值、证据时间及结论，不将历史恢复与当前故障混淆。",
		"cron": null,
		"timezone": "Asia/Shanghai"
	}`
	created := api.Post("/api/v1/inspections/plans", strings.NewReader(create),
		map[string][]string{"Cookie": {"__Host-quoin-session=test"}, "Content-Type": {"application/json"}})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}

	// 编辑页原样加载后直接保存：载荷与创建同字段 + expectedRowVersion(row 1)。
	response := api.Put("/api/v1/inspections/plans/mall-shop-paas-acceptance", strings.NewReader(unchangedUpdateBody),
		map[string][]string{"Cookie": {"__Host-quoin-session=test"}, "Content-Type": {"application/json"}})
	if response.Code != http.StatusOK {
		t.Fatalf("unchanged save must succeed (实机 bug: 422 unexpected property), status=%d body=%s", response.Code, response.Body.String())
	}
	var updated struct {
		RowVersion         int64   `json:"rowVersion"`
		TemplateVersion    *string `json:"templateVersion"`
		CheckDescription   *string `json:"checkDescription"`
		MetricUnit         *string `json:"metricUnit"`
		ReportInstructions *string `json:"reportInstructions"`
		Cron               *string `json:"cron"`
		Timezone           string  `json:"timezone"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.RowVersion != 2 {
		t.Fatalf("rowVersion=%d, want 2", updated.RowVersion)
	}
	if updated.CheckDescription == nil || !strings.Contains(*updated.CheckDescription, "1 表示连接成功") ||
		updated.MetricUnit == nil || *updated.MetricUnit != "1=连接成功，0=连接失败" ||
		updated.ReportInstructions == nil || !strings.Contains(*updated.ReportInstructions, "分别列出Redis和MySQL") {
		t.Fatalf("semantics lost on update: %+v", updated)
	}
	if updated.TemplateVersion != nil || updated.Cron != nil || updated.Timezone != "Asia/Shanghai" {
		t.Fatalf("nullable/timezone round trip wrong: %+v", updated)
	}

	// 路径与体标识不一致仍是确定性的 identity_conflict（非 schema 层）。
	conflict := strings.Replace(unchangedUpdateBody, `"planKey": "mall-shop-paas-acceptance"`, `"planKey": "another-plan"`, 1)
	conflict = strings.Replace(conflict, `"expectedRowVersion": 1`, `"expectedRowVersion": 2`, 1)
	rejected := api.Put("/api/v1/inspections/plans/mall-shop-paas-acceptance", strings.NewReader(conflict),
		map[string][]string{"Cookie": {"__Host-quoin-session=test"}, "Content-Type": {"application/json"}})
	if rejected.Code != http.StatusUnprocessableEntity || !strings.Contains(rejected.Body.String(), "identity_conflict") {
		t.Fatalf("identity conflict status=%d body=%s", rejected.Code, rejected.Body.String())
	}
}
