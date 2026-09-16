package appinspection

// 分析语义字段的 HTTP 契约：计划创建/读取携带可选 checkDescription /
// metricUnit / reportInstructions；超长字段由 schema 约束拒绝；重分析请求
// 携带仅本次 reportInstructions 覆盖。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCreatePlanWithAnalysisSemanticsOverHTTP(t *testing.T) {
	_, db, api := newPlanScopeHarness(t)
	now := "2026-09-16T00:00:00Z"
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'fixture',1,'" + now + "','" + now + "')",
		"INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,zeroblob(32),1,'test','" + now + "','" + now + "','2099-01-01T00:00:00Z','2099-01-01T00:00:00Z')",
		"INSERT INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,zeroblob(12),zeroblob(16),'" + now + "')",
		"INSERT INTO connections(id,name,type,enabled,row_version,created_at) VALUES(1,'mall-prometheus','prometheus',1,1,'" + now + "')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	body := `{
		"clientCommandId": "cmd-semantics-plan-01",
		"planKey": "semantics-plan",
		"displayName": "语义计划",
		"enabled": true,
		"connectionName": "mall-prometheus",
		"pluginId": "prometheus",
		"templateId": "promql_instant",
		"params": {"expression": "up"},
		"scope": {"kind": "integration"},
		"checkDescription": "连通性检查",
		"metricUnit": "1=在线",
		"reportInstructions": "报告需逐检查项给出结论。",
		"timezone": "UTC"
	}`
	response := api.Post("/api/v1/inspections/plans", strings.NewReader(body),
		map[string][]string{"Cookie": {"__Host-quoin-session=test"}, "Content-Type": {"application/json"}})
	if response.Code != http.StatusCreated {
		t.Fatalf("create plan status=%d body=%s", response.Code, response.Body.String())
	}
	readResponse := api.Get("/api/v1/inspections/plans/semantics-plan", "Cookie: __Host-quoin-session=test")
	var payload struct {
		CheckDescription   *string `json:"checkDescription"`
		MetricUnit         *string `json:"metricUnit"`
		ReportInstructions *string `json:"reportInstructions"`
	}
	if err := json.NewDecoder(readResponse.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.CheckDescription == nil || *payload.CheckDescription != "连通性检查" ||
		payload.MetricUnit == nil || *payload.MetricUnit != "1=在线" ||
		payload.ReportInstructions == nil || *payload.ReportInstructions != "报告需逐检查项给出结论。" {
		t.Fatalf("semantics round trip wrong: %+v", payload)
	}
	// 超过长度上界的字段被 schema 约束拒绝（422 请求体验证）。
	tooLong := `{
		"clientCommandId": "cmd-semantics-plan-02",
		"planKey": "over-plan",
		"displayName": "超长计划",
		"enabled": true,
		"connectionName": "mall-prometheus",
		"pluginId": "prometheus",
		"templateId": "promql_instant",
		"params": {"expression": "up"},
		"scope": {"kind": "integration"},
		"metricUnit": "` + strings.Repeat("单", 101) + `",
		"timezone": "UTC"
	}`
	rejected := api.Post("/api/v1/inspections/plans", strings.NewReader(tooLong),
		map[string][]string{"Cookie": {"__Host-quoin-session=test"}, "Content-Type": {"application/json"}})
	if rejected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("over-length unit status=%d body=%s", rejected.Code, rejected.Body.String())
	}
}
