package app

// quoin_routed 工具编排测试（ADR-0011）：BeginToolCall 对 quoin_routed 工具
// 的异步执行路径——插件 handler 派发、封存落库（tool_call 终态 + committed
// payload）、ExternalToolResult 帧下发、哨兵错误分类，以及对 Plinth 迟到
// CompleteToolCall 的确定性拒绝。平台 artifact 工具（artifact_read）走进程内
// ArtifactService 能力同路径覆盖。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/plugins"
	_ "github.com/Suknna/quoin/internal/plugins/builtin"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/knowledge"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	_ "modernc.org/sqlite"
)

// routedToolFixture 是一次 quoin_routed 编排测试的全部部件。
type routedToolFixture struct {
	db        *sql.DB
	service   *RuntimeService
	analyses  *analysis.Service
	attempts  *attempt.Service
	attemptID int64
	invoke    plugins.ToolInvoker // 可替换的 stub handler
	// preDispatch 在 Queued 状态内追加播种（如输入 Artifact 授权链）。
	preDispatch func(t *testing.T, db *sql.DB)
	bootID      string
	framesMu    sync.Mutex
	frames      []*runtimev1.ControlEnvelope
	frameSignal chan struct{}
}

// routedAwaitExternalResult 等待编排发出的 ExternalToolResult 帧。
func (fixture *routedToolFixture) awaitExternalResult(t *testing.T) *runtimev1.ExternalToolResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fixture.framesMu.Lock()
		for _, envelope := range fixture.frames {
			if result := envelope.GetExternalToolResult(); result != nil {
				fixture.framesMu.Unlock()
				return result
			}
		}
		fixture.framesMu.Unlock()
		select {
		case <-fixture.frameSignal:
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatal("ExternalToolResult frame was not sent")
	return nil
}

// newRoutedToolFixture 构建带真实目录/attempt ledger 的编排测试夹具：
// 已启用 model provider 链（chat_model grant 闭包）+ 一个 Running 的
// initial_analysis attempt + 一条 pending 的 thanos_query tool call（真实
// 冻结目录，handler 换成 stub）。
func newRoutedToolFixture(t *testing.T, preDispatch func(t *testing.T, db *sql.DB)) *routedToolFixture {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/routed.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	lease := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)
	seedQualifiedModelProvider(t, db, now, lease)

	fixture := &routedToolFixture{db: db, bootID: "routed-boot", frameSignal: make(chan struct{}, 16), preDispatch: preDispatch}
	fixture.invoke = func(context.Context, plugins.ToolExecution) (json.RawMessage, error) {
		return json.RawMessage(`{"success":true,"startedAt":"2026-09-20T08:00:00Z","finishedAt":"2026-09-20T08:00:01Z","status":"success","resultType":"vector","sampleCount":1,"truncated":false,"totalBytes":2,"totalLines":0,"output":"routed-fixture"}`), nil
	}

	// 分析链：alert_sources → occurrence → initial_analyses → attempt。
	mustExec(t, db, `INSERT INTO alert_sources(id,source_key,protocol,enabled,created_at) VALUES(1,'routed-source','alertmanager',1,?)`, now)
	mustExec(t, db, `INSERT INTO alert_occurrences(id,source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,first_seen_at,last_state_change_at) VALUES(1,1,?,?,'Firing','{}',?,?,?)`,
		[]byte{0, 0, 0, 0, 0, 0, 0, 9}, now, strings.Repeat("c", 64), now, now)
	mustExec(t, db, `INSERT INTO initial_analyses(id,occurrence_id,state,input_snapshot_digest,created_by,created_at) VALUES(1,1,'Queued',?,NULL,?)`, strings.Repeat("a", 64), now)
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at) VALUES(1,'initial_analysis','analysis',1,'Queued','routed-test','initial-analysis-v2',?)`, now)

	analyses := analysis.NewService(db)
	attempts := analyses.Attempts()
	// 真实冻结目录（含 quoin_routed 的 thanos_query v4），handler 换成可
	// 替换的 stub（Definition 保持不动，冻结/安装一致性检查照常通过）。
	catalogJSON, _, err := attempt.FrozenCatalogJSONForCreation(attempts.Catalogs, "initial-analysis-v2")
	if err != nil {
		t.Fatal(err)
	}
	stubEntry, known := attempts.Catalogs.Handlers["thanos_query"]
	if !known {
		t.Fatal("thanos_query handler missing from the assembled catalogs")
	}
	entry := stubEntry
	entry.Invoke = plugins.ToolInvoker(func(ctx context.Context, exec plugins.ToolExecution) (json.RawMessage, error) {
		return fixture.invoke(ctx, exec)
	})
	attempts.Catalogs.Handlers["thanos_query"] = entry

	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,tool_catalog_json,created_at) VALUES(1,1,'initial_analysis_v1','initial-analysis-renderer-v1',?,?,?)`,
		strings.Repeat("a", 64), string(catalogJSON), now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,occurrence_id) VALUES(1,1,'user',?,1)`, strings.Repeat("d", 64))
	mustExec(t, db, `INSERT INTO attempt_connection_grants(id,attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at) VALUES(1,1,'chat_model',1,1,1,1,?)`, now)
	// Queued 状态内的附加播种（如输入 Artifact 授权链）。
	if fixture.preDispatch != nil {
		fixture.preDispatch(t, db)
	}
	mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id=?,connection_epoch=1,lease_until=?,runtime_release_version='routed-test',row_version=row_version+1 WHERE id=1`, fixture.bootID, lease)
	mustExec(t, db, `UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=1`, now, now)

	// 物理模型调用 + pending tool call（execution_mode 与生产一致取
	// definition 的 quoin_routed；编排分流仍以 attempt 冻结目录为权威）。
	mustExec(t, db, `INSERT INTO model_calls(id,attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,status,started_at)
		VALUES(1,1,1,0,'chat','chat',1,'initial-analysis-renderer-v1','initial-analysis-v2',?,?,?,?,?,4096,1024,0,'running',?)`,
		strings.Repeat("1", 64), "initial-analysis-tools-v5", strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64), strings.Repeat("5", 64), now)
	// tool_calls 的插入闭包要求模型调用已成功且携带完整输出；输出声明各
	// 用例的提案（thanos_query@0 插件路径，artifact_read@1 平台工具，
	// alerts_recent@2，knowledge_search@3 / knowledge_get@4/@5 知识工具）。
	mustExec(t, db, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(1,1,'system',?,'system_contract'),(1,2,'system',?,'tool_schema')`, strings.Repeat("1", 64), strings.Repeat("1", 64))
	mustExec(t, db, `INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(1,1,'{"assistantText":"","finishReason":"tool_calls","tool_calls":[{"id":"call-routed-1","name":"thanos_query","arguments":{"query":"up"}},{"id":"call-routed-2","name":"artifact_read","arguments":{"artifactId":"1","offset":2,"limit":1}},{"id":"call-routed-3","name":"alerts_recent","arguments":{"viewKey":"mall"}},{"id":"call-k-search","name":"knowledge_search","arguments":{"query":"连接池"}},{"id":"call-k-get-1","name":"knowledge_get","arguments":{"versionId":301}},{"id":"call-k-get-2","name":"knowledge_get","arguments":{"versionId":302}}]}',?,'tool_calls',?)`,
		strings.Repeat("6", 64), now)
	mustExec(t, db, `UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=1 AND status='running'`, now)
	arguments := []byte(`{"query":"up"}`)
	mustExec(t, db, `INSERT INTO tool_calls(id,attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at)
		VALUES(1,1,1,1,0,'call-routed-1','thanos_query','4',?,?,'quoin_routed','return_to_model','pending',?)`,
		string(arguments), hex.EncodeToString(sha256SumBytes(arguments)), now)

	// 只读 reader（编排路径的 schema/catalog 读经此缝）。
	if err := analyses.SetReader(fixtureReadOnlyPool(t, db)); err != nil {
		t.Fatal(err)
	}

	fixture.analyses = analyses
	fixture.attempts = attempts
	fixture.attemptID = 1
	fixture.service = &RuntimeService{
		Slots:        qruntime.NewService(),
		Analyses:     analyses,
		SteleGateway: NewSteleGateway(),
		sendEnvelopeForTest: func(slot string, envelope *runtimev1.ControlEnvelope) error {
			if slot != qruntime.SlotPlinth {
				t.Errorf("slot=%q, want plinth", slot)
			}
			fixture.framesMu.Lock()
			fixture.frames = append(fixture.frames, envelope)
			fixture.framesMu.Unlock()
			select {
			case fixture.frameSignal <- struct{}{}:
			default:
			}
			return nil
		},
	}
	// 结果帧经当前活动流下发（deliverExternalToolResult 查 Slots.View）：
	// fixture 挂一条与 attempt 绑定同 boot/epoch 的流，等价于生产里未发生
	// 过重连的路径。
	fixture.service.Slots.AttachStream(qruntime.SlotPlinth, fixture.bootID, 1)
	return fixture
}

// resetFrames 清空已捕获帧（区分前序与被测执行的 ExternalToolResult）。
func (fixture *routedToolFixture) resetFrames() {
	fixture.framesMu.Lock()
	fixture.frames = nil
	fixture.framesMu.Unlock()
}

// beginRoutedToolCall 驱动一次 BeginToolCall 帧（编排入口）。
func (fixture *routedToolFixture) beginRoutedToolCall(t *testing.T, toolCallID int64) *runtimev1.ControlEnvelope {
	t.Helper()
	fixture.service.handleBeginToolCallRouted(context.Background(), &runtimev1.ControlEnvelope{
		ConnectionEpoch: 1, CorrelationId: 77, BootId: fixture.bootID,
		Msg: &runtimev1.ControlEnvelope_BeginToolCall{BeginToolCall: &runtimev1.BeginToolCall{AttemptId: fixture.attemptID, ToolCallId: toolCallID}},
	}, &runtimev1.BeginToolCall{AttemptId: fixture.attemptID, ToolCallId: toolCallID})
	fixture.framesMu.Lock()
	defer fixture.framesMu.Unlock()
	for _, envelope := range fixture.frames {
		if ack := envelope.GetBeginToolCallAck(); ack != nil && ack.GetToolCallId() == toolCallID {
			return envelope
		}
	}
	t.Fatal("BeginToolCallAck was not sent")
	return nil
}

// awaitToolCallStatus 轮询 tool_calls 终态。
func (fixture *routedToolFixture) awaitToolCallStatus(t *testing.T, toolCallID int64, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := fixture.db.QueryRow(`SELECT status FROM tool_calls WHERE id=?`, toolCallID).Scan(&status); err == nil && status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("tool call %d did not reach status %s", toolCallID, want)
}

// TestQuoinRoutedToolCallExecutesAndSeals 覆盖成功路径：stub handler 结果
// 封存为 succeeded tool call（result_json 即 handler 输出），并以
// ExternalToolResult 帧携带 committed payload 推给 Plinth。
func TestQuoinRoutedToolCallExecutesAndSeals(t *testing.T) {
	fixture := newRoutedToolFixture(t, nil)
	if ack := fixture.beginRoutedToolCall(t, 1); !ack.GetBeginToolCallAck().GetAccepted() {
		t.Fatalf("begin ack=%+v", ack.GetBeginToolCallAck())
	}
	fixture.awaitToolCallStatus(t, 1, "succeeded")
	result := fixture.awaitExternalResult(t)
	if result.GetOutcome() != runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED {
		t.Fatalf("outcome=%v", result.GetOutcome())
	}
	payload := result.GetPayload()
	if payload.GetSchemaKind() != "thanos_query_result_v1" || !strings.Contains(string(payload.GetCanonicalJson()), `"output":"routed-fixture"`) {
		t.Fatalf("payload=%+v", payload)
	}
	digest := sha256.Sum256(payload.GetCanonicalJson())
	if hex.EncodeToString(digest[:]) != hex.EncodeToString(payload.GetContentDigest()) {
		t.Fatal("committed payload digest mismatch")
	}
	if result.GetErrorCode() != "" {
		t.Fatalf("unexpected error code %q", result.GetErrorCode())
	}
	// 迟到的 Plinth CompleteToolCall 必须被确定性拒绝（重放/旧帧防御）。
	complete := &runtimev1.CompleteToolCall{
		AttemptId: 1, ToolCallId: 1, Outcome: runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED,
		Payload: &runtimev1.ResultPayload{SchemaKind: "thanos_query_result_v1", CanonicalJson: []byte(`{"success":true}`)},
	}
	fixture.service.handleCompleteToolCallRouted(context.Background(), &runtimev1.ControlEnvelope{
		ConnectionEpoch: 1, CorrelationId: 78, BootId: fixture.bootID,
		Msg: &runtimev1.ControlEnvelope_CompleteToolCall{CompleteToolCall: complete},
	}, complete)
	fixture.framesMu.Lock()
	defer fixture.framesMu.Unlock()
	var late *runtimev1.CompleteToolCallAck
	for _, envelope := range fixture.frames {
		if ack := envelope.GetCompleteToolCallAck(); ack != nil && ack.GetToolCallId() == 1 {
			late = ack
		}
	}
	if late == nil || late.GetAccepted() {
		t.Fatalf("late worker completion must be rejected, ack=%+v", late)
	}
}

// TestQuoinRoutedToolCallSealsSentinelFailures 覆盖执行级失败的封存：网关
// 配额拒绝（ErrPlatformRateLimited）映射为稳定错误码 rate_limited，结构化
// 失败载荷携带工具自身 schema kind，tool call 终态 failed。
func TestQuoinRoutedToolCallSealsSentinelFailures(t *testing.T) {
	fixture := newRoutedToolFixture(t, nil)
	fixture.invoke = func(context.Context, plugins.ToolExecution) (json.RawMessage, error) {
		return nil, fmt.Errorf("quota: %w", plugins.ErrPlatformRateLimited)
	}
	if ack := fixture.beginRoutedToolCall(t, 1); !ack.GetBeginToolCallAck().GetAccepted() {
		t.Fatalf("begin ack=%+v", ack.GetBeginToolCallAck())
	}
	fixture.awaitToolCallStatus(t, 1, "failed")
	result := fixture.awaitExternalResult(t)
	if result.GetOutcome() != runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_FAILED {
		t.Fatalf("outcome=%v", result.GetOutcome())
	}
	if result.GetErrorCode() != "rate_limited" {
		t.Fatalf("error code=%q, want rate_limited", result.GetErrorCode())
	}
	if payload := result.GetPayload(); payload.GetSchemaKind() != "thanos_query_result_v1" || !strings.Contains(string(payload.GetCanonicalJson()), `"success":false`) {
		t.Fatalf("failure payload=%+v", payload)
	}
	var stored string
	if err := fixture.db.QueryRow(`SELECT result_json FROM tool_calls WHERE id=1`).Scan(&stored); err != nil || !strings.Contains(stored, `"errorCode":"rate_limited"`) {
		t.Fatalf("stored result=%s err=%v", stored, err)
	}
}

// TestQuoinRoutedToolErrorCode 分类映射的纯函数覆盖。
func TestQuoinRoutedToolErrorCode(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("x: %w", plugins.ErrPlatformRateLimited), "rate_limited"},
		{fmt.Errorf("x: %w", plugins.ErrCredentialUnavailable), "credential_unavailable"},
		{fmt.Errorf("x: %w", plugins.ErrPlatformUnreachable), "unreachable"},
		{fmt.Errorf("boom: %w", context.DeadlineExceeded), "timeout"},
		{errors.New("plain"), "tool_error"},
	}
	for _, testCase := range cases {
		if got := routedToolErrorCode(testCase.err); got != testCase.want {
			t.Fatalf("code(%v)=%s, want %s", testCase.err, got, testCase.want)
		}
	}
}

// TestQuoinRoutedArtifactReadExecutor 覆盖平台 artifact 工具的进程内执行：
// artifact_read 读取已授权 Artifact，结果契约（success/output/startLine/
// nextLine/eof/artifact 定位符）与原 Plinth 执行器一致。
func TestQuoinRoutedArtifactReadExecutor(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "artifacts")
	// 播种一个授权给该 attempt 的文本输入 Artifact（blob 文件 + 元数据行 +
	// 输入项引用 + attempt_artifact_grants；授权链必须在 Queued 状态内完成）。
	content := "line-one\nline-two\nline-three\n"
	sum := sha256.Sum256([]byte(content))
	shaHex := hex.EncodeToString(sum[:])
	fixture := newRoutedToolFixture(t, func(t *testing.T, db *sql.DB) {
		t.Helper()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if err := os.MkdirAll(filepath.Join(storeDir, "blobs"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(storeDir, "blobs", shaHex+".blob"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `INSERT INTO source_materials(id,kind,digest,size_bytes,content,created_at) VALUES(1,'knowledge_import',?,0,'fixture',?)`, fmt.Sprintf("%064x", 1), now)
		mustExec(t, db, `INSERT INTO artifact_blobs(id,sha256,size_bytes,storage_key,created_at) VALUES(1,?,?,'blobs/x.blob',?)`, shaHex, len(content), now)
		mustExec(t, db, `INSERT INTO artifacts(id,blob_id,kind,media_type,sensitive,retention_kind,owner_type,owner_id,created_at) VALUES(1,1,'attachment','text/plain',0,'long_term','source_material',1,?)`, now)
		mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,artifact_id) VALUES(1,2,'artifact',?,1)`, strings.Repeat("e", 64))
		mustExec(t, db, `INSERT INTO attempt_artifact_grants(attempt_id,artifact_id,source_kind,source_id,granted_at) VALUES(1,1,'input_snapshot',1,?)`, now)
	})
	store, err := artifact.NewStore(fixture.db, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	// 只读 reader 注入（checkReadGrant 走 store 的读缝）。
	if err := store.SetReader(fixtureReadOnlyPool(t, fixture.db)); err != nil {
		t.Fatal(err)
	}
	fixture.service.Artifacts = store

	now := time.Now().UTC().Format(time.RFC3339Nano)
	arguments := []byte(`{"artifactId":"1","offset":2,"limit":1}`)
	mustExec(t, fixture.db, `INSERT INTO tool_calls(id,attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at)
		VALUES(2,1,1,1,1,'call-routed-2','artifact_read','2',?,?,'quoin_routed','return_to_model','pending',?)`,
		string(arguments), hex.EncodeToString(sha256SumBytes(arguments)), now)

	// 前序 tool call（thanos_query@0）必须先到终态，artifact 工具才能开始
	// （tool call 开始闭包的顺序围栏）。
	if ack := fixture.beginRoutedToolCall(t, 1); !ack.GetBeginToolCallAck().GetAccepted() {
		t.Fatalf("predecessor begin ack=%+v", ack.GetBeginToolCallAck())
	}
	fixture.awaitToolCallStatus(t, 1, "succeeded")
	fixture.resetFrames()

	if ack := fixture.beginRoutedToolCall(t, 2); !ack.GetBeginToolCallAck().GetAccepted() {
		t.Fatalf("begin ack=%+v", ack.GetBeginToolCallAck())
	}
	fixture.awaitToolCallStatus(t, 2, "succeeded")
	result := fixture.awaitExternalResult(t)
	payload := result.GetPayload()
	if payload.GetSchemaKind() != "artifact_read_result_v1" {
		t.Fatalf("schema kind=%q", payload.GetSchemaKind())
	}
	canonical := string(payload.GetCanonicalJson())
	for _, expected := range []string{`"success":true`, `"output":"line-two\n"`, `"startLine":2`, `"nextLine":3`, `"eof":false`, `"id":"1"`, `"mediaType":"text/plain"`} {
		if !strings.Contains(canonical, expected) {
			t.Fatalf("canonical=%s missing %s", canonical, expected)
		}
	}
}

// TestQuoinRoutedSpilledResultSealsWithCommittedArtifact 复现实机
// 2026-09-21 首条调查的失败链（attempt=8 tool_call=7）：thanos_query 结果超限
// 溢出为 tool_result Artifact 后，封存必须携带该 Artifact 链接——否则
// EvidenceFor 以 "spilled thanos_query result lacks the committed artifact"
// 拒绝，tool call 永久卡 running，worker 90s 等待超时后续跑 BeginToolCall
// 被顺序围栏拒绝（1811），整个 attempt 以 worker_protocol_error 失败。
// 本测试按生产 runQueryTool 的真实溢出形状驱动：Spill 提交 → 载荷携带
// truncated+定位符 → 封存 succeeded + result_artifact_id + Evidence + 帧。
func TestQuoinRoutedSpilledResultSealsWithCommittedArtifact(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "artifacts")
	// 溢出提交读取 generated 保留期配置（bootstrap 播种；测试库自播种）。
	fixture := newRoutedToolFixture(t, func(t *testing.T, db *sql.DB) {
		t.Helper()
		mustExec(t, db, `INSERT INTO artifact_retention_settings(id,generated_retention_days,row_version,updated_at) VALUES(1,90,1,?)`,
			time.Now().UTC().Format(time.RFC3339Nano))
	})
	store, err := artifact.NewStore(fixture.db, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetReader(fixtureReadOnlyPool(t, fixture.db)); err != nil {
		t.Fatal(err)
	}
	fixture.service.Artifacts = store
	// 与 app.go 组装一致：封存事务内写 tool result 读授权，RefFor 的闭包
	// 查询依赖它。
	fixture.attempts.ToolResultGrants = store.InsertToolResultGrant

	body := []byte(`{"status":"success","data":{"resultType":"vector","result":[]},` + strings.Repeat(`"padding":"0123456789abcdef",`, 4096) + `"tail":"end"}`)
	fixture.invoke = func(ctx context.Context, exec plugins.ToolExecution) (json.RawMessage, error) {
		artifactID, spillErr := exec.Spill(ctx, body, "application/json")
		if spillErr != nil {
			return nil, spillErr
		}
		sum := sha256.Sum256(body)
		payload, marshalErr := json.Marshal(map[string]any{
			"success": true, "status": "success", "resultType": "vector", "sampleCount": 0,
			"startedAt": "2026-09-21T15:23:51.2Z", "finishedAt": "2026-09-21T15:23:51.3Z",
			"truncated": true, "totalBytes": len(body), "totalLines": 1,
			"output": "…（完整输出已存入 Artifact）",
			"artifact": map[string]any{
				"id": fmt.Sprintf("%d", artifactID), "mediaType": "application/json",
				"sha256": hex.EncodeToString(sum[:]), "sizeBytes": len(body), "totalLines": 1,
			},
		})
		if marshalErr != nil {
			return nil, marshalErr
		}
		return payload, nil
	}

	if ack := fixture.beginRoutedToolCall(t, 1); !ack.GetBeginToolCallAck().GetAccepted() {
		t.Fatalf("begin ack=%+v", ack.GetBeginToolCallAck())
	}
	// 没有修复时这里超时：封存被 EvidenceFor 拒绝，tool call 悬死 running。
	fixture.awaitToolCallStatus(t, 1, "succeeded")

	var resultArtifactID int64
	var resultJSON string
	if err := fixture.db.QueryRow(`SELECT result_artifact_id,result_json FROM tool_calls WHERE id=1`).Scan(&resultArtifactID, &resultJSON); err != nil {
		t.Fatal(err)
	}
	if resultArtifactID == 0 {
		t.Fatal("spilled result sealed without the committed artifact link")
	}
	if !strings.Contains(resultJSON, `"truncated":true`) {
		t.Fatalf("sealed preview=%s", resultJSON)
	}
	// 溢出结果的 Evidence 闭包：evidence 行携带同一 artifact。
	var evidenceArtifact sql.NullInt64
	if err := fixture.db.QueryRow(`SELECT artifact_id FROM evidence WHERE tool_call_id=1`).Scan(&evidenceArtifact); err != nil {
		t.Fatalf("evidence row missing for spilled tool call: %v", err)
	}
	if !evidenceArtifact.Valid || evidenceArtifact.Int64 != resultArtifactID {
		t.Fatalf("evidence artifact=%v, want %d", evidenceArtifact, resultArtifactID)
	}
	// 帧必须携带 artifact 引用（模型据此 artifact_read 续读）。
	result := fixture.awaitExternalResult(t)
	if result.GetOutcome() != runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED {
		t.Fatalf("outcome=%v", result.GetOutcome())
	}
	if ref := result.GetArtifactRef(); ref == nil || ref.GetArtifactId() != resultArtifactID {
		t.Fatalf("external result artifact ref=%+v, want artifact %d", result.GetArtifactRef(), resultArtifactID)
	}
}

// TestQuoinRoutedSealRejectionConvergesAsFailedResult 覆盖封存被拒后的收敛：
// 载荷声明溢出但从未提交 Artifact（协议冲突）时，首次封存被 EvidenceFor 拒
// 绝——收敛路径必须把该 tool call 封存为确定性 failed 结果并下发帧，让
// agent 循环继续（同 attempt 的后续 tool call 可以开始），而不是悬死
// running 直到 worker_protocol_error。
func TestQuoinRoutedSealRejectionConvergesAsFailedResult(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "artifacts")
	fixture := newRoutedToolFixture(t, func(t *testing.T, db *sql.DB) {
		t.Helper()
		mustExec(t, db, `INSERT INTO artifact_retention_settings(id,generated_retention_days,row_version,updated_at) VALUES(1,90,1,?)`,
			time.Now().UTC().Format(time.RFC3339Nano))
	})
	store, err := artifact.NewStore(fixture.db, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetReader(fixtureReadOnlyPool(t, fixture.db)); err != nil {
		t.Fatal(err)
	}
	fixture.service.Artifacts = store
	fixture.attempts.ToolResultGrants = store.InsertToolResultGrant

	// 成功载荷声明 truncated 并指向一个从未提交的 artifact 定位符：与实机
	// 同一条失败路径（EvidenceFor：spilled result lacks the committed
	// artifact），但保持确定性、不依赖真 spills。
	fixture.invoke = func(context.Context, plugins.ToolExecution) (json.RawMessage, error) {
		return json.RawMessage(`{"success":true,"status":"success","resultType":"vector","sampleCount":1,"startedAt":"2026-09-21T15:23:51.2Z","finishedAt":"2026-09-21T15:23:51.3Z","truncated":true,"totalBytes":1024,"totalLines":1,"output":"preview","artifact":{"id":"999","mediaType":"application/json","sha256":"` + strings.Repeat("0", 64) + `","sizeBytes":1024,"totalLines":1}}`), nil
	}

	if ack := fixture.beginRoutedToolCall(t, 1); !ack.GetBeginToolCallAck().GetAccepted() {
		t.Fatalf("begin ack=%+v", ack.GetBeginToolCallAck())
	}
	// 收敛封存：failed + seal_rejected（不是悬死 running）。
	fixture.awaitToolCallStatus(t, 1, "failed")
	var errorCode string
	if err := fixture.db.QueryRow(`SELECT result_json FROM tool_calls WHERE id=1`).Scan(&errorCode); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errorCode, `"errorCode":"seal_rejected"`) {
		t.Fatalf("converged result=%s", errorCode)
	}
	result := fixture.awaitExternalResult(t)
	if result.GetOutcome() != runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_FAILED {
		t.Fatalf("outcome=%v", result.GetOutcome())
	}
	if result.GetErrorCode() != "seal_rejected" {
		t.Fatalf("error code=%q", result.GetErrorCode())
	}

	// 续跑围栏：同一 attempt 的下一个 tool call 现在可以开始（实机在这里
	// 被 1811 拒绝并拖垮整个 attempt）。提案序数 1 是夹具已声明的
	// artifact_read（call-routed-2），保持与 proposal 闭包一致。
	now := time.Now().UTC().Format(time.RFC3339Nano)
	followUp := []byte(`{"artifactId":"1","offset":2,"limit":1}`)
	mustExec(t, fixture.db, `INSERT INTO tool_calls(id,attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at)
		VALUES(7,1,1,1,1,'call-routed-2','artifact_read','2',?,?,'quoin_routed','return_to_model','pending',?)`,
		string(followUp), hex.EncodeToString(sha256SumBytes(followUp)), now)
	fixture.resetFrames()
	if ack := fixture.beginRoutedToolCall(t, 7); !ack.GetBeginToolCallAck().GetAccepted() {
		t.Fatalf("follow-up begin rejected (continuation deadlock): %+v", ack.GetBeginToolCallAck())
	}
	// 到达任意终态即可（artifact 1 未播种，预期 failed：artifact_read_failed）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := fixture.db.QueryRow(`SELECT status FROM tool_calls WHERE id=7`).Scan(&status); err == nil && (status == "succeeded" || status == "failed") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("follow-up tool call never reached a terminal state")
}

func sha256SumBytes(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}

// TestQuoinRoutedAlertsRecentExecutor 覆盖 ADR-0012 平台工具 alerts_recent
// 的进程内执行：只读查询 Quoin 告警库，结果契约（alerts/total/window + 归一
// 语义字段与 viewKeys）按 occurrence 冻结列投影。
func TestQuoinRoutedAlertsRecentExecutor(t *testing.T) {
	fixture := newRoutedToolFixture(t, func(t *testing.T, db *sql.DB) {
		t.Helper()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		mustExec(t, db, `INSERT INTO alert_occurrences(id,source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,severity,title,annotations_canonical,resource,first_seen_at,last_state_change_at) VALUES(2,1,?,'2026-09-20T07:30:00Z','Firing','{}',?,'critical','CheckoutLatencyHigh','{"summary":"P95 over threshold"}','checkout:8080',?,?)`,
			[]byte{0, 0, 0, 0, 0, 0, 1, 2}, strings.Repeat("c", 64), now, now)
		mustExec(t, db, `INSERT INTO alert_enrichments(occurrence_id,enrichment_json,evaluated_at) VALUES(2,'{"fields":{"team":"payments"},"rules":[]}',?)`, now)
		mustExec(t, db, `INSERT INTO business_views(id,view_key,display_name,description,connection_id,label_conditions_json,alert_source_keys_json,row_version,created_at,updated_at) VALUES(1,'mall','商城','',NULL,'{}','["routed-source"]',1,?,?)`, now, now)
		mustExec(t, db, `INSERT INTO alert_occurrence_correlations(occurrence_id,view_id,view_key,display_name,matched_at) VALUES(2,1,'mall','商城',?)`, now)
	})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	arguments := []byte(`{"viewKey":"mall","severityMin":"high","hours":168,"limit":5}`)
	mustExec(t, fixture.db, `INSERT INTO tool_calls(id,attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at)
		VALUES(3,1,1,1,2,'call-routed-3','alerts_recent','1',?,?,'quoin_routed','return_to_model','pending',?)`,
		string(arguments), hex.EncodeToString(sha256SumBytes(arguments)), now)

	// 前序 tool call 必须先到终态（tool call 开始闭包的顺序围栏）。
	if ack := fixture.beginRoutedToolCall(t, 1); !ack.GetBeginToolCallAck().GetAccepted() {
		t.Fatalf("predecessor begin ack=%+v", ack.GetBeginToolCallAck())
	}
	fixture.awaitToolCallStatus(t, 1, "succeeded")
	fixture.resetFrames()

	if ack := fixture.beginRoutedToolCall(t, 3); !ack.GetBeginToolCallAck().GetAccepted() {
		t.Fatalf("begin ack=%+v", ack.GetBeginToolCallAck())
	}
	fixture.awaitToolCallStatus(t, 3, "succeeded")
	result := fixture.awaitExternalResult(t)
	payload := result.GetPayload()
	if payload.GetSchemaKind() != "alerts_recent_result_v1" {
		t.Fatalf("schema kind=%q", payload.GetSchemaKind())
	}
	canonical := string(payload.GetCanonicalJson())
	for _, expected := range []string{`"total":1`, `"occurrenceId":"2"`, `"severity":"critical"`, `"title":"CheckoutLatencyHigh"`, `"state":"Firing"`, `"viewKeys":["mall"]`} {
		if !strings.Contains(canonical, expected) {
			t.Fatalf("canonical=%s missing %s", canonical, expected)
		}
	}
	// viewKey 过滤命中的正是带关联快照的告警；无关联的 fixture occurrence 1
	// 不在结果里（同 fixture 只有一条命中）。
}

// seedKnowledgeForTools 在夹具库内播种一份已确认知识（版本 301）与一份已停止
// 复用知识（版本 302），插入顺序遵循 confirm.go 的生产闭包。
func seedKnowledgeForTools(t *testing.T, db *sql.DB, versionID int64, title string, exited bool) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	material, err := db.Exec(`INSERT INTO source_materials(kind,digest,size_bytes,content,created_by,created_at) VALUES('knowledge_import',?,?,'导入原文',NULL,?)`, fmt.Sprintf("%064x", versionID), len("导入原文"), now)
	if err != nil {
		t.Fatal(err)
	}
	materialID, _ := material.LastInsertId()
	batch, err := db.Exec(`INSERT INTO knowledge_import_batches(source_material_id,state,created_by,created_at) VALUES(?,'Processing',NULL,?)`, materialID, now)
	if err != nil {
		t.Fatal(err)
	}
	batchID, _ := batch.LastInsertId()
	candidate, err := db.Exec(`INSERT INTO knowledge_candidates(import_batch_id,source_type,source_id,state,original_suggestion_json,draft_title,draft_body,created_by,created_at) VALUES(?,'source_material',?,'AwaitingConfirmation','{}',?,?,NULL,?)`, batchID, materialID, title, "数据库连接池打满时先检查慢查询再扩容。", now)
	if err != nil {
		t.Fatal(err)
	}
	candidateID, _ := candidate.LastInsertId()
	knowledge, err := db.Exec(`INSERT INTO reusable_knowledge(created_by,created_at) VALUES(NULL,?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	knowledgeID, _ := knowledge.LastInsertId()
	if _, err = db.Exec(`UPDATE knowledge_candidates SET state='Confirmed',confirmed_knowledge_id=?,row_version=row_version+1 WHERE id=?`, knowledgeID, candidateID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO knowledge_versions(id,knowledge_id,version_seq,title,body,source_candidate_id,created_by,created_at) VALUES(?,?,1,?,?,?,NULL,?)`, versionID, knowledgeID, title, "数据库连接池打满时先检查慢查询再扩容。", candidateID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE reusable_knowledge SET current_version_id=?,row_version=row_version+1 WHERE id=?`, versionID, knowledgeID); err != nil {
		t.Fatal(err)
	}
	if exited {
		if _, err = db.Exec(`INSERT INTO knowledge_version_retrieval_state(knowledge_version_id,exited,exited_at,exit_reason,updated_at) VALUES(?,1,?,'stopped',?)`, versionID, now, now); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err = db.Exec(`INSERT INTO knowledge_version_retrieval_state(knowledge_version_id,updated_at) VALUES(?,?)`, versionID, now); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO knowledge_search_docs(knowledge_version_id,title,body) VALUES(?,?,?)`, versionID, title, "数据库连接池打满时先检查慢查询再扩容。"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`UPDATE knowledge_import_batches SET state='AwaitingConfirmation',row_version=row_version+1 WHERE id=?`, batchID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE knowledge_import_batches SET state='Completed',row_version=row_version+1 WHERE id=?`, batchID); err != nil {
		t.Fatal(err)
	}
}

// wireKnowledgeService 构造带只读 reader 的知识域服务并挂到 RuntimeService。
func wireKnowledgeService(t *testing.T, fixture *routedToolFixture) {
	t.Helper()
	runner := execution.NewRunner(fixture.db, execution.NewRegistry(), nil)
	service, err := knowledge.NewServiceWithReader(fixtureReadOnlyPool(t, fixture.db), fixture.db, runner)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.Knowledge = service
}

// beginKnowledgeToolCall 播种一条 pending 的知识工具调用并驱动 BeginToolCall。
func beginKnowledgeToolCall(t *testing.T, fixture *routedToolFixture, toolCallID int64, toolIndex int, providerID, toolName, arguments string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	mustExec(t, fixture.db, `INSERT INTO tool_calls(id,attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at)
		VALUES(?,1,1,1,?,?,?,'1',?,?,'quoin_routed','return_to_model','pending',?)`,
		toolCallID, toolIndex, providerID, toolName, arguments, hex.EncodeToString(sha256SumBytes([]byte(arguments))), now)
	// 前序 tool call 必须先到终态（tool call 开始闭包的顺序围栏）；已终态则
	// 幂等跳过（同测试内多次播种）。
	var predecessorStatus string
	if err := fixture.db.QueryRow(`SELECT status FROM tool_calls WHERE id=1`).Scan(&predecessorStatus); err != nil {
		t.Fatal(err)
	}
	if predecessorStatus != "succeeded" {
		if ack := fixture.beginRoutedToolCall(t, 1); !ack.GetBeginToolCallAck().GetAccepted() {
			t.Fatalf("predecessor begin ack=%+v", ack.GetBeginToolCallAck())
		}
		fixture.awaitToolCallStatus(t, 1, "succeeded")
	}
	fixture.resetFrames()
	if ack := fixture.beginRoutedToolCall(t, toolCallID); !ack.GetBeginToolCallAck().GetAccepted() {
		t.Fatalf("begin ack=%+v", ack.GetBeginToolCallAck())
	}
}

// TestQuoinRoutedKnowledgeSearchExecutor 覆盖 knowledge_search 的进程内执行：
// 复用知识域双通道查询（无 embedding 配置时语义通道诚实为空、FTS 命中返回），
// 结果并列两个通道并携带命中依据与定位符。
func TestQuoinRoutedKnowledgeSearchExecutor(t *testing.T) {
	fixture := newRoutedToolFixture(t, func(t *testing.T, db *sql.DB) {
		seedKnowledgeForTools(t, db, 301, "数据库连接池打满处理", false)
	})
	wireKnowledgeService(t, fixture)
	beginKnowledgeToolCall(t, fixture, 4, 3, "call-k-search", "knowledge_search", `{"query":"连接池","limit":5}`)

	fixture.awaitToolCallStatus(t, 4, "succeeded")
	result := fixture.awaitExternalResult(t)
	payload := result.GetPayload()
	if payload.GetSchemaKind() != "knowledge_search_result_v1" {
		t.Fatalf("schema kind=%q", payload.GetSchemaKind())
	}
	canonical := string(payload.GetCanonicalJson())
	for _, expected := range []string{
		`"success":true`,
		`"exactTextMatches":[{`,
		`"versionId":"301"`,
		`"title":"数据库连接池打满处理"`,
		`"basis":"exact_text"`,
		`"semanticMatches":[]`,
	} {
		if !strings.Contains(canonical, expected) {
			t.Fatalf("canonical=%s missing %s", canonical, expected)
		}
	}
}

// TestQuoinRoutedKnowledgeGetExecutor 覆盖 knowledge_get 的进程内执行：合格版本
// 返回正文与定位符；已停止复用版本返回稳定错误 knowledge_version_ineligible
// （停止复用的知识不能被新检索使用），失败是 return_to_model 结果不阻塞
// attempt 主流程。
func TestQuoinRoutedKnowledgeGetExecutor(t *testing.T) {
	fixture := newRoutedToolFixture(t, func(t *testing.T, db *sql.DB) {
		seedKnowledgeForTools(t, db, 301, "数据库连接池打满处理", false)
		seedKnowledgeForTools(t, db, 302, "已停止复用知识", true)
	})
	wireKnowledgeService(t, fixture)

	beginKnowledgeToolCall(t, fixture, 5, 4, "call-k-get-1", "knowledge_get", `{"versionId":301}`)
	fixture.awaitToolCallStatus(t, 5, "succeeded")
	result := fixture.awaitExternalResult(t)
	payload := result.GetPayload()
	if payload.GetSchemaKind() != "knowledge_get_result_v1" {
		t.Fatalf("schema kind=%q", payload.GetSchemaKind())
	}
	canonical := string(payload.GetCanonicalJson())
	for _, expected := range []string{
		`"success":true`,
		`"versionId":"301"`,
		`"knowledgeId":"1"`,
		`"truncated":false`,
		"先检查慢查询再扩容",
	} {
		if !strings.Contains(canonical, expected) {
			t.Fatalf("canonical=%s missing %s", canonical, expected)
		}
	}

	beginKnowledgeToolCall(t, fixture, 6, 5, "call-k-get-2", "knowledge_get", `{"versionId":302}`)
	fixture.awaitToolCallStatus(t, 6, "failed")
	failed := fixture.awaitExternalResult(t)
	if failed.GetErrorCode() != "knowledge_version_ineligible" {
		t.Fatalf("stopped-reuse version error code=%q", failed.GetErrorCode())
	}
	if !strings.Contains(string(failed.GetPayload().GetCanonicalJson()), "停止复用") {
		t.Fatalf("failure payload=%s", failed.GetPayload().GetCanonicalJson())
	}
}

// TestQuoinRoutedKnowledgeToolsWithoutDomainWiring：知识域未装配（空 Service）
// 时工具确定性失败（knowledge_unavailable），不阻塞 attempt 主流程。
func TestQuoinRoutedKnowledgeToolsWithoutDomainWiring(t *testing.T) {
	fixture := newRoutedToolFixture(t, nil)
	beginKnowledgeToolCall(t, fixture, 4, 3, "call-k-search", "knowledge_search", `{"query":"连接池"}`)
	fixture.awaitToolCallStatus(t, 4, "failed")
	result := fixture.awaitExternalResult(t)
	if result.GetErrorCode() != "knowledge_unavailable" {
		t.Fatalf("error code=%q", result.GetErrorCode())
	}
}

// TestReconcileResendsSealedToolResultsOnCurrentStream 覆盖断流窗口丢失首发
// 的收敛：同 boot 重连（epoch 1 → 2）后，对账确认 attempt 仍活跃时补发其已
// 封存 quoin_routed 结果，且补发帧携带当前流的 boot/epoch（行内旧 epoch 的
// 帧会被 SendToFenced 围栏拒绝，永远不可达）。
func TestReconcileResendsSealedToolResultsOnCurrentStream(t *testing.T) {
	fixture := newRoutedToolFixture(t, nil)
	if ack := fixture.beginRoutedToolCall(t, 1); !ack.GetBeginToolCallAck().GetAccepted() {
		t.Fatalf("begin ack=%+v", ack.GetBeginToolCallAck())
	}
	fixture.awaitToolCallStatus(t, 1, "succeeded")
	first := fixture.awaitExternalResult(t)
	if first.GetOutcome() != runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED {
		t.Fatalf("first outcome=%v", first.GetOutcome())
	}
	// 同 boot 重连：当前流 epoch 前进到 2，attempt 行内仍是派发时的旧绑定。
	fixture.service.Slots.AttachStream(qruntime.SlotPlinth, fixture.bootID, 2)
	fixture.resetFrames()

	boot := fixture.bootID
	rowEpoch := int64(1)
	fixture.service.alignReconcileReport(context.Background(), fixture.bootID, []attempt.View{{
		ID: fixture.attemptID, AttemptType: "initial_analysis", ScopeType: "analysis", ScopeID: 1,
		State: "Running", BootID: &boot, ConnectionEpoch: &rowEpoch,
	}}, []int64{fixture.attemptID})

	resent := fixture.awaitExternalResult(t)
	if resent.GetOutcome() != runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED {
		t.Fatalf("resent outcome=%v", resent.GetOutcome())
	}
	if !strings.Contains(string(resent.GetPayload().GetCanonicalJson()), `"output":"routed-fixture"`) {
		t.Fatalf("resent payload=%s", resent.GetPayload().GetCanonicalJson())
	}
	fixture.framesMu.Lock()
	for _, envelope := range fixture.frames {
		if envelope.GetExternalToolResult() == nil {
			continue
		}
		if envelope.GetBootId() != fixture.bootID || envelope.GetConnectionEpoch() != 2 {
			fixture.framesMu.Unlock()
			t.Fatalf("resent frame rides boot=%q epoch=%d, want current stream %q epoch 2",
				envelope.GetBootId(), envelope.GetConnectionEpoch(), fixture.bootID)
		}
	}
	fixture.framesMu.Unlock()
	// 幂等：第二次对账补发的帧与第一次内容一致（Plinth 侧无 waiter 只审计丢弃）。
	fixture.resetFrames()
	fixture.service.alignReconcileReport(context.Background(), fixture.bootID, []attempt.View{{
		ID: fixture.attemptID, AttemptType: "initial_analysis", ScopeType: "analysis", ScopeID: 1,
		State: "Running", BootID: &boot, ConnectionEpoch: &rowEpoch,
	}}, []int64{fixture.attemptID})
	again := fixture.awaitExternalResult(t)
	if string(again.GetPayload().GetCanonicalJson()) != string(resent.GetPayload().GetCanonicalJson()) {
		t.Fatal("resend is not byte-identical across reconcile rounds")
	}
}

// TestReconcileUnreportedAssignedAgentAttemptConvergesAsLoss 覆盖从未被
// runtime 接受的 agent attempt：旧重派发携带行内旧 epoch，必然被当前流的
// 发送围栏拒绝——对账改为确定性 loss 收口（Interrupted），不再静默悬挂。
func TestReconcileUnreportedAssignedAgentAttemptConvergesAsLoss(t *testing.T) {
	fixture := newRoutedToolFixture(t, func(t *testing.T, db *sql.DB) {
		t.Helper()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		lease := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)
		// 播种一个独立的 Assigned attempt（id=2，挂在独立 occurrence 上：
		// 同一 occurrence 同时最多一个 active 分析）：派发绑定一旦写入即
		// 不可变，不能从 Running 回置，只能按真实派发路径成形。
		mustExec(t, db, `INSERT INTO alert_occurrences(id,source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,first_seen_at,last_state_change_at) VALUES(2,1,?,?,'Firing','{}',?,?,?)`,
			[]byte{0, 0, 0, 0, 0, 0, 1, 3}, now, strings.Repeat("c", 64), now, now)
		mustExec(t, db, `INSERT INTO initial_analyses(id,occurrence_id,state,input_snapshot_digest,created_by,created_at) VALUES(2,2,'Queued',?,NULL,?)`, strings.Repeat("b", 64), now)
		mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at) VALUES(2,'initial_analysis','analysis',2,'Queued','routed-test','initial-analysis-v2',?)`, now)
		mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,tool_catalog_json,created_at)
			SELECT 2,2,schema_kind,renderer_version,?,tool_catalog_json,? FROM attempt_input_snapshots WHERE id=1`,
			strings.Repeat("b", 64), now)
		mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,occurrence_id) VALUES(2,1,'user',?,2)`, strings.Repeat("d", 64))
		mustExec(t, db, `INSERT INTO attempt_connection_grants(id,attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at) VALUES(2,2,'chat_model',1,1,1,1,?)`, now)
		mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='routed-boot',connection_epoch=1,lease_until=?,runtime_release_version='routed-test',row_version=row_version+1 WHERE id=2`, lease)
	})
	fixture.resetFrames()

	boot := fixture.bootID
	rowEpoch := int64(1)
	fixture.service.alignReconcileReport(context.Background(), fixture.bootID, []attempt.View{{
		ID: 2, AttemptType: "initial_analysis", ScopeType: "analysis", ScopeID: 2,
		State: "Assigned", BootID: &boot, ConnectionEpoch: &rowEpoch,
	}}, nil)

	var state, reason string
	if err := fixture.db.QueryRow(`SELECT state,termination_reason FROM execution_attempts WHERE id=2`).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "Interrupted" || reason != "lease_expired" {
		t.Fatalf("unreported Assigned attempt=(%s,%s), want (Interrupted,lease_expired)", state, reason)
	}
	var analysisState string
	if err := fixture.db.QueryRow(`SELECT state FROM initial_analyses WHERE id=2`).Scan(&analysisState); err != nil {
		t.Fatal(err)
	}
	if analysisState != "Interrupted" {
		t.Fatalf("owning analysis state=%s, want Interrupted", analysisState)
	}
	// 不再发出任何 DispatchAttempt 帧（旧重派发已退役）。
	fixture.framesMu.Lock()
	defer fixture.framesMu.Unlock()
	for _, envelope := range fixture.frames {
		if envelope.GetDispatchAttempt() != nil {
			t.Fatalf("re-dispatch frame must not be sent anymore: %+v", envelope.GetDispatchAttempt())
		}
	}
}
