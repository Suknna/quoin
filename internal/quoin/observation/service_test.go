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
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/connections"
	_ "modernc.org/sqlite"
)

type harness struct {
	db        *sql.DB
	registry  *plugins.Registry
	enabled   []string
	service   *Service
	conns     *connections.Service
	principal int64
}

func newHarness(t *testing.T, connectionType string) *harness {
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
	if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,enabled,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,'x',1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	seedPlinthSlot(t, db, now, "plinth")
	// The maintenance singleton exists in every real database; connection
	// commands read it inside their transactions.
	if _, err := db.Exec(`INSERT INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`); err != nil {
		t.Fatal(err)
	}
	// The real registration boundary: descriptors plus the deployment
	// enablement set the app wiring would resolve at boot.
	registry, enabled := observationTestRegistry(t)
	rootKey := make([]byte, 32)
	h := &harness{
		db: db, registry: registry, enabled: enabled,
		service:   NewService(db, registry, enabled),
		conns:     connections.NewService(db, func() ([]byte, error) { return rootKey, nil }),
		principal: 1,
	}
	seedEnabledMetricsConnection(t, h, connectionType, fmt.Sprintf("main-%s", connectionType), now, rootKey)
	return h
}

// observationTestRegistry pins an isolated descriptor catalog so these tests
// stay decoupled from the shared builtin catalog's tool-schema churn. The
// discovery metadata mirrors the production prometheus/thanos declarations.
func observationTestRegistry(t *testing.T) (*plugins.Registry, []string) {
	t.Helper()
	registry := plugins.NewRegistry()
	descriptors := []plugins.Descriptor{
		{
			ID: "prometheus", Version: "1", DisplayName: "Prometheus", Description: "metrics source",
			Capabilities:   []plugins.Capability{plugins.CapabilityDiscover},
			ConnectionKind: "prometheus",
			DiscoverObjects: []plugins.DiscoverObject{
				{ObjectType: "target", IdentityLabels: []string{"job", "instance"}, Query: "up", Limit: 500},
			},
		},
		{
			ID: "thanos", Version: "1", DisplayName: "Thanos", Description: "global metrics source",
			Capabilities:   []plugins.Capability{plugins.CapabilityDiscover},
			ConnectionKind: "thanos",
			DiscoverObjects: []plugins.DiscoverObject{
				{ObjectType: "target", IdentityLabels: []string{"job", "instance"}, Query: "up", Limit: 500},
			},
		},
	}
	for _, descriptor := range descriptors {
		if err := registry.RegisterDescriptor(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	return registry, []string{"prometheus", "thanos"}
}

func seedPlinthSlot(t *testing.T, db *sql.DB, now, slot string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO runtime_slots(slot,state,row_version,created_at) VALUES(?,'unregistered',1,?)`, slot, now); err != nil {
		t.Fatal(err)
	}
	credential, err := db.Exec(`INSERT INTO runtime_credentials(slot,generation,token_digest,created_at,confirmed_at,row_version) VALUES(?,1,?,?,?,1)`, slot, make([]byte, 32), now, now)
	if err != nil {
		t.Fatal(err)
	}
	credentialID, _ := credential.LastInsertId()
	if _, err := db.Exec(`UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=2 WHERE slot=?`, credentialID, slot); err != nil {
		t.Fatal(err)
	}
}

// seedEnabledMetricsConnection drives the real probe→enable path so the
// admission's grant resolution sees a genuinely enabled, current connection.
func seedEnabledMetricsConnection(t *testing.T, h *harness, connectionType, name, now string, rootKey []byte) {
	t.Helper()
	if _, err := h.db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now); err != nil {
		t.Fatal(err)
	}
	service := h.conns
	connections.ProbeContractSource = func() string { return string(gencontracts.ConnectionProbesYAML) }
	configJSON, _ := json.Marshal(map[string]any{"type": connectionType, "baseUrl": "https://metrics.test", "authType": "none"})
	summary, err := service.Create(context.Background(), connections.CreateInput{Name: name, Type: connectionType, NonSecretJSON: configJSON}, 1, "seed-observation-create-"+name)
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
	if err := h.db.QueryRow(`SELECT id FROM connection_probe_results WHERE attempt_id=?`, attemptID).Scan(&probeID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enable(context.Background(), summary.Name, summary.RowVersion, probeID, 1); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) startRun(t *testing.T, commandID, trigger string, scheduledFor *string) SourceObservationRun {
	t.Helper()
	run, err := h.service.StartRun(context.Background(), h.principal, commandID, fmt.Sprintf("main-prometheus"), trigger, scheduledFor)
	if err != nil {
		t.Fatalf("start source observation run: %v", err)
	}
	return run
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
	// A reused command id with a different request is a conflict.
	if _, err := h.service.StartRun(context.Background(), h.principal, "cmd-obs-start-0002", "main-prometheus", "enablement", nil); !errors.Is(err, ErrCommandReused) {
		t.Fatalf("reused command = %v, want ErrCommandReused", err)
	}
}

func TestStartRunRefusesUnobservableConnections(t *testing.T) {
	h := newHarness(t, "prometheus")
	if _, err := h.service.StartRun(context.Background(), h.principal, "cmd-obs-missing-0001", "no-such-connection", "manual", nil); !errors.Is(err, ErrNotObservable) {
		t.Fatalf("missing connection = %v, want ErrNotObservable", err)
	}
	// Disabling the deployment's plugin set fails admission closed: without
	// an enabled discover-capable catalog nothing may be observed.
	disabled := NewService(h.db, h.registry, []string{"alertmanager"})
	if _, err := disabled.StartRun(context.Background(), h.principal, "cmd-obs-disabled-0001", "main-prometheus", "manual", nil); !errors.Is(err, ErrNotObservable) {
		t.Fatalf("disabled plugin admission = %v, want ErrNotObservable", err)
	}
}

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

	detail, err := h.service.GetRun(context.Background(), "main-prometheus", int64(parseID(t, run.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "Completed" || len(detail.Objects) != 1 || detail.Objects[0].Status != "ok" || detail.Objects[0].EvidenceID == nil {
		t.Fatalf("run after success wrong: %#v", detail)
	}
	items, _, err := h.service.ListResources(context.Background(), "main-prometheus", "", "", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("projected resources = %d, want 2", len(items))
	}
	for _, item := range items {
		if item.State != "observed" || item.Labels["job"] != "web" || item.IdentityLabels["job"] != "web" {
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

	detail, err := h.service.GetRun(context.Background(), "main-prometheus", int64(parseID(t, second.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "Failed" || detail.ResultDetail == nil || detail.Objects[0].Status != "error" {
		t.Fatalf("failed run wrong: %#v", detail)
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
	detail, err = h.service.GetRun(context.Background(), "main-prometheus", int64(parseID(t, third.ID)))
	if err != nil {
		t.Fatal(err)
	}
	// The schema keeps the warned Run header detail-free: the object row
	// carries the exact frozen gap reason.
	if detail.State != "CompletedWithWarnings" || detail.Objects[0].Status != "gap" || detail.Objects[0].GapReason == nil {
		t.Fatalf("warned run wrong: %#v", detail)
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

	detail, err := h.service.GetRun(context.Background(), "main-prometheus", int64(parseID(t, second.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "Completed" {
		t.Fatalf("second complete run = %s", detail.State)
	}
	items, _, err := h.service.ListResources(context.Background(), "main-prometheus", "", "not_observed", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].IdentityLabels["instance"] != "two:80" {
		t.Fatalf("absent identity not expressed: %#v", items)
	}
	// The updated identity keeps its full label refresh.
	resources, _, err := h.service.ListResources(context.Background(), "main-prometheus", "", "observed", 0, 50)
	if err != nil || len(resources) != 1 || resources[0].Labels["pod"] != "one-v2" {
		t.Fatalf("label refresh wrong: %#v %v", resources, err)
	}
}

// reEnableConnection re-runs the real probe→enable cycle for an existing
// disabled connection so subsequent admission sees it observable again.
func reEnableConnection(t *testing.T, h *harness, name string) {
	t.Helper()
	attemptID, err := h.conns.StartProbe(context.Background(), name, nil, nil)
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
	if _, err := h.conns.Enable(context.Background(), name, rowVersion, probeID, 1); err != nil {
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
	if _, err := h.conns.Disable(context.Background(), "main-prometheus", enabledRowVersion); err != nil {
		t.Fatalf("disable connection: %v", err)
	}
	cancelled, err := h.service.CancelUnobservable(context.Background())
	if err != nil || len(cancelled) == 0 {
		t.Fatalf("cancel unobservable = %v, %v; want fenced children", cancelled, err)
	}
	detail, err := h.service.GetRun(context.Background(), "main-prometheus", int64(parseID(t, run.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "Cancelled" || detail.ResultDetail == nil {
		t.Fatalf("cancelled run wrong: %#v", detail)
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
		runs, _, err := h.service.ListRuns(context.Background(), "main-prometheus", 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, run := range runs {
			if run.TriggerKind == trigger {
				n++
			}
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
	if _, err := h.conns.Disable(context.Background(), "main-prometheus", enabledRowVersion); err != nil {
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
