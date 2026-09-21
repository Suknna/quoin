package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/connections"
	providerledger "github.com/Suknna/quoin/internal/quoin/connections/modelprovider"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	_ "modernc.org/sqlite"
)

// The supervisor ResultProposal reaches this parser after a real HTTP probe.
// Keep both Prometheus-compatible schema kinds explicit so a new producer
// cannot be accepted under the wrong connection identity.
func TestParseTypedChildAcceptsPrometheusProbeResult(t *testing.T) {
	detail := json.RawMessage(`{"kind":"prometheus","query":"vector(1)","responseType":"vector","sampleCount":1,"sampleValue":"1"}`)
	child, err := parseTypedChild("connection_probe_prometheus_v1", detail)
	if err != nil {
		t.Fatal(err)
	}
	if child.Thanos == nil || child.Thanos.Query != "vector(1)" || child.Thanos.ResponseType != "vector" || child.Thanos.SampleCount != 1 || child.Thanos.SampleValue != "1" {
		t.Fatalf("Prometheus typed child did not preserve the validated probe: %+v", child)
	}
}

func TestParseTypedChildRejectsCrossTypeMetricsResult(t *testing.T) {
	detail := json.RawMessage(`{"kind":"thanos","query":"vector(1)","responseType":"vector","sampleCount":1,"sampleValue":"1"}`)
	if _, err := parseTypedChild("connection_probe_prometheus_v1", detail); err == nil {
		t.Fatal("Prometheus schema kind must reject a Thanos detail identity")
	}
}

// ---------------------------------------------------------------------------
// model_provider typed 结果收口（review follow-up）：取得凭据前的失败以
// typed 契约经真实 commitProbeResultPayload → CommitProbeResult 封存，且仅
// 这 4 个提前失败码（outcome=failed、能力全 false）允许用 attempt 冻结的
// revision 权威补齐配置列；普通成功/完整失败保留载荷字段，由 schema 触发器
// 按冻结 config 严格拒绝不匹配——错误模型的 passed 结果绝不能被洗成资格。
// ---------------------------------------------------------------------------

// newModelProviderProbeFixture seeds one model_provider connection plus a
// connection_probe attempt bound to a live Plinth stream and accepted into
// Running, ready for a ResultProposal adjudication.
func newModelProviderProbeFixture(t *testing.T, configJSON string) (*sql.DB, *RuntimeService, int64) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/model-probe.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	mustExec(t, db, `INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now)
	mustExec(t, db, `INSERT INTO connections(id,name,type,enabled,row_version,revalidation_required,created_at) VALUES(2,'main-model','model_provider',0,1,0,?)`, now)
	mustExec(t, db, `INSERT INTO connection_revisions(id,connection_id,revision_seq,config_json,created_at) VALUES(2,2,1,?,?)`, configJSON, now)
	mustExec(t, db, `INSERT INTO credential_generations(id,connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(2,2,1,1,1,?,?,?)`, []byte("model-nonce!"), make([]byte, 16), now)
	mustExec(t, db, `UPDATE connections SET current_revision_id=2,current_credential_generation_id=2,row_version=2 WHERE id=2`)
	attemptID := int64(6)
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,operation_correlation_id,initiator_type,initiator_id,created_at)
		VALUES(6,'connection_probe','connection',2,'Queued','q','corr-model-probe','system',0,?)`, now)
	modelDigest := sha256HexOf([]byte(`{"connectionName":"main-model"}`))
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(6,6,'connection_probe_v1','v1',?,?)`, modelDigest, now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(6,1,'connection_config',?,2)`, modelDigest)
	mustExec(t, db, `INSERT INTO attempt_connection_grants(id,attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(6,6,'model_probe_chat',2,2,2,?),(7,6,'model_probe_embedding',2,2,2,?)`, now, now)
	// Bind to the live Plinth stream exactly like dispatchPlinthProbe, then
	// accept Assigned→Running through the real audited transition.
	mustExec(t, db, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='plinth-boot',connection_epoch=3,lease_until=?,runtime_release_version='q',row_version=row_version+1 WHERE id=?`,
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano), attemptID)
	reader := fixtureReadOnlyPool(t, db)
	connections.ProbeContractSource = func() string { return "contract_version: 1" }
	conns := connections.NewService(db, func() ([]byte, error) { return make([]byte, 32), nil })
	if err := conns.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	service := NewRuntimeControl(qruntime.NewService(), "test", conns, db)
	if err := conns.AcceptProbe(context.Background(), attemptID, "plinth-boot", 3); err != nil {
		t.Fatalf("accept probe attempt into Running: %v", err)
	}
	return db, service, attemptID
}

// probeCanonical builds the supervisor's canonical typed result payload.
func probeCanonical(t *testing.T, outcome, detail string) []byte {
	t.Helper()
	canonical, err := json.Marshal(probeResultJSON{
		Outcome: outcome, Detail: json.RawMessage(detail),
		StartedAt: "2026-09-21T00:00:00Z", FinishedAt: "2026-09-21T00:00:05Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

// sealedModelProviderChild reads the sealed typed child columns for one attempt.
func sealedModelProviderChild(t *testing.T, db *sql.DB, attemptID int64) (chatModelID string, embeddingModelID sql.NullString, contextBudget, maxOutput int, capabilities [7]int, detailJSON string) {
	t.Helper()
	err := db.QueryRow(`SELECT m.chat_model_id,m.embedding_model_id,m.context_budget_tokens,m.max_output_tokens,m.streaming_supported,m.native_tool_calling_supported,m.multi_tool_call_supported,m.cancellation_observed,m.usage_observed,m.request_id_observed,m.embedding_supported,m.detail_json
		FROM model_provider_connection_probe_results m JOIN connection_probe_results r ON r.id=m.probe_result_id WHERE r.attempt_id=?`, attemptID).
		Scan(&chatModelID, &embeddingModelID, &contextBudget, &maxOutput,
			&capabilities[0], &capabilities[1], &capabilities[2], &capabilities[3], &capabilities[4], &capabilities[5], &capabilities[6], &detailJSON)
	if err != nil {
		t.Fatalf("read sealed model provider child: %v", err)
	}
	return
}

// TestCommitProbeResultSealsPreCredentialTypedFailure 覆盖取得凭据前的失败
// （缺 grant / FetchCredentialGrant 失败 / grant 类型错）：supervisor 现在以
// typed 契约提案，Quoin 必须封存 header + typed child + Failed 终态，配置派
// 生列来自冻结 revision，能力位全部为 0（不伪造能力）。
func TestCommitProbeResultSealsPreCredentialTypedFailure(t *testing.T) {
	db, service, attemptID := newModelProviderProbeFixture(t, `{"type":"openai","baseUrl":"https://provider.test","chatModelId":"chat-1","contextBudgetTokens":512,"maxOutputTokens":128}`)
	canonical := probeCanonical(t, "failed", `{"kind":"model_provider","error":"credential_grant_fetch_failed"}`)
	if err := service.commitProbeResultPayload(context.Background(), attemptID, "connection_probe_model_provider_v1", canonical, "plinth-boot", 3); err != nil {
		t.Fatalf("pre-credential typed failure must close the attempt: %v", err)
	}
	var state, termination, outcome string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	mustQuery(t, db, `SELECT termination_reason FROM execution_attempts WHERE id=?`, &termination, attemptID)
	mustQuery(t, db, `SELECT outcome FROM connection_probe_results WHERE attempt_id=?`, &outcome, attemptID)
	if state != "Failed" || termination != "invalid_response" || outcome != "failed" {
		t.Fatalf("sealed=(%s,%s,%s), want (Failed,invalid_response,failed)", state, termination, outcome)
	}
	chatModelID, embeddingModelID, contextBudget, maxOutput, capabilities, detailJSON := sealedModelProviderChild(t, db, attemptID)
	if chatModelID != "chat-1" || contextBudget != 512 || maxOutput != 128 {
		t.Fatalf("config columns=(%q,%d,%d), want the frozen revision values (chat-1,512,128)", chatModelID, contextBudget, maxOutput)
	}
	if embeddingModelID.Valid {
		t.Fatalf("no embedding was observed, embedding model must stay NULL: %v", embeddingModelID)
	}
	for index, capability := range capabilities {
		if capability != 0 {
			t.Fatalf("capability column %d=%d, want 0 (no capability may be fabricated)", index, capability)
		}
	}
	if !strings.Contains(detailJSON, "credential_grant_fetch_failed") {
		t.Fatalf("detail_json=%s, want the stable failure code preserved", detailJSON)
	}
}

// TestCommitProbeResultSealsInvertedBudgetsConfig 覆盖合法可保存的倒挂预算
// config（contextBudgetTokens=100/maxOutputTokens=5000，写入路径只要求 > 0）：
// 提前失败结果从真实 commitProbeResultPayload 到 Failed 有界封存，封存预算
// 列如实保留冻结值——不做 Normalize 式钳制（钳制值不等于冻结值会被触发器
// 拒绝并遗留 Running）。schema CHECK 只要求各列 >= 1，如实值可存。
func TestCommitProbeResultSealsInvertedBudgetsConfig(t *testing.T) {
	db, service, attemptID := newModelProviderProbeFixture(t, `{"type":"openai","baseUrl":"https://provider.test","chatModelId":"chat-1","contextBudgetTokens":100,"maxOutputTokens":5000}`)
	canonical := probeCanonical(t, "failed", `{"kind":"model_provider","error":"credential_grant_fetch_failed"}`)
	if err := service.commitProbeResultPayload(context.Background(), attemptID, "connection_probe_model_provider_v1", canonical, "plinth-boot", 3); err != nil {
		t.Fatalf("pre-credential failure over an inverted-budgets config must close the attempt: %v", err)
	}
	var state, outcome string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	mustQuery(t, db, `SELECT outcome FROM connection_probe_results WHERE attempt_id=?`, &outcome, attemptID)
	if state != "Failed" || outcome != "failed" {
		t.Fatalf("sealed=(%s,%s), want (Failed,failed) — never a leftover Running", state, outcome)
	}
	chatModelID, _, contextBudget, maxOutput, capabilities, _ := sealedModelProviderChild(t, db, attemptID)
	if chatModelID != "chat-1" || contextBudget != 100 || maxOutput != 5000 {
		t.Fatalf("config columns=(%q,%d,%d), want the frozen values preserved as-is (chat-1,100,5000)", chatModelID, contextBudget, maxOutput)
	}
	for index, capability := range capabilities {
		if capability != 0 {
			t.Fatalf("capability column %d=%d, want 0 (no capability may be fabricated)", index, capability)
		}
	}
}

// 注：DB 真正缺失 model_probe_chat grant 的形态无法经合法 schema 路径构造
// （grants 追加不可删，且 schema 触发器在派发与 header 落库两层都强制要求
// model_probe_chat grant），supplementFrozenModelProviderColumns 的 ErrNoRows
// 拒绝路径因此只是防御性收口，不单测不可达状态。

// TestCommitProbeResultRejectsFullRunConfigMismatch 覆盖完整失败（已观察到
// 能力事实）的严格路径：载荷配置字段保留原样，与冻结 revision 不匹配时由
// schema 触发器拒绝，不做权威洗白，也不遗留任何封存行。
func TestCommitProbeResultRejectsFullRunConfigMismatch(t *testing.T) {
	db, service, attemptID := newModelProviderProbeFixture(t, `{"type":"openai","baseUrl":"https://provider.test","chatModelId":"chat-1","contextBudgetTokens":512,"maxOutputTokens":128}`)
	canonical := probeCanonical(t, "failed", `{"kind":"model_provider","chatModelId":"forged-model","contextBudgetTokens":1,"maxOutputTokens":1,"streamingSupported":true,"nativeToolCallingSupported":true,"multiToolCallSupported":false,"cancellationObserved":false,"usageObserved":true,"requestIdObserved":true,"embeddingSupported":false,"error":"chat stream 请求失败"}`)
	err := service.commitProbeResultPayload(context.Background(), attemptID, "connection_probe_model_provider_v1", canonical, "plinth-boot", 3)
	if err == nil {
		t.Fatal("full-run result with mismatched config columns must be rejected, never washed into the frozen revision")
	}
	var state string
	var headers int
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	mustQuery(t, db, `SELECT COUNT(*) FROM connection_probe_results WHERE attempt_id=?`, &headers, attemptID)
	if state != "Running" || headers != 0 {
		t.Fatalf("rejected mismatch left (%s,%d headers), want (Running,0)", state, headers)
	}
}

// seedProbeChatLedger 写入 passed 闭包触发器要求的真实模型调用事实（一次
// chat 成功 + 一次 chat 取消，与 supervisor 提交 passed 前的 ledger 对一致）。
func seedProbeChatLedger(t *testing.T, db *sql.DB, attemptID, chatGrantID int64) {
	t.Helper()
	reader := fixtureReadOnlyPool(t, db)
	for callSeq, completion := range []providerledger.Completion{
		{Outcome: "succeeded", ProviderRequestID: "req-chat", InputTokens: 1, OutputTokens: 1, TotalTokens: 2, FinishReason: "stop", ResponseJSON: `{"assistantText":"ready","finishReason":"stop","tool_calls":[]}`, ResponseDigest: fmt.Sprintf("%064x", 1), ResponseComplete: true},
		{Outcome: "cancelled", FailureReason: "cancelled", ProviderRequestID: "req-cancel"},
	} {
		callID, err := providerledger.Begin(context.Background(), db, reader, attemptID, chatGrantID, callSeq+1, 0, "chat", "chat-1", fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3), fmt.Sprintf("%064x", 4), fmt.Sprintf("%064x", 5), 512, 128, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := providerledger.WriteInputLineage(context.Background(), db, reader, callID, "chat", fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3), attemptID); err != nil {
			t.Fatal(err)
		}
		if err := providerledger.Complete(context.Background(), db, reader, attemptID, callID, completion); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCommitProbeResultRejectsPassedWithForgedChatModelId 覆盖 passed 伪造
// chatModelId 的拒绝回归：先证明诚实 passed 载荷（配置列与冻结 revision 一
// 致 + 真实调用事实）能正常封存，再用同一前置提交伪造配置的 passed——必须
// 被触发器拒绝，不得被权威补齐洗成合法资格。
func TestCommitProbeResultRejectsPassedWithForgedChatModelId(t *testing.T) {
	// 控制组：诚实载荷封存成功，证明前置事实本身成立。
	honestDB, honest, honestAttempt := newModelProviderProbeFixture(t, `{"type":"openai","baseUrl":"https://provider.test","chatModelId":"chat-1","contextBudgetTokens":512,"maxOutputTokens":128}`)
	seedProbeChatLedger(t, honestDB, honestAttempt, 6)
	honestCanonical := probeCanonical(t, "passed", `{"kind":"model_provider","chatModelId":"chat-1","embeddingModelId":null,"contextBudgetTokens":512,"maxOutputTokens":128,"streamingSupported":true,"nativeToolCallingSupported":true,"multiToolCallSupported":true,"cancellationObserved":true,"usageObserved":true,"requestIdObserved":true,"embeddingSupported":false}`)
	if err := honest.commitProbeResultPayload(context.Background(), honestAttempt, "connection_probe_model_provider_v1", honestCanonical, "plinth-boot", 3); err != nil {
		t.Fatalf("honest passed qualification must seal: %v", err)
	}
	var honestState string
	mustQuery(t, honestDB, `SELECT state FROM execution_attempts WHERE id=?`, &honestState, honestAttempt)
	if honestState != "Succeeded" {
		t.Fatalf("control attempt state=%s, want Succeeded", honestState)
	}

	// 回归组：同样的真实调用事实 + 伪造 chatModelId 的 passed 载荷。
	db, service, attemptID := newModelProviderProbeFixture(t, `{"type":"openai","baseUrl":"https://provider.test","chatModelId":"chat-1","contextBudgetTokens":512,"maxOutputTokens":128}`)
	seedProbeChatLedger(t, db, attemptID, 6)
	forged := probeCanonical(t, "passed", `{"kind":"model_provider","chatModelId":"forged-model","embeddingModelId":null,"contextBudgetTokens":512,"maxOutputTokens":128,"streamingSupported":true,"nativeToolCallingSupported":true,"multiToolCallSupported":true,"cancellationObserved":true,"usageObserved":true,"requestIdObserved":true,"embeddingSupported":false}`)
	if err := service.commitProbeResultPayload(context.Background(), attemptID, "connection_probe_model_provider_v1", forged, "plinth-boot", 3); err == nil {
		t.Fatal("passed result with a forged chatModelId must be rejected, never washed into a qualification")
	}
	var headers int
	mustQuery(t, db, `SELECT COUNT(*) FROM connection_probe_results WHERE attempt_id=?`, &headers, attemptID)
	if headers != 0 {
		t.Fatalf("forged passed result sealed %d headers, want 0", headers)
	}
}

// TestCommitProbeResultSealsWhenFrozenConfigOmitsBudgets 覆盖冻结 revision 省
// 略可选预算元数据（合法旧 revision 形态）：封存预算列按冻结探测契约的固定
// 边界（与 supervisor 派发边界一致）补齐，满足 schema CHECK 并有界终结。
func TestCommitProbeResultSealsWhenFrozenConfigOmitsBudgets(t *testing.T) {
	db, service, attemptID := newModelProviderProbeFixture(t, `{"type":"openai","baseUrl":"https://provider.test","chatModelId":"chat-1"}`)
	canonical := probeCanonical(t, "failed", `{"kind":"model_provider","error":"revision_config_unparseable"}`)
	if err := service.commitProbeResultPayload(context.Background(), attemptID, "connection_probe_model_provider_v1", canonical, "plinth-boot", 3); err != nil {
		t.Fatalf("budget-less frozen config must still converge: %v", err)
	}
	var state string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	if state != "Failed" {
		t.Fatalf("attempt state=%s, want Failed (never a leftover Running)", state)
	}
	chatModelID, _, contextBudget, maxOutput, _, _ := sealedModelProviderChild(t, db, attemptID)
	if chatModelID != "chat-1" {
		t.Fatalf("chat model id=%q, want the frozen chat-1", chatModelID)
	}
	if contextBudget != 32768 || maxOutput != 4096 {
		t.Fatalf("probe bounds=(%d,%d), want the frozen probe contract defaults (32768,4096)", contextBudget, maxOutput)
	}
}

// TestCommitProbeResultReplayAndFencingHold 覆盖同一结果的重发幂等与 boot/epoch
// 围栏：重复提交不再产生第二份 header，错误围栏直接拒绝且不动封存。
func TestCommitProbeResultReplayAndFencingHold(t *testing.T) {
	db, service, attemptID := newModelProviderProbeFixture(t, `{"type":"openai","baseUrl":"https://provider.test","chatModelId":"chat-1","contextBudgetTokens":512,"maxOutputTokens":128}`)
	canonical := probeCanonical(t, "failed", `{"kind":"model_provider","error":"missing_model_probe_chat_grant"}`)
	if err := service.commitProbeResultPayload(context.Background(), attemptID, "connection_probe_model_provider_v1", canonical, "plinth-boot", 3); err != nil {
		t.Fatal(err)
	}
	// 重发同一载荷：围栏后的迟到结果必须被拒绝且不产生第二份 header。
	if err := service.commitProbeResultPayload(context.Background(), attemptID, "connection_probe_model_provider_v1", canonical, "plinth-boot", 3); err == nil {
		t.Fatal("replayed result after closure must be rejected")
	}
	var headers int
	mustQuery(t, db, `SELECT COUNT(*) FROM connection_probe_results WHERE attempt_id=?`, &headers, attemptID)
	if headers != 1 {
		t.Fatalf("result headers=%d, want exactly one (idempotent replay)", headers)
	}
	// 另一个 Running attempt 携带错误 boot/epoch：围栏拒绝，attempt 保持 Running
	// 等待正确结果，不产生任何 header。
	fencedDB, fenced, fencedAttempt := newModelProviderProbeFixture(t, `{"type":"openai","baseUrl":"https://provider.test","chatModelId":"chat-2","contextBudgetTokens":64,"maxOutputTokens":32}`)
	err := fenced.commitProbeResultPayload(context.Background(), fencedAttempt, "connection_probe_model_provider_v1", canonical, "other-boot", 9)
	if err == nil || !strings.Contains(err.Error(), "fence") {
		t.Fatalf("wrong-boot result must hit the fence, got %v", err)
	}
	var fencedState string
	var fencedHeaders int
	mustQuery(t, fencedDB, `SELECT state FROM execution_attempts WHERE id=?`, &fencedState, fencedAttempt)
	mustQuery(t, fencedDB, `SELECT COUNT(*) FROM connection_probe_results WHERE attempt_id=?`, &fencedHeaders, fencedAttempt)
	if fencedState != "Running" || fencedHeaders != 0 {
		t.Fatalf("fenced attempt=(%s,%d headers), want (Running,0) — only the fence rejected the frame", fencedState, fencedHeaders)
	}
}

// TestFrozenModelProviderColumnsPreservesExplicitConfig 直测冻结配置派生规
// 则：显式值（含 maxOutput >= context 的合法可存倒挂形态）一律如实保留——
// 封存不假装执行 supervisor 的执行前归一；只有缺字段才用冻结探测契约缺省；
// 不可解析配置返回错误（fail-closed，不吞解析错误）。
func TestFrozenModelProviderColumnsPreservesExplicitConfig(t *testing.T) {
	explicit, err := connections.FrozenModelProviderColumns([]byte(`{"type":"openai","baseUrl":"https://p","chatModelId":"m","embeddingModelId":"e","contextBudgetTokens":100,"maxOutputTokens":50}`))
	if err != nil {
		t.Fatal(err)
	}
	if explicit.ChatModelID != "m" || explicit.EmbeddingModelID == nil || *explicit.EmbeddingModelID != "e" || explicit.ContextBudgetTokens != 100 || explicit.MaxOutputTokens != 50 {
		t.Fatalf("explicit config must stay authoritative: %+v", explicit)
	}
	missing, err := connections.FrozenModelProviderColumns([]byte(`{"chatModelId":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if missing.ContextBudgetTokens != 32768 || missing.MaxOutputTokens != 4096 {
		t.Fatalf("omitted budgets must fall back to the frozen probe defaults: %+v", missing)
	}
	// 合法可保存的倒挂预算（写入路径只要求 > 0）：如实保留，不做 Normalize
	// 式钳制——触发器要求封存列与冻结值相等，钳制会让提前失败永远封不了。
	inverted, err := connections.FrozenModelProviderColumns([]byte(`{"chatModelId":"m","contextBudgetTokens":100,"maxOutputTokens":5000}`))
	if err != nil {
		t.Fatal(err)
	}
	if inverted.ContextBudgetTokens != 100 || inverted.MaxOutputTokens != 5000 {
		t.Fatalf("inverted explicit budgets must be preserved as-is, got (%d,%d)", inverted.ContextBudgetTokens, inverted.MaxOutputTokens)
	}
	if _, err := connections.FrozenModelProviderColumns([]byte(`{"chatModelId":`)); err == nil {
		t.Fatal("unparseable frozen config must return an error instead of being silently swallowed")
	}
}
