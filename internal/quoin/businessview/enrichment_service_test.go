package businessview

// 富化规则命令面测试（ADR-0012）：创建/更新/读取投影、确定性校验拒绝、
// key 不可改写且永不删除（退役 = enabled=0）、row_version 并发前提，以及
// 命令台账/审计经共享执行器自动持久化。

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

func newEnrichmentHarness(t *testing.T) (*EnrichmentService, *sql.DB) {
	t.Helper()
	db, reader := newTestSQLDB(t)
	now := "2026-09-13T00:00:00Z"
	idle, absolute := "2036-09-13T00:00:00Z", "2036-09-20T00:00:00Z"
	for _, statement := range []string{
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'fixture',1,'` + now + `','` + now + `')`,
		`INSERT INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,zeroblob(12),zeroblob(16),'` + now + `')`,
		`INSERT INTO alert_sources(id,source_key,protocol,enabled,created_at) VALUES(1,'am-prod','alertmanager',1,'` + now + `')`,
		`INSERT INTO alert_sources(id,source_key,protocol,enabled,row_version,created_at,disabled_at) VALUES(2,'am-disabled','alertmanager',0,2,'` + now + `','` + now + `')`,
		`INSERT INTO alert_source_credentials(id,source_id,digest,state,created_at) VALUES(1,1,randomblob(32),'Active','` + now + `')`,
		`INSERT INTO alert_source_credentials(id,source_id,digest,state,created_at) VALUES(2,2,randomblob(32),'Active','` + now + `')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,randomblob(32),1,'fixture','` + now + `','` + now + `','` + idle + `','` + absolute + `')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	service, err := NewEnrichmentServiceWithReader(reader, runner)
	if err != nil {
		t.Fatal(err)
	}
	return service, db
}

func enrichmentCtx(t *testing.T) context.Context {
	t.Helper()
	return sessionContext(t, 1, execution.SessionRef{ID: 1, AuthRevision: 1}, "enrichment-"+t.Name())
}

func TestEnrichmentRuleCreateUpdateRead(t *testing.T) {
	service, _ := newEnrichmentHarness(t)
	ctx := enrichmentCtx(t)
	rule, err := service.CreateEnrichmentRule(ctx, 1, "create-rule-0001", EnrichmentRuleInput{
		RuleKey:         "db-team",
		DisplayName:     "DB 值班富化",
		Description:     "数据库告警统一值班字段",
		Enabled:         true,
		LabelConditions: map[string]string{"service": "mysql"},
		AlertSourceKeys: []string{"am-prod"},
		Outputs:         map[string]string{"team": "db", "service": "mysql"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rule.Enabled || rule.Priority != 100 || rule.Outputs["team"] != "db" || rule.RowVersion != 1 {
		t.Fatalf("created rule = %+v", rule)
	}
	// key 退役不复用。
	if _, err := service.CreateEnrichmentRule(ctx, 1, "create-rule-0002", EnrichmentRuleInput{
		RuleKey: "db-team", DisplayName: "重复", Outputs: map[string]string{"team": "x"},
	}); err == nil {
		t.Fatal("duplicate key must be rejected")
	}

	updated, err := service.UpdateEnrichmentRule(ctx, 1, "update-rule-0001", EnrichmentRuleInput{
		RuleKey:     "db-team",
		DisplayName: "DB 值班富化 v2",
		Enabled:     false,
		Outputs:     map[string]string{"team": "dba"},
		Priority:    50,
	}, rule.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Enabled || updated.Priority != 50 || updated.Outputs["team"] != "dba" || updated.RowVersion != 2 {
		t.Fatalf("updated rule = %+v", updated)
	}

	// 陈旧版本号确定性冲突。
	if _, err := service.UpdateEnrichmentRule(ctx, 1, "update-rule-0002", EnrichmentRuleInput{
		RuleKey: "db-team", DisplayName: "DB 值班富化 v3", Outputs: map[string]string{"team": "dba"},
	}, rule.RowVersion); err == nil {
		t.Fatal("stale row_version must conflict")
	}

	list, err := service.ListEnrichmentRules(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	fetched, err := service.GetEnrichmentRule(ctx, "db-team")
	if err != nil || fetched.RuleKey != "db-team" || fetched.Enabled {
		t.Fatalf("get = %+v err=%v", fetched, err)
	}
	if _, err := service.GetEnrichmentRule(ctx, "ghost"); err == nil {
		t.Fatal("unknown key must be not_found")
	}
}

// 校验拒绝：词表外 key、空显示名、空值条件、未知/停用来源、空/空串 outputs、
// 非法 priority。
func TestEnrichmentRuleValidationRejections(t *testing.T) {
	service, _ := newEnrichmentHarness(t)
	ctx := enrichmentCtx(t)
	cases := []struct {
		name  string
		input EnrichmentRuleInput
	}{
		{"bad key", EnrichmentRuleInput{RuleKey: "Bad_Key", DisplayName: "x", Outputs: map[string]string{"a": "b"}}},
		{"empty display", EnrichmentRuleInput{RuleKey: "ok-key", DisplayName: "", Outputs: map[string]string{"a": "b"}}},
		{"empty condition value", EnrichmentRuleInput{RuleKey: "ok-key", DisplayName: "x", LabelConditions: map[string]string{"env": ""}, Outputs: map[string]string{"a": "b"}}},
		{"unknown source", EnrichmentRuleInput{RuleKey: "ok-key", DisplayName: "x", AlertSourceKeys: []string{"ghost"}, Outputs: map[string]string{"a": "b"}}},
		{"disabled source", EnrichmentRuleInput{RuleKey: "ok-key", DisplayName: "x", AlertSourceKeys: []string{"am-disabled"}, Outputs: map[string]string{"a": "b"}}},
		{"empty outputs", EnrichmentRuleInput{RuleKey: "ok-key", DisplayName: "x", Outputs: map[string]string{}}},
		{"blank output value", EnrichmentRuleInput{RuleKey: "ok-key", DisplayName: "x", Outputs: map[string]string{"team": ""}}},
		{"negative priority", EnrichmentRuleInput{RuleKey: "ok-key", DisplayName: "x", Outputs: map[string]string{"a": "b"}, Priority: -1}},
	}
	for _, testCase := range cases {
		if _, err := service.CreateEnrichmentRule(ctx, 1, "reject-"+testCase.name, testCase.input); err == nil {
			t.Fatalf("%s must be rejected", testCase.name)
		} else {
			var rejection *ConflictError
			if !errors.As(err, &rejection) {
				t.Fatalf("%s must map to a deterministic conflict, got %v", testCase.name, err)
			}
		}
	}
}

// key 不可改写、行不可删除（退役 = enabled=0，退役不复用）；命令成功经共享
// 执行器自动持久化台账与审计。
func TestEnrichmentRuleKeyImmutableAndNoDelete(t *testing.T) {
	service, db := newEnrichmentHarness(t)
	ctx := enrichmentCtx(t)
	_, err := service.CreateEnrichmentRule(ctx, 1, "frozen-rule-0001", EnrichmentRuleInput{
		RuleKey: "team-default", DisplayName: "全局默认", Enabled: true, Outputs: map[string]string{"team": "unknown"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE enrichment_rules SET rule_key='renamed' WHERE rule_key='team-default'`); err == nil {
		t.Fatal("rule key must be immutable")
	}
	if _, err := db.Exec(`DELETE FROM enrichment_rules WHERE rule_key='team-default'`); err == nil {
		t.Fatal("enrichment rules must never be deleted")
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM enrichment_rules WHERE rule_key='team-default' AND enabled=1`); got != 1 {
		t.Fatalf("rejected writes must leave the rule intact: %d", got)
	}
	// 退役走 enabled=0 + row_version 前提的整体提交更新。
	retired, err := service.UpdateEnrichmentRule(ctx, 1, "retire-rule-0001", EnrichmentRuleInput{
		RuleKey: "team-default", DisplayName: "全局默认", Enabled: false,
		Outputs: map[string]string{"team": "unknown"},
	}, 1)
	if err != nil || retired.Enabled {
		t.Fatalf("retirement via enabled=0 failed: %+v %v", retired, err)
	}
}
