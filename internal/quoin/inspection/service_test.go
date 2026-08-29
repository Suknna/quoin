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

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	_ "github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/browser"
	"github.com/Suknna/quoin/internal/quoin/businesssystem"
	quoinconfig "github.com/Suknna/quoin/internal/quoin/config"
	"github.com/Suknna/quoin/internal/quoin/labelcontract"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
	_ "modernc.org/sqlite"
)

const mixedSystemYAML = `system_key: payments
display_name: 支付系统
enabled: true
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
resource_discoveries: []
inspection_plans:
  - key: mixed-plan
    display_name: 混合巡检
    cron: "* * * * *"
    checks:
      - key: up-instant
        display_name: Up Instant
        analysis_question: 当前可用吗？
        kind: promql
        query:
          mode: instant
          expression: 'up{business_system="payments"}'
      - key: status-page
        display_name: 状态页
        analysis_question: 状态页是否正常?
        kind: browser
        journey_id: page.status-marker.v1
        journey_params:
          path: /status
  - key: intentionally-empty
    display_name: 暂无检查的计划
    cron: "* * * * *"
    checks: []
`

type testHarness struct {
	db        *sql.DB
	service   *Service
	attempts  *attempt.Service
	systems   *businesssystem.Service
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
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaSeed()); err != nil {
		t.Fatal(err)
	}
	contracts := labelcontract.NewService(db)
	if _, err := contracts.CreateDraft(context.Background(), 1, "seed-contract-0001", []byte("label_contract:\n  business_system_label: business_system\n"), quoinconfig.Limits{}); err != nil {
		t.Fatal(err)
	}
	if _, err := contracts.Activate(context.Background(), 1, "seed-activate-0001", labelcontract.ActivateInput{ContractVersion: 1, ExpectedStateRowVersion: 1, ExpectedTargetRowVersion: 1}); err != nil {
		t.Fatal(err)
	}
	h := &testHarness{db: db, service: NewService(db), attempts: attempt.NewService(db), systems: businesssystem.NewService(db), principal: 1}
	h.service.JourneyCore = h.systems.CommitJourneyProposalScoped
	return h
}

// publishMixedPlan uploads and publishes the mixed plan through the real
// business-system path and returns the published version id.
func (h *testHarness) publishMixedPlan(t *testing.T) int64 {
	t.Helper()
	draft, err := h.systems.Upload(context.Background(), h.principal, "seed-upload-"+fmt.Sprint(time.Now().UnixNano()), businesssystem.UploadInput{
		YAMLBody: []byte(mixedSystemYAML), TargetLabelContractVersion: 1,
	}, quoinconfig.Limits{})
	if err != nil {
		t.Fatalf("upload mixed plan: %v", err)
	}
	var versionID int64
	if _, err := fmt.Sscanf(draft.ID, "%d", &versionID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.systems.Publish(context.Background(), h.principal, "seed-publish-"+fmt.Sprint(time.Now().UnixNano()), "payments", versionID, nil); err != nil {
		t.Fatalf("publish mixed plan: %v", err)
	}
	return versionID
}

// seedBrowserIdentity mirrors the verification-journey harness: a real
// identity revision with the embedded probe journey, optionally published
// through the real manual-login/publish path.
func (h *testHarness) seedBrowserIdentity(t *testing.T, withProfile bool) {
	t.Helper()
	browsers := browser.NewService(h.db)
	_, catalogVersion, catalogDigest, err := quoinconfig.JourneyCatalog()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.Exec(`INSERT OR IGNORE INTO sessions(id,user_id,session_token_digest,client_label,auth_revision_at_issue,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(91,1,?, 'test',1,?,?,?,'2030-01-01T00:00:00Z')`, make([]byte, 32), now, now, now); err != nil {
		t.Fatal(err)
	}
	identity, _, err := browsers.Configure(context.Background(), 1, browser.ConfigureInput{
		SystemKey: "payments", Name: "状态身份", StartURL: "http://fixture.internal/login",
		Probe:           browser.ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: json.RawMessage(`{"authenticatedUrlPrefix":"http://fixture.internal/authenticated"}`)},
		ClientCommandID: "seed-identity-" + fmt.Sprint(time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("configure identity: %v", err)
	}
	if !withProfile {
		return
	}
	operation, err := browsers.StartManualLogin(context.Background(), "payments", 1, 91, identity.RowVersion, "seed-login-"+fmt.Sprint(time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("start manual login: %v", err)
	}
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id='boot-pub',lintel_connection_epoch=1,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, operation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Running',started_at=?,row_version=row_version+1 WHERE id=? AND state='Starting'`, now, operation.ID); err != nil {
		t.Fatal(err)
	}
	request, err := browsers.PreparePublish(context.Background(), "payments", operation.ID, 1, operation.RowVersion+2, "seed-publish-profile-"+fmt.Sprint(time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("prepare publish: %v", err)
	}
	if err := browsers.HandlePublishResult(context.Background(), browser.PublishResult{
		OperationID: operation.ID, CommandID: request.CommandID, Generation: request.NewGeneration,
		ChromiumRevision: "rev-test", ManifestDigest: make([]byte, 32), Accepted: true, BootID: "boot-pub", Epoch: 1,
		Probe: browser.ProbeResult{Phase: "publish", Result: "Authenticated", JourneyID: "authentication.url-prefix.v1", JourneyVersion: 1, CatalogDigest: catalogDigest, CatalogVersion: catalogVersion, ObservedAt: now},
	}); err != nil {
		t.Fatalf("publish profile: %v", err)
	}
	if _, err := h.db.Exec(`UPDATE browser_operations SET stop_confirmed_at=?,stop_confirmation_basis='stop_ack',row_version=row_version+1 WHERE id=? AND stop_confirmed_at IS NULL`, now, operation.ID); err != nil {
		t.Fatal(err)
	}
}

func schemaSeed() string {
	now := "2026-08-28T00:00:00Z"
	return strings.Join([]string{
		`INSERT INTO label_contract_state(id,row_version,updated_at) VALUES(1,1,'` + now + `')`,
		`INSERT INTO users(id,username,display_name,role,enabled,password_phc,row_version,created_at,updated_at)
		 VALUES (1,'admin','Admin','admin',1,'$argon2id$fixture',1,'` + now + `','` + now + `')`,
		`INSERT INTO root_key_state(id, binding_revision, verifier_nonce, verifier_ciphertext, bound_at) VALUES (1, 1, zeroblob(12), zeroblob(16), '` + now + `')`,
		`INSERT INTO connections(id, name, type, enabled, row_version, revalidation_required, created_at) VALUES (1, 'thanos', 'thanos', 1, 1, 0, '` + now + `')`,
		`INSERT INTO connection_revisions(id, connection_id, revision_seq, config_json, created_at) VALUES (1, 1, 1, '{}', '` + now + `')`,
		`INSERT INTO credential_generations(id, connection_id, generation_seq, envelope_version, key_binding_revision, nonce, ciphertext, created_at) VALUES (1, 1, 1, 1, 1, zeroblob(12), zeroblob(16), '` + now + `')`,
		`UPDATE connections SET current_revision_id=1, current_credential_generation_id=1, row_version=2 WHERE id=1`,
		`INSERT INTO runtime_slots(slot, state, row_version, created_at) VALUES ('plinth','unregistered',1,'` + now + `'),('lintel','unregistered',1,'` + now + `')`,
		`INSERT INTO runtime_credentials(slot, generation, token_digest, created_at, confirmed_at, first_authenticated_at, row_version) VALUES ('plinth', 1, zeroblob(32), '` + now + `', '` + now + `', NULL, 1),('lintel', 1, zeroblob(32), '` + now + `', '` + now + `', NULL, 1)`,
		`UPDATE runtime_slots SET state='registered', current_credential_id=(SELECT id FROM runtime_credentials WHERE slot='plinth'), row_version=2 WHERE slot='plinth'`,
		`UPDATE runtime_slots SET state='registered', current_credential_id=(SELECT id FROM runtime_credentials WHERE slot='lintel'), row_version=2 WHERE slot='lintel'`,
	}, "; ")
}

func (h *testHarness) promqlAttemptID(t *testing.T, runID int64) int64 {
	t.Helper()
	var id int64
	if err := h.db.QueryRow(`SELECT id FROM execution_attempts WHERE scope_type='run_check' AND scope_id=? AND check_key='up-instant'`, runID).Scan(&id); err != nil {
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

func promqlSuccessProposal(attemptID, runID int64, outcome, mode string) []byte {
	result := any(map[string]any{"resultType": "vector", "result": []any{
		map[string]any{"metric": map[string]string{"job": "quoin"}, "value": []any{0, "1"}},
	}})
	window := any(nil)
	gap := any(nil)
	if outcome != "success" {
		result = nil
		gap = "query_failed"
	}
	if mode == "range" && outcome == "success" {
		window = map[string]any{"startAt": "2026-08-27T23:00:00Z", "endAt": "2026-08-28T00:00:00Z", "stepSeconds": 60}
	}
	body, _ := json.Marshal(map[string]any{
		"schemaKind": "inspection_promql_result_v1", "attemptId": attemptID, "inspectionRunId": runID,
		"checkKey": "up-instant", "queryMode": mode, "outcome": outcome, "observedAt": "2026-08-28T00:00:01Z",
		"executionWindow": window, "result": result, "warnings": []string{}, "errors": []string{}, "gapReason": gap,
	})
	return body
}

func TestCreateMixedRunAndPromQLClosure(t *testing.T) {
	h := newTestHarness(t)
	h.publishMixedPlan(t)
	h.seedBrowserIdentity(t, false)
	ctx := context.Background()
	detail, err := h.service.CreateInspectionRun(ctx, h.principal, "cmd-1", "payments", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "Running" || detail.EvidenceAt == nil {
		t.Fatalf("run should be Running with evidence_at, got %+v", detail)
	}
	// Without a published profile the browser check settles terminally as an
	// authentication_required gap while the PromQL child stays Queued.
	var browserState, browserStatus, browserGap string
	if err := h.db.QueryRow(`
		SELECT a.state, x.status, x.gap_reason FROM inspection_check_results x
		JOIN execution_attempts a ON a.id=x.attempt_id WHERE x.run_id=? AND x.check_key='status-page'`, detail.RunID).
		Scan(&browserState, &browserStatus, &browserGap); err != nil {
		t.Fatal(err)
	}
	if browserState != "Failed" || browserStatus != "gap" || browserGap != "authentication_required" {
		t.Fatalf("browser child should settle authentication_required, got %s/%s/%s", browserState, browserStatus, browserGap)
	}
	attemptID := h.promqlAttemptID(t, detail.RunID)
	var kind string
	var grantCount int
	if err := h.db.QueryRow(`SELECT schema_kind FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "inspection_promql_execution_v1" {
		t.Fatalf("promql child input kind = %s", kind)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM attempt_connection_grants WHERE attempt_id=? AND purpose='config_thanos_query'`, attemptID).Scan(&grantCount); err != nil {
		t.Fatal(err)
	}
	if grantCount != 1 {
		t.Fatalf("promql child grant count = %d", grantCount)
	}
	h.dispatchPromQL(t, attemptID)
	if err := h.service.CommitPromQLProposal(ctx, attemptID, "plinth-boot", 1, promqlSuccessProposal(attemptID, detail.RunID, "success", "instant")); err != nil {
		t.Fatal(err)
	}
	var state, checkStatus string
	var evidenceID int64
	if err := h.db.QueryRow(`SELECT a.state, x.status, x.evidence_id FROM execution_attempts a JOIN inspection_check_results x ON x.attempt_id=a.id WHERE a.id=?`, attemptID).Scan(&state, &checkStatus, &evidenceID); err != nil {
		t.Fatal(err)
	}
	if state != "Succeeded" || checkStatus != "ok" || evidenceID < 1 {
		t.Fatalf("promql closure = %s/%s/evidence %d", state, checkStatus, evidenceID)
	}
	final, err := h.service.GetRun(ctx, "payments", detail.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "CompletedWithGaps" || final.ReportCount != 0 {
		t.Fatalf("run should converge CompletedWithGaps without a report yet, got %s reports=%d", final.State, final.ReportCount)
	}
	// The browser gap stayed visible instead of erasing the run's evidence.
	var gaps int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_check_results WHERE run_id=? AND status='gap'`, detail.RunID).Scan(&gaps); err != nil {
		t.Fatal(err)
	}
	if gaps != 1 {
		t.Fatalf("expected one persisted gap, got %d", gaps)
	}
	if err := h.service.CommitPromQLProposal(ctx, attemptID, "plinth-boot", 1, promqlSuccessProposal(attemptID, detail.RunID, "success", "instant")); err != nil {
		t.Fatalf("identical replay must be idempotent: %v", err)
	}
	mutated := promqlSuccessProposal(attemptID, detail.RunID, "success", "instant")
	mutated[10] = 'x'
	if err := h.service.CommitPromQLProposal(ctx, attemptID, "plinth-boot", 1, mutated); err == nil {
		t.Fatal("mutated replay must be rejected")
	}
}

func TestBusyIdentitySettlesLocalGap(t *testing.T) {
	h := newTestHarness(t)
	h.publishMixedPlan(t)
	h.seedBrowserIdentity(t, true)
	// An open manual-login operation holds the identity: the browser check
	// must settle as an identity_busy local gap on its Queued child.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, catalogVersion, catalogDigest, err := quoinconfig.JourneyCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO browser_operations(identity_id, identity_revision_id, kind, actor_user_id, actor_session_id, state, journey_catalog_digest, journey_catalog_version, requested_at, row_version)
		SELECT i.id, i.current_revision_id, 'manual_login', 1, 91, 'Queued', ?, ?, ?, 1 FROM browser_identities i
		JOIN business_systems s ON s.id=i.business_system_id WHERE s.key='payments'`, catalogDigest, catalogVersion, now); err != nil {
		t.Fatal(err)
	}
	detail, err := h.service.CreateInspectionRun(context.Background(), h.principal, "cmd-1", "payments", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	var state, status, gap string
	if err := h.db.QueryRow(`
		SELECT a.state, x.status, x.gap_reason FROM inspection_check_results x
		JOIN execution_attempts a ON a.id=x.attempt_id WHERE x.run_id=? AND x.check_key='status-page'`, detail.RunID).
		Scan(&state, &status, &gap); err != nil {
		t.Fatal(err)
	}
	if state != "Succeeded" || status != "gap" || gap != "identity_busy" {
		t.Fatalf("busy identity should settle identity_busy on a closed child, got %s/%s/%s", state, status, gap)
	}
}

func TestCommitPromQLProposalFences(t *testing.T) {
	h := newTestHarness(t)
	h.publishMixedPlan(t)
	h.seedBrowserIdentity(t, false)
	ctx := context.Background()
	detail, err := h.service.CreateInspectionRun(ctx, h.principal, "cmd-1", "payments", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	attemptID := h.promqlAttemptID(t, detail.RunID)
	h.dispatchPromQL(t, attemptID)
	if err := h.service.CommitPromQLProposal(ctx, attemptID, "other-boot", 1, promqlSuccessProposal(attemptID, detail.RunID, "success", "instant")); !errors.Is(err, attempt.ErrLateResult) {
		t.Fatalf("wrong boot must be a late result, got %v", err)
	}
	var parsed map[string]any
	instant := promqlSuccessProposal(attemptID, detail.RunID, "success", "instant")
	if err := json.Unmarshal(instant, &parsed); err != nil {
		t.Fatal(err)
	}
	parsed["executionWindow"] = map[string]any{"startAt": "2026-08-27T23:00:00Z", "endAt": "2026-08-28T00:00:00Z", "stepSeconds": 60}
	windowed, _ := json.Marshal(parsed)
	if err := h.service.CommitPromQLProposal(ctx, attemptID, "plinth-boot", 1, windowed); err == nil {
		t.Fatal("instant result with executionWindow must be rejected")
	}
	delete(parsed, "executionWindow")
	parsed["queryMode"] = "range"
	modeMismatch, _ := json.Marshal(parsed)
	if err := h.service.CommitPromQLProposal(ctx, attemptID, "plinth-boot", 1, modeMismatch); err == nil {
		t.Fatal("queryMode mismatching the frozen input must be rejected")
	}
}

func TestCreateRunRejectionsAndReplay(t *testing.T) {
	h := newTestHarness(t)
	h.publishMixedPlan(t)
	ctx := context.Background()
	_, err := h.service.CreateInspectionRun(ctx, h.principal, "cmd-1", "payments", "missing-plan")
	var rejection *RejectionError
	if !errors.As(err, &rejection) || rejection.Code != "not_found" {
		t.Fatalf("unknown plan must reject not_found, got %v", err)
	}
	if _, err = h.service.CreateInspectionRun(ctx, h.principal, "cmd-1", "payments", "missing-plan"); !errors.As(err, &rejection) || rejection.Code != "not_found" {
		t.Fatalf("replayed rejection must be identical, got %v", err)
	}
	if _, err = h.service.CreateInspectionRun(ctx, h.principal, "cmd-1", "payments", "mixed-plan"); !errors.Is(err, ErrCommandReused) {
		t.Fatalf("command reuse with a different request must be refused, got %v", err)
	}
	first, err := h.service.CreateInspectionRun(ctx, h.principal, "cmd-2", "payments", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CreateInspectionRun(ctx, h.principal, "cmd-3", "payments", "mixed-plan"); !errors.As(err, &rejection) || rejection.Code != "active_conflict" {
		t.Fatalf("second concurrent run must surface active_conflict, got %v", err)
	}
	replayed, err := h.service.CreateInspectionRun(ctx, h.principal, "cmd-2", "payments", "mixed-plan")
	if err != nil || replayed.RunID != first.RunID {
		t.Fatalf("same command must replay the committed run: %v %+v", err, replayed)
	}
}

func TestBrowserChildFreezesRealCatalogBinding(t *testing.T) {
	h := newTestHarness(t)
	h.publishMixedPlan(t)
	h.seedBrowserIdentity(t, true)
	if _, err := h.db.Exec(`UPDATE browser_operations SET stop_confirmed_at='2026-08-28T00:00:00Z', stop_confirmation_basis='stop_ack' WHERE stop_confirmed_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	detail, err := h.service.CreateInspectionRun(context.Background(), h.principal, "cmd-1", "payments", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	// A ready free identity leaves the browser child bare and Queued. Admission
	// serializes its identity, creates the operation and only then freezes the
	// operation-bound input so Start and Dispatch use identical bytes.
	var state string
	var snapshots int
	if err := h.db.QueryRow(`
		SELECT a.state, (SELECT COUNT(*) FROM attempt_input_snapshots s WHERE s.attempt_id=a.id)
		FROM execution_attempts a
		WHERE a.scope_type='run_check' AND a.scope_id=? AND a.check_key='status-page'`, detail.RunID).
		Scan(&state, &snapshots); err != nil {
		t.Fatal(err)
	}
	if state != "Queued" || snapshots != 0 {
		t.Fatalf("ready identity child should stay bare Queued before admission, got %s/%d", state, snapshots)
	}
	admitted, err := h.service.AdmitNextJourneyChild(context.Background())
	if err != nil || !admitted {
		t.Fatalf("admission must create the operation-bound snapshot: admitted=%v err=%v", admitted, err)
	}
	var digest string
	if err := h.db.QueryRow(`SELECT s.content_digest FROM execution_attempts a JOIN attempt_input_snapshots s ON s.attempt_id=a.id WHERE a.scope_type='run_check' AND a.scope_id=? AND a.check_key='status-page'`, detail.RunID).Scan(&digest); err != nil || len(digest) != 64 {
		t.Fatalf("admitted child lacks frozen input: digest=%q err=%v", digest, err)
	}
	var settled int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_check_results WHERE run_id=? AND check_key='status-page'`, detail.RunID).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if settled != 0 {
		t.Fatalf("ready free identity must not settle a result at creation, got %d", settled)
	}
}

func TestCreateRunThanosUnavailableTyped(t *testing.T) {
	h := newTestHarness(t)
	if _, err := h.db.Exec(`UPDATE connections SET enabled=0, row_version=row_version+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	h.publishMixedPlan(t)
	h.seedBrowserIdentity(t, false)
	_, err := h.service.CreateInspectionRun(context.Background(), h.principal, "cmd-1", "payments", "mixed-plan")
	if !errors.Is(err, thanos.ErrThanosUnavailable) {
		t.Fatalf("unusable thanos must surface the typed sentinel, got %v", err)
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

func TestCreateScheduledInspectionRunDoesNotMislabelThanosFailure(t *testing.T) {
	h := newTestHarness(t)
	if _, err := h.db.Exec(`UPDATE connections SET enabled=0, row_version=row_version+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	versionID := h.publishMixedPlan(t)
	plans, err := h.service.ScheduledPlans(context.Background())
	if err != nil || len(plans) != 1 {
		t.Fatalf("scheduled plans = %#v, %v", plans, err)
	}
	_, err = h.service.CreateScheduledInspectionRun(context.Background(), plans[0], time.Date(2026, time.August, 29, 0, 0, 0, 0, time.UTC), RuntimeAvailability{Plinth: true, Lintel: true})
	if !errors.Is(err, thanos.ErrThanosUnavailable) {
		t.Fatalf("unavailable Thanos must not become runtime_unavailable, got %v", err)
	}
	var runs, gaps int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_runs WHERE config_version_id=?`, versionID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_check_results WHERE gap_reason='runtime_unavailable'`).Scan(&gaps); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || gaps != 0 {
		t.Fatalf("Thanos failure must roll back rather than forge runtime gap: runs=%d gaps=%d", runs, gaps)
	}
}

func TestCreateScheduledInspectionRunIsDeterministicAndRecordsUnavailableSlots(t *testing.T) {
	h := newTestHarness(t)
	versionID := h.publishMixedPlan(t)
	plans, err := h.service.ScheduledPlans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].ConfigVersionID != versionID {
		t.Fatalf("current scheduled plans = %+v, want version %d", plans, versionID)
	}
	boundary := time.Date(2026, time.August, 28, 0, 30, 0, 0, time.UTC)
	availability := RuntimeAvailability{}
	first, err := h.service.CreateScheduledInspectionRun(context.Background(), plans[0], boundary, availability)
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
	replayed, err := restarted.CreateScheduledInspectionRun(context.Background(), plans[0], boundary, availability)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.RunID != first.RunID {
		t.Fatalf("same deterministic key created a second run: %d then %d", first.RunID, replayed.RunID)
	}
	var count int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_runs WHERE business_system_id=(SELECT id FROM business_systems WHERE key='payments') AND plan_key='mixed-plan' AND scheduled_for=?`, boundary.Format(time.RFC3339Nano)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("scheduled key rows = %d, want 1", count)
	}
}

func TestScheduledPlansIgnorePlansWithoutChecks(t *testing.T) {
	h := newTestHarness(t)
	h.publishMixedPlan(t)

	plans, err := h.service.ScheduledPlans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].PlanKey != "mixed-plan" {
		t.Fatalf("scheduled plans = %#v, want only checked mixed-plan", plans)
	}
}

func TestCreateScheduledInspectionRunRecordsOverlapWithoutBackfill(t *testing.T) {
	h := newTestHarness(t)
	h.publishMixedPlan(t)
	h.seedBrowserIdentity(t, false)
	manual, err := h.service.CreateInspectionRun(context.Background(), h.principal, "manual-active", "payments", "mixed-plan")
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
	detail, err := h.service.CreateScheduledInspectionRun(context.Background(), plans[0], boundary, RuntimeAvailability{})
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "SkippedOverlap" || detail.TriggerKind != "schedule" {
		t.Fatalf("overlap result = %+v, want terminal SkippedOverlap", detail)
	}
}
