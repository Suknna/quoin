package inspection

// SQLite domain tests over the frozen schema: manual mixed Run creation
// against a real published config version, run_check child freezing
// (inspection_promql_execution_v1 with its Thanos grant, the real
// inspection_collection_v1 journey binding), deterministic browser local
// gaps, PromQL ResultProposal closure fences, and convergence semantics.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	_ "github.com/Suknna/quoin/internal/plugins/builtin"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	_ "github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/evidence"
	"github.com/Suknna/quoin/internal/quoin/execution"
	_ "modernc.org/sqlite"
)

// commandContext returns a background context carrying execution metadata for
// an authenticated admin command: the session proof reference points at the
// seeded session (schemaSeed), mirroring the admission layer. The runner
// re-verifies this session inside its transaction (VerifyExecutionSession) and
// attempt creators centrally persist the metadata onto new rows (ADR-0006).
func commandContext(t *testing.T) context.Context {
	t.Helper()
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlationID,
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "test-req"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

type testHarness struct {
	db        *sql.DB
	service   *Service
	attempts  *attempt.Service
	principal int64
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/test.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gen.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaSeed()); err != nil {
		t.Fatal(err)
	}
	previousProbeContractSource := connections.ProbeContractSource
	connections.ProbeContractSource = func() string { return string(gen.ConnectionProbesYAML) }
	t.Cleanup(func() { connections.ProbeContractSource = previousProbeContractSource })
	seedMetricsConnection(t, db)
	service := NewService(db)
	// Reads are fail-closed until the real read-only reader is wired; the
	// harness installs a mode=ro pool over the same file so every read path
	// exercises the split, never the write pool.
	var file string
	if err := db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&file); err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReadOnly(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if err := service.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	attempts := attempt.NewService(db)
	if err := attempts.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	return &testHarness{db: db, service: service, attempts: attempts, principal: 1}
}

// seedPlan 创建一个启用的独立巡检计划（fixture-metrics 接入，无业务系统）。
func (h *testHarness) seedPlan(t *testing.T, planKey string) string {
	t.Helper()
	return h.seedPlanWithCron(t, planKey, "* * * * *")
}

func (h *testHarness) seedPlanWithCron(t *testing.T, planKey, cron string) string {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var cronAny any
	if cron != "" {
		cronAny = cron
	}
	if _, err := h.db.Exec(`INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_json,scope_kind,cron,timezone,row_version,created_by,created_at,updated_at)
		VALUES(?, '巡检计划', 1, 1, 'thanos', 'promql_instant', '1', ?, ?, 'integration', ?, 'UTC', 1, 1, ?, ?)`,
		planKey, `{"expression":"up"}`, `{"kind":"integration"}`, cronAny, now, now); err != nil {
		t.Fatal(err)
	}
	return planKey
}

// seedMultiCheckPlan 创建 objects 范围计划：Run 创建时确定性展开两个检查。
func (h *testHarness) seedMultiCheckPlan(t *testing.T, planKey string) string {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// 对象必须携带已观测身份标签：空标签等同无界查询，Run 创建 fail closed。
	for _, identity := range []string{"obj-a", "obj-b"} {
		labels, _ := json.Marshal(map[string]string{"job": "fixture", "instance": identity})
		if _, err := h.db.Exec(`INSERT INTO observed_source_objects(connection_id,object_type,identity_key,labels_json,current,created_at)
			VALUES(1,'target',?,?,1,?)`, identity, string(labels), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.db.Exec(`INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_json,scope_kind,cron,timezone,row_version,created_by,created_at,updated_at)
		VALUES(?, '多检查计划', 1, 1, 'thanos', 'promql_instant', '1', ?, ?, 'objects', '* * * * *', 'UTC', 1, 1, ?, ?)`,
		planKey, `{"expression":"up"}`,
		`{"kind":"objects","objects":[{"objectType":"target","identityKey":"obj-a"},{"objectType":"target","identityKey":"obj-b"}]}`,
		now, now); err != nil {
		t.Fatal(err)
	}
	return planKey
}

// seedMetricsConnection creates the explicitly declared metrics authority
// through the public connection service before config upload freezes its ID.
// wireConnectionsReader attaches a mode=ro reader pool over the fixture
// database to the connections service: constructors are fail-closed now, so
// every fixture-owned service needs the read seam wired before its probe and
// query paths run.
func wireConnectionsReader(t *testing.T, db *sql.DB, service *connections.Service) {
	t.Helper()
	var file string
	if err := db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&file); err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReadOnly(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if err := service.SetReader(reader); err != nil {
		t.Fatal(err)
	}
}

// seedMetricsConnection creates the explicitly declared metrics authority
// through the public connection service before config upload freezes its ID.
func seedMetricsConnection(t *testing.T, db *sql.DB) {
	t.Helper()
	service := connections.NewService(db, func() ([]byte, error) { return make([]byte, 32), nil })
	wireConnectionsReader(t, db, service)
	created, err := service.Create(commandContext(t), connections.CreateInput{
		Name:          "fixture-metrics",
		Type:          connections.TypeThanos,
		NonSecretJSON: []byte(`{"type":"thanos","baseUrl":"https://metrics.fixture","authType":"none"}`),
	}, 1, "seed-metrics-connection")
	if err != nil {
		t.Fatalf("create metrics connection: %v", err)
	}
	if created.ID != 1 {
		t.Fatalf("metrics connection ID = %d, want 1", created.ID)
	}
	enableQualifiedMetricsConnection(t, db, service, created, "fixture-metrics-probe")
}

// seedAlternateMetricsConnection preserves an available metrics connection while
// tests prove that execution honors the unavailable connection explicitly named
// by the uploaded configuration instead of falling back to a global singleton.
func seedAlternateMetricsConnection(t *testing.T, db *sql.DB) {
	t.Helper()
	service := connections.NewService(db, func() ([]byte, error) { return make([]byte, 32), nil })
	wireConnectionsReader(t, db, service)
	created, err := service.Create(commandContext(t), connections.CreateInput{
		Name:          "alternate-metrics",
		Type:          connections.TypePrometheus,
		NonSecretJSON: []byte(`{"type":"prometheus","baseUrl":"https://alternate-metrics.fixture","authType":"none"}`),
	}, 1, "seed-alternate-metrics-connection")
	if err != nil {
		t.Fatalf("create alternate metrics connection: %v", err)
	}
	enableQualifiedMetricsConnection(t, db, service, created, "alternate-metrics-probe")
}

// enableQualifiedMetricsConnection exercises the public probe lifecycle so the
// enable qualification references a passed result on the connection's current
// revision and credential generation.
func enableQualifiedMetricsConnection(t *testing.T, db *sql.DB, service *connections.Service, summary connections.Summary, bootID string) {
	t.Helper()
	ctx := commandContext(t)
	attemptID, err := service.StartProbe(ctx, summary.Name)
	if err != nil {
		t.Fatalf("start metrics probe: %v", err)
	}
	// The supervisor-driven lifecycle steps restore the probe attempt's
	// persisted correlation themselves; the harness hands them an unwired
	// background scope (a wired user context is rejected as unrelated).
	if _, _, _, ok, err := service.BindQueuedToStream(context.Background(), attemptID, bootID, 1, 5*time.Minute); err != nil || !ok {
		t.Fatalf("bind metrics probe: %v ok=%v", err, ok)
	}
	if err := service.AcceptProbe(context.Background(), attemptID, bootID, 1); err != nil {
		t.Fatalf("accept metrics probe: %v", err)
	}
	result := connections.TypedProbeResult{
		Outcome: "passed", ResultDigest: fmt.Sprintf("%064x", attemptID),
		StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z",
	}
	child := &connections.TypedChild{Thanos: &connections.ThanosProbeChild{
		Query: "vector(1)", ResponseType: "vector", SampleCount: 1, SampleValue: "1", DetailJSON: `{"kind":"metrics"}`,
	}}
	if err := service.CommitProbeResult(context.Background(), attemptID, bootID, 1, result, child); err != nil {
		t.Fatalf("commit metrics probe: %v", err)
	}
	var probeID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM connection_probe_results WHERE attempt_id=?`, attemptID).Scan(&probeID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enable(ctx, summary.Name, summary.RowVersion, probeID, 1); err != nil {
		t.Fatalf("enable qualified metrics connection: %v", err)
	}
}

func schemaSeed() string {
	now := "2026-08-28T00:00:00Z"
	// The seeded admin session backs the command contexts' session proof
	// references: enabled initialized admin, current auth revision, unexpired
	// idle/absolute windows (VerifyExecutionSession re-checks all of these
	// inside the runner transaction).
	return strings.Join([]string{
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at)
		 VALUES (1,'admin','Admin','admin',1,1,'$argon2id$fixture',1,'` + now + `','` + now + `')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at)
		 VALUES (1,1,zeroblob(32),1,'test harness','` + now + `','` + now + `','2099-01-01T00:00:00Z','2099-01-01T00:00:00Z')`,
		`INSERT INTO root_key_state(id, binding_revision, verifier_nonce, verifier_ciphertext, bound_at) VALUES (1, 1, zeroblob(12), zeroblob(16), '` + now + `')`,
	}, "; ")
}

func (h *testHarness) promqlAttemptID(t *testing.T, runID int64) int64 {
	t.Helper()
	var id int64
	if err := h.db.QueryRow(`SELECT id FROM execution_attempts WHERE scope_type='run_check' AND scope_id=? ORDER BY check_key LIMIT 1`, runID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (h *testHarness) dispatchPromQL(t *testing.T, attemptID int64) {
	t.Helper()
	if err := h.attempts.BindToSlot(context.Background(), attemptID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(context.Background(), attemptID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
}

func pluginSuccessProposal(t *testing.T, h *testHarness, attemptID, runID int64, outcome string) []byte {
	t.Helper()
	var checkKey string
	if err := h.db.QueryRow(`SELECT check_key FROM execution_attempts WHERE id=?`, attemptID).Scan(&checkKey); err != nil {
		t.Fatal(err)
	}
	result := any(map[string]any{"resultType": "vector", "result": []any{
		map[string]any{"metric": map[string]string{"job": "quoin"}, "value": []any{0, "1"}},
	}})
	window := any(nil)
	gap := any(nil)
	if outcome != "success" {
		result = nil
		gap = "query_failed"
	}
	body, err := json.Marshal(map[string]any{
		"schemaKind": "inspection_plugin_result_v1", "attemptId": attemptID, "inspectionRunId": runID,
		"checkKey": checkKey, "outcome": outcome, "observedAt": "2026-08-28T00:00:01Z",
		"executionWindow": window, "result": result, "warnings": []string{}, "errors": []string{}, "gapReason": gap,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestCreatePlanRunAndPluginClosure(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "mixed-plan")
	ctx := commandContext(t)
	detail, err := h.service.CreatePlanRun(ctx, h.principal, "cmd-1", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "Running" || detail.EvidenceAt == nil {
		t.Fatalf("run should be Running with evidence_at, got %+v", detail)
	}
	attemptID := h.promqlAttemptID(t, detail.RunID)
	var kind string
	var grantCount int
	if err := h.db.QueryRow(`SELECT schema_kind FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "inspection_plugin_execution_v1" {
		t.Fatalf("plugin child input kind = %s", kind)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM attempt_connection_grants WHERE attempt_id=? AND purpose='config_thanos_query'`, attemptID).Scan(&grantCount); err != nil {
		t.Fatal(err)
	}
	if grantCount != 1 {
		t.Fatalf("plugin child grant count = %d", grantCount)
	}
	h.dispatchPromQL(t, attemptID)
	if err := h.service.CommitPluginProposal(context.Background(), attemptID, "plinth-boot", 1, pluginSuccessProposal(t, h, attemptID, detail.RunID, "success")); err != nil {
		t.Fatal(err)
	}
	var state, checkStatus string
	var evidenceID int64
	if err := h.db.QueryRow(`SELECT a.state, x.status, x.evidence_id FROM execution_attempts a JOIN inspection_check_results x ON x.attempt_id=a.id WHERE a.id=?`, attemptID).Scan(&state, &checkStatus, &evidenceID); err != nil {
		t.Fatal(err)
	}
	if state != "Succeeded" || checkStatus != "ok" || evidenceID < 1 {
		t.Fatalf("plugin closure = %s/%s/evidence %d", state, checkStatus, evidenceID)
	}
	// Evidence reads run on the same fail-closed read-only seam; wire the
	// harness reader instead of a bare write-pool constructor.
	evidenceService := evidence.NewService(h.db)
	if err := evidenceService.SetReader(h.service.Reader()); err != nil {
		t.Fatal(err)
	}
	evidenceDetail, err := evidenceService.Get(ctx, evidenceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidenceDetail.Connections) != 1 || evidenceDetail.Connections[0].Type != "thanos" {
		t.Fatalf("plugin evidence connections = %#v, want frozen Thanos grant", evidenceDetail.Connections)
	}
	final, err := h.service.GetRun(ctx, detail.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "Completed" || final.ReportCount != 0 {
		t.Fatalf("run should converge Completed without a report yet, got %s reports=%d", final.State, final.ReportCount)
	}
	if err := h.service.CommitPluginProposal(context.Background(), attemptID, "plinth-boot", 1, pluginSuccessProposal(t, h, attemptID, detail.RunID, "success")); err != nil {
		t.Fatalf("identical replay must be idempotent: %v", err)
	}
	mutated := pluginSuccessProposal(t, h, attemptID, detail.RunID, "success")
	mutated[10] = 'x'
	if err := h.service.CommitPluginProposal(context.Background(), attemptID, "plinth-boot", 1, mutated); err == nil {
		t.Fatal("mutated replay must be rejected")
	}
}

func TestCommitPluginProposalFences(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "mixed-plan")
	ctx := commandContext(t)
	detail, err := h.service.CreatePlanRun(ctx, h.principal, "cmd-1", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	attemptID := h.promqlAttemptID(t, detail.RunID)
	h.dispatchPromQL(t, attemptID)
	if err := h.service.CommitPluginProposal(context.Background(), attemptID, "other-boot", 1, pluginSuccessProposal(t, h, attemptID, detail.RunID, "success")); !errors.Is(err, attempt.ErrLateResult) {
		t.Fatalf("wrong boot must be a late result, got %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(pluginSuccessProposal(t, h, attemptID, detail.RunID, "success"), &parsed); err != nil {
		t.Fatal(err)
	}
	parsed["executionWindow"] = map[string]any{"startAt": "2026-08-27T23:00:00Z", "endAt": "2026-08-28T00:00:00Z", "stepSeconds": 60}
	windowed, _ := json.Marshal(parsed)
	if err := h.service.CommitPluginProposal(context.Background(), attemptID, "plinth-boot", 1, windowed); err == nil {
		t.Fatal("instant result with executionWindow must be rejected")
	}
	// A truncated collection must not fabricate Evidence or clear resources.
	parsed["executionWindow"] = nil
	parsed["outcome"] = "gap"
	parsed["gapReason"] = "partial_response"
	parsed["result"] = nil
	truncated, _ := json.Marshal(parsed)
	if err := h.service.CommitPluginProposal(context.Background(), attemptID, "plinth-boot", 1, truncated); err != nil {
		t.Fatalf("typed partial_response gap must commit: %v", err)
	}
	var status, gap string
	var evidenceCount int
	if err := h.db.QueryRow(`SELECT status, gap_reason, (SELECT COUNT(*) FROM evidence WHERE attempt_id=?) FROM inspection_check_results WHERE attempt_id=?`, attemptID, attemptID).Scan(&status, &gap, &evidenceCount); err != nil {
		t.Fatal(err)
	}
	if status != "gap" || gap != "partial_response" || evidenceCount != 0 {
		t.Fatalf("truncated result = %s/%s evidence=%d", status, gap, evidenceCount)
	}
}

func TestCreatePlanRunRejectionsAndReplay(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "mixed-plan")
	ctx := commandContext(t)
	_, err := h.service.CreatePlanRun(ctx, h.principal, "cmd-1", "missing-plan")
	var rejection *RejectionError
	if !errors.As(err, &rejection) || rejection.Code != "not_found" {
		t.Fatalf("unknown plan must reject not_found, got %v", err)
	}
	if _, err = h.service.CreatePlanRun(ctx, h.principal, "cmd-1", "missing-plan"); !errors.As(err, &rejection) || rejection.Code != "not_found" {
		t.Fatalf("replayed rejection must be identical, got %v", err)
	}
	if _, err = h.service.CreatePlanRun(ctx, h.principal, "cmd-1", "mixed-plan"); !errors.Is(err, ErrCommandReused) {
		t.Fatalf("command reuse with a different request must be refused, got %v", err)
	}
	first, err := h.service.CreatePlanRun(ctx, h.principal, "cmd-2", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CreatePlanRun(ctx, h.principal, "cmd-3", "mixed-plan"); !errors.As(err, &rejection) || rejection.Code != "active_conflict" {
		t.Fatalf("second concurrent run must surface active_conflict, got %v", err)
	}
	replayed, err := h.service.CreatePlanRun(ctx, h.principal, "cmd-2", "mixed-plan")
	if err != nil || replayed.RunID != first.RunID {
		t.Fatalf("same command must replay the committed run: %v %+v", err, replayed)
	}
}

func TestCreatePlanRunHonorsItsOwnConnection(t *testing.T) {
	h := newTestHarness(t)
	seedAlternateMetricsConnection(t, h.db)
	h.seedPlan(t, "mixed-plan")
	// The plan binds connection 1; disabling it must reject the command
	// instead of silently falling back to the alternate metrics connection.
	if _, err := h.db.Exec(`UPDATE connections SET enabled=0, row_version=row_version+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	_, err := h.service.CreatePlanRun(commandContext(t), h.principal, "cmd-1", "mixed-plan")
	var rejection *RejectionError
	if !errors.As(err, &rejection) || rejection.Code != "plan_disabled" {
		t.Fatalf("disabled bound connection must reject plan_disabled, got %v", err)
	}
	// The transaction rolled back: no orphan run or children survive.
	var runs, children int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM execution_attempts WHERE scope_type='run_check'`).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || children != 0 {
		t.Fatalf("rejected creation must leave no orphans: runs=%d children=%d", runs, children)
	}
}

func TestScheduledPlansRequireEnabledPlanAndConnection(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "enabled-plan")
	if _, err := h.db.Exec(`UPDATE inspection_plans SET enabled=0, row_version=row_version+1 WHERE plan_key='enabled-plan'`); err != nil {
		t.Fatal(err)
	}
	plans, err := h.service.ScheduledPlans(context.Background())
	if err != nil || len(plans) != 0 {
		t.Fatalf("disabled plan must not be scheduled: %#v %v", plans, err)
	}
	if _, err := h.db.Exec(`UPDATE inspection_plans SET enabled=1, row_version=row_version+1 WHERE plan_key='enabled-plan'`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE connections SET enabled=0, row_version=row_version+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	plans, err = h.service.ScheduledPlans(context.Background())
	if err != nil || len(plans) != 0 {
		t.Fatalf("disabled connection must not be scheduled: %#v %v", plans, err)
	}
	// 指标接入重新启用必须显式追加新的不可变 qualification 事件。
	qual := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.Exec(`INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at)
		SELECT 1, row_version+1, (SELECT id FROM connection_probe_results WHERE connection_id=1 AND outcome='passed' ORDER BY id DESC LIMIT 1), 1, ? FROM connections WHERE id=1`, qual); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE connections SET enabled=1, row_version=row_version+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	plans, err = h.service.ScheduledPlans(context.Background())
	if err != nil || len(plans) != 1 || plans[0].PlanKey != "enabled-plan" || plans[0].PlanID == 0 {
		t.Fatalf("scheduled plans = %#v %v", plans, err)
	}
}

func TestCreateScheduledPlanRunIsDeterministicAndRecordsUnavailableSlots(t *testing.T) {
	h := newTestHarness(t)
	h.seedMultiCheckPlan(t, "multi-plan")
	plans, err := h.service.ScheduledPlans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].PlanKey != "multi-plan" {
		t.Fatalf("scheduled plans = %+v", plans)
	}
	boundary := time.Date(2026, time.August, 28, 0, 30, 0, 0, time.UTC)
	availability := RuntimeAvailability{}
	first, err := h.service.CreateScheduledPlanRun(context.Background(), plans[0], boundary, availability)
	if err != nil {
		t.Fatal(err)
	}
	if first.TriggerKind != "schedule" || first.ScheduledFor == nil || *first.ScheduledFor != boundary.Format(time.RFC3339Nano) || first.State != "CompletedWithGaps" {
		t.Fatalf("scheduled unavailable run = %+v", first)
	}
	var gaps, attempts, undispatchedFailures int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_check_results WHERE run_id=? AND gap_reason='runtime_unavailable'`, first.RunID).Scan(&gaps); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM execution_attempts WHERE scope_type='run_check' AND scope_id=?`, first.RunID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM execution_attempts WHERE scope_type='run_check' AND scope_id=? AND state='Failed' AND runtime_slot IS NULL AND accepted_at IS NULL`, first.RunID).Scan(&undispatchedFailures); err != nil {
		t.Fatal(err)
	}
	if gaps != 2 || attempts != 2 || undispatchedFailures != 2 {
		t.Fatalf("unavailable slots must persist two undispatched terminal child gaps, gaps=%d attempts=%d failed=%d", gaps, attempts, undispatchedFailures)
	}
	// A fresh Service models process restart: its only duplicate memory is the
	// committed SQLite key, not a scheduler-local cursor.
	restarted := NewService(h.db)
	restarted.now = h.service.now
	replayed, err := restarted.CreateScheduledPlanRun(context.Background(), plans[0], boundary, availability)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.RunID != first.RunID {
		t.Fatalf("same deterministic key created a second run: %d then %d", first.RunID, replayed.RunID)
	}
	var count int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_runs WHERE plan_key='multi-plan' AND scheduled_for=?`, boundary.Format(time.RFC3339Nano)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("scheduled key rows = %d, want 1", count)
	}
}

func TestCreateScheduledPlanRunRecordsOverlapWithoutBackfill(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "mixed-plan")
	manual, err := h.service.CreatePlanRun(commandContext(t), h.principal, "manual-active", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	if manual.State != "Running" {
		t.Fatalf("manual run state = %s, want Running", manual.State)
	}
	plans, err := h.service.ScheduledPlans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	boundary := time.Date(2026, time.August, 28, 0, 30, 0, 0, time.UTC)
	detail, err := h.service.CreateScheduledPlanRun(context.Background(), plans[0], boundary, RuntimeAvailability{})
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "SkippedOverlap" || detail.TriggerKind != "schedule" {
		t.Fatalf("overlap result = %+v, want terminal SkippedOverlap", detail)
	}
}
