package inspection

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/plinth/agent"
)

func TestFrozenReportInputReachesCurrentAgent(t *testing.T) {
	start, end := "2026-09-27T00:00:00Z", "2026-09-27T01:00:00Z"
	input := reportInput{
		SchemaKind: "inspection_analysis_v1", AttemptID: 11, InspectionRunID: 12,
		ReportVersion: 2, PlanKey: "shop-plan", ConnectionName: "thanos-prod",
		TemplateID: "promql_range", TemplateVersion: "v2",
		EvidenceIDs: []int64{19}, ArtifactIDs: []int64{20},
		ModelContract: reportModelContract{ModelID: "fixture", ContextBudgetTokens: 4096, MaxOutputTokens: 1024},
		Plan: &planReportContext{
			Key: "shop-plan", Params: map[string]any{"expression": "up"},
			Scope:            map[string]any{"kind": "businessView", "businessViewKey": "shop"},
			CheckDescription: &start, MetricUnit: &end,
		},
		Checks: []reportCheckItem{{CheckKey: "check-1", DisplayName: "可用性", Status: "gap", GapReason: ptrString("no_data")}},
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := agent.ParseInspectionInput(canonical)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := agent.BuildInspectionMessages(parsed)
	if err != nil {
		t.Fatal(err)
	}
	user := messages[1].Content
	startIndex := strings.Index(user, "【本次巡检冻结上下文】\n")
	endIndex := strings.Index(user, "\n逐项给出可见结论或明确缺口；")
	if startIndex < 0 || endIndex < 0 {
		t.Fatalf("structured report context missing: %s", user)
	}
	var rendered map[string]any
	if err := json.Unmarshal([]byte(user[startIndex+len("【本次巡检冻结上下文】\n"):endIndex]), &rendered); err != nil {
		t.Fatal(err)
	}
	var expectedPlan, expectedCheck map[string]any
	planJSON, _ := json.Marshal(input.Plan)
	checkJSON, _ := json.Marshal(input.Checks[0])
	_ = json.Unmarshal(planJSON, &expectedPlan)
	_ = json.Unmarshal(checkJSON, &expectedCheck)
	gotPlan, _ := json.Marshal(rendered["plan"])
	wantPlan, _ := json.Marshal(expectedPlan)
	gotCheck, _ := json.Marshal(rendered["checks"].([]any)[0])
	wantCheck, _ := json.Marshal(expectedCheck)
	if string(gotPlan) != string(wantPlan) || string(gotCheck) != string(wantCheck) {
		t.Fatalf("run facts dropped: plan=%s check=%s", gotPlan, gotCheck)
	}
	for key, want := range map[string]any{
		"planKey": "shop-plan", "connectionName": "thanos-prod", "templateId": "promql_range",
		"templateVersion": "v2", "reportVersion": float64(2),
	} {
		if rendered[key] != want {
			t.Fatalf("model context %s = %v, want %v", key, rendered[key], want)
		}
	}
	if strings.Contains(user, "contextBudgetTokens") || strings.Contains(user, "toolCatalog") {
		t.Fatal("execution-only metadata must not be exposed to the model")
	}
}

func ptrString(value string) *string { return &value }
