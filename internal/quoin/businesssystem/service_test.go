package businesssystem

// SQLite harness tests over the frozen schema: the first-upload Disabled
// creation, immutable version appends, the publish pointer transaction with
// its projection sync and contract fence, and command replay semantics
// (DATA-CONFIG-001/003/004, HTTP-CONFIG-001/002).

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/config"
	"github.com/Suknna/quoin/internal/quoin/labelcontract"
	_ "modernc.org/sqlite"
)

const activeContractYAML = "label_contract:\n  business_system_label: business_system\n"

const validSystemYAML = `system_key: payments
display_name: 支付系统
enabled: false
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
resource_discoveries:
  - key: web-pods
    display_name: Web Pods
    selector: 'up{business_system="payments", job="web"}'
    identity_labels: [job, instance]
inspection_plans:
  - key: daily-check
    display_name: Daily Check
    cron: "30 8 * * *"
    checks:
      - key: up-instant
        display_name: Up Instant
        analysis_question: 当前可用吗？
        kind: promql
        query:
          mode: instant
          expression: 'up{business_system="payments"}'
      - key: latency-range
        display_name: Latency Range
        analysis_question: 时延趋势？
        kind: promql
        query:
          mode: range
          expression: 'rate(http_requests_total{business_system="payments"}[5m])'
          range_seconds: 3600
          step_seconds: 60
`

type harness struct {
	db        *sql.DB
	systems   *Service
	contracts *labelcontract.Service
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
	if _, err := db.Exec(`INSERT INTO label_contract_state(id,row_version,updated_at) VALUES(1,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,enabled,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,'x',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	contracts := labelcontract.NewService(db)
	// Contract v1 active through the real zero-system activation command.
	if _, err := contracts.CreateDraft(context.Background(), 1, "seed-contract-0001", []byte(activeContractYAML), config.Limits{}); err != nil {
		t.Fatal(err)
	}
	if _, err := contracts.Activate(context.Background(), 1, "seed-activate-0001", labelcontract.ActivateInput{ContractVersion: 1, ExpectedStateRowVersion: 1, ExpectedTargetRowVersion: 1}); err != nil {
		t.Fatal(err)
	}
	return &harness{db: db, systems: NewService(db), contracts: contracts, principal: 1}
}

func (h *harness) upload(t *testing.T, body string, contractVersion int64, commandID string) (ConfigVersionDetail, error) {
	t.Helper()
	return h.systems.Upload(context.Background(), h.principal, commandID, UploadInput{YAMLBody: []byte(body), TargetLabelContractVersion: contractVersion}, config.Limits{})
}

func (h *harness) mustUpload(t *testing.T, body string, contractVersion int64, commandID string) ConfigVersionDetail {
	t.Helper()
	detail, err := h.upload(t, body, contractVersion, commandID)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return detail
}

func TestFirstUploadCreatesDisabledSystemWithDraft(t *testing.T) {
	h := newHarness(t)
	detail := h.mustUpload(t, validSystemYAML, 1, "cmd-upload-0001")
	if detail.VersionSeq != 1 || detail.State != "draft" || detail.PublishedAt != nil {
		t.Fatalf("first draft wrong: %#v", detail)
	}
	if detail.SystemKey != "payments" || detail.Timezone != "Asia/Shanghai" || detail.ResourceRefreshIntervalSeconds != 300 {
		t.Fatalf("root projection wrong: %#v", detail)
	}
	if len(detail.Discoveries) != 1 || len(detail.Plans) != 1 || len(detail.Plans[0].Checks) != 2 {
		t.Fatalf("typed projections missing: %#v", detail)
	}
	check := detail.Plans[0].Checks[1]
	if check.QueryMode != "range" || check.RangeSeconds == nil || *check.RangeSeconds != 3600 {
		t.Fatalf("range check projection wrong: %#v", check)
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
	var checks int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM config_checks`).Scan(&checks)
	if checks != 2 {
		t.Fatalf("check rows wrong: %d", checks)
	}
}

func TestSecondUploadAppendsDraft(t *testing.T) {
	h := newHarness(t)
	h.mustUpload(t, validSystemYAML, 1, "cmd-upload-0002")
	modified := strings.Replace(validSystemYAML, "display_name: 支付系统", "display_name: 支付平台", 1)
	detail := h.mustUpload(t, modified, 1, "cmd-upload-0003")
	if detail.VersionSeq != 2 || detail.DisplayName != "支付平台" {
		t.Fatalf("second draft wrong: %#v", detail)
	}
	var systems int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM business_systems`).Scan(&systems)
	if systems != 1 {
		t.Fatalf("same system_key must not create a second system: %d", systems)
	}
}

func TestUploadValidatesAgainstTargetContract(t *testing.T) {
	h := newHarness(t)
	// Missing target contract version.
	_, err := h.upload(t, validSystemYAML, 99, "cmd-upload-x1")
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !strings.Contains(validation.Errors[0].Path, "targetLabelContractVersion") {
		t.Fatalf("missing target must be a field error, got %v", err)
	}
	// Ownership violations carry YAML paths.
	badSelector := strings.Replace(validSystemYAML, `selector: 'up{business_system="payments", job="web"}'`, "selector: 'up{job=\"web\"}'", 1)
	_, err = h.upload(t, badSelector, 1, "cmd-upload-x2")
	if !errors.As(err, &validation) || !strings.Contains(validation.Errors[0].Path, "resource_discoveries[0].selector") {
		t.Fatalf("ownership failure must point at the selector: %v", err)
	}
	// Wrong optional catalog digest.
	_, err = h.systems.Upload(context.Background(), 1, "cmd-upload-x3", UploadInput{
		YAMLBody: []byte(validSystemYAML), TargetLabelContractVersion: 1, JourneyCatalogDigest: strings.Repeat("0", 64),
	}, config.Limits{})
	if !errors.As(err, &validation) || !strings.Contains(validation.Errors[0].Path, "journeyCatalogDigest") {
		t.Fatalf("wrong catalog digest must be a field error: %v", err)
	}
	var versions int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM business_system_config_versions`).Scan(&versions)
	if versions != 0 {
		t.Fatalf("rejected uploads must not persist: %d", versions)
	}
}

func TestUploadRetiredContractTargetRejected(t *testing.T) {
	h := newHarness(t)
	// Activate a second contract (carrying the current pointer precondition)
	// so contract v1 becomes retired.
	if _, err := h.contracts.CreateDraft(context.Background(), 1, "seed-contract-0002", []byte("label_contract:\n  business_system_label: biz\n"), config.Limits{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.contracts.Activate(context.Background(), 1, "seed-activate-0002", labelcontract.ActivateInput{
		ContractVersion: 2, ExpectedStateRowVersion: 2, ExpectedTargetRowVersion: 1, ExpectedCurrentContractID: ptrInt64(1),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := h.upload(t, validSystemYAML, 1, "cmd-upload-x4")
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !strings.Contains(validation.Errors[0].Reason, "retired") {
		t.Fatalf("retired target must be rejected: %v", err)
	}
}

func TestPublishSwitchesPointerAndProjection(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, validSystemYAML, 1, "cmd-upload-0010")
	detail, err := h.systems.Publish(context.Background(), 1, "cmd-publish-0010", "payments", mustID(t, draft.ID), nil)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if detail.CurrentConfigVersionID == nil || *detail.CurrentConfigVersionID != draft.ID {
		t.Fatalf("current pointer wrong: %#v", detail)
	}
	if detail.RowVersion != 2 || detail.Enabled || detail.Timezone == nil || *detail.Timezone != "Asia/Shanghai" || detail.ResourceRefreshIntervalSeconds == nil || *detail.ResourceRefreshIntervalSeconds != 300 {
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
	enabled := strings.Replace(validSystemYAML, "enabled: false", "enabled: true", 1)
	draft := h.mustUpload(t, enabled, 1, "cmd-upload-0011")
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
	first := h.mustUpload(t, validSystemYAML, 1, "cmd-upload-0020")
	second := h.mustUpload(t, strings.Replace(validSystemYAML, "resource_refresh_interval_seconds: 300", "resource_refresh_interval_seconds: 301", 1), 1, "cmd-upload-0021")
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

func TestPublishContractFenceRejectsNonCurrentTarget(t *testing.T) {
	h := newHarness(t)
	// A draft (non-current) contract v2 reusing the same label name: uploads
	// targeting it exist, but the normal publish path must refuse until its
	// atomic activation.
	if _, err := h.contracts.CreateDraft(context.Background(), 1, "seed-contract-0003", []byte(activeContractYAML), config.Limits{}); err != nil {
		t.Fatal(err)
	}
	draft := h.mustUpload(t, validSystemYAML, 2, "cmd-upload-0030")
	_, err := h.systems.Publish(context.Background(), 1, "cmd-publish-0030", "payments", mustID(t, draft.ID), nil)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Detail, "联合激活") {
		t.Fatalf("non-current contract target must hit the atomic-activation fence: %v", err)
	}
	var current sql.NullInt64
	_ = h.db.QueryRow(`SELECT current_config_version_id FROM business_systems WHERE key='payments'`).Scan(&current)
	if current.Valid {
		t.Fatal("fenced publish must not move the pointer")
	}
}

func TestUploadCommandReplay(t *testing.T) {
	h := newHarness(t)
	first := h.mustUpload(t, validSystemYAML, 1, "cmd-upload-0040")
	replayed, err := h.upload(t, validSystemYAML, 1, "cmd-upload-0040")
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("replay must return the original draft: %v %#v", err, replayed)
	}
	var versions int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM business_system_config_versions`).Scan(&versions)
	if versions != 1 {
		t.Fatalf("replay must not append a version: %d", versions)
	}
	_, err = h.upload(t, strings.Replace(validSystemYAML, "300", "301", 1), 1, "cmd-upload-0040")
	if !errors.Is(err, ErrCommandReused) {
		t.Fatalf("same id with different content must conflict: %v", err)
	}
}

func TestListAndGetProjections(t *testing.T) {
	h := newHarness(t)
	draft := h.mustUpload(t, validSystemYAML, 1, "cmd-upload-0050")
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
	h.mustUpload(t, validSystemYAML, 1, "cmd-list-cursor-001")
	billingYAML := strings.ReplaceAll(strings.ReplaceAll(validSystemYAML, "payments", "billing"), "支付系统", "账单系统")
	h.mustUpload(t, billingYAML, 1, "cmd-list-cursor-002")

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

func mustID(t *testing.T, locator string) int64 {
	t.Helper()
	parsed, err := strconv.ParseInt(locator, 10, 64)
	if err != nil {
		t.Fatalf("locator %q: %v", locator, err)
	}
	return parsed
}

func ptrInt64(value int64) *int64 { return &value }
