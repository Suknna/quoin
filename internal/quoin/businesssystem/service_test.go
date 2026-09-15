package businesssystem

// SQLite harness tests over the frozen schema. The retired upload/publish
// write commands are gone (ADR0004): tests build lawful historical state
// through test-only SQL fixtures (mustUpload/publishFixture) that reuse the
// production parser/compile seam and satisfy every frozen trigger. What
// remains under test are the retained reads, deployment verification
// acceptance, and the historical read/convergence surfaces
// (DATA-CONFIG-001/003/004, HTTP-CONFIG-001/002).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"strconv"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/config"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/execution"
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
	attempts  *attempt.Service
	principal int64
}

// readOnlyFixturePool opens an independent read-only pool over the same
// database file (execution.OpenReadOnly is the trusted factory) with the
// fixture's cleanup lifetime.
func readOnlyFixturePool(t *testing.T, db *sql.DB) audit.Reader {
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
	return reader
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
	if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'x',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,randomblob(32),1,'businesssystem-test',?,?,?,?)`, now, now, "2036-09-15T00:00:00Z", "2036-09-22T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	seedPlinthRuntime(t, db, now)
	seedThanosExecutionPath(t, db, now)
	seedLintelRuntime(t, db, now)
	systems := NewService(db)
	// Verification history/evidence reads serve the trusted read-only pool.
	reader := readOnlyFixturePool(t, db)
	systems.SetReader(reader)
	// The child attempt state machine shares the same pool: correlation and
	// history reads go through it, writes keep the writer.
	attempts := attempt.NewService(db)
	if err := attempts.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	return &harness{db: db, systems: systems, attempts: attempts, principal: 1}
}

// adminContext builds the authenticated admin execution context the shared
// runner requires: the seeded enabled initialized admin session proves the
// actor inside the runner transaction's session recheck.
func (h *harness) adminContext(t *testing.T) context.Context {
	t.Helper()
	meta := execution.Metadata{
		CorrelationID: strings.ToLower(strings.Repeat("v", 32)),
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: h.principal},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-verify"},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
	}
	ctx, err := execution.WithMetadata(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
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
	service.SetReader(readOnlyFixturePool(t, db))
	connections.ProbeContractSource = func() string { return string(gencontracts.ConnectionProbesYAML) }
	configJSON, _ := json.Marshal(map[string]any{"type": connectionType, "baseUrl": "https://metrics.test", "authType": "none"})
	summary, err := service.Create(businessSystemAdminContext(t), connections.CreateInput{Name: name, Type: connectionType, NonSecretJSON: configJSON}, 1, "seed-metrics-create-"+name)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := service.StartProbe(businessSystemAdminContext(t), summary.Name, nil, nil)
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
	if _, err := service.Enable(businessSystemAdminContext(t), summary.Name, summary.RowVersion, probeID, 1); err != nil {
		t.Fatal(err)
	}
}

// businessSystemAdminContext is the trusted-entry execution metadata the
// connection commands re-verify in-transaction (admin user 1, session 1).
func businessSystemAdminContext(t *testing.T) context.Context {
	t.Helper()
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "corr-businesssystem-seed",
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-businesssystem-seed"},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
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

// mustUpload recreates the retired upload's persisted historical state with
// test-only SQL. It parses and compiles the declaration through the production
// config seam, then writes exactly the immutable rows the retired producer
// wrote (Disabled system, draft version, scopes, typed projections) so the
// frozen triggers stay in force. Call sites keep their historical command IDs;
// the fixture persists no command ledger row because retained reads never
// consult one.
func (h *harness) mustUpload(t *testing.T, body string, arguments ...any) ConfigVersionDetail {
	t.Helper()
	declaration, fields := config.ParseBusinessSystem([]byte(body), config.Limits{})
	if len(fields) != 0 {
		t.Fatalf("historical fixture declaration must parse: %v", fields)
	}
	document, err := config.CompileBusinessSystemDocument(declaration)
	if err != nil {
		t.Fatalf("historical fixture declaration must compile: %v", err)
	}
	_, catalogVersion, catalogDigest, err := config.JourneyCatalog()
	if err != nil {
		t.Fatal(err)
	}
	// Resolve stable reference names into immutable locators, exactly as the
	// retired upload did inside its serialized transaction.
	if err := h.db.QueryRow(`SELECT id,type FROM connections WHERE name=?`, document.MetricsConnectionRef).Scan(&document.MetricsConnectionID, new(string)); err != nil {
		t.Fatalf("fixture metrics connection %q: %v", document.MetricsConnectionRef, err)
	}
	for index, ref := range document.AlertSourceRefs {
		var sourceID int64
		if err := h.db.QueryRow(`SELECT id FROM alert_sources WHERE source_key=?`, ref).Scan(&sourceID); err != nil {
			t.Fatalf("fixture alert source[%d] %q: %v", index, ref, err)
		}
		document.AlertSourceIDs = append(document.AlertSourceIDs, sourceID)
	}
	declarationJSON, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal resolved fixture declaration: %v", err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(declarationJSON))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var systemID int64
	if err := h.db.QueryRow(`SELECT id FROM business_systems WHERE key=?`, document.SystemKey).Scan(&systemID); errors.Is(err, sql.ErrNoRows) {
		result, insertErr := h.db.Exec(`INSERT INTO business_systems(key,display_name,enabled,row_version,created_at) VALUES(?,?,0,1,?)`, document.SystemKey, document.DisplayName, now)
		if insertErr != nil {
			t.Fatal(insertErr)
		}
		systemID, _ = result.LastInsertId()
	} else if err != nil {
		t.Fatal(err)
	}
	var versionSeq int64
	if err := h.db.QueryRow(`SELECT COALESCE(MAX(version_seq),0)+1 FROM business_system_config_versions WHERE business_system_id=?`, systemID).Scan(&versionSeq); err != nil {
		t.Fatal(err)
	}
	result, err := h.db.Exec(`INSERT INTO business_system_config_versions(business_system_id,version_seq,state,yaml_body,parser_version,schema_version,label_contract_version_id,declaration_json,description,discovery_refresh_seconds,journey_catalog_digest,journey_catalog_version,digest,created_by,created_at,system_key,display_name,metrics_connection_id,enabled,timezone) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		systemID, versionSeq, "draft", body, config.ParserVersion, config.SchemaVersion(config.SchemaBusinessSystem), nil, string(declarationJSON), document.Description, document.DiscoveryRefreshIntervalSeconds, catalogDigest, catalogVersion, digest, h.principal, now, document.SystemKey, document.DisplayName, document.MetricsConnectionID, boolToInt(document.Enabled), document.Timezone)
	if err != nil {
		t.Fatal(err)
	}
	versionID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := seedFixtureProjections(t, h.db, versionID, document); err != nil {
		t.Fatal(err)
	}
	detail, err := h.systems.GetVersion(context.Background(), document.SystemKey, versionID)
	if err != nil {
		t.Fatalf("read back historical fixture draft: %v", err)
	}
	return detail
}

// seedFixtureProjections writes the same typed projection rows the retired
// upload persisted (DATA-CONFIG-003): scopes, alert attribution, discoveries
// and plan/check columns.
func seedFixtureProjections(t *testing.T, db *sql.DB, versionID int64, document config.BusinessSystemDocument) error {
	t.Helper()
	for _, resource := range document.Resources {
		selectors, err := json.Marshal(resource.MatchLabels)
		if err != nil {
			return err
		}
		identity, err := json.Marshal(resource.IdentityLabels)
		if err != nil {
			return err
		}
		allowed, err := json.Marshal(resource.AllowedMetrics)
		if err != nil {
			return err
		}
		if _, err := db.Exec(`INSERT INTO config_resource_scopes(config_version_id,resource_key,display_name,discovery_metric,selectors_json,identity_labels_json,allowed_metrics_json) VALUES(?,?,?,?,?,?,?)`, versionID, resource.Name, resource.DisplayName, resource.DiscoveryMetric, string(selectors), string(identity), string(allowed)); err != nil {
			return err
		}
	}
	for _, sourceID := range document.AlertSourceIDs {
		if _, err := db.Exec(`INSERT INTO config_alert_source_refs(config_version_id,alert_source_id) VALUES(?,?)`, versionID, sourceID); err != nil {
			return err
		}
	}
	for labelName, labelValue := range document.AlertSourceLabels {
		if _, err := db.Exec(`INSERT INTO config_alert_label_conditions(config_version_id,label_name,label_value) VALUES(?,?,?)`, versionID, labelName, labelValue); err != nil {
			return err
		}
	}
	for _, discovery := range document.Discoveries {
		labels, err := json.Marshal(discovery.IdentityLabels)
		if err != nil {
			return err
		}
		if _, err := db.Exec(`INSERT INTO config_discoveries(config_version_id,discovery_key,display_name,selector,identity_labels_json) VALUES(?,?,?,?,?)`, versionID, discovery.Key, discovery.DisplayName, discovery.Selector, string(labels)); err != nil {
			return err
		}
	}
	for _, plan := range document.Plans {
		planInsert, err := db.Exec(`INSERT INTO config_plans(config_version_id,plan_key,display_name,timezone,cron) VALUES(?,?,?,?,?)`, versionID, plan.Key, plan.DisplayName, plan.Timezone, nullableString(plan.Cron))
		if err != nil {
			return err
		}
		planID, err := planInsert.LastInsertId()
		if err != nil {
			return err
		}
		for _, check := range plan.Checks {
			var queryMode, expression, journeyID, journeyParams any
			var rangeSeconds, stepSeconds any
			if check.Kind == "promql" {
				queryMode, expression = check.QueryMode, check.Expression
				if check.QueryMode == "range" {
					rangeSeconds, stepSeconds = check.RangeSeconds, check.StepSeconds
				}
			} else {
				journeyID = check.JourneyID
				params, err := json.Marshal(check.JourneyParams)
				if err != nil {
					return err
				}
				journeyParams = string(params)
			}
			if _, err := db.Exec(`INSERT INTO config_checks(plan_id,check_key,display_name,analysis_question,kind,query_mode,expression,range_seconds,step_seconds,journey_id,journey_params_json) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, planID, check.Key, check.DisplayName, check.AnalysisQuestion, check.Kind, queryMode, expression, rangeSeconds, stepSeconds, journeyID, journeyParams); err != nil {
				return err
			}
		}
	}
	return nil
}

// publishFixture derives a historical published pointer through the same
// lawful single UPDATE the retired publish command ran: the target version's
// root projection travels in the same UPDATE, so the frozen pointer,
// projection, and derived-state triggers validate the mutation and write the
// published/superseded history. No trigger or constraint is disabled.
func (h *harness) publishFixture(t *testing.T, systemKey string, versionID int64) BusinessSystemDetail {
	t.Helper()
	var displayName, timezone string
	var enabled int64
	if err := h.db.QueryRow(`SELECT display_name,enabled,timezone FROM business_system_config_versions WHERE id=? AND business_system_id=(SELECT id FROM business_systems WHERE key=?)`, versionID, systemKey).Scan(&displayName, &enabled, &timezone); err != nil {
		t.Fatalf("publish fixture target version: %v", err)
	}
	if _, err := h.db.Exec(`UPDATE business_systems SET current_config_version_id=?, display_name=?, enabled=?, timezone=?, row_version=row_version+1 WHERE key=? AND current_config_version_id IS NULL`, versionID, displayName, enabled, timezone, systemKey); err != nil {
		t.Fatalf("publish fixture pointer move: %v", err)
	}
	detail, err := h.systems.GetSystem(context.Background(), systemKey)
	if err != nil {
		t.Fatalf("publish fixture readback: %v", err)
	}
	return detail
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
