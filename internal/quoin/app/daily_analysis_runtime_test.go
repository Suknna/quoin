package app

// daily_report_get 执行器行为测试（ADR-0014）：授权是冻结身份的逐字比对——
// 只有 Attempt 创建时冻结的 (configKey, localDate, version) 可读；越界定位
// 符、畸形参数与非日报 Attempt 一律确定性失败。执行只读已提交的不可变版本
// 行：同一输入重复调用返回同一载荷，绝不触发任何平台查询。

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	_ "github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

func newDailyReportToolTestDB(t *testing.T) (*sql.DB, audit.Reader) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/daily-tool.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gen.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	// 根密钥绑定行与管理员（qualification.created_by 引用）：credential
	// 围栏与启用链引用它们。
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var file string
	if err := db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&file); err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReadOnly(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if _, err := db.Exec(`INSERT INTO root_key_state(id, binding_revision, verifier_nonce, verifier_ciphertext, bound_at) VALUES (1, 1, zeroblob(12), zeroblob(16), ?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at)
		VALUES (1,'admin','Admin','admin',1,1,'$argon2id$fixture',1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	return db, reader
}

func seedDailyReportToolFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	content := `{"schemaKind":"inspection_daily_report_v1","configKey":"core-daily","localDate":"2026-09-27","sources":[{"planKey":"plan-a","status":"gap"}],"totals":{"checksOk":0,"checksGap":1,"checksError":0,"sourcesGap":1}}`
	mustExec(t, db, `INSERT INTO inspection_daily_report_configs(id,config_key,display_name,enabled,timezone,trigger_time,plan_keys_json,row_version,created_at,updated_at)
		VALUES(1,'core-daily','核心日报',1,'UTC','06:00','["plan-a"]',1,?,?)`, now, now)
	mustExec(t, db, `INSERT INTO inspection_daily_reports(id,config_id,config_key,config_row_version,local_date,timezone,window_start_utc,window_end_utc,trigger_kind,scheduled_for,cutoff_at,contributions_json,state,sealed_at,row_version,created_at)
		VALUES(1,1,'core-daily',1,'2026-09-27','UTC','2026-09-27T00:00:00Z','2026-09-28T00:00:00Z','manual',NULL,?,'[]','Sealed',?,1,?)`,
		now, now, now)
	mustExec(t, db, `INSERT INTO inspection_daily_report_versions(report_id,version,content,created_at) VALUES(1,2,?,?)`, content, now)
	// chat_model grant（dispatch_ready 围栏要求）：最小合格探测链。
	mustExec(t, db, `INSERT INTO connections(id,name,type,enabled,created_at) VALUES(1,'model','model_provider',0,?)`, now)
	mustExec(t, db, `INSERT INTO connection_revisions(id,connection_id,revision_seq,config_json,created_at) VALUES(1,1,1,'{"chatModelId":"fixture-chat-1","contextBudgetTokens":4096,"maxOutputTokens":1024}',?)`, now)
	mustExec(t, db, `INSERT INTO credential_generations(id,connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(1,1,1,1,1,?,?,?)`,
		[]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, []byte(strings.Repeat("f", 32)), now)
	mustExec(t, db, `UPDATE connections SET current_revision_id=1, current_credential_generation_id=1, row_version=row_version+1 WHERE id=1`)
	// 非日报 Attempt（无冻结日报身份）：探测 Attempt 承载合格探测链；探测
	// purpose 的 grant 先冻结，Assigned 后探测结果才允许闭合，最后探测成功。
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES(2,'connection_probe','connection',1,'Queued','tool-test',NULL,?)`, now)
	mustExec(t, db, `INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(2,'model_probe_chat',1,1,1,?)`, now)
	mustExec(t, db, `INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(2,'model_probe_embedding',1,1,1,?)`, now)
	// 派发围栏第一条：探测 Attempt 同样需要可重建输入谱系（快照 + 定位项）。
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,created_at)
		VALUES(2,2,'connection_probe_v1','connection-probe-v1',?,?)`, strings.Repeat("0", 64), now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id)
		VALUES(2,1,'user',?,1)`, strings.Repeat("0", 64))
	mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='tool-test',row_version=row_version+1 WHERE id=2`, now)
	mustExec(t, db, `UPDATE execution_attempts SET state='Running',accepted_at=?,row_version=row_version+1 WHERE id=2`, now)
	mustExec(t, db, `INSERT INTO connection_probe_results(id,attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at)
		VALUES(1,2,1,'model_provider',1,1,1,'model-provider-capabilities',1,?,'passed',?,?,?,?)`,
		strings.Repeat("0", 64), strings.Repeat("1", 64), now, now, now)
	// chat_model purpose 要求连接真实启用合格：真实探测调用（chat 成功 + chat
	// 取消；embedding_supported=0 免去 embedding 调用）+ 能力行 + 显式
	// qualification + 启用转换（与 seedModelProvider 同一事实序列）。
	mustExec(t, db, `INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,status,started_at)
		VALUES(2,1,0,'chat','fixture-chat-1',(SELECT id FROM attempt_connection_grants WHERE attempt_id=2 AND purpose='model_probe_chat'),'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,'running',?)`,
		strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), now)
	mustExec(t, db, `INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at)
		VALUES((SELECT id FROM model_calls WHERE attempt_id=2 AND call_seq=1),1,'{"assistantText":"ok","finishReason":"stop","tool_calls":[]}',?, 'stop',?)`, strings.Repeat("1", 64), now)
	mustExec(t, db, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind)
		VALUES((SELECT id FROM model_calls WHERE attempt_id=2 AND call_seq=1),1,'system',?,'system_contract'),
		      ((SELECT id FROM model_calls WHERE attempt_id=2 AND call_seq=1),2,'system',?,'tool_schema')`,
		strings.Repeat("1", 64), strings.Repeat("1", 64))
	mustExec(t, db, `UPDATE model_calls SET status='succeeded',usage_json='{}',ended_at=? WHERE attempt_id=2 AND call_seq=1`, now)
	mustExec(t, db, `INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,status,started_at)
		VALUES(2,2,0,'chat','fixture-chat-1',(SELECT id FROM attempt_connection_grants WHERE attempt_id=2 AND purpose='model_probe_chat'),'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,'running',?)`,
		strings.Repeat("2", 64), strings.Repeat("2", 64), strings.Repeat("2", 64), strings.Repeat("2", 64), strings.Repeat("2", 64), now)
	mustExec(t, db, `UPDATE model_calls SET status='cancelled',termination_reason='cancelled',ended_at=? WHERE attempt_id=2 AND call_seq=2`, now)
	mustExec(t, db, `INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,detail_json)
		VALUES(1,'fixture-chat-1',4096,1024,1,1,1,1,1,1,0,'{}')`)
	mustExec(t, db, `INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(1,3,1,1,?)`, now)
	mustExec(t, db, `UPDATE connections SET enabled=1,revalidation_required=0,row_version=row_version+1 WHERE id=1 AND row_version=2`)
	// 日报 Attempt 先以 Queued 建档（快照闭包要求），授权齐备后按合法状态机
	// Queued → Assigned → Running。
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES(1,'inspection_daily_analysis','daily_report',1,'Queued','tool-test','inspection-daily-analysis-v1',?)`, now)
	mustExec(t, db, `INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		VALUES(1,'chat_model',1,1,1,1,?)`, now)
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,inspection_report_version,created_at)
		VALUES(1,1,'inspection_daily_analysis_v1','v1',?,2,?)`, strings.Repeat("0", 64), now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,inspection_daily_report_id)
		VALUES(1,1,'daily_report',?,1)`, strings.Repeat("0", 64))
	mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='tool-test',row_version=row_version+1 WHERE id=1`, now)
	mustExec(t, db, `UPDATE execution_attempts SET state='Running',accepted_at=?,row_version=row_version+1 WHERE id=1`, now)
}

func dailyToolLoaded(attemptID int64, arguments string) *routedToolContext {
	return &routedToolContext{
		attemptID: attemptID, toolCallID: 5, toolName: "daily_report_get", failureMode: "return_to_model",
		resultSchemaKind: "daily_report_get_result_v1", arguments: json.RawMessage(arguments),
		bootID: "boot", epoch: 1,
	}
}

func TestDailyReportGetToolExecutorEnforcesFrozenLocator(t *testing.T) {
	db, reader := newDailyReportToolTestDB(t)
	seedDailyReportToolFixture(t, db)
	attempts := attempt.NewService(db)
	if err := attempts.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	service := &RuntimeService{}
	ctx := context.Background()

	// 冻结定位符逐字命中：返回封存版本文档原文，truncated=false。
	seal := service.invokeDailyReportGetTool(ctx, attempts, dailyToolLoaded(1, `{"configKey":"core-daily","localDate":"2026-09-27","version":2}`))
	if seal.outcome != "succeeded" {
		t.Fatalf("frozen locator must succeed, got %s/%s/%s", seal.outcome, seal.errorCode, seal.errorDetail)
	}
	var payload map[string]any
	if err := json.Unmarshal(seal.canonical, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["truncated"] != false || payload["configKey"] != "core-daily" || payload["localDate"] != "2026-09-27" {
		t.Fatalf("payload header = %+v", payload)
	}
	report, isMap := payload["report"].(map[string]any)
	if !isMap || report["schemaKind"] != "inspection_daily_report_v1" || report["localDate"] != "2026-09-27" {
		t.Fatalf("sealed document = %+v", payload["report"])
	}
	// 幂等：同一冻结输入重复执行返回同一载荷（只读已提交行，无任何副作用）。
	replay := service.invokeDailyReportGetTool(ctx, attempts, dailyToolLoaded(1, `{"configKey":"core-daily","localDate":"2026-09-27","version":2}`))
	if string(replay.canonical) != string(seal.canonical) {
		t.Fatal("replayed execution must return the identical frozen document")
	}

	// 越界定位符确定性失败：只读冻结身份，不给相邻版本/配置开口子。
	for name, arguments := range map[string]string{
		"wrong version":   `{"configKey":"core-daily","localDate":"2026-09-27","version":1}`,
		"wrong configKey": `{"configKey":"other-daily","localDate":"2026-09-27","version":2}`,
		"wrong localDate": `{"configKey":"core-daily","localDate":"2026-09-26","version":2}`,
	} {
		seal = service.invokeDailyReportGetTool(ctx, attempts, dailyToolLoaded(1, arguments))
		if seal.outcome != "failed" || seal.errorCode != "forbidden_locator" {
			t.Fatalf("%s: seal = %s/%s, want failed/forbidden_locator", name, seal.outcome, seal.errorCode)
		}
		if !strings.Contains(seal.errorDetail, "frozen daily report identity") {
			t.Fatalf("%s: detail = %q", name, seal.errorDetail)
		}
	}
	// 畸形参数与缺失字段确定性失败。
	for name, arguments := range map[string]string{
		"unknown field":   `{"configKey":"core-daily","localDate":"2026-09-27","version":2,"query":"up"}`,
		"bad date shape":  `{"configKey":"core-daily","localDate":"2026-9-7","version":2}`,
		"zero version":    `{"configKey":"core-daily","localDate":"2026-09-27","version":0}`,
		"missing version": `{"configKey":"core-daily","localDate":"2026-09-27"}`,
		"unparseable":     `{not-json`,
	} {
		seal = service.invokeDailyReportGetTool(ctx, attempts, dailyToolLoaded(1, arguments))
		if seal.outcome != "failed" || seal.errorCode != "invalid_arguments" {
			t.Fatalf("%s: seal = %s/%s, want failed/invalid_arguments", name, seal.outcome, seal.errorCode)
		}
	}
	// 非日报 Attempt：无冻结身份，直接拒绝。
	seal = service.invokeDailyReportGetTool(ctx, attempts, dailyToolLoaded(2, `{"configKey":"core-daily","localDate":"2026-09-27","version":2}`))
	if seal.outcome != "failed" || seal.errorCode != "not_daily_analysis" {
		t.Fatalf("non-daily attempt seal = %s/%s, want failed/not_daily_analysis", seal.outcome, seal.errorCode)
	}
}

func TestDailyReportGetToolExecutorTruncatesOversizedDocument(t *testing.T) {
	db, reader := newDailyReportToolTestDB(t)
	seedDailyReportToolFixture(t, db)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	huge := `{"schemaKind":"inspection_daily_report_v1","pad":"` + strings.Repeat("x", dailyReportToolBodyBound+4096) + `"}`
	mustExec(t, db, `INSERT INTO inspection_daily_reports(id,config_id,config_key,config_row_version,local_date,timezone,window_start_utc,window_end_utc,trigger_kind,scheduled_for,cutoff_at,contributions_json,state,sealed_at,row_version,created_at)
		VALUES(2,1,'big-daily',1,'2026-09-20','UTC','2026-09-20T00:00:00Z','2026-09-21T00:00:00Z','manual',NULL,?,'[]','Sealed',?,1,?)`,
		now, now, now)
	mustExec(t, db, `INSERT INTO inspection_daily_report_versions(report_id,version,content,created_at) VALUES(2,1,?,?)`, huge, now)
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES(3,'inspection_daily_analysis','daily_report',2,'Queued','tool-test','inspection-daily-analysis-v1',?)`, now)
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,inspection_report_version,created_at)
		VALUES(4,3,'inspection_daily_analysis_v1','v1',?,1,?)`, strings.Repeat("0", 64), now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,inspection_daily_report_id)
		VALUES(4,1,'daily_report',?,2)`, strings.Repeat("0", 64))
	mustExec(t, db, `INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		VALUES(3,'chat_model',1,1,1,1,?)`, now)
	mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='tool-test',row_version=row_version+1 WHERE id=3`, now)
	mustExec(t, db, `UPDATE execution_attempts SET state='Running',accepted_at=?,row_version=row_version+1 WHERE id=3`, now)

	attempts := attempt.NewService(db)
	if err := attempts.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	service := &RuntimeService{}
	seal := service.invokeDailyReportGetTool(context.Background(), attempts, dailyToolLoaded(3, `{"configKey":"big-daily","localDate":"2026-09-20","version":1}`))
	if seal.outcome != "succeeded" {
		t.Fatalf("oversized document seal = %s/%s", seal.outcome, seal.errorCode)
	}
	var payload map[string]any
	if err := json.Unmarshal(seal.canonical, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["truncated"] != true {
		t.Fatalf("oversized document must be marked truncated: %+v", payload)
	}
	preview, isString := payload["reportPreview"].(string)
	if !isString || len(preview) != dailyReportToolBodyBound {
		t.Fatalf("preview length = %d, want the fixed bound", len(preview))
	}
	if _, hasReport := payload["report"]; hasReport {
		t.Fatal("truncated payload must not carry the full document")
	}
}
