package app

// daily_report_get 执行器行为测试（ADR-0014）：授权是冻结身份的逐字比对——
// 只有 Attempt 创建时冻结的 (configKey, localDate, version) 可读；越界定位
// 符、畸形参数与非日报 Attempt 一律确定性失败。执行只读已提交的不可变版本
// 行：同一输入重复调用返回同一载荷，绝不触发任何平台查询。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	sealedContent, err := json.Marshal(map[string]any{
		"schemaKind": "inspection_daily_report_v1", "configKey": "core-daily", "localDate": "2026-09-27",
		"timezone": "UTC", "windowStartUtc": "2026-09-27T00:00:00Z", "windowEndUtc": "2026-09-28T00:00:00Z",
		"sealedAt": "2026-09-28T08:00:00Z",
		"sources": []map[string]any{{
			"planKey": "plan-a", "status": "gap",
			"checks": []map[string]any{{
				"runId": 7, "checkKey": "latency", "status": "ok",
				"observedAt": "2026-09-27T12:00:00Z", "evidenceId": 11,
				"measurement": map[string]any{
					"resultType": "vector", "series": 2, "samples": 2,
					"firstValue": "0.4", "lastValue": "9.9", "lastAt": "1790000000",
					"entries": []map[string]any{
						{
							"labels": map[string]string{"instance": "a", "job": "quoin"}, "samples": 1,
							"firstValue": "0.4", "lastValue": "0.4", "lastAt": "1790000000",
							"minValue": "0.4", "minAt": "1790000000", "maxValue": "0.4", "maxAt": "1790000000",
						},
						{
							"labels": map[string]string{"instance": "b"}, "samples": 1,
							"firstValue": "9.9", "lastValue": "9.9", "lastAt": "1790000001",
							"maxValue": "9.9", "maxAt": "1790000001",
						},
					},
				},
			}},
		}},
		"totals": map[string]any{"checksOk": 0, "checksGap": 1, "checksError": 0, "sourcesGap": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := string(sealedContent)
	mustExec(t, db, `INSERT INTO inspection_daily_report_configs(id,config_key,display_name,enabled,timezone,trigger_time,plan_keys_json,row_version,created_at,updated_at)
		VALUES(1,'core-daily','核心日报',1,'UTC','06:00','["plan-a"]',1,?,?)`, now, now)
	mustExec(t, db, `INSERT INTO inspection_daily_reports(id,config_id,config_key,config_row_version,local_date,timezone,window_start_utc,window_end_utc,trigger_kind,scheduled_for,cutoff_at,contributions_json,state,sealed_at,row_version,created_at)
		VALUES(1,1,'core-daily',1,'2026-09-27','UTC','2026-09-27T00:00:00Z','2026-09-28T00:00:00Z','manual',NULL,'2026-09-28T06:00:00Z','[]','Sealed',?,1,?)`,
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
		VALUES(1,'inspection_daily_analysis','daily_report',1,'Queued','tool-test','inspection-daily-analysis-v2',?)`, now)
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

	// 冻结定位符逐字命中：小文档单页返回完整 sources，nextCursor 缺省 = 取全。
	seal := service.invokeDailyReportGetTool(ctx, attempts, dailyToolLoaded(1, `{"configKey":"core-daily","localDate":"2026-09-27","version":2}`))
	if seal.outcome != "succeeded" {
		t.Fatalf("frozen locator must succeed, got %s/%s/%s", seal.outcome, seal.errorCode, seal.errorDetail)
	}
	var payload struct {
		ConfigKey   string            `json:"configKey"`
		LocalDate   string            `json:"localDate"`
		Version     int64             `json:"version"`
		Cursor      string            `json:"cursor"`
		NextCursor  *string           `json:"nextCursor"`
		Sources     []json.RawMessage `json:"sources"`
		WindowStart string            `json:"windowStartUtc"`
		WindowEnd   string            `json:"windowEndUtc"`
		SchemaKind  string            `json:"schemaKind"`
	}
	if err := json.Unmarshal(seal.canonical, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ConfigKey != "core-daily" || payload.LocalDate != "2026-09-27" || payload.Version != 2 {
		t.Fatalf("payload locator = %+v", payload)
	}
	if payload.Cursor != "s0:c0" || payload.NextCursor != nil {
		t.Fatalf("single page must be the complete traversal: cursor=%q next=%v", payload.Cursor, payload.NextCursor)
	}
	if payload.SchemaKind != "inspection_daily_report_v1" || len(payload.Sources) != 1 || payload.WindowStart != "2026-09-27T00:00:00Z" {
		t.Fatalf("sealed page envelope = %+v", payload)
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

func TestDailyReportGetToolExecutorPagesOversizedDocument(t *testing.T) {
	db, reader := newDailyReportToolTestDB(t)
	seedDailyReportToolFixture(t, db)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// 构造一个 >64KB 的封存文档：逐序列测量摘要 + 大量来源/检查项，页界必然
	// 触发多页续读。
	document := buildOversizedDailyReportDocument(t, "big-daily", "2026-09-20", 48, 6)
	if len(document) <= dailyReportToolPageBound {
		t.Fatalf("fixture document must exceed the page bound, got %d bytes", len(document))
	}
	mustExec(t, db, `INSERT INTO inspection_daily_reports(id,config_id,config_key,config_row_version,local_date,timezone,window_start_utc,window_end_utc,trigger_kind,scheduled_for,cutoff_at,contributions_json,state,sealed_at,row_version,created_at)
		VALUES(2,1,'big-daily',1,'2026-09-20','UTC','2026-09-20T00:00:00Z','2026-09-21T00:00:00Z','manual',NULL,?,'[]','Sealed',?,1,?)`,
		now, now, now)
	mustExec(t, db, `INSERT INTO inspection_daily_report_versions(report_id,version,content,created_at) VALUES(2,1,?,?)`, document, now)
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES(3,'inspection_daily_analysis','daily_report',2,'Queued','tool-test','inspection-daily-analysis-v2',?)`, now)
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,inspection_report_version,created_at)
		VALUES(4,3,'inspection_daily_analysis_v1','v2',?,1,?)`, strings.Repeat("0", 64), now)
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
	ctx := context.Background()
	// 无网关装配：整个遍历绝不触碰任何平台调用路径（nil 网关下必须全部成功）。
	if service.SteleGateway != nil {
		t.Fatal("fixture must run without a stele gateway to prove read-only retrieval")
	}
	locator := func(cursor string) string {
		arguments := `{"configKey":"big-daily","localDate":"2026-09-20","version":1`
		if cursor != "" {
			arguments += `,"cursor":"` + cursor + `"}`
		} else {
			arguments += `}`
		}
		return arguments
	}
	// 切片续读会把一个 source 拆成多个片段：按 planKey 顺序合并后与原文档
	// 逐字节比对（片段内的检查项字节必须原样保留）。
	type sourceFragment struct {
		PlanKey    string            `json:"planKey"`
		Status     string            `json:"status"`
		GapReasons []string          `json:"gapReasons,omitempty"`
		Checks     []json.RawMessage `json:"checks,omitempty"`
	}
	fragmentsByPlan := map[string]*sourceFragment{}
	var planOrder []string
	cursor := ""
	visited := map[string]bool{}
	pages := 0
	for {
		seal := service.invokeDailyReportGetTool(ctx, attempts, dailyToolLoaded(3, locator(cursor)))
		if seal.outcome != "succeeded" {
			t.Fatalf("page %d seal = %s/%s/%s", pages, seal.outcome, seal.errorCode, seal.errorDetail)
		}
		if len(seal.canonical) > dailyReportToolPageBound+512 {
			t.Fatalf("page %d carries %d bytes, want a bounded page", pages, len(seal.canonical))
		}
		// 同一续读坐标重复调用返回同一页：封存行不可变，分页确定性。
		replay := service.invokeDailyReportGetTool(ctx, attempts, dailyToolLoaded(3, locator(cursor)))
		if replay.outcome != "succeeded" || string(replay.canonical) != string(seal.canonical) {
			t.Fatalf("cursor %q must replay the identical page", cursor)
		}
		var payload struct {
			ConfigKey    string            `json:"configKey"`
			LocalDate    string            `json:"localDate"`
			Version      int64             `json:"version"`
			Cursor       string            `json:"cursor"`
			NextCursor   *string           `json:"nextCursor"`
			Sources      []json.RawMessage `json:"sources"`
			WindowStart  string            `json:"windowStartUtc"`
			WindowEnd    string            `json:"windowEndUtc"`
			SchemaKind   string            `json:"schemaKind"`
			TotalSources json.RawMessage   `json:"totals"`
		}
		if err := json.Unmarshal(seal.canonical, &payload); err != nil {
			t.Fatal(err)
		}
		// 每一页都携带精确不可变定位符与窗口事实。
		if payload.ConfigKey != "big-daily" || payload.LocalDate != "2026-09-20" || payload.Version != 1 {
			t.Fatalf("page %d locator = %s/%s/%d", pages, payload.ConfigKey, payload.LocalDate, payload.Version)
		}
		if payload.WindowStart != "2026-09-20T00:00:00Z" || payload.WindowEnd != "2026-09-21T00:00:00Z" || payload.SchemaKind != "inspection_daily_report_v1" {
			t.Fatalf("page %d envelope = %+v", pages, payload)
		}
		wantCursor := cursor
		if wantCursor == "" {
			wantCursor = "s0:c0"
		}
		if payload.Cursor != wantCursor {
			t.Fatalf("page %d cursor echo = %q, want %q", pages, payload.Cursor, wantCursor)
		}
		if visited[payload.Cursor] {
			t.Fatalf("page %d revisits cursor %q: traversal must not loop", pages, payload.Cursor)
		}
		visited[payload.Cursor] = true
		for _, raw := range payload.Sources {
			var fragment sourceFragment
			if err := json.Unmarshal(raw, &fragment); err != nil {
				t.Fatal(err)
			}
			merged, seen := fragmentsByPlan[fragment.PlanKey]
			if !seen {
				merged = &sourceFragment{PlanKey: fragment.PlanKey, Status: fragment.Status, GapReasons: fragment.GapReasons}
				fragmentsByPlan[fragment.PlanKey] = merged
				planOrder = append(planOrder, fragment.PlanKey)
			}
			merged.Checks = append(merged.Checks, fragment.Checks...)
		}
		pages++
		if pages > 32 {
			t.Fatal("pagination did not terminate within 32 pages")
		}
		if payload.NextCursor == nil {
			break
		}
		cursor = *payload.NextCursor
	}
	if pages < 2 {
		t.Fatalf("oversized document must require multiple calls, got %d page(s)", pages)
	}
	// 完整遍历恰好覆盖原文档的全部 sources：标识与缺口原样、检查项序列逐
	// 字节一致（顺序不变，无丢失、无重复）。
	var original struct {
		Sources []sourceFragment `json:"sources"`
	}
	if err := json.Unmarshal([]byte(document), &original); err != nil {
		t.Fatal(err)
	}
	if len(planOrder) != len(original.Sources) {
		t.Fatalf("paged traversal collected %d sources, want %d", len(planOrder), len(original.Sources))
	}
	for index, originalSource := range original.Sources {
		merged := fragmentsByPlan[originalSource.PlanKey]
		if merged == nil {
			t.Fatalf("source %d (%s) missing from paged traversal", index, originalSource.PlanKey)
		}
		if merged.Status != originalSource.Status || strings.Join(merged.GapReasons, "|") != strings.Join(originalSource.GapReasons, "|") {
			t.Fatalf("source %d identity drifted across pages", index)
		}
		if len(merged.Checks) != len(originalSource.Checks) {
			t.Fatalf("source %d collected %d checks, want %d", index, len(merged.Checks), len(originalSource.Checks))
		}
		for checkIndex := range originalSource.Checks {
			if string(merged.Checks[checkIndex]) != string(originalSource.Checks[checkIndex]) {
				t.Fatalf("source %d check %d drifted across pages", index, checkIndex)
			}
		}
	}
	// 首页若必须切片某个超界 source，其续读页必须从该 source 的检查项中间
	// 恢复：游标 c>0 的页必须存在。
	hasMidSource := false
	for seenCursor := range visited {
		if match := dailyReportCursorPattern.FindStringSubmatch(seenCursor); match != nil && match[2] != "0" {
			hasMidSource = true
		}
	}
	if !hasMidSource {
		t.Fatal("oversized fixture must exercise at least one mid-source continuation")
	}

	// 越权定位符在分页下同样确定性失败（授权先于分页）。
	for name, arguments := range map[string]string{
		"wrong version with cursor":  `{"configKey":"big-daily","localDate":"2026-09-20","version":2,"cursor":"` + cursor + `"}`,
		"wrong configKey first page": `{"configKey":"core-daily","localDate":"2026-09-20","version":1}`,
	} {
		seal := service.invokeDailyReportGetTool(ctx, attempts, dailyToolLoaded(3, arguments))
		if seal.outcome != "failed" || seal.errorCode != "forbidden_locator" {
			t.Fatalf("%s: seal = %s/%s, want failed/forbidden_locator", name, seal.outcome, seal.errorCode)
		}
	}
	// 非法游标确定性失败：形状不符与越过文档末尾。
	for name, arguments := range map[string]string{
		"malformed cursor": `{"configKey":"big-daily","localDate":"2026-09-20","version":1,"cursor":"page-two"}`,
		"past the end":     `{"configKey":"big-daily","localDate":"2026-09-20","version":1,"cursor":"s99:c0"}`,
	} {
		seal := service.invokeDailyReportGetTool(ctx, attempts, dailyToolLoaded(3, arguments))
		if seal.outcome != "failed" || seal.errorCode != "invalid_cursor" {
			t.Fatalf("%s: seal = %s/%s, want failed/invalid_cursor", name, seal.outcome, seal.errorCode)
		}
	}
}

// buildOversizedDailyReportDocument marshals a sealed daily report document
// with sources × checks large enough to force multi-page retrieval and at
// least one mid-source (check-level) continuation: the first source alone
// exceeds the page bound, so it must be split at its check array.
func buildOversizedDailyReportDocument(t *testing.T, configKey, localDate string, sources, checks int) string {
	document := map[string]any{
		"schemaKind": "inspection_daily_report_v1", "configKey": configKey, "localDate": localDate,
		"timezone": "UTC", "windowStartUtc": "2026-09-20T00:00:00Z", "windowEndUtc": "2026-09-21T00:00:00Z",
		"sealedAt": "2026-09-21T02:00:00Z",
		"totals":   map[string]any{"checksOk": sources * checks, "checksGap": 0, "checksError": 0, "sourcesGap": 0},
	}
	sourceList := []map[string]any{}
	for sourceIndex := 0; sourceIndex < sources; sourceIndex++ {
		sourceChecks := checks
		if sourceIndex == 0 {
			sourceChecks = 300 // 单个 source 超页界：强制检查项级切片续读
		}
		checkList := []map[string]any{}
		for checkIndex := 0; checkIndex < sourceChecks; checkIndex++ {
			checkList = append(checkList, map[string]any{
				"runId":      sourceIndex*1000 + checkIndex + 1,
				"checkKey":   fmt.Sprintf("check-%02d-%02d-%s", sourceIndex, checkIndex, strings.Repeat("d", 220)),
				"status":     "ok",
				"observedAt": "2026-09-20T12:00:00Z",
				"evidenceId": sourceIndex*1000 + checkIndex + 1,
				"measurement": map[string]any{
					"resultType": "vector", "series": 1, "samples": 1,
					"entries": []map[string]any{{
						"labels":  map[string]string{"job": "quoin", "checkKey": fmt.Sprintf("check-%02d-%02d", sourceIndex, checkIndex)},
						"samples": 1, "firstValue": "1", "lastValue": "1",
					}},
				},
			})
		}
		sourceList = append(sourceList, map[string]any{
			"planKey":     fmt.Sprintf("plan-%02d-%s", sourceIndex, strings.Repeat("p", 180)),
			"displayName": "填充来源", "connectionName": "core-prom", "enabled": true, "sourceEnabled": true,
			"status": "ok", "checks": checkList,
		})
	}
	document["sources"] = sourceList
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestDailyAlertsGetToolExecutorBoundsToFrozenWindow(t *testing.T) {
	db, reader := newDailyReportToolTestDB(t)
	seedDailyReportToolFixture(t, db)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// 夹具已固定报告 1 的窗口与采证截止（2026-09-27 / 截止 06:00Z），使窗口
	// 与迟到边界的成员资格完全确定。
	mustExec(t, db, `INSERT INTO alert_sources(id,source_key,protocol,enabled,row_version,created_at) VALUES(1,'am-prod','alertmanager',1,1,?)`, now)
	mustExec(t, db, `INSERT INTO business_views(id,view_key,display_name,row_version,created_at,updated_at) VALUES(1,'checkout','结账视图',1,?,?)`, now, now)
	seedAlertOccurrence := func(t *testing.T, id int64, startsAt, firstSeen, title string) {
		t.Helper()
		mustExec(t, db, `INSERT INTO alert_occurrences(id,source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,severity,title,annotations_canonical,resource,first_seen_at,last_state_change_at)
			VALUES(?,1,x'0102030405060708',?,'Firing','{}',?,'warning',?,'{}','checkout-db',?,?)`, id, startsAt, strings.Repeat("a", 64), title, firstSeen, firstSeen)
	}
	seedAlertOccurrence(t, 101, "2026-09-27T10:00:00Z", "2026-09-27T10:00:05Z", "DB 超时")
	seedAlertOccurrence(t, 102, "2026-09-27T02:00:00Z", "2026-09-27T02:00:05Z", "队列积压")
	// 窗口结束之后 0.5 秒（小数边界）：窗口外，绝不出现。
	seedAlertOccurrence(t, 103, "2026-09-28T00:00:00.5Z", "2026-09-28T00:00:06Z", "越界尾")
	// 窗口开始之前 0.5 秒：窗口外，绝不出现。
	seedAlertOccurrence(t, 104, "2026-09-26T23:59:59.5Z", "2026-09-27T00:00:06Z", "越界头")
	// 窗口内但采证截止之后才提交（迟到证据）：绝不出现。
	seedAlertOccurrence(t, 105, "2026-09-27T23:00:00Z", "2026-09-28T07:00:00Z", "迟到证据")
	// 首观测恰好等于采证截止：包含（截止是闭边界）。
	seedAlertOccurrence(t, 106, "2026-09-27T09:00:00Z", "2026-09-28T06:00:00Z", "截止边界")
	mustExec(t, db, `INSERT INTO alert_occurrence_correlations(occurrence_id,view_id,view_key,display_name,matched_at) VALUES(101,1,'checkout','结账视图','2026-09-27T10:00:05Z')`)

	attempts := attempt.NewService(db)
	if err := attempts.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	service := &RuntimeService{}
	if service.SteleGateway != nil {
		t.Fatal("fixture must run without a stele gateway to prove read-only retrieval")
	}
	ctx := context.Background()
	alertsLoaded := func(attemptID int64, arguments string) *routedToolContext {
		return &routedToolContext{
			attemptID: attemptID, toolCallID: 9, toolName: "daily_alerts_get", failureMode: "return_to_model",
			resultSchemaKind: "daily_alerts_get_result_v1", arguments: json.RawMessage(arguments),
			bootID: "boot", epoch: 1,
		}
	}
	locator := `"configKey":"core-daily","localDate":"2026-09-27","version":2`

	// 冻结窗口成员资格：仅窗口内且截止前提交的观测，按 startsAt 降序。
	seal := service.invokeDailyAlertsGetTool(ctx, attempts, alertsLoaded(1, `{`+locator+`}`))
	if seal.outcome != "succeeded" {
		t.Fatalf("frozen locator must succeed, got %s/%s/%s", seal.outcome, seal.errorCode, seal.errorDetail)
	}
	var payload struct {
		ConfigKey   string `json:"configKey"`
		LocalDate   string `json:"localDate"`
		Version     int64  `json:"version"`
		Total       int    `json:"total"`
		HasMore     bool   `json:"hasMore"`
		Offset      int64  `json:"offset"`
		WindowStart string `json:"windowStartUtc"`
		WindowEnd   string `json:"windowEndUtc"`
		CutoffAt    string `json:"cutoffAt"`
		Alerts      []struct {
			OccurrenceID string   `json:"occurrenceId"`
			SourceKey    string   `json:"sourceKey"`
			Title        string   `json:"title"`
			StartsAt     string   `json:"startsAt"`
			ViewKeys     []string `json:"viewKeys"`
		} `json:"alerts"`
	}
	if err := json.Unmarshal(seal.canonical, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Total != 3 || payload.HasMore {
		t.Fatalf("window alerts total=%d hasMore=%v, want exactly the 3 in-window pre-cutoff observations", payload.Total, payload.HasMore)
	}
	if payload.WindowStart != "2026-09-27T00:00:00Z" || payload.WindowEnd != "2026-09-28T00:00:00Z" || payload.CutoffAt != "2026-09-28T06:00:00Z" {
		t.Fatalf("frozen window echo = %s/%s/%s", payload.WindowStart, payload.WindowEnd, payload.CutoffAt)
	}
	wantOrder := []struct {
		id, title string
		viewKeys  []string
	}{{"101", "DB 超时", []string{"checkout"}}, {"106", "截止边界", []string{}}, {"102", "队列积压", []string{}}}
	for index, want := range wantOrder {
		got := payload.Alerts[index]
		if got.OccurrenceID != want.id || got.Title != want.title || got.SourceKey != "am-prod" {
			t.Fatalf("alert %d = %+v, want %s/%s", index, got, want.id, want.title)
		}
		if len(got.ViewKeys) != len(want.viewKeys) {
			t.Fatalf("alert %s viewKeys = %v, want %v", want.id, got.ViewKeys, want.viewKeys)
		}
		if want.viewKeys != nil && len(want.viewKeys) > 0 && got.ViewKeys[0] != "checkout" {
			t.Fatalf("alert %s must carry its frozen correlation", want.id)
		}
	}
	// 确定性分页：limit=1 逐页取全，hasMore 精确收敛。
	collected := []string{}
	offset := 0
	for page := 0; ; page++ {
		if page > 8 {
			t.Fatal("alert paging did not terminate")
		}
		seal := service.invokeDailyAlertsGetTool(ctx, attempts, alertsLoaded(1, fmt.Sprintf(`{%s,"offset":%d,"limit":1}`, locator, offset)))
		if seal.outcome != "succeeded" {
			t.Fatalf("alert page %d seal = %s/%s", page, seal.outcome, seal.errorCode)
		}
		var pagePayload struct {
			Total   int  `json:"total"`
			HasMore bool `json:"hasMore"`
			Offset  int64
			Alerts  []struct {
				OccurrenceID string `json:"occurrenceId"`
			} `json:"alerts"`
		}
		if err := json.Unmarshal(seal.canonical, &pagePayload); err != nil {
			t.Fatal(err)
		}
		if pagePayload.Total != 3 {
			t.Fatalf("page %d total = %d", page, pagePayload.Total)
		}
		for _, alert := range pagePayload.Alerts {
			collected = append(collected, alert.OccurrenceID)
		}
		if !pagePayload.HasMore {
			break
		}
		offset++
	}
	if strings.Join(collected, ",") != "101,106,102" {
		t.Fatalf("paged collection = %v, want the deterministic frozen window order", collected)
	}

	// 越权定位符与非日报 Attempt 确定性失败；畸形分页参数拒绝。
	for name, testCase := range map[string]struct {
		attemptID int64
		arguments string
		code      string
	}{
		"wrong localDate":   {1, `{"configKey":"core-daily","localDate":"2026-09-26","version":2}`, "forbidden_locator"},
		"wrong version":     {1, `{"configKey":"core-daily","localDate":"2026-09-27","version":1}`, "forbidden_locator"},
		"non-daily attempt": {2, `{` + locator + `}`, "not_daily_analysis"},
		"bad limit":         {1, `{` + locator + `,"limit":99}`, "invalid_arguments"},
		"negative offset":   {1, `{` + locator + `,"offset":-1}`, "invalid_arguments"},
		"unknown field":     {1, `{` + locator + `,"query":"up"}`, "invalid_arguments"},
	} {
		seal := service.invokeDailyAlertsGetTool(ctx, attempts, alertsLoaded(testCase.attemptID, testCase.arguments))
		if seal.outcome != "failed" || seal.errorCode != testCase.code {
			t.Fatalf("%s: seal = %s/%s, want failed/%s", name, seal.outcome, seal.errorCode, testCase.code)
		}
	}
}
