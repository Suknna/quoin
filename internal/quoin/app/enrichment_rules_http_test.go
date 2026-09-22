package app_test

// 富化规则 HTTP 回归（2026-09-21 实机验收）：EnrichmentRuleBody 一旦被改回
// 未导出类型，Huma 对匿名嵌入类型的 schema 生成会静默跳过其字段，请求体
// schema 只剩 ruleKey，页面的任何创建/更新都 422 "unexpected property" 且
// 文案是通用"请求字段不满足要求"。这里用页面的真实 payload 断言 201，并
// 保留严格性断言（未知字段仍 422 且带 fieldErrors）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/test/support"
)

func newEnrichmentHTTPServer(t *testing.T) (*httptest.Server, map[string]string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.example.com",
		DataDirectory:             filepath.Join(root, "data"),
		BackupDirectory:           filepath.Join(root, "backup"),
		RootKeyFile:               filepath.Join(secrets, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(secrets, "runtime-tls.key"),
		RuntimeClientCAFile:       filepath.Join(secrets, "stele-service-token"),
	}
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	initial, err := auth.GenerateInitialPassword()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureBootstrapAdmin(ctx, initial, time.Now().UTC().Add(auth.InitialPasswordLifetime)); err != nil {
		t.Fatal(err)
	}
	seedAlertSource(t, database.SQL, "mall-shop-alertmanager")
	server := httptest.NewServer(mustHandler(t, service, database.SQL, database.Reader, config.PublicOrigin, config.RootKeyFile))
	t.Cleanup(server.Close)

	origin := map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json"}
	login := mustPost(t, server, origin, `/api/v1/auth/login`, `{"username":"admin","password":"`+initial+`"}`, http.StatusOK)
	cookie := splitCookie(login.headers.Values("Set-Cookie")[0])
	password := "Correct horse battery staple 2026!"
	sessionOrigin := merge(origin, map[string]string{"Cookie": cookie})
	mustDo(t, server, http.MethodPut, sessionOrigin, `/api/v1/auth/password`, `{"currentPassword":"`+initial+`","newPassword":"`+password+`"}`, http.StatusNoContent)
	formal := mustPost(t, server, origin, `/api/v1/auth/login`, `{"username":"admin","password":"`+password+`"}`, http.StatusOK)
	formalCookie := splitCookie(formal.headers.Values("Set-Cookie")[0])
	return server, merge(origin, map[string]string{"Cookie": formalCookie})
}

func seedAlertSource(t *testing.T, db *sql.DB, sourceKey string) {
	t.Helper()
	now := "2026-09-21T00:00:00Z"
	if _, err := db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES(?,'alertmanager',1,?)`, sourceKey, now); err != nil {
		t.Fatal(err)
	}
}

func TestEnrichmentRuleCreateAcceptsPagePayload(t *testing.T) {
	server, sessionOrigin := newEnrichmentHTTPServer(t)

	// 与页面对话框发出的字段一字不差（含空 description 与 alertSourceKeys）。
	payload := `{
		"clientCommandId": "0123456789abcdef0123456789abcdef",
		"ruleKey": "acceptance-mall-owner",
		"displayName": "商城验收归属标注",
		"description": "",
		"enabled": true,
		"labelConditions": {"system_id": "mall-shop"},
		"alertSourceKeys": ["mall-shop-alertmanager"],
		"outputs": {"team": "mall-ops", "environment": "acceptance"},
		"priority": 100
	}`
	created := mustPost(t, server, sessionOrigin, `/api/v1/enrichment-rules`, payload, http.StatusCreated)
	var rule struct {
		RuleKey         string            `json:"ruleKey"`
		Outputs         map[string]string `json:"outputs"`
		Priority        int               `json:"priority"`
		RowVersion      int64             `json:"rowVersion"`
		Enabled         bool              `json:"enabled"`
		AlertSourceKeys []string          `json:"alertSourceKeys"`
	}
	if err := json.Unmarshal([]byte(created.body), &rule); err != nil {
		t.Fatal(err)
	}
	if rule.RuleKey != "acceptance-mall-owner" || !rule.Enabled || rule.Priority != 100 ||
		rule.Outputs["team"] != "mall-ops" || rule.Outputs["environment"] != "acceptance" ||
		len(rule.AlertSourceKeys) != 1 || rule.AlertSourceKeys[0] != "mall-shop-alertmanager" {
		t.Fatalf("created rule = %s", created.body)
	}

	// 严格性不回退：未知字段仍 422，且带 frozen ErrorModel 的 fieldErrors。
	unknown := mustPost(t, server, sessionOrigin, `/api/v1/enrichment-rules`,
		`{"clientCommandId":"0123456789abcdef0123456789abcdef","ruleKey":"x-team","displayName":"x","description":"","enabled":true,"labelConditions":{},"outputs":{"team":"x"},"priority":1,"ghostField":true}`,
		http.StatusUnprocessableEntity)
	assertFrozenProblem(t, unknown, "validation_failed")
}
