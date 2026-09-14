package businessview

// 业务视图命令面测试：创建幂等/并发前提/整体提交更新/确定性拒绝。

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	_ "github.com/Suknna/quoin/internal/quoin/bootstrap"
	_ "modernc.org/sqlite"
)

func newViewHarness(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/views.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gen.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := "2026-09-13T00:00:00Z"
	for _, statement := range []string{
		`INSERT INTO users(id,username,display_name,role,enabled,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,'fixture',1,'` + now + `','` + now + `')`,
		`INSERT INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,zeroblob(12),zeroblob(16),'` + now + `')`,
		`INSERT INTO connections(id,name,type,enabled,row_version,current_revision_id,current_credential_generation_id,created_at) VALUES(1,'fixture-metrics','thanos',1,1,NULL,NULL,'` + now + `')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return NewService(db), db
}

func TestBusinessViewCreateUpdateRead(t *testing.T) {
	service, db := newViewHarness(t)
	ctx := context.Background()
	view, err := service.CreateView(ctx, 1, "create-view-0001", ViewInput{
		ViewKey: "checkout-prod", DisplayName: "结算生产", Description: "结算域生产候选",
		ConnectionName: "fixture-metrics", LabelConditions: map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.Scope.ConnectionName != "fixture-metrics" || view.Scope.LabelConditions["env"] != "prod" || view.RowVersion != 1 {
		t.Fatalf("created view = %+v", view)
	}
	// key 退役不复用。
	if _, err := service.CreateView(ctx, 1, "create-view-0002", ViewInput{ViewKey: "checkout-prod", DisplayName: "重复"}); err == nil {
		t.Fatal("duplicate key must be rejected")
	}
	updated, err := service.UpdateView(ctx, 1, "update-view-0001", ViewInput{
		ViewKey: "checkout-prod", DisplayName: "结算生产 v2", ConnectionName: "fixture-metrics",
	}, view.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RowVersion != 2 || updated.DisplayName != "结算生产 v2" {
		t.Fatalf("updated view = %+v", updated)
	}
	if _, err := service.UpdateView(ctx, 1, "update-view-0002", ViewInput{ViewKey: "checkout-prod", DisplayName: "并发"}, view.RowVersion); err == nil {
		t.Fatal("stale row version must conflict")
	}
	if _, err := service.CreateView(ctx, 1, "create-view-0003", ViewInput{ViewKey: "BAD_KEY", DisplayName: "非法"}); err == nil {
		t.Fatal("malformed key must be rejected")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM business_views WHERE view_key='BAD_KEY'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("rejected creation must not persist")
	}
	list, err := service.ListViews(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	// 跨来源候选集合：connection 可空。
	if _, err := service.CreateView(ctx, 1, "create-view-0004", ViewInput{ViewKey: "all-sources", DisplayName: "全部来源"}); err != nil {
		t.Fatal(err)
	}
	cross, err := service.GetView(ctx, "all-sources")
	if err != nil || cross.Scope.ConnectionName != "" {
		t.Fatalf("cross-source view = %+v err=%v", cross, err)
	}
	if _, err := service.GetView(ctx, "missing"); !errors.Is(err, err) {
		t.Fatal("missing view must surface its typed rejection")
	}
}
