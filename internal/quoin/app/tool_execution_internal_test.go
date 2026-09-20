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

	// 物理模型调用 + pending tool call。execution_mode 列受既有 SQL CHECK
	// 约束（'quoin_routed' 词表迁移属 schema 契约侧）；编排分流以 attempt
	// 冻结目录为权威，不受该列值影响。
	mustExec(t, db, `INSERT INTO model_calls(id,attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,status,started_at)
		VALUES(1,1,1,0,'chat','chat',1,'initial-analysis-renderer-v1','initial-analysis-v2',?,?,?,?,?,4096,1024,0,'running',?)`,
		strings.Repeat("1", 64), "initial-analysis-tools-v5", strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64), strings.Repeat("5", 64), now)
	// tool_calls 的插入闭包要求模型调用已成功且携带完整输出；输出声明两个
	// 提案（thanos_query@0 供插件路径用例，artifact_read@1 供平台工具用例）。
	mustExec(t, db, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(1,1,'system',?,'system_contract'),(1,2,'system',?,'tool_schema')`, strings.Repeat("1", 64), strings.Repeat("1", 64))
	mustExec(t, db, `INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(1,1,'{"assistantText":"","finishReason":"tool_calls","tool_calls":[{"id":"call-routed-1","name":"thanos_query","arguments":{"query":"up"}},{"id":"call-routed-2","name":"artifact_read","arguments":{"artifactId":"1","offset":2,"limit":1}}]}',?,'tool_calls',?)`,
		strings.Repeat("6", 64), now)
	mustExec(t, db, `UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=1 AND status='running'`, now)
	arguments := []byte(`{"query":"up"}`)
	mustExec(t, db, `INSERT INTO tool_calls(id,attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at)
		VALUES(1,1,1,1,0,'call-routed-1','thanos_query','4',?,?,'worker_local','return_to_model','pending',?)`,
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
		VALUES(2,1,1,1,1,'call-routed-2','artifact_read','2',?,?,'worker_local','return_to_model','pending',?)`,
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

func sha256SumBytes(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}
