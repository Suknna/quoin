package observation

// SQLite harness tests over the frozen schema: admission idempotence and
// dedupe, the frozen child work graph, result adjudication with its
// completeness rule (only a complete success may express absence), and the
// durable schedule/cancel reconciliation. The fake upstream is the proposal
// JSON boundary itself: Quoin never talks to the platform, the Plinth
// supervisor does (covered by the supervisor adapter tests).

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
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/execution"
	_ "modernc.org/sqlite"
)

type harness struct {
	db        *sql.DB
	dbPath    string
	registry  *plugins.Registry
	enabled   []string
	service   *Service
	conns     *connections.Service
	principal int64
	// sessionID is the administrator's verified session bound into every
	// manual-refresh context (execution.SessionRef).
	sessionID int64
}

func newHarness(t *testing.T, connectionType string) *harness {
	t.Helper()
	directory := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+directory+"/test.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// initialized=1 and a live session are what auth.VerifyExecutionSession
	// re-verifies inside the admission transaction.
	if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'x',1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	sessionResult, err := db.Exec(`INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,?,1,'observation-test',?,?,?,?)`,
		make([]byte, 32), now, now,
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
		time.Now().UTC().Add(24*time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	sessionID, _ := sessionResult.LastInsertId()
	// The maintenance singleton exists in every real database; connection
	// commands read it inside their transactions.
	if _, err := db.Exec(`INSERT INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`); err != nil {
		t.Fatal(err)
	}
	// The real registration boundary: descriptors plus the deployment
	// enablement set the app wiring would resolve at boot.
	registry, enabled := observationTestRegistry(t)
	rootKey := make([]byte, 32)
	// The connections fixture service gets the same validated read-only
	// reader (OpenReadOnly over this fixture file) — its probe lifecycle's
	// pure reads fail closed without it.
	conns := connections.NewService(db, func() ([]byte, error) { return rootKey, nil })
	connsReader, err := execution.OpenReadOnly(directory + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connsReader.Close() })
	if err := conns.SetReader(connsReader); err != nil {
		t.Fatal(err)
	}
	h := &harness{
		db: db, dbPath: directory + "/test.db", registry: registry, enabled: enabled,
		service:   newTestService(t, db, directory+"/test.db", registry, enabled),
		conns:     conns,
		principal: 1, sessionID: sessionID,
	}
	// The connection seeding commands are audited administrator mutations:
	// they run under the seeded administrator's verified session context.
	seedCtx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "seed-observation-bootstrap",
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Initiator:     execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-seed"},
		Session:       execution.SessionRef{ID: sessionID, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	seedEnabledMetricsConnection(t, seedCtx, h, connectionType, fmt.Sprintf("main-%s", connectionType), now, rootKey)
	return h
}

// newTestService composes the production read seam onto NewService: a real
// query_only reader over the same fixture file, opened through the
// execution.OpenReadOnly factory and validated by runner.SetReader's probe.
// A second writable handle is never an acceptable reader.
func newTestService(t *testing.T, db *sql.DB, dbPath string, registry *plugins.Registry, enabled []string) *Service {
	t.Helper()
	service := NewService(db, registry, enabled)
	reader, err := execution.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	if err := service.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	return service
}

// observationTestRegistry pins an isolated descriptor catalog so these tests
// stay decoupled from the shared builtin catalog's tool-schema churn. The
// discovery metadata mirrors the production prometheus/thanos declarations.
func observationTestRegistry(t *testing.T) (*plugins.Registry, []string) {
	t.Helper()
	registry := plugins.NewRegistry()
	descriptors := []plugins.Plugin{
		{
			ID: "prometheus", Version: "1", DisplayName: "Prometheus", Description: "metrics source",
			ConnectionKind: "prometheus",
			DiscoverObjects: []plugins.DiscoverObject{
				{ObjectType: "target", IdentityLabels: []string{"job", "instance"}, Query: "up", Limit: 500},
			},
		},
		{
			ID: "thanos", Version: "1", DisplayName: "Thanos", Description: "global metrics source",
			ConnectionKind: "thanos",
			DiscoverObjects: []plugins.DiscoverObject{
				{ObjectType: "target", IdentityLabels: []string{"job", "instance"}, Query: "up", Limit: 500},
			},
		},
	}
	for _, descriptor := range descriptors {
		if err := registry.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	return registry, []string{"prometheus", "thanos"}
}

// seedEnabledMetricsConnection drives the real probe→enable path so the
// admission's grant resolution sees a genuinely enabled, current connection.
func seedEnabledMetricsConnection(t *testing.T, ctx context.Context, h *harness, connectionType, name, now string, rootKey []byte) {
	t.Helper()
	if _, err := h.db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now); err != nil {
		t.Fatal(err)
	}
	service := h.conns
	connections.ProbeContractSource = func() string { return string(gencontracts.ConnectionProbesYAML) }
	configJSON, _ := json.Marshal(map[string]any{"type": connectionType, "baseUrl": "https://metrics.test", "authType": "none"})
	summary, err := service.Create(ctx, connections.CreateInput{Name: name, Type: connectionType, NonSecretJSON: configJSON}, 1, "seed-observation-create-"+name)
	if err != nil {
		t.Fatalf("SEED create: %v", err)
	}
	attemptID, err := service.StartProbe(ctx, summary.Name)
	if err != nil {
		t.Fatal(err)
	}
	// The bind/accept/result steps are runtime machinery that runs after the
	// originating request ended: on a bare context the connections service
	// restores the attempt's persisted correlation ("seed-observation-bootstrap")
	// with the system task actor — an inherited scope is rejected as unrelated.
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
	if err := h.db.QueryRow(`SELECT id FROM connection_probe_results WHERE attempt_id=?`, attemptID).Scan(&probeID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enable(ctx, summary.Name, summary.RowVersion, probeID, 1); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) startRun(t *testing.T, commandID, trigger string, scheduledFor *string) SourceObservationRun {
	t.Helper()
	var ctx context.Context = context.Background()
	if trigger == "manual" {
		ctx = h.adminContext(t)
	}
	run, err := h.service.StartRun(ctx, h.principal, commandID, fmt.Sprintf("main-prometheus"), trigger, scheduledFor)
	if err != nil {
		t.Fatalf("start source observation run: %v", err)
	}
	return run
}

// adminContext builds the exact execution metadata the app HTTP layer will
// attach for a manual refresh: the administrator actor with its verified
// session reference. Tests exercise the real contract, never a fallback.
func (h *harness) adminContext(t *testing.T) context.Context {
	t.Helper()
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	admin := execution.Principal{Kind: execution.PrincipalUser, ID: h.principal}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlation,
		Actor:         admin,
		Initiator:     admin,
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-observation-test"},
		Session:       execution.SessionRef{ID: h.sessionID, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func (h *harness) bindChildToRunning(t *testing.T, attemptID int64) {
	t.Helper()
	attempts := h.service.Attempts()
	if err := attempts.BindToStream(context.Background(), attemptID, "result-boot", 1, time.Minute, "test"); err != nil {
		t.Fatalf("bind source observation child: %v", err)
	}
	if err := attempts.Accept(context.Background(), attemptID, "result-boot", 1); err != nil {
		t.Fatalf("accept source observation child: %v", err)
	}
}

func (h *harness) commitProposal(t *testing.T, runID string, attemptID int64, outcome string, objects []map[string]any, gapReason *string) {
	t.Helper()
	proposal := map[string]any{
		"schemaKind":       ResultSchemaKind,
		"attemptId":        attemptID,
		"observationRunId": parseID(t, runID),
		"objectType":       "target",
		"outcome":          outcome,
		"observedAt":       time.Now().UTC().Format(time.RFC3339Nano),
		"objects":          objects,
		"warnings":         []string{},
		"errors":           []string{},
		"gapReason":        gapReason,
	}
	raw, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.service.CommitProposal(context.Background(), attemptID, "result-boot", 1, raw); err != nil {
		t.Fatalf("commit source observation proposal: %v", err)
	}
}

func identityObjects(series ...map[string]string) []map[string]any {
	objects := make([]map[string]any, 0, len(series))
	for _, labels := range series {
		identity := map[string]string{"job": labels["job"], "instance": labels["instance"]}
		full := map[string]string{}
		for name, value := range labels {
			full[name] = value
		}
		objects = append(objects, map[string]any{"identity": identity, "labels": full})
	}
	return objects
}

func resourceStates(t *testing.T, h *harness) map[string]map[string]any {
	t.Helper()
	rows, err := h.db.Query(`SELECT identity_key,current,stale FROM observed_source_objects ORDER BY identity_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	states := map[string]map[string]any{}
	for rows.Next() {
		var key string
		var current, stale int
		if err := rows.Scan(&key, &current, &stale); err != nil {
			t.Fatal(err)
		}
		states[key] = map[string]any{"current": current, "stale": stale}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return states
}

// runObjectRows asserts the per-object projection rows directly (the HTTP
// read surface is retired; the DB rows remain the authority).
func (h *harness) runObjectRows(t *testing.T, runID int64) []struct {
	ObjectType string
	Status     string
	GapReason  *string
	EvidenceID *string
} {
	t.Helper()
	rows, err := h.db.Query(`SELECT object_type,status,gap_reason,evidence_id FROM observation_run_objects WHERE observation_run_id=? ORDER BY object_type`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row = struct {
		ObjectType string
		Status     string
		GapReason  *string
		EvidenceID *string
	}
	out := []row{}
	for rows.Next() {
		var item row
		if err := rows.Scan(&item.ObjectType, &item.Status, &item.GapReason, &item.EvidenceID); err != nil {
			t.Fatal(err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// observedResourceRows reads the projected identity table for assertions,
// decoding the label payload the projection persists.
func (h *harness) observedResourceRows(t *testing.T) []struct {
	Current bool
	Labels  map[string]string
} {
	t.Helper()
	rows, err := h.db.Query(`SELECT current,labels_json FROM observed_source_objects ORDER BY identity_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row = struct {
		Current bool
		Labels  map[string]string
	}
	out := []row{}
	for rows.Next() {
		var current int
		var labelsJSON string
		if err := rows.Scan(&current, &labelsJSON); err != nil {
			t.Fatal(err)
		}
		labels := map[string]string{}
		if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
			t.Fatal(err)
		}
		out = append(out, row{Current: current == 1, Labels: labels})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStartRunFreezesRunningRootWithPluginOwnedChildren(t *testing.T) {
	h := newHarness(t, "prometheus")
	run := h.startRun(t, "cmd-obs-start-0001", "manual", nil)
	if run.State != "Running" || run.TriggerKind != "manual" || run.EvidenceAt == nil {
		t.Fatalf("run state wrong: %#v", run)
	}
	if len(run.Objects) != 1 || run.Objects[0].ObjectType != "target" || run.Objects[0].Status != "gap" {
		t.Fatalf("object children wrong: %#v", run.Objects)
	}
	if run.Objects[0].AttemptID == nil {
		t.Fatalf("object child must freeze its attempt binding: %#v", run.Objects[0])
	}
	// The frozen input is descriptor-owned: query, identity labels and limit
	// come from the registry catalog, not from any core constant. Snapshot
	// rows store only the digest; the rebuilder owns the canonical bytes.
	attemptID := *run.Objects[0].AttemptID
	var input struct {
		SchemaKind       string   `json:"schemaKind"`
		AttemptID        int64    `json:"attemptId"`
		ObservationRunID int64    `json:"observationRunId"`
		PluginID         string   `json:"pluginId"`
		ObjectType       string   `json:"objectType"`
		Query            string   `json:"query"`
		IdentityLabels   []string `json:"identityLabels"`
		Limit            int      `json:"limit"`
	}
	rebuilt, err := h.service.Attempts().SnapshotRebuilder(context.Background(), parseID(t, attemptID))
	if err != nil {
		t.Fatalf("rebuild frozen input: %v", err)
	}
	if err := json.Unmarshal(rebuilt, &input); err != nil {
		t.Fatal(err)
	}
	if input.SchemaKind != ExecutionSchemaKind || input.PluginID != "prometheus" || input.ObjectType != "target" {
		t.Fatalf("frozen input identity wrong: %#v", input)
	}
	if input.Query != "up" || len(input.IdentityLabels) != 2 || input.Limit != 500 {
		t.Fatalf("descriptor metadata did not freeze into the input: %#v", input)
	}
	var purpose string
	if err := h.db.QueryRow(`SELECT purpose FROM attempt_connection_grants WHERE attempt_id=?`, attemptID).Scan(&purpose); err != nil {
		t.Fatal(err)
	}
	if purpose != "config_thanos_query" {
		t.Fatalf("source grant purpose = %q", purpose)
	}
}

func parseID(t *testing.T, raw string) int64 {
	t.Helper()
	var id int64
	if _, err := fmt.Sscan(raw, &id); err != nil {
		t.Fatalf("parse locator %q: %v", raw, err)
	}
	return id
}

func TestStartRunIsIdempotentPerConnectionAndCommand(t *testing.T) {
	h := newHarness(t, "prometheus")
	first := h.startRun(t, "cmd-obs-start-0002", "manual", nil)
	// A second command on the same connection piggybacks on the active root.
	second := h.startRun(t, "cmd-obs-start-0003", "manual", nil)
	if first.ID != second.ID {
		t.Fatalf("active connection forked roots: %s vs %s", first.ID, second.ID)
	}
	// The same command replays its stored result.
	replay := h.startRun(t, "cmd-obs-start-0002", "manual", nil)
	if replay.ID != first.ID {
		t.Fatalf("command replay returned %s, want %s", replay.ID, first.ID)
	}
	// A reused command id with a different request is a conflict. The ledger
	// key includes the principal, so the probe stays on the manual identity;
	// the replay pre-stage rejects before any business runs.
	if _, err := h.service.StartRun(h.adminContext(t), h.principal, "cmd-obs-start-0002", "other-connection", "manual", nil); !errors.Is(err, ErrCommandReused) {
		t.Fatalf("reused command = %v, want ErrCommandReused", err)
	}
}

func TestStartRunRefusesUnobservableConnections(t *testing.T) {
	h := newHarness(t, "prometheus")
	if _, err := h.service.StartRun(h.adminContext(t), h.principal, "cmd-obs-missing-0001", "no-such-connection", "manual", nil); !errors.Is(err, ErrNotObservable) {
		t.Fatalf("missing connection = %v, want ErrNotObservable", err)
	}
	// Disabling the deployment's plugin set fails admission closed: without
	// an enabled discover-capable catalog nothing may be observed.
	disabled := NewService(h.db, h.registry, []string{"alertmanager"})
	if _, err := disabled.StartRun(h.adminContext(t), h.principal, "cmd-obs-disabled-0001", "main-prometheus", "manual", nil); !errors.Is(err, ErrNotObservable) {
		t.Fatalf("disabled plugin admission = %v, want ErrNotObservable", err)
	}
}

// TestStartRunManualRequiresVerifiedAdminContext pins the fail-closed
// identity contract of the manual refresh: no execution metadata, an
// unproven session, a non-admin session or a mismatched principal never
// reaches the mutation — there is no fallback that could fake the actor.
func TestStartRunManualRequiresVerifiedAdminContext(t *testing.T) {
	h := newHarness(t, "prometheus")

	// A context without execution metadata is a wiring bug, rejected closed.
	if _, err := h.service.StartRun(context.Background(), h.principal, "cmd-obs-ctx-0001", "main-prometheus", "manual", nil); !errors.Is(err, execution.ErrMissingContext) {
		t.Fatalf("missing metadata = %v, want execution.ErrMissingContext", err)
	}

	// A user actor without a session reference cannot be verified.
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	admin := execution.Principal{Kind: execution.PrincipalUser, ID: h.principal}
	sessionless, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlation, Actor: admin, Initiator: admin,
		Source: execution.Source{Kind: execution.SourceHTTP},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.StartRun(sessionless, h.principal, "cmd-obs-ctx-0002", "main-prometheus", "manual", nil); !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("sessionless actor = %v, want auth.ErrActorChanged", err)
	}

	// An operator session is not an administrator.
	operatorID, operatorSession := seedOperatorWithSession(t, h)
	operator := execution.Principal{Kind: execution.PrincipalUser, ID: operatorID}
	operatorCtx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlation + "-op", Actor: operator, Initiator: operator,
		Source:  execution.Source{Kind: execution.SourceHTTP},
		Session: execution.SessionRef{ID: operatorSession, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.StartRun(operatorCtx, operatorID, "cmd-obs-ctx-0003", "main-prometheus", "manual", nil); !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("operator admission = %v, want auth.ErrActorChanged", err)
	}

	// The command principal must be the context actor.
	if _, err := h.service.StartRun(h.adminContext(t), 999, "cmd-obs-ctx-0004", "main-prometheus", "manual", nil); err == nil || strings.Contains(err.Error(), "not observable") {
		t.Fatalf("mismatched principal = %v, want the principal match failure", err)
	}
}

// seedOperatorWithSession adds one enabled operator (the single-admin index
// is untouched) with a live session.
func seedOperatorWithSession(t *testing.T, h *harness) (int64, int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := h.db.Exec(`INSERT INTO users(username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES('operator','Operator','operator',1,1,'x',1,?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	userID, _ := result.LastInsertId()
	digest := make([]byte, 32)
	digest[0] = byte(userID) // unique per user; the digest column is UNIQUE
	session, err := h.db.Exec(`INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,1,'operator-test',?,?,?,?)`,
		userID, digest, now, now,
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
		time.Now().UTC().Add(24*time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	sessionID, _ := session.LastInsertId()
	return userID, sessionID
}

// TestObservationRunLifecycleAudited pins the automatic audit trail of the
// run lifecycle: the manual acceptance is audited under the administrator,
// the scheduled tick under the system principal with its correlation carried
// onto the created attempts, and the terminal transition writes the run's
// completed/failed record — while the per-resource telemetry upserts stay
// outside the audit trail by design.
func TestObservationRunLifecycleAudited(t *testing.T) {
	h := newHarness(t, "prometheus")
	run := h.startRun(t, "cmd-obs-audit-0001", "manual", nil)

	var actorType string
	var actorID int64
	if err := h.db.QueryRow(`SELECT actor_type,actor_id FROM audit_events WHERE action='observation.source.start'`).Scan(&actorType, &actorID); err != nil {
		t.Fatalf("manual admission audit event: %v", err)
	}
	if actorType != "user" || actorID != h.principal {
		t.Fatalf("manual admission audit actor = %s/%d", actorType, actorID)
	}

	// The created attempt carries the admission correlation (attempt.CreateOn).
	var correlated int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM execution_attempts a
		JOIN observation_run_objects o ON o.attempt_id=a.id
		WHERE a.scope_type='observation_run' AND a.scope_id=? AND a.operation_correlation_id IS NOT NULL`, parseID(t, run.ID)).Scan(&correlated); err != nil {
		t.Fatal(err)
	}
	if correlated != 1 {
		t.Fatalf("attempts without inherited correlation = %d, want 1", correlated)
	}

	// The terminal transition of the manual run writes its completed record.
	queued, err := h.service.QueuedObservationAttempts(context.Background())
	if err != nil || len(queued) != 1 {
		t.Fatalf("queued children = %v, %v; want the manual root's child", queued, err)
	}
	h.bindChildToRunning(t, queued[0])
	h.commitProposal(t, run.ID, queued[0], "success", identityObjects(
		map[string]string{"job": "web", "instance": "one:80"},
	), nil)
	var completed, failed int
	if err := h.db.QueryRow(`SELECT
			COUNT(*) FILTER(WHERE action='observation.run.complete'),
			COUNT(*) FILTER(WHERE action='observation.run.fail')
		FROM audit_events WHERE domain_ref_type='observation_run'`).Scan(&completed, &failed); err != nil {
		t.Fatal(err)
	}
	if completed != 1 || failed != 0 {
		t.Fatalf("terminal audit records complete=%d fail=%d, want 1/0", completed, failed)
	}

	// With the run terminal, the next scheduler pass admits a tick under the
	// explicit system scope; its admission is audited for the system principal.
	h.service.UseClock(func() time.Time { return time.Now().UTC().Add(10 * time.Minute) })
	if err := h.service.AdmitDue(context.Background(), time.Now().UTC().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT actor_type,actor_id FROM audit_events WHERE action='observation.source.start' AND actor_type='system'`).Scan(&actorType, &actorID); err != nil {
		t.Fatalf("scheduled admission audit event: %v", err)
	}
	if actorID != 0 {
		t.Fatalf("scheduled audit actor id = %d, want 0", actorID)
	}

	// A failed execution flips the terminal record to the fail action.
	failedRun := h.startRun(t, "cmd-obs-audit-0002", "manual", nil)
	queued, err = h.service.QueuedObservationAttempts(context.Background())
	if err != nil || len(queued) == 0 {
		t.Fatalf("queued children = %v, %v", queued, err)
	}
	h.bindChildToRunning(t, queued[0])
	h.commitProposal(t, failedRun.ID, queued[0], "error", nil, gapReason("query_failed"))
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='observation.run.fail' AND domain_ref_type='observation_run'`).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Fatalf("failed run terminal records = %d, want 1", failed)
	}
}

func gapReason(reason string) *string { return &reason }

// TestSetReaderServesReads pins the composition seam: after SetReader the
// read models are served through the injected (read-only) query surface.

func TestCommitProposalSuccessProjectsAndCompletes(t *testing.T) {
	h := newHarness(t, "prometheus")
	run := h.startRun(t, "cmd-obs-result-0001", "manual", nil)
	queued, err := h.service.QueuedObservationAttempts(context.Background())
	if err != nil || len(queued) != 1 {
		t.Fatalf("queued children = %v, %v; want exactly one", queued, err)
	}
	h.bindChildToRunning(t, queued[0])
	h.commitProposal(t, run.ID, queued[0], "success", identityObjects(
		map[string]string{"job": "web", "instance": "one:80", "pod": "one-v1"},
		map[string]string{"job": "web", "instance": "two:80"},
	), nil)

	var runState string
	runID := int64(parseID(t, run.ID))
	if err := h.db.QueryRow(`SELECT state FROM observation_runs WHERE id=?`, runID).Scan(&runState); err != nil {
		t.Fatal(err)
	}
	objects := h.runObjectRows(t, runID)
	if runState != "Completed" || len(objects) != 1 || objects[0].Status != "ok" || objects[0].EvidenceID == nil {
		t.Fatalf("run after success wrong: state=%q objects=%v", runState, objects)
	}
	resources := h.observedResourceRows(t)
	if len(resources) != 2 {
		t.Fatalf("projected resources = %d, want 2", len(resources))
	}
	for _, item := range resources {
		if !item.Current || item.Labels["job"] != "web" {
			t.Fatalf("projected resource wrong: %#v", item)
		}
	}
	// The attempt sealed its terminal state with the same transaction.
	var attemptState string
	if err := h.db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, queued[0]).Scan(&attemptState); err != nil {
		t.Fatal(err)
	}
	if attemptState != "Succeeded" {
		t.Fatalf("attempt state = %q, want Succeeded", attemptState)
	}
}

func TestCommitProposalReplayIdempotentAndConflicting(t *testing.T) {
	h := newHarness(t, "prometheus")
	run := h.startRun(t, "cmd-obs-replay-0001", "manual", nil)
	queued, err := h.service.QueuedObservationAttempts(context.Background())
	if err != nil || len(queued) != 1 {
		t.Fatalf("queued children = %v, %v", queued, err)
	}
	h.bindChildToRunning(t, queued[0])
	objects := identityObjects(map[string]string{"job": "web", "instance": "one:80"})
	proposal := map[string]any{
		"schemaKind":       ResultSchemaKind,
		"attemptId":        queued[0],
		"observationRunId": parseID(t, run.ID),
		"objectType":       "target",
		"outcome":          "success",
		"observedAt":       time.Now().UTC().Format(time.RFC3339Nano),
		"objects":          objects,
		"warnings":         []string{},
		"errors":           []string{},
		"gapReason":        nil,
	}
	raw, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.service.CommitProposal(context.Background(), queued[0], "result-boot", 1, raw); err != nil {
		t.Fatalf("initial commit = %v", err)
	}
	// The byte-identical proposal replays cleanly (ResultAck redelivery);
	// replay idempotence is defined on the immutable digest, not the intent.
	if err := h.service.CommitProposal(context.Background(), queued[0], "result-boot", 1, raw); err != nil {
		t.Fatalf("identical replay = %v, want nil", err)
	}
	// A different payload with the same identity is a conflict, never an
	// overwrite.
	proposal["observedAt"] = "2030-01-01T00:00:00Z"
	conflicting, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.service.CommitProposal(context.Background(), queued[0], "result-boot", 1, conflicting); err == nil {
		t.Fatal("conflicting replay was accepted")
	}
}

func TestCommitProposalGapNeverClearsObservedResources(t *testing.T) {
	h := newHarness(t, "prometheus")
	// First pass observes two targets.
	firstPass := h.startRun(t, "cmd-obs-gap-0001", "manual", nil)
	queued, err := h.service.QueuedObservationAttempts(context.Background())
	if err != nil || len(queued) != 1 {
		t.Fatalf("queued children = %v, %v", queued, err)
	}
	h.bindChildToRunning(t, queued[0])
	h.commitProposal(t, firstPass.ID, queued[0], "success", identityObjects(
		map[string]string{"job": "web", "instance": "one:80"},
		map[string]string{"job": "web", "instance": "two:80"},
	), nil)
	// The connection went dark: the second run's query fails. A failed or
	// incomplete pass must not withdraw the last complete projection.
	second := h.startRun(t, "cmd-obs-gap-0002", "manual", nil)
	queued, err = h.service.QueuedObservationAttempts(context.Background())
	if err != nil || len(queued) != 1 {
		t.Fatalf("queued children = %v, %v", queued, err)
	}
	h.bindChildToRunning(t, queued[0])
	gap := "query_failed"
	h.commitProposal(t, second.ID, queued[0], "error", nil, &gap)

	var failedState string
	var failedDetail sql.NullString
	if err := h.db.QueryRow(`SELECT state,result_detail FROM observation_runs WHERE id=?`, int64(parseID(t, second.ID))).Scan(&failedState, &failedDetail); err != nil {
		t.Fatal(err)
	}
	if failedState != "Failed" || !failedDetail.Valid || h.runObjectRows(t, int64(parseID(t, second.ID)))[0].Status != "error" {
		t.Fatalf("failed run wrong: state=%q detail=%v", failedState, failedDetail.Valid)
	}
	states := resourceStates(t, h)
	if len(states) != 2 {
		t.Fatalf("failed pass changed the projection: %#v", states)
	}
	for key, state := range states {
		if state["current"] != 1 {
			t.Fatalf("identity %q lost current after failed pass: %v", key, state)
		}
	}
	// A gap outcome (partial response) is equally non-destructive and warns.
	third := h.startRun(t, "cmd-obs-gap-0003", "manual", nil)
	queued, err = h.service.QueuedObservationAttempts(context.Background())
	if err != nil || len(queued) != 1 {
		t.Fatalf("queued children = %v, %v", queued, err)
	}
	h.bindChildToRunning(t, queued[0])
	partial := "partial_response"
	h.commitProposal(t, third.ID, queued[0], "gap", nil, &partial)
	var warnedState string
	if err := h.db.QueryRow(`SELECT state FROM observation_runs WHERE id=?`, int64(parseID(t, third.ID))).Scan(&warnedState); err != nil {
		t.Fatal(err)
	}
	// The schema keeps the warned Run header detail-free: the object row
	// carries the exact frozen gap reason.
	warnedObjects := h.runObjectRows(t, int64(parseID(t, third.ID)))
	if warnedState != "CompletedWithWarnings" || len(warnedObjects) != 1 || warnedObjects[0].Status != "gap" || warnedObjects[0].GapReason == nil {
		t.Fatalf("warned run wrong: state=%q objects=%v", warnedState, warnedObjects)
	}
	if len(resourceStates(t, h)) != 2 {
		t.Fatal("gap pass changed the projection")
	}
}

func TestCompleteSuccessExpressesAbsenceAsNotObserved(t *testing.T) {
	h := newHarness(t, "prometheus")
	first := h.startRun(t, "cmd-obs-absence-0001", "manual", nil)
	queued, err := h.service.QueuedObservationAttempts(context.Background())
	if err != nil || len(queued) != 1 {
		t.Fatalf("queued children = %v, %v", queued, err)
	}
	h.bindChildToRunning(t, queued[0])
	h.commitProposal(t, first.ID, queued[0], "success", identityObjects(
		map[string]string{"job": "web", "instance": "one:80"},
		map[string]string{"job": "web", "instance": "two:80"},
	), nil)
	// The second complete pass no longer sees instance two: only a complete
	// success may express that absence, as not_observed (never stale, which
	// stays an explicitly recorded fact).
	second := h.startRun(t, "cmd-obs-absence-0002", "manual", nil)
	queued, err = h.service.QueuedObservationAttempts(context.Background())
	if err != nil || len(queued) != 1 {
		t.Fatalf("queued children = %v, %v", queued, err)
	}
	h.bindChildToRunning(t, queued[0])
	h.commitProposal(t, second.ID, queued[0], "success", identityObjects(
		map[string]string{"job": "web", "instance": "one:80", "pod": "one-v2"},
	), nil)

	var secondState string
	if err := h.db.QueryRow(`SELECT state FROM observation_runs WHERE id=?`, int64(parseID(t, second.ID))).Scan(&secondState); err != nil {
		t.Fatal(err)
	}
	if secondState != "Completed" {
		t.Fatalf("second complete run = %s", secondState)
	}
	projected := h.observedResourceRows(t)
	if len(projected) != 2 {
		t.Fatalf("projection must keep both identities: %#v", projected)
	}
	for _, item := range projected {
		if item.Labels["instance"] == "two:80" && item.Current {
			t.Fatalf("absent identity not expressed: %#v", item)
		}
		// The updated identity keeps its full label refresh.
		if item.Labels["instance"] == "one:80" && (!item.Current || item.Labels["pod"] != "one-v2") {
			t.Fatalf("label refresh wrong: %#v", item)
		}
	}
}

// reEnableConnection re-runs the real probe→enable cycle for an existing
// disabled connection so subsequent admission sees it observable again.
func reEnableConnection(t *testing.T, h *harness, name string) {
	t.Helper()
	// The administrator probe start is an audited user command with its
	// verified session; the runtime bind/accept/result steps run unwired and
	// restore the attempt's persisted correlation.
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	admin, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlation + "-enable",
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: h.principal},
		Initiator:     execution.Principal{Kind: execution.PrincipalUser, ID: h.principal},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-reenable"},
		Session:       execution.SessionRef{ID: h.sessionID, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := h.conns.StartProbe(admin, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := h.conns.BindQueuedToStream(context.Background(), attemptID, "reseed-boot", 1, 5*time.Minute); err != nil || !ok {
		t.Fatalf("rebind probe: %v ok=%v", err, ok)
	}
	if err := h.conns.AcceptProbe(context.Background(), attemptID, "reseed-boot", 1); err != nil {
		t.Fatal(err)
	}
	if err := h.conns.CommitProbeResult(context.Background(), attemptID, "reseed-boot", 1, connections.TypedProbeResult{Outcome: "passed", ResultDigest: fmt.Sprintf("%064x", attemptID), StartedAt: "2026-01-02T00:00:00Z", FinishedAt: "2026-01-02T00:00:01Z"}, &connections.TypedChild{Thanos: &connections.ThanosProbeChild{Query: "vector(1)", ResponseType: "vector", SampleCount: 1, SampleValue: "1", DetailJSON: `{"kind":"prometheus"}`}}); err != nil {
		t.Fatal(err)
	}
	var probeID int64
	if err := h.db.QueryRow(`SELECT id FROM connection_probe_results WHERE attempt_id=?`, attemptID).Scan(&probeID); err != nil {
		t.Fatal(err)
	}
	var rowVersion int64
	if err := h.db.QueryRow(`SELECT row_version FROM connections WHERE name=?`, name).Scan(&rowVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := h.conns.Enable(admin, name, rowVersion, probeID, 1); err != nil {
		t.Fatal(err)
	}
}

func TestAdmitDueScheduleTicksAndCancellationReconcile(t *testing.T) {
	h := newHarness(t, "prometheus")
	// A connection disabled after a Run was admitted cancels that Run and
	// fences its children.
	run := h.startRun(t, "cmd-obs-sched-0001", "enablement", nil)
	if run.TriggerKind != "enablement" || run.State != "Running" {
		t.Fatalf("enablement run wrong: %#v", run)
	}
	// A connection disabled after a Run was admitted cancels that Run and
	// fences its children. The disable goes through the real service command
	// so the row-version trigger stays authoritative.
	var enabledRowVersion int64
	if err := h.db.QueryRow(`SELECT row_version FROM connections WHERE name='main-prometheus'`).Scan(&enabledRowVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := h.conns.Disable(h.adminContext(t), "main-prometheus", enabledRowVersion); err != nil {
		t.Fatalf("disable connection: %v", err)
	}
	cancelled, err := h.service.CancelUnobservable(context.Background())
	if err != nil || len(cancelled) == 0 {
		t.Fatalf("cancel unobservable = %v, %v; want fenced children", cancelled, err)
	}
	var cancelledState string
	var cancelledDetail sql.NullString
	if err := h.db.QueryRow(`SELECT state,result_detail FROM observation_runs WHERE id=?`, int64(parseID(t, run.ID))).Scan(&cancelledState, &cancelledDetail); err != nil {
		t.Fatal(err)
	}
	if cancelledState != "Cancelled" || !cancelledDetail.Valid {
		t.Fatalf("cancelled run wrong: state=%q detail=%v", cancelledState, cancelledDetail.Valid)
	}
	reEnableConnection(t, h, "main-prometheus")
	// Scheduling: the first pass admits immediately, the second within the
	// interval is a no-op, and the interval boundary admits a fresh tick.
	now := time.Now().UTC()
	if err := h.service.AdmitDue(context.Background(), now); err != nil {
		t.Fatalf("admit due: %v", err)
	}
	countRuns := func(trigger string) int {
		t.Helper()
		var n int
		if err := h.db.QueryRow(`SELECT COUNT(*) FROM observation_runs WHERE trigger_kind=?`, trigger).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if countRuns("schedule") != 1 {
		t.Fatalf("first schedule tick wrong: %#v", countRuns("schedule"))
	}
	if err := h.service.AdmitDue(context.Background(), now.Add(10*time.Second)); err != nil {
		t.Fatalf("admit due within interval: %v", err)
	}
	if countRuns("schedule") != 1 {
		t.Fatalf("interval boundary forked a tick: %d runs", countRuns("schedule"))
	}
	// The still-Running tick is the at-most-one-active-run fence: even past
	// the interval boundary nothing new is admitted until it turns terminal.
	later := now.Add(time.Duration(DefaultIntervalSeconds+60) * time.Second)
	if err := h.service.AdmitDue(context.Background(), later); err != nil {
		t.Fatalf("admit due with active run: %v", err)
	}
	if countRuns("schedule") != 1 {
		t.Fatalf("active run did not fence the next tick: %d", countRuns("schedule"))
	}
	// Terminate the active tick through the real disable path, then the next
	// boundary admits a fresh tick.
	if err := h.db.QueryRow(`SELECT row_version FROM connections WHERE name='main-prometheus'`).Scan(&enabledRowVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := h.conns.Disable(h.adminContext(t), "main-prometheus", enabledRowVersion); err != nil {
		t.Fatalf("re-disable: %v", err)
	}
	if _, err := h.service.CancelUnobservable(context.Background()); err != nil {
		t.Fatalf("cancel active tick: %v", err)
	}
	reEnableConnection(t, h, "main-prometheus")
	if err := h.service.AdmitDue(context.Background(), later); err != nil {
		t.Fatalf("admit due after interval: %v", err)
	}
	if countRuns("schedule") != 2 {
		t.Fatalf("interval boundary did not admit: %d schedule runs", countRuns("schedule"))
	}
	// The tick command key is the durable dedupe authority: re-running the
	// same boundary must not fork a second root for that tick.
	tick := later.Truncate(time.Duration(DefaultIntervalSeconds) * time.Second).Format(time.RFC3339)
	if _, err := h.service.StartRun(context.Background(), 0, fmt.Sprintf("source-observation:%d:%s", connectionID(t, h, "main-prometheus"), tick), "main-prometheus", "schedule", &tick); err != nil {
		t.Fatalf("replayed tick: %v", err)
	}
	if countRuns("schedule") != 2 {
		t.Fatalf("replayed tick forked a root: %d schedule runs", countRuns("schedule"))
	}
}

func connectionID(t *testing.T, h *harness, name string) int64 {
	t.Helper()
	var id int64
	if err := h.db.QueryRow(`SELECT id FROM connections WHERE name=?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
