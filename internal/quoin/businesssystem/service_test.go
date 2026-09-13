package businesssystem

// SQLite harness tests over the frozen schema: the first-upload Disabled
// creation, immutable version appends, the publish pointer transaction with
// its projection sync and contract fence, and command replay semantics
// (DATA-CONFIG-001/003/004, HTTP-CONFIG-001/002).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/config"
	"github.com/Suknna/quoin/internal/quoin/connections"
	_ "modernc.org/sqlite"
)

const validSystemYAML = `apiVersion: quoin/v1
kind: BusinessSystem
metadata:
  name: payments
  displayName: 支付系统
  description: 支付业务
spec:
  metrics:
    connectionRef: main-thanos
    matchLabels: {business_system: payments}
    resources:
      - name: web-pods
        displayName: Web Pods
        matchLabels: {job: web}
        discoveryMetric: up
        identityLabels: [job, instance]
        allowedMetrics: [up, http_requests_total]
  alerts:
    sourceRefs: []
    matchLabels: {}
  inspections:
    - name: daily-check
      displayName: Daily Check
      schedule: "30 8 * * *"
      timezone: Asia/Shanghai
      checks:
        - name: up-instant
          resourceRef: web-pods
          expression: up
          question: 当前可用吗？
        - name: latency-range
          resourceRef: web-pods
          expression: rate(http_requests_total[5m])
          question: 时延趋势？
          queryMode: range
          rangeSeconds: 3600
          stepSeconds: 60
`

type harness struct {
	db        *sql.DB
	systems   *Service
	principal int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/test.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,enabled,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,'x',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	seedPlinthRuntime(t, db, now)
	seedThanosExecutionPath(t, db, now)
	seedLintelRuntime(t, db, now)
	return &harness{db: db, systems: NewService(db), principal: 1}
}

// seedThanosExecutionPath supplies only the non-secret frozen database facts
// consumed by Config Verification creation. Runtime grant fulfillment has its
// own encryption integration tests; this harness verifies that Quoin never
// needs to decrypt those bytes to create an execution Attempt.
func seedThanosExecutionPath(t *testing.T, db *sql.DB, now string) {
	seedMetricsExecutionPath(t, db, now, connections.TypeThanos, "main-thanos")
}

// seedPrometheusExecutionPath exercises Config Verification’s production grant
// creation against the distinct Prometheus discriminator rather than relying
// on Thanos-compatible SQL shortcuts.
func seedPrometheusExecutionPath(t *testing.T, db *sql.DB, now string) {
	seedMetricsExecutionPath(t, db, now, connections.TypePrometheus, "main-prometheus")
}

func seedMetricsExecutionPath(t *testing.T, db *sql.DB, now, connectionType, name string) {
	t.Helper()
	if _, err := db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now); err != nil {
		t.Fatal(err)
	}
	rootKey := make([]byte, 32)
	service := connections.NewService(db, func() ([]byte, error) { return rootKey, nil })
	connections.ProbeContractSource = func() string { return string(gencontracts.ConnectionProbesYAML) }
	configJSON, _ := json.Marshal(map[string]any{"type": connectionType, "baseUrl": "https://metrics.test", "authType": "none"})
	summary, err := service.Create(context.Background(), connections.CreateInput{Name: name, Type: connectionType, NonSecretJSON: configJSON}, 1, "seed-metrics-create-"+name)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := service.StartProbe(context.Background(), summary.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := service.BindQueuedToStream(context.Background(), attemptID, "seed-boot", 1, 5*time.Minute); err != nil || !ok {
		t.Fatalf("bind metrics probe: %v ok=%v", err, ok)
	}
	if err := service.AcceptProbe(context.Background(), attemptID, "seed-boot", 1); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitProbeResult(context.Background(), attemptID, "seed-boot", 1, connections.TypedProbeResult{Outcome: "passed", ResultDigest: fmt.Sprintf("%064x", attemptID), StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z"}, &connections.TypedChild{Thanos: &connections.ThanosProbeChild{Query: "vector(1)", ResponseType: "vector", SampleCount: 1, SampleValue: "1", DetailJSON: fmt.Sprintf(`{"kind":%q}`, connectionType)}}); err != nil {
		t.Fatal(err)
	}
	var probeID int64
	if err := db.QueryRow(`SELECT id FROM connection_probe_results WHERE attempt_id=?`, attemptID).Scan(&probeID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enable(context.Background(), summary.Name, summary.RowVersion, probeID, 1); err != nil {
		t.Fatal(err)
	}
}

func seedLintelRuntime(t *testing.T, db *sql.DB, now string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO runtime_slots(slot,state,row_version,created_at) VALUES('lintel','unregistered',1,?)`, now); err != nil {
		t.Fatal(err)
	}
	credential, err := db.Exec(`INSERT INTO runtime_credentials(slot,generation,token_digest,created_at,confirmed_at,row_version) VALUES('lintel',1,?,?,?,1)`, make([]byte, 32), now, now)
	if err != nil {
		t.Fatal(err)
	}
	credentialID, _ := credential.LastInsertId()
	if _, err := db.Exec(`UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=2 WHERE slot='lintel'`, credentialID); err != nil {
		t.Fatal(err)
	}
}

func seedPlinthRuntime(t *testing.T, db *sql.DB, now string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO runtime_slots(slot,state,row_version,created_at) VALUES('plinth','unregistered',1,?)`, now); err != nil {
		t.Fatal(err)
	}
	credential, err := db.Exec(`INSERT INTO runtime_credentials(slot,generation,token_digest,created_at,confirmed_at,row_version) VALUES('plinth',1,?,?,?,1)`, make([]byte, 32), now, now)
	if err != nil {
		t.Fatal(err)
	}
	credentialID, _ := credential.LastInsertId()
	if _, err := db.Exec(`UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=2 WHERE slot='plinth'`, credentialID); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) upload(t *testing.T, body string, arguments ...any) (ConfigVersionDetail, error) {
	t.Helper()
	// The final value is always the command ID. An obsolete positional version
	// remains accepted for shared test migration but never affects the upload.
	commandID, ok := arguments[len(arguments)-1].(string)
	if !ok {
		t.Fatalf("upload command ID must be a string: %#v", arguments)
	}
	return h.systems.Upload(context.Background(), h.principal, commandID, UploadInput{YAMLBody: []byte(body)}, config.Limits{})
}

func (h *harness) mustUpload(t *testing.T, body string, arguments ...any) ConfigVersionDetail {
	t.Helper()
	// The final value is always the command ID. Accept the obsolete positional
	// version only while shared tests are migrated; it is never persisted or
	// used to synthesize a global Label Contract.
	commandID, ok := arguments[len(arguments)-1].(string)
	if !ok {
		t.Fatalf("upload command ID must be a string: %#v", arguments)
	}
	detail, err := h.upload(t, body, commandID)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return detail
}

func TestFirstUploadCreatesDisabledSystemWithDraft(t *testing.T) {
	h := newHarness(t)
	detail := h.mustUpload(t, validSystemYAML, 0, "cmd-upload-0001")
	if detail.VersionSeq != 1 || detail.State != "draft" || detail.PublishedAt != nil {
		t.Fatalf("first draft wrong: %#v", detail)
	}
	if detail.SystemKey != "payments" || detail.MetricsConnectionID != "1" || detail.LabelContractVersionID != "" {
		t.Fatalf("declaration root projection wrong: %#v", detail)
	}
	var declaration struct {
		Description string `json:"description"`
		Resources   []struct {
			Name string `json:"name"`
		} `json:"resources"`
	}
	var declarationJSON string
	if err := h.db.QueryRow(`SELECT declaration_json FROM business_system_config_versions WHERE id=?`, detail.ID).Scan(&declarationJSON); err != nil || json.Unmarshal([]byte(declarationJSON), &declaration) != nil || declaration.Description != "支付业务" || len(declaration.Resources) != 1 {
		t.Fatalf("compiled declaration missing: err=%v json=%s value=%#v", err, declarationJSON, declaration)
	}
	var scopes int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM config_resource_scopes WHERE config_version_id=?`, detail.ID).Scan(&scopes); err != nil || scopes != 1 {
		t.Fatalf("resource scopes missing: count=%d err=%v", scopes, err)
	}
	var enabled int
	var timezone sql.NullString
	var current sql.NullInt64
	if err := h.db.QueryRow(`SELECT enabled,timezone,current_config_version_id FROM business_systems WHERE key='payments'`).Scan(&enabled, &timezone, &current); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 || timezone.Valid || current.Valid {
		t.Fatalf("new system must be Disabled and unconfigured: enabled=%d tz=%v current=%v", enabled, timezone, current)
	}
	// The stored YAML is byte-exact and the projection rows persist.
	var yamlBody string
	if err := h.db.QueryRow(`SELECT yaml_body FROM business_system_config_versions WHERE id=?`, detail.ID).Scan(&yamlBody); err != nil || yamlBody != validSystemYAML {
		t.Fatalf("yaml body must be verbatim: %v", err)
	}
	var allowedMetrics string
	if err := h.db.QueryRow(`SELECT allowed_metrics_json FROM config_resource_scopes WHERE config_version_id=? AND resource_key='web-pods'`, detail.ID).Scan(&allowedMetrics); err != nil || allowedMetrics != `["up","http_requests_total"]` {
		t.Fatalf("compiled scope whitelist wrong: value=%s err=%v", allowedMetrics, err)
	}
}

func TestSecondUploadAppendsDraft(t *testing.T) {
	h := newHarness(t)
	h.mustUpload(t, validSystemYAML, 0, "cmd-upload-0002")
	modified := strings.Replace(validSystemYAML, "displayName: 支付系统", "displayName: 支付平台", 1)
	detail := h.mustUpload(t, modified, 0, "cmd-upload-0003")
	if detail.VersionSeq != 2 || detail.DisplayName != "支付平台" {
		t.Fatalf("second draft wrong: %#v", detail)
	}
	var systems int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM business_systems`).Scan(&systems)
	if systems != 1 {
		t.Fatalf("same system_key must not create a second system: %d", systems)
	}
}

func TestUploadValidatesDeclarationScopeAndCatalog(t *testing.T) {
	h := newHarness(t)
	// Scope validation is declaration-local; it must not query a Label Contract.
	badMetric := strings.Replace(validSystemYAML, `expression: up`, `expression: forbidden_metric`, 1)
	_, err := h.upload(t, badMetric, 0, "cmd-upload-x1")
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !strings.Contains(validation.Errors[0].Path, "expression") {
		t.Fatalf("scope violation must be a field error, got %v", err)
	}
	_, err = h.systems.Upload(context.Background(), 1, "cmd-upload-x3", UploadInput{YAMLBody: []byte(validSystemYAML), JourneyCatalogDigest: strings.Repeat("0", 64)}, config.Limits{})
	if !errors.As(err, &validation) || !strings.Contains(validation.Errors[0].Path, "journeyCatalogDigest") {
		t.Fatalf("wrong catalog digest must be a field error: %v", err)
	}
	var versions int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM business_system_config_versions`).Scan(&versions)
	if versions != 0 {
		t.Fatalf("rejected uploads must not persist: %d", versions)
	}
}

func TestPublishSwitchesPointerAndProjection(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, validSystemYAML, 0, "cmd-upload-0010")
	detail, err := h.systems.Publish(context.Background(), 1, "cmd-publish-0010", "payments", mustID(t, draft.ID), nil)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if detail.CurrentConfigVersionID == nil || *detail.CurrentConfigVersionID != draft.ID {
		t.Fatalf("current pointer wrong: %#v", detail)
	}
	if detail.RowVersion != 2 || !detail.Enabled || detail.Timezone == nil || *detail.Timezone != "UTC" {
		t.Fatalf("root projection must sync from the published version: %#v", detail)
	}
	// The version derives published with a one-time published_at; the
	// projection rows are readable through the current pointer.
	published, err := h.systems.GetVersion(context.Background(), "payments", mustID(t, draft.ID))
	if err != nil || published.State != "published" || published.PublishedAt == nil {
		t.Fatalf("published derivation wrong: %v %#v", err, published)
	}
	if len(published.Discoveries) != 1 || len(published.Plans[0].Checks) != 2 {
		t.Fatalf("current projections missing: %#v", published)
	}
	var publishedCount int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM business_system_config_versions WHERE state='published' AND published_at IS NOT NULL`).Scan(&publishedCount)
	if publishedCount != 1 {
		t.Fatalf("exactly one published version: %d", publishedCount)
	}
	var auditCount int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='business_system.config.publish' AND outcome='success'`).Scan(&auditCount)
	if auditCount != 1 {
		t.Fatalf("publish audit missing: %d", auditCount)
	}
}

func TestPublishEnablesSystemThroughProjection(t *testing.T) {
	h := newHarness(t)
	enabled := validSystemYAML
	draft := h.mustUpload(t, enabled, 0, "cmd-upload-0011")
	detail, err := h.systems.Publish(context.Background(), 1, "cmd-publish-0011", "payments", mustID(t, draft.ID), nil)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !detail.Enabled {
		t.Fatal("publishing an enabled=true version must enable the system")
	}
}

func TestPublishConflictFences(t *testing.T) {
	h := newHarness(t)
	first := h.mustUpload(t, validSystemYAML, 0, "cmd-upload-0020")
	second := h.mustUpload(t, strings.Replace(validSystemYAML, "displayName: 支付系统", "displayName: 支付系统 v2", 1), 0, "cmd-upload-0021")
	// Stale expected (null) after the first publish commits.
	if _, err := h.systems.Publish(context.Background(), 1, "cmd-publish-0020", "payments", mustID(t, first.ID), nil); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	_, err := h.systems.Publish(context.Background(), 1, "cmd-publish-0022", "payments", mustID(t, second.ID), nil)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Code != "current_pointer_conflict" || conflict.CurrentVersion == nil || *conflict.CurrentVersion != mustID(t, first.ID) {
		t.Fatalf("stale fence must conflict with the actual current: %v", err)
	}
	// Publishing an already-published version is a pointer conflict.
	_, err = h.systems.Publish(context.Background(), 1, "cmd-publish-0023", "payments", mustID(t, first.ID), ptrInt64(mustID(t, first.ID)))
	if !errors.As(err, &conflict) {
		t.Fatalf("re-publish must conflict: %v", err)
	}
	// Correct fence advances and supersedes the old version.
	detail, err := h.systems.Publish(context.Background(), 1, "cmd-publish-0024", "payments", mustID(t, second.ID), ptrInt64(mustID(t, first.ID)))
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if detail.RowVersion != 3 || *detail.CurrentConfigVersionID != second.ID {
		t.Fatalf("second publish wrong: %#v", detail)
	}
	states := map[string]string{}
	rows, err := h.db.Query(`SELECT id,state FROM business_system_config_versions`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		states[id] = state
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if states[first.ID] != "superseded" || states[second.ID] != "published" {
		t.Fatalf("state derivation wrong: %v", states)
	}
	// Missing system or version is NotFound.
	if _, err := h.systems.Publish(context.Background(), 1, "cmd-publish-0025", "nope", 1, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown system must be NotFound: %v", err)
	}
}

func TestPublishDoesNotRequireContract(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, validSystemYAML, 0, "cmd-upload-0030")
	if _, err := h.systems.Publish(context.Background(), 1, "cmd-publish-0030", "payments", mustID(t, draft.ID), nil); err != nil {
		t.Fatalf("publish must not depend on a contract: %v", err)
	}
}

func TestUploadCommandReplay(t *testing.T) {
	h := newHarness(t)
	first := h.mustUpload(t, validSystemYAML, 0, "cmd-upload-0040")
	replayed, err := h.upload(t, validSystemYAML, 0, "cmd-upload-0040")
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("replay must return the original draft: %v %#v", err, replayed)
	}
	var versions int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM business_system_config_versions`).Scan(&versions)
	if versions != 1 {
		t.Fatalf("replay must not append a version: %d", versions)
	}
	_, err = h.upload(t, strings.Replace(validSystemYAML, "displayName: 支付系统", "displayName: 重放冲突", 1), 0, "cmd-upload-0040")
	if !errors.Is(err, ErrCommandReused) {
		t.Fatalf("same id with different content must conflict: %v", err)
	}
}

func TestListAndGetProjections(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, validSystemYAML, 0, "cmd-upload-0050")
	items, nextCursor, err := h.systems.ListSystems(context.Background(), nil, "", "", 50)
	if err != nil || nextCursor != "" || len(items) != 1 || items[0].Key != "payments" || items[0].ConfigVersionCount != 1 {
		t.Fatalf("list wrong: %v %q %#v", err, nextCursor, items)
	}
	enabledOnly := true
	if items, _, err := h.systems.ListSystems(context.Background(), &enabledOnly, "", "", 50); err != nil || len(items) != 0 {
		t.Fatalf("enabled filter wrong: %v %#v", err, items)
	}
	detail, err := h.systems.GetSystem(context.Background(), "payments")
	if err != nil || detail.BrowserIdentityState != "none" || len(detail.Discoveries) != 0 {
		t.Fatalf("unpublished system detail wrong: %v %#v", err, detail)
	}
	versions, _, err := h.systems.ListVersions(context.Background(), "payments", "", 50)
	if err != nil || len(versions) != 1 || versions[0].ID != draft.ID {
		t.Fatalf("version list wrong: %v %#v", err, versions)
	}
	if _, err := h.systems.GetSystem(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing system must be NotFound: %v", err)
	}
	if _, err := h.systems.GetVersion(context.Background(), "payments", 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version must be NotFound: %v", err)
	}
}

func TestListSystemsReturnsIDKeysetCursor(t *testing.T) {
	h := newHarness(t)
	h.mustUpload(t, validSystemYAML, 0, "cmd-list-cursor-001")
	billingYAML := strings.ReplaceAll(strings.ReplaceAll(validSystemYAML, "payments", "billing"), "支付系统", "账单系统")
	h.mustUpload(t, billingYAML, 0, "cmd-list-cursor-002")

	firstPage, nextCursor, err := h.systems.ListSystems(context.Background(), nil, "", "", 1)
	if err != nil || len(firstPage) != 1 || nextCursor == "" {
		t.Fatalf("first page must have an ID cursor: err=%v cursor=%q items=%#v", err, nextCursor, firstPage)
	}
	secondPage, finalCursor, err := h.systems.ListSystems(context.Background(), nil, "", nextCursor, 1)
	if err != nil || len(secondPage) != 1 || finalCursor != "" {
		t.Fatalf("second page wrong: err=%v cursor=%q items=%#v", err, finalCursor, secondPage)
	}
	if firstPage[0].Key == secondPage[0].Key {
		t.Fatalf("keyset cursor replayed the first row: %q", firstPage[0].Key)
	}
}

// TestVerificationConfigLocatorBindsCanonicalDeclarationWithoutContract proves
// that deployment verification freezes the exact configuration version, not a
// retired Label Contract. The second insert deliberately pairs one system with
// another system's declaration and must remain rejected.
func TestVerificationConfigLocatorBindsCanonicalDeclarationWithoutContract(t *testing.T) {
	h := newHarness(t)
	payments := h.mustUpload(t, validSystemYAML, "cmd-locator-payments-001")
	billingYAML := strings.ReplaceAll(strings.ReplaceAll(validSystemYAML, "payments", "billing"), "支付系统", "账单系统")
	billing := h.mustUpload(t, billingYAML, "cmd-locator-billing-001")

	const now = "2026-01-01T00:00:00Z"
	if _, err := h.db.Exec(`INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,?,1,'locator test',?,?,?,?)`, make([]byte, 32), now, now, "2026-01-02T00:00:00Z", "2026-01-08T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	manifest, err := h.db.Exec(`INSERT INTO verification_invocation_manifests(admin_session_id,principal_user_id,release_subject_digest,catalog_digest,result_profile_digest,deployment_config_digest,public_origin_digest,applicable_set_digest,item_count,item_set_digest,manifest_digest,canonical_input_digest,started_at,deadline_at,created_at) VALUES(1,1,?,?,?,?,?,?,2,?,?,?,?,?,?)`, strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64), strings.Repeat("e", 64), strings.Repeat("f", 64), strings.Repeat("0", 64), strings.Repeat("1", 64), strings.Repeat("2", 64), now, "2026-01-01T08:00:00Z", now)
	if err != nil {
		t.Fatal(err)
	}
	manifestID, _ := manifest.LastInsertId()
	insertItem := func(sequence int64) int64 {
		t.Helper()
		item, err := h.db.Exec(`INSERT INTO verification_invocation_items(invocation_id,item_seq,scenario_id,cell_id,object_kind,input_digest,created_at) VALUES(?,?,?,'default','config',?,?)`, manifestID, sequence, "config-locator", strings.Repeat(strconv.FormatInt(sequence, 10), 64), now)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := item.LastInsertId()
		return id
	}

	paymentsItem := insertItem(1)
	var paymentsSystemID, billingConfigID int64
	if err := h.db.QueryRow(`SELECT business_system_id FROM business_system_config_versions WHERE id=?`, mustID(t, payments.ID)).Scan(&paymentsSystemID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO verification_config_item_locators(item_id,business_system_id,config_version_id,label_contract_version_id) VALUES(?,?,?,NULL)`, paymentsItem, paymentsSystemID, mustID(t, payments.ID)); err != nil {
		t.Fatalf("canonical declaration locator with NULL contract: %v", err)
	}
	billingItem := insertItem(2)
	if err := h.db.QueryRow(`SELECT id FROM business_system_config_versions WHERE id=?`, mustID(t, billing.ID)).Scan(&billingConfigID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO verification_config_item_locators(item_id,business_system_id,config_version_id,label_contract_version_id) VALUES(?,?,?,NULL)`, billingItem, paymentsSystemID, billingConfigID); err == nil || !strings.Contains(err.Error(), "exact frozen declaration") {
		t.Fatalf("mismatched canonical configuration must be rejected, err=%v", err)
	}
}

func mustID(t *testing.T, locator string) int64 {
	t.Helper()
	parsed, err := strconv.ParseInt(locator, 10, 64)
	if err != nil {
		t.Fatalf("locator %q: %v", locator, err)
	}
	return parsed
}

func ptrInt64(value int64) *int64 { return &value }
