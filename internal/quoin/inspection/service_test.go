package inspection

// SQLite domain tests over the frozen schema: manual mixed Run creation
// against a real published config version, run_check child freezing
// (inspection_promql_execution_v1 with its Thanos grant, the real
// inspection_collection_v1 journey binding), deterministic browser local
// gaps, PromQL ResultProposal closure fences, and convergence semantics.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	if final.State != "CompletedWithGaps" || final.Report != nil {
		t.Fatalf("run should converge CompletedWithGaps without a report yet, got %s report=%v", final.State, final.Report)
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

// seedModelProvider registers one enabled qualified model provider through
// the same durable facts the capability-probe path writes (ported from the
// analysis harness seedProviderChain).
func (h *testHarness) seedModelProvider(t *testing.T) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var existing int64
	if err := h.db.QueryRow(`SELECT id FROM connections WHERE type='model_provider' AND enabled=1`).Scan(&existing); err == nil {
		return
	}
	connection, err := h.db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES('model','model_provider',0,?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	connectionID, _ := connection.LastInsertId()
	revision, err := h.db.Exec(`INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_at) VALUES(?,1,?,?)`, connectionID, `{"baseUrl":"https://provider.test","chatModelId":"fixture-chat-1","contextBudgetTokens":4096,"maxOutputTokens":1024}`, now)
	if err != nil {
		t.Fatal(err)
	}
	revisionID, _ := revision.LastInsertId()
	nonce := make([]byte, 12)
	for i := range nonce {
		nonce[i] = byte(7*31 + i)
	}
	generation, err := h.db.Exec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(?,1,1,1,?,?,?)`, connectionID, nonce, []byte(strings.Repeat("f", 32)), now)
	if err != nil {
		t.Fatal(err)
	}
	generationID, _ := generation.LastInsertId()
	if _, err = h.db.Exec(`UPDATE connections SET current_revision_id=?, current_credential_generation_id=?, row_version=row_version+1 WHERE id=?`, revisionID, generationID, connectionID); err != nil {
		t.Fatal(err)
	}
	probeAttempt, err := h.db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES('connection_probe','connection',?,'Queued','test',?)`, connectionID, now)
	if err != nil {
		t.Fatal(err)
	}
	probeAttemptID, _ := probeAttempt.LastInsertId()
	probeSnapshot, err := h.db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,'connection_probe_v1','connection-probe-v1',?,?)`, probeAttemptID, strings.Repeat("0", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	probeSnapshotID, _ := probeSnapshot.LastInsertId()
	if _, err = h.db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,1,'user',?,?)`, probeSnapshotID, strings.Repeat("0", 64), revisionID); err != nil {
		t.Fatal(err)
	}
	var probeChatGrantID, probeEmbeddingGrantID int64
	for _, purpose := range []string{"model_probe_chat", "model_probe_embedding"} {
		grant, grantErr := h.db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(?,?,?,?,?,?)`, probeAttemptID, purpose, connectionID, revisionID, generationID, now)
		if grantErr != nil {
			t.Fatal(grantErr)
		}
		if purpose == "model_probe_chat" {
			probeChatGrantID, _ = grant.LastInsertId()
		} else {
			probeEmbeddingGrantID, _ = grant.LastInsertId()
		}
	}
	if _, err = h.db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=? AND state='Queued'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned'`, now, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	probe, err := h.db.Exec(`INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,1,'model-provider-capabilities',1,?,?,?,?,?,?)`,
		probeAttemptID, connectionID, "model_provider", revisionID, generationID, strings.Repeat("0", 64), "passed", strings.Repeat("1", 64), now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	probeResultID, _ := probe.LastInsertId()
	h.seedProbeModelCall(t, probeAttemptID, probeChatGrantID, 1, now, true)
	h.seedProbeModelCall(t, probeAttemptID, probeChatGrantID, 4, now, false)
	h.seedProbeEmbeddingCall(t, probeAttemptID, probeEmbeddingGrantID, probeSnapshotID, now)
	if _, err = h.db.Exec(`INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,embedding_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,embedding_vector_dim,detail_json) VALUES(?,'fixture-chat-1','fixture-embed-1',4096,1024,1,1,1,1,1,1,1,16,'{}')`, probeResultID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=? AND state='Running'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(?,3,?,?,?)`, connectionID, probeResultID, h.principal, now); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`UPDATE connections SET enabled=1,revalidation_required=0,row_version=row_version+1 WHERE id=? AND row_version=2`, connectionID); err != nil {
		t.Fatal(err)
	}
}

func (h *testHarness) seedProbeModelCall(t *testing.T, attemptID, grantID int64, callSeq int64, now string, succeeded bool) {
	t.Helper()
	digest := strings.Repeat("1", 64)
	call, err := h.db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,?,'0','chat','fixture-chat-1',?,'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,0,'running',?)`,
		attemptID, callSeq, grantID, digest, digest, digest, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	if _, err = h.db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, callID, digest, callID, digest); err != nil {
		t.Fatal(err)
	}
	if succeeded {
		if _, err = h.db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"ok","finishReason":"stop","tool_calls":[]}',?, 'stop',?)`, callID, digest, now); err != nil {
			t.Fatal(err)
		}
		if _, err = h.db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err = h.db.Exec(`UPDATE model_calls SET status='cancelled',termination_reason='cancelled',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
}

func (h *testHarness) seedProbeEmbeddingCall(t *testing.T, attemptID, grantID, snapshotID int64, now string) {
	t.Helper()
	digest := strings.Repeat("1", 64)
	call, err := h.db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,input_snapshot_digest,rendered_request_digest,estimated_input_tokens,status,started_at) VALUES(?,'6','0','embedding','fixture-embed-1',?,?,?,0,'running',?)`,
		attemptID, grantID, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	if _, err = h.db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,1,'user',?,?)`, callID, digest, snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,created_at) VALUES(?,1,'{}',?,?)`, callID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":0,"total_tokens":1}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
}

func (h *testHarness) analysisAttemptID(t *testing.T, runID int64) int64 {
	t.Helper()
	var id int64
	if err := h.db.QueryRow(`SELECT id FROM execution_attempts WHERE attempt_type='inspection_analysis' AND scope_type='run' AND scope_id=?`, runID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedSucceededModelCall drives one complete chat call for the Running
// analysis attempt and returns its id (production drives this through the
// agent runtime; the domain test seals the same durable facts).
func (h *testHarness) seedSucceededModelCall(t *testing.T, attemptID int64, promptDigest string) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	call, err := h.db.Exec(`
		INSERT INTO model_calls(attempt_id, call_seq, retry_seq, operation, model_id, connection_grant_id, provider_request_id, prompt_renderer_version, agent_version, prompt_digest, tool_schema_version, tool_schema_digest, input_snapshot_digest, rendered_request_digest, context_budget_tokens, max_output_tokens, estimated_input_tokens, evicted_turn_count, usage_json, latency_ms, status, termination_reason, started_at, ended_at)
		VALUES(?, 1, 0, 'chat', 'fixture-chat-1', (SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'), NULL, 'v1', 'agent-v1', ?, 'v1', ?, ?, ?, 2, 1, 0, 0, NULL, NULL, 'running', NULL, ?, NULL)`,
		attemptID, attemptID, promptDigest, promptDigest, promptDigest, promptDigest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, err := call.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for seq, kind := range []string{"system_contract", "tool_schema"} {
		if _, err = h.db.Exec(`
			INSERT INTO model_call_input_items(model_call_id, item_seq, item_role, source_digest, synthetic_kind)
			VALUES(?,?, 'system', ?, ?)`, callID, seq+1, promptDigest, kind); err != nil {
			t.Fatal(err)
		}
	}
	response := `{"tool_calls":[]}`
	if _, err = h.db.Exec(`
		INSERT INTO model_call_outputs(model_call_id, complete, response_json, response_digest, finish_reason, created_at)
		VALUES(?, 1, ?, ?, 'stop', ?)`, callID, response, promptDigest, now); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`UPDATE model_calls SET status='succeeded', usage_json='{}', latency_ms=1, ended_at=? WHERE id=?`, now, callID); err != nil {
		t.Fatal(err)
	}
	return callID
}

func reportProposalBody(attemptID, runID, callID int64, content string, evidenceIDs []int64, promptDigest string) []byte {
	evidenceJSON, _ := json.Marshal(evidenceIDs)
	evidenceSum := sha256.Sum256(evidenceJSON)
	evidenceDigest := hex.EncodeToString(evidenceSum[:])
	canonical := fmt.Sprintf("inspection_report_result_v1|%d|%d|%d|success|%s|%s|[]|[]|%s|%s",
		attemptID, runID, callID, content, string(evidenceJSON), evidenceDigest, promptDigest)
	resultSum := sha256.Sum256([]byte(canonical))
	body, _ := json.Marshal(map[string]any{
		"schemaKind": "inspection_report_result_v1", "attemptId": attemptID, "inspectionRunId": runID,
		"modelCallId": callID, "outcome": "success", "content": content,
		"evidenceIds": evidenceIDs, "artifactIds": []int64{}, "knowledgeVersionIds": []int64{},
		"resultDigest": hex.EncodeToString(resultSum[:]), "evidenceDigest": evidenceDigest, "promptDigest": promptDigest,
	})
	return body
}

func TestImmutableReportClosure(t *testing.T) {
	h := newTestHarness(t)
	h.publishMixedPlan(t)
	h.seedBrowserIdentity(t, false)
	h.seedModelProvider(t)
	ctx := context.Background()
	detail, err := h.service.CreateInspectionRun(ctx, h.principal, "cmd-1", "payments", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	attemptID := h.promqlAttemptID(t, detail.RunID)
	h.dispatchPromQL(t, attemptID)
	if err := h.service.CommitPromQLProposal(ctx, attemptID, "plinth-boot", 1, promqlSuccessProposal(attemptID, detail.RunID, "success", "instant")); err != nil {
		t.Fatal(err)
	}
	// Convergence created exactly one analysis attempt with the structured
	// preallocated version and the full frozen locator set.
	analysisID := h.analysisAttemptID(t, detail.RunID)
	var version int64
	var kind string
	if err := h.db.QueryRow(`SELECT inspection_report_version, schema_kind FROM attempt_input_snapshots WHERE attempt_id=?`, analysisID).Scan(&version, &kind); err != nil {
		t.Fatal(err)
	}
	if version != 1 || kind != "inspection_analysis_v1" {
		t.Fatalf("analysis snapshot = v%d/%s", version, kind)
	}
	var itemCount int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM attempt_input_items WHERE snapshot_id=(SELECT id FROM attempt_input_snapshots WHERE attempt_id=?)`, analysisID).Scan(&itemCount); err != nil {
		t.Fatal(err)
	}
	// run + 2 check results + 1 evidence + config + contract = 6 items.
	if itemCount != 6 {
		t.Fatalf("analysis frozen items = %d", itemCount)
	}
	if err := h.attempts.BindToSlot(ctx, analysisID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, analysisID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := strings.Repeat("c", 64)
	callID := h.seedSucceededModelCall(t, analysisID, promptDigest)
	var evidenceIDs []int64
	rows, err := h.db.Query(`SELECT evidence_id FROM inspection_check_results WHERE run_id=? AND evidence_id IS NOT NULL ORDER BY check_key`, detail.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		evidenceIDs = append(evidenceIDs, id)
	}
	rows.Close()
	if len(evidenceIDs) != 1 {
		t.Fatalf("expected one collected evidence, got %d", len(evidenceIDs))
	}
	if err := h.service.CommitReportProposal(ctx, analysisID, "plinth-boot", 1, reportProposalBody(analysisID, detail.RunID, callID, "巡检报告正文", evidenceIDs, promptDigest)); err != nil {
		t.Fatal(err)
	}
	final, err := h.service.GetRun(ctx, "payments", detail.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Report == nil || final.Report.Version != 1 || final.Report.Content != "巡检报告正文" || final.Report.ModelID != "fixture-chat-1" {
		t.Fatalf("immutable report missing or wrong: %+v", final.Report)
	}
	var analysisState string
	if err := h.db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, analysisID).Scan(&analysisState); err != nil {
		t.Fatal(err)
	}
	if analysisState != "Succeeded" {
		t.Fatalf("analysis attempt state = %s", analysisState)
	}
	// Same-bytes replay is idempotent; tampered content never lands.
	if err := h.service.CommitReportProposal(ctx, analysisID, "plinth-boot", 1, reportProposalBody(analysisID, detail.RunID, callID, "巡检报告正文", evidenceIDs, promptDigest)); err != nil {
		t.Fatalf("identical replay must be idempotent: %v", err)
	}
	if err := h.service.CommitReportProposal(ctx, analysisID, "plinth-boot", 1, reportProposalBody(analysisID, detail.RunID, callID, "篡改内容", evidenceIDs, promptDigest)); err == nil {
		t.Fatal("tampered report replay must be rejected")
	}
	var reports int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_reports WHERE run_id=?`, detail.RunID).Scan(&reports); err != nil {
		t.Fatal(err)
	}
	if reports != 1 {
		t.Fatalf("exactly one immutable report must exist, got %d", reports)
	}
}

func journeyInspectionResult(attemptID, operationID int64) []byte {
	_, catalogVersion, catalogDigest, err := quoinconfig.JourneyCatalog()
	if err != nil {
		panic(err)
	}
	catalog := map[string]any{"digest": catalogDigest, "version": catalogVersion}
	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	body, _ := json.Marshal(map[string]any{
		"schemaKind": "browser_journey_result_v1", "attemptId": attemptID, "operationId": operationID,
		"outcome": "success",
		"probeResults": []map[string]any{
			{"phase": "admission", "result": "Authenticated", "journeyId": "authentication.url-prefix.v1", "journeyVersion": 1, "catalog": catalog, "reasonCode": nil, "observedAt": observedAt},
			{"phase": "completion", "result": "Authenticated", "journeyId": "authentication.url-prefix.v1", "journeyVersion": 1, "catalog": catalog, "reasonCode": nil, "observedAt": observedAt},
		},
		"evidence":        []map[string]any{{"kind": "structured", "primary": true, "observedAt": observedAt, "content": map[string]any{"statusText": "SYSTEM OK"}, "artifactId": nil}},
		"traceArtifactId": nil, "traceIntegrity": nil, "gapCode": nil, "originalGapCode": nil, "terminalReason": nil, "errorDetail": nil,
	})
	return body
}

func TestRunCheckJourneyResultClosure(t *testing.T) {
	h := newTestHarness(t)
	h.publishMixedPlan(t)
	h.seedBrowserIdentity(t, true)
	h.seedModelProvider(t)
	ctx := context.Background()
	detail, err := h.service.CreateInspectionRun(ctx, h.principal, "cmd-1", "payments", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	// The ready free identity left the browser child bare; admission creates
	// its operation and freezes the operation-bound input.
	admitted, err := h.service.AdmitNextJourneyChild(ctx)
	if err != nil || !admitted {
		t.Fatalf("admission should admit the browser child: admitted=%v err=%v", admitted, err)
	}
	var browserAttempt, operationID int64
	if err := h.db.QueryRow(`SELECT a.id,o.id FROM execution_attempts a JOIN browser_operations o ON o.owner_attempt_id=a.id WHERE a.scope_id=? AND a.check_key='status-page'`, detail.RunID).Scan(&browserAttempt, &operationID); err != nil {
		t.Fatal(err)
	}
	var kind string
	if err := h.db.QueryRow(`SELECT schema_kind FROM attempt_input_snapshots WHERE attempt_id=?`, browserAttempt).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "inspection_collection_v1" {
		t.Fatalf("admission should freeze inspection_collection_v1, got %s", kind)
	}
	// Walk the durable dispatch fences exactly like the runtime loop.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id='boot-j1',lintel_connection_epoch=1,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Running',started_at=?,row_version=row_version+1 WHERE id=? AND state='Starting'`, now, operationID); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.BindToSlot(ctx, browserAttempt, "lintel", "boot-j1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, browserAttempt, "boot-j1", 1); err != nil {
		t.Fatal(err)
	}
	// Settle the PromQL child too so the run can converge all-ok.
	promqlAttempt := h.promqlAttemptID(t, detail.RunID)
	h.dispatchPromQL(t, promqlAttempt)
	if err := h.service.CommitPromQLProposal(ctx, promqlAttempt, "plinth-boot", 1, promqlSuccessProposal(promqlAttempt, detail.RunID, "success", "instant")); err != nil {
		t.Fatal(err)
	}
	if err := h.service.CommitJourneyProposal(ctx, browserAttempt, "boot-j1", 1, journeyInspectionResult(browserAttempt, operationID)); err != nil {
		t.Fatal(err)
	}
	var status string
	var evidenceID int64
	if err := h.db.QueryRow(`SELECT status,evidence_id FROM inspection_check_results WHERE run_id=? AND check_key='status-page'`, detail.RunID).Scan(&status, &evidenceID); err != nil {
		t.Fatal(err)
	}
	if status != "ok" || evidenceID < 1 {
		t.Fatalf("journey result should derive ok + evidence, got %s/%d", status, evidenceID)
	}
	var browserState, operationState string
	if err := h.db.QueryRow(`SELECT a.state,o.state FROM execution_attempts a JOIN browser_operations o ON o.owner_attempt_id=a.id WHERE a.id=?`, browserAttempt).Scan(&browserState, &operationState); err != nil {
		t.Fatal(err)
	}
	if browserState != "Succeeded" || operationState != "Succeeded" {
		t.Fatalf("journey closure = attempt %s / operation %s", browserState, operationState)
	}
	final, err := h.service.GetRun(ctx, "payments", detail.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "Completed" {
		t.Fatalf("all-ok run should converge Completed, got %s", final.State)
	}
	// Completed collection immediately owns its analysis attempt.
	var analysis int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM execution_attempts WHERE attempt_type='inspection_analysis' AND scope_type='run' AND scope_id=?`, detail.RunID).Scan(&analysis); err != nil {
		t.Fatal(err)
	}
	if analysis != 1 {
		t.Fatalf("convergence should create one analysis attempt, got %d", analysis)
	}
}
