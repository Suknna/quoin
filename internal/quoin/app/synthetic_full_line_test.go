package app_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/inspection"
	"github.com/Suknna/quoin/internal/quoin/testfixture"
	"github.com/Suknna/quoin/test/plugins/synthetic"
)

// syntheticCollectionGateway is the same credential-free, read-only platform
// seam used by the real dispatcher. The integration test controls its reply;
// it does not bypass the plugin's registered collection handler.
type syntheticCollectionGateway struct{}

func (syntheticCollectionGateway) Call(_ context.Context, request plugins.PlatformRequest) (*plugins.PlatformResponse, error) {
	if request.Method != "GET" || request.Path != "/collect" || request.Query.Get("expression") != "echo" {
		return nil, fmt.Errorf("unexpected synthetic collection request: %+v", request)
	}
	return &plugins.PlatformResponse{StatusCode: 200, Body: []byte(`{"value":"ok"}`)}, nil
}

// assertSyntheticAnalysisDailyLine extends the real Stele webhook → durable
// queue → authenticated Quoin Relay test on the SAME database. After the
// normalized occurrence is accepted, no brand-specific host switch is used:
// initial analysis freezes the source, the plugin's declared HTTP collection
// freezes its own grant and Evidence, and the cross-source daily workflow
// seals those facts and a versioned Agent result.
func assertSyntheticAnalysisDailyLine(t *testing.T, harness *relayTestHarness, registry *plugins.Registry, admin context.Context, sourceID int64, observedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	db := harness.database.SQL
	enabled := []string{"alertmanager", "prometheus", "synthetic-plugin"}
	catalogs, err := attempt.BuildCatalogs(registry, enabled)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.SeedModelProviderChain(t, db)
	var occurrenceID int64
	if err := db.QueryRow(`SELECT id FROM alert_occurrences WHERE source_id=? AND external_identity='upstream-1'`, sourceID).Scan(&occurrenceID); err != nil {
		t.Fatal(err)
	}
	analyses := analysis.NewService(db)
	if err := analyses.SetReader(harness.database.Reader); err != nil {
		t.Fatal(err)
	}
	analyses.UseSourceScopes(attempt.SourceScopes(registry, enabled))
	analyses.Attempts().Catalogs = catalogs
	analyses.Evidence().RegisterEntryProjectors(catalogs.HandlersTable())
	initial, err := analyses.Create(admin, occurrenceID, 1, "synthetic-full-line-analysis")
	if err != nil || initial.AttemptID < 1 {
		t.Fatalf("synthetic normalized alert cannot start initial analysis: %+v err=%v", initial, err)
	}
	if _, err := analyses.Attempts().DispatchInputFor(admin, initial.AttemptID); err != nil {
		t.Fatalf("synthetic initial analysis input cannot be rebuilt: %v", err)
	}
	if err := analyses.Attempts().BindToSlot(ctx, initial.AttemptID, "plinth", "synthetic-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := analyses.Attempts().Accept(ctx, initial.AttemptID, "synthetic-boot", 1); err != nil {
		t.Fatal(err)
	}
	seedSyntheticSucceededModelCall(t, db, initial.AttemptID, strings.Repeat("a", 64))
	canonical, _ := json.Marshal("合成告警已接收并归一化；仍需结合采证结果判断。")
	analysisDigest := sha256.Sum256(canonical)
	if err := analyses.CommitResult(ctx, analysis.Result{
		AttemptID: initial.AttemptID, BootID: "synthetic-boot", Epoch: 1,
		Succeeded: true, SchemaKind: analysis.OutputSchemaKind, Canonical: canonical, Digest: analysisDigest[:],
	}); err != nil {
		t.Fatalf("synthetic normalized alert cannot seal its initial Agent analysis: %v", err)
	}
	var initialOutput string
	if err := db.QueryRow(`SELECT content FROM initial_analysis_outputs WHERE analysis_id=?`, initial.AnalysisID).Scan(&initialOutput); err != nil || initialOutput == "" {
		t.Fatalf("synthetic initial Agent output=%q err=%v", initialOutput, err)
	}

	// One enabled synthetic HTTP instance at a frozen revision and credential
	// generation. Probe/enable and external gateway calls are covered in their
	// own host contract tests; this fixture focuses on the assembled analysis
	// and inspection paths sharing the accepted source facts.
	stamp := observedAt.Format(time.RFC3339Nano)
	_ = testfixture.SeedHTTPConnectionPair(t, db, "synthetic-full-line", synthetic.ConnectionKindValue, observedAt)
	frozenNow := observedAt
	inspections := inspection.NewServiceWithClock(db, func() time.Time { return frozenNow })
	if err := inspections.SetReader(harness.database.Reader); err != nil {
		t.Fatal(err)
	}
	if err := inspections.UsePluginRegistry(registry, enabled); err != nil {
		t.Fatal(err)
	}
	inspections.Attempts().Catalogs = catalogs
	if _, err := inspections.CreatePlan(admin, 1, "synthetic-full-line-plan", inspection.PlanInput{
		PlanKey: "synthetic-full-line", DisplayName: "Synthetic HTTP check", Enabled: true,
		ConnectionName: "synthetic-full-line", PluginID: "synthetic-plugin", TemplateID: synthetic.TemplateID,
		Params: map[string]any{"expression": "echo"}, ScopeKind: "integration", Timezone: "UTC",
	}); err != nil {
		t.Fatal(err)
	}
	run, err := inspections.CreatePlanRun(admin, 1, "synthetic-full-line-run", "synthetic-full-line")
	if err != nil {
		t.Fatal(err)
	}
	var childID int64
	if err := db.QueryRow(`SELECT id FROM execution_attempts WHERE attempt_type='inspection_collection' AND scope_id=?`, run.RunID).Scan(&childID); err != nil {
		t.Fatal(err)
	}
	var grantPurpose string
	if err := db.QueryRow(`SELECT purpose FROM attempt_connection_grants WHERE attempt_id=?`, childID).Scan(&grantPurpose); err != nil || grantPurpose != synthetic.CollectionGrantPurpose {
		t.Fatalf("collection grant purpose=%q err=%v", grantPurpose, err)
	}
	entry, ok := catalogs.Handlers[synthetic.CollectToolName]
	if !ok {
		t.Fatal("assembled synthetic collection tool is missing")
	}
	arguments, _ := json.Marshal(map[string]any{
		"templateId": synthetic.TemplateID, "templateVersion": synthetic.TemplateVersion,
		"params": map[string]any{"expression": "echo"}, "evidenceAt": stamp,
		"scopeKind": "integration", "targets": []any{},
	})
	collectedJSON, err := entry.Invoke(ctx, plugins.ToolExecution{Arguments: arguments, Platform: syntheticCollectionGateway{}})
	if err != nil {
		t.Fatal(err)
	}
	var collected plugins.CollectResult
	if err := json.Unmarshal(collectedJSON, &collected); err != nil || len(collected.Checks) != 1 || !collected.Checks[0].Succeeded {
		t.Fatalf("synthetic collection=%s err=%v", collectedJSON, err)
	}
	var evidenceEnvelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(collected.Checks[0].EvidenceJSON, &evidenceEnvelope); err != nil || len(evidenceEnvelope.Result) == 0 {
		t.Fatalf("synthetic plugin must return the canonical collection evidence envelope: %s err=%v", collected.Checks[0].EvidenceJSON, err)
	}
	if err := inspections.Attempts().BindToSlot(ctx, childID, "plinth", "synthetic-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := inspections.Attempts().Accept(ctx, childID, "synthetic-boot", 1); err != nil {
		t.Fatal(err)
	}
	proposal, _ := json.Marshal(map[string]any{
		"schemaKind": "inspection_plugin_result_v1", "attemptId": childID, "inspectionRunId": run.RunID,
		"checkKey": synthetic.TemplateID, "outcome": "success", "observedAt": stamp,
		"executionWindow": nil, "result": evidenceEnvelope.Result,
		"warnings": []string{}, "errors": []string{}, "gapReason": nil,
	})
	if err := inspections.CommitPluginProposal(ctx, childID, "synthetic-boot", 1, proposal); err != nil {
		t.Fatal(err)
	}
	collectMetricsPlanForFullLine(t, db, harness.connection, inspections, admin, observedAt)
	if _, err := inspections.CreateDailyReportConfig(admin, 1, "synthetic-full-line-daily", inspection.DailyReportConfigInput{
		ConfigKey: "synthetic-daily", DisplayName: "Synthetic daily", Enabled: true,
		Timezone: "UTC", TriggerTime: "09:00", PlanKeys: []string{"synthetic-full-line", "metrics-full-line"},
	}); err != nil {
		t.Fatal(err)
	}
	localDate := observedAt.Format("2006-01-02")
	frozenNow = time.Date(observedAt.Year(), observedAt.Month(), observedAt.Day()+1, 9, 0, 0, 0, time.UTC)
	report, err := inspections.CreateManualDailyReport(admin, 1, "synthetic-full-line-backfill", "synthetic-daily", localDate)
	if err != nil {
		t.Fatal(err)
	}
	frozenNow = frozenNow.Add(2*time.Hour + time.Second)
	if err := inspections.SealDueDailyReports(ctx, frozenNow); err != nil {
		t.Fatal(err)
	}
	sealed, err := inspections.GetDailyReport(ctx, "synthetic-daily", localDate)
	if err != nil || sealed.Latest == nil || len(sealed.Latest.Sources) != 2 {
		t.Fatalf("synthetic report=%+v err=%v", sealed, err)
	}
	var syntheticSeen, metricsSeen bool
	for _, source := range sealed.Latest.Sources {
		if len(source.Checks) != 1 || source.Checks[0].EvidenceID == nil || source.Checks[0].Status != "ok" {
			t.Fatalf("cross-source report contains incomplete source: %+v", source)
		}
		check := source.Checks[0]
		switch source.PlanKey {
		case "synthetic-full-line":
			syntheticSeen = string(check.Result) == string(evidenceEnvelope.Result) && check.Measurement == nil
		case "metrics-full-line":
			metricsSeen = len(check.Result) == 0 && check.Measurement != nil && check.Measurement.ResultType == "vector"
		}
	}
	if !syntheticSeen || !metricsSeen {
		t.Fatalf("synthetic JSON and existing metrics Evidence did not coexist in one frozen report: %+v", sealed.Latest.Sources)
	}
	if err := inspections.EnsureDueDailyReportAnalyses(ctx); err != nil {
		t.Fatal(err)
	}
	var reportID, agentAttemptID int64
	if err := db.QueryRow(`SELECT id FROM inspection_daily_reports WHERE config_key=? AND local_date=?`, "synthetic-daily", localDate).Scan(&reportID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT id FROM execution_attempts WHERE attempt_type='inspection_daily_analysis' AND scope_id=?`, reportID).Scan(&agentAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := inspections.Attempts().DispatchInputFor(admin, agentAttemptID); err != nil {
		t.Fatalf("synthetic Agent input cannot rebuild from frozen Evidence: %v", err)
	}
	if err := inspections.Attempts().BindToSlot(ctx, agentAttemptID, "plinth", "synthetic-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := inspections.Attempts().Accept(ctx, agentAttemptID, "synthetic-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := strings.Repeat("d", 64)
	callID := seedSyntheticSucceededModelCall(t, db, agentAttemptID, promptDigest)
	content := "Synthetic daily: value=ok; inspect the referenced Run and Evidence."
	dailyCanonical := fmt.Sprintf("inspection_daily_analysis_result_v1|%d|%d|%d|success|%s|%s", agentAttemptID, reportID, callID, content, promptDigest)
	digest := sha256.Sum256([]byte(dailyCanonical))
	output, _ := json.Marshal(map[string]any{
		"schemaKind": "inspection_daily_analysis_result_v1", "attemptId": agentAttemptID,
		"dailyReportId": reportID, "configKey": "synthetic-daily", "localDate": localDate,
		"reportVersion": 1, "modelCallId": callID, "outcome": "success", "content": content,
		"resultDigest": hex.EncodeToString(digest[:]), "promptDigest": promptDigest,
	})
	if err := inspections.CommitDailyAnalysisProposal(ctx, agentAttemptID, "synthetic-boot", 1, output); err != nil {
		t.Fatal(err)
	}
	versions, err := inspections.ListDailyReportAnalyses(ctx, "synthetic-daily", localDate)
	if err != nil || len(versions) != 1 || versions[0].AttemptState != "Succeeded" || versions[0].ReportVersion != report.LatestVersion+1 {
		t.Fatalf("synthetic daily Agent result=%+v err=%v", versions, err)
	}
}

func seedSyntheticSucceededModelCall(t *testing.T, db *sql.DB, attemptID int64, promptDigest string) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,provider_request_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,usage_json,latency_ms,status,termination_reason,started_at,ended_at)
		VALUES(?,1,0,'chat','fixture-chat-1',(SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'),NULL,'v1','agent-v1',?,'v1',?,?,?,2,1,0,0,NULL,NULL,'running',NULL,?,NULL)`, attemptID, attemptID, promptDigest, promptDigest, promptDigest, promptDigest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, err := call.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for seq, kind := range []string{"system_contract", "tool_schema"} {
		if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,?,'system',?,?)`, callID, seq+1, promptDigest, kind); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"tool_calls":[]}',?,'stop',?)`, callID, promptDigest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET status='succeeded',usage_json='{}',latency_ms=1,ended_at=? WHERE id=?`, now, callID); err != nil {
		t.Fatal(err)
	}
	return callID
}

// An existing metrics source contributes a real Run/Evidence into the same
// report as the new JSON plugin. The legacy metrics executor has its own
// dispatch test; here the authoritative proposal/closure is the boundary
// under test, without inserting fake daily check rows directly into SQL.
func collectMetricsPlanForFullLine(t *testing.T, db *sql.DB, conns *connections.Service, inspections *inspection.Service, admin context.Context, observedAt time.Time) {
	t.Helper()
	created, err := conns.Create(admin, connections.CreateInput{
		Name: "metrics-full-line", Type: connections.TypePrometheus,
		NonSecretJSON: []byte(`{"type":"prometheus","baseUrl":"https://metrics.test","authType":"none"}`),
	}, 1, "metrics-full-line-create")
	if err != nil {
		t.Fatal(err)
	}
	probeAttempt, err := conns.StartProbe(admin, created.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := conns.BindQueuedToStream(context.Background(), probeAttempt, "synthetic-boot", 1, 5*time.Minute); err != nil || !ok {
		t.Fatalf("metrics qualification probe bind: ok=%v err=%v", ok, err)
	}
	if err := conns.AcceptProbe(context.Background(), probeAttempt, "synthetic-boot", 1); err != nil {
		t.Fatal(err)
	}
	probeResult := connections.TypedProbeResult{
		Outcome: "passed", ResultDigest: strings.Repeat("2", 64),
		StartedAt: observedAt.UTC().Format(time.RFC3339Nano), FinishedAt: observedAt.Add(time.Second).UTC().Format(time.RFC3339Nano),
	}
	probeChild := &connections.TypedChild{Thanos: &connections.ThanosProbeChild{
		Query: "vector(1)", ResponseType: "vector", SampleCount: 1, SampleValue: "1", DetailJSON: `{"kind":"prometheus"}`,
	}}
	if err := conns.CommitProbeResult(context.Background(), probeAttempt, "synthetic-boot", 1, probeResult, probeChild); err != nil {
		t.Fatal(err)
	}
	var probeID int64
	if err := db.QueryRow(`SELECT id FROM connection_probe_results WHERE attempt_id=?`, probeAttempt).Scan(&probeID); err != nil {
		t.Fatal(err)
	}
	if _, err := conns.Enable(admin, created.Name, created.RowVersion, probeID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := inspections.CreatePlan(admin, 1, "metrics-full-line-plan", inspection.PlanInput{
		PlanKey: "metrics-full-line", DisplayName: "Existing metrics check", Enabled: true,
		ConnectionName: "metrics-full-line", PluginID: "prometheus", TemplateID: "promql_instant",
		Params: map[string]any{"expression": "up"}, ScopeKind: "integration", Timezone: "UTC",
	}); err != nil {
		t.Fatal(err)
	}
	run, err := inspections.CreatePlanRun(admin, 1, "metrics-full-line-run", "metrics-full-line")
	if err != nil {
		t.Fatal(err)
	}
	var childID int64
	if err := db.QueryRow(`SELECT id FROM execution_attempts WHERE attempt_type='inspection_collection' AND scope_id=?`, run.RunID).Scan(&childID); err != nil {
		t.Fatal(err)
	}
	if err := inspections.Attempts().BindToSlot(context.Background(), childID, "plinth", "synthetic-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := inspections.Attempts().Accept(context.Background(), childID, "synthetic-boot", 1); err != nil {
		t.Fatal(err)
	}
	vector := map[string]any{"resultType": "vector", "result": []any{map[string]any{
		"metric": map[string]string{"job": "api"}, "value": []any{observedAt.Unix(), "1"},
	}}}
	proposal, _ := json.Marshal(map[string]any{
		"schemaKind": "inspection_plugin_result_v1", "attemptId": childID, "inspectionRunId": run.RunID,
		"checkKey": "promql_instant", "outcome": "success", "observedAt": observedAt.Format(time.RFC3339Nano),
		"executionWindow": nil, "result": vector, "warnings": []string{}, "errors": []string{}, "gapReason": nil,
	})
	if err := inspections.CommitPluginProposal(context.Background(), childID, "synthetic-boot", 1, proposal); err != nil {
		t.Fatal(err)
	}
}
