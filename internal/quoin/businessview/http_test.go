package businessview

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/danielgtaylor/huma/v2/humatest"
)

func TestViewHTTPUsesDeclaredNestedScope(t *testing.T) {
	service, db := newViewHarness(t)
	defer db.Close()
	_, api := humatest.New(t)
	(&Handler{Views: service, Authenticate: func(context.Context, string) (int64, error) { return 1, nil }}).Register(api)
	created := api.Post("/api/v1/business-views", "Content-Type: application/json", strings.NewReader(`{"clientCommandId":"http-create-view","viewKey":"lab-services","displayName":"实验服务","description":"","scope":{"labelConditions":{"job":"mysql"}}}`))
	if created.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", created.Code, created.Body.String())
	}
	view, err := service.GetView(context.Background(), "lab-services")
	if err != nil {
		t.Fatal(err)
	}
	if view.Scope.LabelConditions["job"] != "mysql" {
		t.Fatalf("scope lost: %+v", view.Scope)
	}
}

// 回归：更新请求体由匿名嵌入 ViewBody 与 ExpectedRowVersion 组成。ViewBody
// 一旦失去导出（重命名回小写），Huma 会把嵌入字段从请求 schema 中整体丢弃，
// 封闭校验随即用 422 拒绝前端完全合法的载荷（编辑保存不可用的真实故障）。
func TestViewHTTPUpdateAcceptsEditorPayloadAndAdvancesRowVersion(t *testing.T) {
	service, db := newViewHarness(t)
	defer db.Close()
	_, api := humatest.New(t)
	(&Handler{Views: service, Authenticate: func(context.Context, string) (int64, error) { return 1, nil }}).Register(api)

	// 真实 UI 创建动作：key=mall-mysql-view，scope 指向 mall-prometheus 与 job=mall-mysql-exporter。
	created := api.Post("/api/v1/business-views", "Content-Type: application/json", strings.NewReader(`{"clientCommandId":"ui-create-mall-mysql","viewKey":"mall-mysql-view","displayName":"商城MySQL监控","description":"","scope":{"connectionName":"fixture-metrics","labelConditions":{"job":"mall-mysql-exporter"}}}`))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}

	// 更新前 schema 必须展开嵌入字段，否则前端载荷会被逐字段拒绝。
	schemaRef := api.OpenAPI().Paths["/api/v1/business-views/{viewKey}"].Put.RequestBody.Content["application/json"].Schema
	updateSchema, ok := api.OpenAPI().Components.Schemas.Map()[strings.TrimPrefix(schemaRef.Ref, "#/components/schemas/")]
	if !ok {
		t.Fatalf("missing update body schema %s", schemaRef.Ref)
	}
	for _, name := range []string{"clientCommandId", "displayName", "description", "scope", "expectedRowVersion"} {
		if _, present := updateSchema.Properties[name]; !present {
			t.Fatalf("update body schema lost property %q: required=%v", name, updateSchema.Required)
		}
	}
	for _, name := range updateSchema.Required {
		if name == "viewKey" {
			t.Fatal("update body must not require viewKey; the path is the only key")
		}
	}

	// 编辑表单保存：只把 job 改为 mall-redis-exporter，载荷与 web 端
	// updateBusinessView 产出的 wire 完全一致（不含 viewKey）。
	updated := api.Put("/api/v1/business-views/mall-mysql-view", "Content-Type: application/json", strings.NewReader(`{"clientCommandId":"ui-update-mall-mysql","displayName":"商城MySQL监控","description":"","scope":{"connectionName":"fixture-metrics","labelConditions":{"job":"mall-redis-exporter"}},"expectedRowVersion":1}`))
	if updated.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updated.Code, updated.Body.String())
	}

	// GET 必须读到 rowVersion 2 与新条件，证明写路径真实落库。
	read := api.Get("/api/v1/business-views/mall-mysql-view")
	if read.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", read.Code, read.Body.String())
	}
	var body struct {
		ViewKey    string    `json:"viewKey"`
		Scope      ViewScope `json:"scope"`
		RowVersion int64     `json:"rowVersion"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ViewKey != "mall-mysql-view" || body.RowVersion != 2 || body.Scope.LabelConditions["job"] != "mall-redis-exporter" || body.Scope.ConnectionName != "fixture-metrics" {
		t.Fatalf("view after update = %+v", body)
	}
}

func TestViewHTTPUnauthenticatedIsNotForbidden(t *testing.T) {
	_, api := humatest.New(t)
	(&Handler{Authenticate: func(context.Context, string) (int64, error) { return 0, auth.ErrUnauthenticated }}).Register(api)
	response := api.Get("/api/v1/business-views")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
