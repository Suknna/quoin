package app

// 注册制 HTTP 连接种类（ADR-0014）的本地只读探测执行器覆盖：探测请求形状来
// 自插件的冻结声明，经网关调用方出向（真实部署中是 Stele 网关流；测试以假
// 调用方替代并记录请求），结果按期望状态裁决并封存进共享 http typed child。
// 传输失败、状态不符与无契约类型各自按确定性失败收口；请求侧绝不携带凭证材
// 料——注入发生在网关之后的 Stele。

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/connections"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

// fakeProbeCaller 记录每次平台请求并回放固定响应（或传输失败）。
type fakeProbeCaller struct {
	requests []plugins.PlatformRequest
	status   int
	err      error
}

func (fake *fakeProbeCaller) Call(_ context.Context, req plugins.PlatformRequest) (*plugins.PlatformResponse, error) {
	fake.requests = append(fake.requests, req)
	if fake.err != nil {
		return nil, fake.err
	}
	return &plugins.PlatformResponse{StatusCode: fake.status, Body: []byte(`{"ok":true}`)}, nil
}

// stubLocalProbeCaller 把本地探测调用方替换为假网关。
func stubLocalProbeCaller(t *testing.T, fake *fakeProbeCaller) {
	t.Helper()
	previous := newLocalProbeCaller
	newLocalProbeCaller = func(*RuntimeService, localConnection) plugins.PlatformCaller { return fake }
	t.Cleanup(func() { newLocalProbeCaller = previous })
}

// newSyntheticProbeFixture 播种一个 synth-http 连接（禁用）及其 Queued 探测
// attempt，kinds 走真实冻结注册表（插件声明 GET /health 期望 200）。
func newSyntheticProbeFixture(t *testing.T, path ...string) (*sql.DB, *RuntimeService, int64) {
	t.Helper()
	probePath := "/health"
	if len(path) > 0 {
		probePath = path[0]
	}
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/synth-probe.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
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
	mustExec(t, db, `INSERT INTO connections(id,name,type,enabled,row_version,revalidation_required,created_at) VALUES(1,'synth-main','synth-http',0,1,0,?)`, now)
	mustExec(t, db, `INSERT INTO connection_revisions(id,connection_id,revision_seq,config_json,created_at) VALUES(1,1,1,'{"type":"synth-http","baseUrl":"https://synth.test","authType":"none"}',?)`, now)
	mustExec(t, db, `INSERT INTO credential_generations(id,connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(1,1,1,1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now)
	mustExec(t, db, `UPDATE connections SET current_revision_id=1,current_credential_generation_id=1,row_version=2 WHERE id=1`)
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,operation_correlation_id,initiator_type,initiator_id,created_at)
		VALUES(5,'connection_probe','connection',1,'Queued','q','corr-synth-probe','system',0,?)`, now)
	inputDigest := sha256HexOf([]byte(`{"connectionName":"synth-main"}`))
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(5,5,'connection_probe_v1','v1',?,?)`, inputDigest, now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(5,1,'connection_config',?,1)`, inputDigest)
	mustExec(t, db, `INSERT INTO attempt_connection_grants(id,attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(5,5,'synth-http_probe',1,1,1,?)`, now)

	reader := fixtureReadOnlyPool(t, db)
	connections.ProbeContractSource = func() string { return "contract_version: 1" }
	conns := connections.NewService(db, func() ([]byte, error) { return make([]byte, 32), nil })
	if err := conns.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	registry := plugins.NewRegistry()
	if err := registry.Register(plugins.Plugin{ID: "synthmetrics", Version: "1", DefaultEnabled: true,
		ConnectionKind: "synth-http", ConnectionTransport: plugins.ConnectionTransportHTTP,
		ConnectionAuthModes: []string{plugins.AuthModeNone}, ConnectionProbePath: probePath}); err != nil {
		t.Fatal(err)
	}
	conns.SetConnectionKinds(registry.ConnectionKindView())
	service := NewRuntimeControl(qruntime.NewService(), "test", conns, db)
	return db, service, 5
}

func TestLocalHTTPProbeSplitsDeclaredQueryFromGatewayPath(t *testing.T) {
	fake := &fakeProbeCaller{status: 200}
	stubLocalProbeCaller(t, fake)
	_, service, _ := newSyntheticProbeFixture(t, "/health?mode=read-only")
	service.runLocalExecutionPass(context.Background())
	if len(fake.requests) != 1 || fake.requests[0].Path != "/health" || fake.requests[0].Query.Get("mode") != "read-only" {
		t.Fatalf("probe query was not sent through the gateway query field: %+v", fake.requests)
	}
}

func TestLocalHTTPProbePassedResultOverFakeGateway(t *testing.T) {
	fake := &fakeProbeCaller{status: 200}
	stubLocalProbeCaller(t, fake)
	db, service, attemptID := newSyntheticProbeFixture(t)
	service.runLocalExecutionPass(context.Background())

	var state, boot string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	mustQuery(t, db, `SELECT boot_id FROM execution_attempts WHERE id=?`, &boot, attemptID)
	if state != "Succeeded" || boot != localExecutionBootID {
		t.Fatalf("probe binding=(%s,%s), want (Succeeded,%s)", state, boot, localExecutionBootID)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("gateway saw %d requests, want 1", len(fake.requests))
	}
	request := fake.requests[0]
	if request.Method != "GET" || request.Path != "/health" || len(request.Body) != 0 {
		t.Fatalf("probe request = %+v, want the frozen GET /health with no body", request)
	}
	if request.Header.Get("Authorization") != "" {
		t.Fatal("probe request must never carry credential material — injection happens at Stele")
	}
	var actionSet string
	var requestPath string
	var expectedStatus, observedStatus int
	mustQuery(t, db, `SELECT r.action_set_id FROM connection_probe_results r WHERE r.attempt_id=?`, &actionSet, attemptID)
	mustQuery(t, db, `SELECT h.request_path FROM connection_probe_results r JOIN http_connection_probe_results h ON h.probe_result_id=r.id WHERE r.attempt_id=?`, &requestPath, attemptID)
	mustQuery(t, db, `SELECT h.expected_status FROM connection_probe_results r JOIN http_connection_probe_results h ON h.probe_result_id=r.id WHERE r.attempt_id=?`, &expectedStatus, attemptID)
	mustQuery(t, db, `SELECT h.observed_status FROM connection_probe_results r JOIN http_connection_probe_results h ON h.probe_result_id=r.id WHERE r.attempt_id=?`, &observedStatus, attemptID)
	if actionSet != connections.HTTPProbeActionSetID || requestPath != "/health" || expectedStatus != 200 || observedStatus != 200 {
		t.Fatalf("probe result=(%s,%s,%d,%d), want (%s,/health,200,200)", actionSet, requestPath, expectedStatus, observedStatus, connections.HTTPProbeActionSetID)
	}
}

func TestLocalHTTPProbeStatusMismatchFailsClosed(t *testing.T) {
	fake := &fakeProbeCaller{status: 503}
	stubLocalProbeCaller(t, fake)
	db, service, attemptID := newSyntheticProbeFixture(t)
	service.runLocalExecutionPass(context.Background())

	var state, outcome, termination string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	mustQuery(t, db, `SELECT outcome FROM connection_probe_results WHERE attempt_id=?`, &outcome, attemptID)
	mustQuery(t, db, `SELECT termination_reason FROM execution_attempts WHERE id=?`, &termination, attemptID)
	if state != "Failed" || outcome != "failed" || termination != "invalid_response" {
		t.Fatalf("mismatch closure=(%s,%s,%s), want (Failed,failed,invalid_response)", state, outcome, termination)
	}
	var detailJSON string
	mustQuery(t, db, `SELECT h.detail_json FROM connection_probe_results r JOIN http_connection_probe_results h ON h.probe_result_id=r.id WHERE r.attempt_id=?`, &detailJSON, attemptID)
	if !strings.Contains(detailJSON, `"observedStatus":503`) || !strings.Contains(detailJSON, `"/health"`) {
		t.Fatalf("failed probe detail = %s, want the observed status and frozen path", detailJSON)
	}
}

func TestLocalHTTPProbeTransportErrorRecordsZeroStatus(t *testing.T) {
	fake := &fakeProbeCaller{err: errors.New("platform unreachable")}
	stubLocalProbeCaller(t, fake)
	db, service, attemptID := newSyntheticProbeFixture(t)
	service.runLocalExecutionPass(context.Background())

	var state, outcome string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	mustQuery(t, db, `SELECT outcome FROM connection_probe_results WHERE attempt_id=?`, &outcome, attemptID)
	if state != "Failed" || outcome != "failed" {
		t.Fatalf("transport failure closure=(%s,%s), want (Failed,failed)", state, outcome)
	}
	var observedStatus int
	mustQuery(t, db, `SELECT h.observed_status FROM connection_probe_results r JOIN http_connection_probe_results h ON h.probe_result_id=r.id WHERE r.attempt_id=?`, &observedStatus, attemptID)
	if observedStatus != 0 {
		t.Fatalf("transport failure recorded observed status %d, want 0", observedStatus)
	}
}

func TestLocalHTTPProbeWithoutContractFailsDeterministically(t *testing.T) {
	db, service, attemptID := newSyntheticProbeFixture(t)
	// 吊销：把 kinds 换成不含该插件的注册表视图后，执行器按确定性失败收口
	// （契约缺失的载荷无法通过 typed child 解析，收口事务拒绝并留待租约清
	// 扫收敛——与未知类型的既有语义一致）。
	registry := plugins.NewRegistry()
	registry.Register(plugins.Plugin{ID: "prometheus", Version: "1", DefaultEnabled: true,
		ConnectionKind: "prometheus", ConnectionTransport: plugins.ConnectionTransportHTTP,
		ConnectionAuthModes: []string{plugins.AuthModeNone}})
	service.Connections.SetConnectionKinds(registry.ConnectionKindView())
	fake := &fakeProbeCaller{status: 200}
	stubLocalProbeCaller(t, fake)
	service.runLocalExecutionPass(context.Background())

	if len(fake.requests) != 0 {
		t.Fatalf("revoked kind must not reach the gateway, saw %d requests", len(fake.requests))
	}
	// 无冻结契约的载荷无法通过 typed child 解析收口：attempt 保持 Running，
	// 由租约清扫按既有语义收敛（与未知类型的确定性缺口一致），绝不向网关
	// 发起请求。
	var state string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	if state != "Running" {
		t.Fatalf("revoked kind probe in state %s, want Running awaiting lease convergence", state)
	}
}
