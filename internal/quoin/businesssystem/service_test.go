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
	seedThanosExecutionPath(t, db, now)
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

// mustUpload recreates the retired upload's persisted historical state with
// test-only SQL. It parses and compiles the declaration through the production
// config seam, then writes exactly the immutable rows the retired producer
// wrote (Disabled system, draft version, scopes, typed projections) so the
// frozen triggers stay in force. Call sites keep their historical command IDs;
// the fixture persists no command ledger row because retained reads never
// consult one.
func (h *harness) mustUpload(t *testing.T, body string, arguments ...any) int64 {
	t.Helper()
	declaration, fields := config.ParseBusinessSystem([]byte(body), config.Limits{})
	if len(fields) != 0 {
		t.Fatalf("historical fixture declaration must parse: %v", fields)
	}
	document, err := config.CompileBusinessSystemDocument(declaration)
	if err != nil {
		t.Fatalf("historical fixture declaration must compile: %v", err)
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
	result, err := h.db.Exec(`INSERT INTO business_system_config_versions(business_system_id,version_seq,state,yaml_body,parser_version,schema_version,label_contract_version_id,declaration_json,description,discovery_refresh_seconds,digest,created_by,created_at,system_key,display_name,metrics_connection_id,enabled,timezone) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		systemID, versionSeq, "draft", body, config.ParserVersion, config.SchemaVersion(config.SchemaBusinessSystem), nil, string(declarationJSON), document.Description, document.DiscoveryRefreshIntervalSeconds, digest, h.principal, now, document.SystemKey, document.DisplayName, document.MetricsConnectionID, boolToInt(document.Enabled), document.Timezone)
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
	return versionID
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
			var rangeSeconds, stepSeconds any
			if check.QueryMode == "range" {
				rangeSeconds, stepSeconds = check.RangeSeconds, check.StepSeconds
			}
			if _, err := db.Exec(`INSERT INTO config_checks(plan_id,check_key,display_name,analysis_question,kind,query_mode,expression,range_seconds,step_seconds) VALUES(?,?,?,?,?,?,?,?,?)`, planID, check.Key, check.DisplayName, check.AnalysisQuestion, check.Kind, check.QueryMode, check.Expression, rangeSeconds, stepSeconds); err != nil {
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
func (h *harness) publishFixture(t *testing.T, systemKey string, versionID int64) {
	t.Helper()
	var displayName, timezone string
	var enabled int64
	if err := h.db.QueryRow(`SELECT display_name,enabled,timezone FROM business_system_config_versions WHERE id=? AND business_system_id=(SELECT id FROM business_systems WHERE key=?)`, versionID, systemKey).Scan(&displayName, &enabled, &timezone); err != nil {
		t.Fatalf("publish fixture target version: %v", err)
	}
	if _, err := h.db.Exec(`UPDATE business_systems SET current_config_version_id=?, display_name=?, enabled=?, timezone=?, row_version=row_version+1 WHERE key=? AND current_config_version_id IS NULL`, versionID, displayName, enabled, timezone, systemKey); err != nil {
		t.Fatalf("publish fixture pointer move: %v", err)
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
