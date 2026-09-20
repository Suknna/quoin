package app

// Local execution tests (ADR-0011): the connection_probe path binds under the
// local identity, executes metrics_probe through the stubbed internal tool
// table and commits the same canonical payload shape the Plinth supervisor
// used to propose — outcome, typed child columns and boot binding included.
// The inspection collection paths (promql + plugin templates) are covered by
// the commit-side tests in internal/quoin/inspection plus the observation
// end-to-end test in source_observation_recovery_test.go; here the probe
// slice is exercised in full because its payload mapping lives in this
// package.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/inspection"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	_ "modernc.org/sqlite"
)

// stubLocalProbeTool replaces the internal tool lookup with a metrics_probe
// stub returning the passed observation.
func stubLocalProbeTool(t *testing.T, reachable bool, detail string) {
	t.Helper()
	previous := localMetricsToolEntry
	localMetricsToolEntry = func(name string) (plugins.ToolEntry, bool) {
		if name != "metrics_probe" {
			return plugins.ToolEntry{}, false
		}
		return plugins.ToolEntry{Timeout: time.Second, Invoke: func(ctx context.Context, exec plugins.ToolExecution) (json.RawMessage, error) {
			probe := struct {
				Reachable    bool   `json:"reachable"`
				LatencyMS    int64  `json:"latencyMs"`
				Kind         string `json:"kind"`
				Query        string `json:"query"`
				ResponseType string `json:"responseType,omitempty"`
				SampleCount  int    `json:"sampleCount,omitempty"`
				SampleValue  string `json:"sampleValue,omitempty"`
				Detail       string `json:"detail,omitempty"`
			}{Reachable: reachable, Kind: exec.Conn.Type, Query: "vector(1)", Detail: detail}
			if reachable {
				probe.ResponseType = "vector"
				probe.SampleCount = 1
				probe.SampleValue = "1"
			}
			return json.Marshal(probe)
		}}, true
	}
	t.Cleanup(func() { localMetricsToolEntry = previous })
}

// newLocalProbeFixture seeds an enabled prometheus connection (reuse of the
// observation recovery seeding shape) plus one Queued connection_probe
// attempt and returns the service with the local execution loop wired.
func newLocalProbeFixture(t *testing.T) (*sql.DB, *RuntimeService, int64) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/local-probe.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
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
	mustExec(t, db, `INSERT INTO connections(id,name,type,enabled,row_version,revalidation_required,created_at) VALUES(1,'main-prometheus','prometheus',0,1,0,?)`, now)
	mustExec(t, db, `INSERT INTO connection_revisions(id,connection_id,revision_seq,config_json,created_at) VALUES(1,1,1,'{"type":"prometheus","baseUrl":"https://metrics.test"}',?)`, now)
	mustExec(t, db, `INSERT INTO credential_generations(id,connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(1,1,1,1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now)
	mustExec(t, db, `UPDATE connections SET current_revision_id=1,current_credential_generation_id=1,row_version=2 WHERE id=1`)
	// One Queued probe attempt with its frozen snapshot and grant.
	mustExec(t, db, `INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,quoin_release_version,operation_correlation_id,initiator_type,initiator_id,created_at)
		VALUES(5,'connection_probe','connection',1,'Queued','q','corr-local-probe','system',0,?)`, now)
	inputDigest := sha256HexOf([]byte(`{"connectionName":"main-prometheus"}`))
	mustExec(t, db, `INSERT INTO attempt_input_snapshots(id,attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(5,5,'connection_probe_v1','v1',?,?)`, inputDigest, now)
	mustExec(t, db, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(5,1,'connection_config',?,1)`, inputDigest)
	mustExec(t, db, `INSERT INTO attempt_connection_grants(id,attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(5,5,'prometheus_probe',1,1,1,?)`, now)

	reader := fixtureReadOnlyPool(t, db)
	connections.ProbeContractSource = func() string { return "contract_version: 1" }
	conns := connections.NewService(db, func() ([]byte, error) { return make([]byte, 32), nil })
	if err := conns.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	analyses := analysis.NewService(db)
	if err := analyses.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	inspections := inspection.NewService(db)
	if err := inspections.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	service := NewRuntimeControl(qruntime.NewService(), "test", conns, db)
	service.Analyses = analyses
	service.Inspections = inspections
	return db, service, 5
}

func sha256HexOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func TestLocalProbeExecutionCommitsPassedResult(t *testing.T) {
	stubLocalProbeTool(t, true, "")
	db, service, attemptID := newLocalProbeFixture(t)
	service.runLocalExecutionPass(context.Background())

	var state, boot string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	if state != "Succeeded" {
		t.Fatalf("local probe left attempt %d in %s, want Succeeded", attemptID, state)
	}
	mustQuery(t, db, `SELECT boot_id FROM execution_attempts WHERE id=?`, &boot, attemptID)
	if boot != localExecutionBootID {
		t.Fatalf("local probe binding boot %q, want %q", boot, localExecutionBootID)
	}
	var outcome, actionSet string
	var sampleValue string
	mustQuery(t, db, `SELECT outcome FROM connection_probe_results WHERE attempt_id=?`, &outcome, attemptID)
	mustQuery(t, db, `SELECT action_set_id FROM connection_probe_results WHERE attempt_id=?`, &actionSet, attemptID)
	if outcome != "passed" || actionSet != "prometheus-query-v1" {
		t.Fatalf("probe result outcome=%s actionSet=%s, want passed/prometheus-query-v1", outcome, actionSet)
	}
	mustQuery(t, db, `SELECT sample_value FROM thanos_connection_probe_results t JOIN connection_probe_results r ON r.id=t.probe_result_id WHERE r.attempt_id=?`, &sampleValue, attemptID)
	if sampleValue != "1" {
		t.Fatalf("typed child sample value %q, want the observed vector(1) sample", sampleValue)
	}
}

func TestLocalProbeExecutionCommitsFailedResultOnContractMismatch(t *testing.T) {
	stubLocalProbeTool(t, false, "查询请求失败: connection refused")
	db, service, attemptID := newLocalProbeFixture(t)
	service.runLocalExecutionPass(context.Background())

	var state, outcome, termination string
	var detailJSON string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	if state != "Failed" {
		t.Fatalf("failed probe left attempt %d in %s, want Failed", attemptID, state)
	}
	mustQuery(t, db, `SELECT outcome FROM connection_probe_results WHERE attempt_id=?`, &outcome, attemptID)
	mustQuery(t, db, `SELECT termination_reason FROM execution_attempts WHERE id=?`, &termination, attemptID)
	if outcome != "failed" || termination != "invalid_response" {
		t.Fatalf("outcome=%s termination=%s, want failed/invalid_response", outcome, termination)
	}
	mustQuery(t, db, `SELECT detail_json FROM thanos_connection_probe_results t JOIN connection_probe_results r ON r.id=t.probe_result_id WHERE r.attempt_id=?`, &detailJSON, attemptID)
	var detail map[string]any
	if err := json.Unmarshal([]byte(detailJSON), &detail); err != nil {
		t.Fatal(err)
	}
	if detail["kind"] != "prometheus" || detail["error"] != "查询请求失败: connection refused" {
		t.Fatalf("failed probe detail=%v, want the typed kind and failure reason", detail)
	}
}

func TestLocalProbeExecutionWithoutGatewayFailsClosed(t *testing.T) {
	// No tool stub: the internal tool lookup misses and the execution must
	// still converge deterministically instead of wedging in Running.
	db, service, attemptID := newLocalProbeFixture(t)
	previous := localMetricsToolEntry
	localMetricsToolEntry = func(string) (plugins.ToolEntry, bool) { return plugins.ToolEntry{}, false }
	t.Cleanup(func() { localMetricsToolEntry = previous })
	service.runLocalExecutionPass(context.Background())

	var state, outcome string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	if state != "Failed" {
		t.Fatalf("executor-less probe left attempt %d in %s, want Failed", attemptID, state)
	}
	mustQuery(t, db, `SELECT outcome FROM connection_probe_results WHERE attempt_id=?`, &outcome, attemptID)
	if outcome != "failed" {
		t.Fatalf("executor-less probe outcome=%s, want failed", outcome)
	}
}
