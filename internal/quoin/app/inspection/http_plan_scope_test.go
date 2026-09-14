package appinspection

// 真实 HTTP→domain 复现：UI 按业务视图创建巡检计划（scope.type=businessView，
// templateVersion 省略）必须走通 CreatePlan 全路径。此测试复现生产 500 并锁定
// wire→storage 归一化契约。

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/inspection"
	_ "github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/danielgtaylor/huma/v2/humatest"
	_ "modernc.org/sqlite"
)

func newPlanScopeHarness(t *testing.T) (*Handler, *sql.DB, humatest.TestAPI) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/plans.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gen.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	_, api := humatest.New(t)
	h := &Handler{Inspections: inspection.NewService(db), Authenticate: func(context.Context, string) (int64, error) { return 1, nil }}
	h.Register(api)
	return h, db, api
}

func TestCreatePlanWithBusinessViewScopeOverHTTP(t *testing.T) {
	handler, db, api := newPlanScopeHarness(t)
	handler.Authenticate = func(context.Context, string) (int64, error) { return 1, nil }

	// 真实前置事实：接入 + 业务视图（跨来源候选，connection_id NULL 合法）。
	now := "2026-09-13T00:00:00Z"
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,role,enabled,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,'fixture',1,'" + now + "','" + now + "')",
		"INSERT INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,zeroblob(12),zeroblob(16),'" + now + "')",
		"INSERT INTO connections(id,name,type,enabled,row_version,created_at) VALUES(1,'mall-prometheus','prometheus',1,1,'" + now + "')",
		"INSERT INTO business_views(view_key,display_name,description,connection_id,label_conditions_json,row_version,created_by,created_at,updated_at) VALUES('mall-mysql-view','商城 MySQL','desc',1,'{\"env\":\"prod\"}',1,1,'" + now + "','" + now + "')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}

	// UI 实际载荷：scope.type=businessView，templateVersion 省略。
	body := `{
		"clientCommandId": "cmd-mall-plan-0001",
		"planKey": "mall-mysql-inspection",
		"displayName": "商城 MySQL 范围巡检",
		"enabled": true,
		"connectionName": "mall-prometheus",
		"pluginId": "prometheus",
		"templateId": "promql_instant",
		"params": {"expression": "up"},
		"scope": {"kind": "businessView", "businessViewKey": "mall-mysql-view"},
		"timezone": "Asia/Shanghai"
	}`
	response := api.Post("/api/v1/inspections/plans", strings.NewReader(body),
		map[string][]string{"Cookie": {"__Host-quoin-session=test"}, "Content-Type": {"application/json"}})
	var problem struct {
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
		Code   string `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatalf("status=%d body not JSON: %v", response.Code, err)
	}
	if response.Code != http.StatusCreated {
		t.Fatalf("create plan status=%d code=%q detail=%q", response.Code, problem.Code, problem.Detail)
	}
	// wire scope.kind 必须归一为 storage business_view 并可读回。
	readResponse := api.Get("/api/v1/inspections/plans/mall-mysql-inspection",
		map[string][]string{"Cookie": {"__Host-quoin-session=test"}})
	var payload struct {
		PlanKey string `json:"planKey"`
		Scope   struct {
			Kind            string `json:"kind"`
			BusinessViewKey string `json:"businessViewKey"`
		} `json:"scope"`
		TemplateVersion *string `json:"templateVersion"`
	}
	if err := json.NewDecoder(readResponse.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.PlanKey != "mall-mysql-inspection" || payload.Scope.Kind != "businessView" || payload.Scope.BusinessViewKey != "mall-mysql-view" {
		t.Fatalf("stored/read scope wrong: %+v", payload)
	}
	if payload.TemplateVersion != nil {
		t.Fatalf("omitted templateVersion must stay nil, got %q", *payload.TemplateVersion)
	}
}
