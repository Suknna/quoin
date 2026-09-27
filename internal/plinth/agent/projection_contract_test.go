package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/analysis"
	"github.com/Suknna/quoin/internal/quoin/investigation"
)

// These fixtures use the actual Quoin producer types rather than a second
// handwritten copy of their JSON: changing a model-visible fact in Quoin must
// either reach the current prompt or explicitly change this seam's contract.
func TestInitialAnalysisQuoinContextReachesModel(t *testing.T) {
	input := analysis.Input{
		Occurrence: analysis.OccurrenceContext{
			ID: "42", State: "Firing", Severity: "critical", Title: "CPU 饱和", Resource: "node-1",
			FirstSeenAt: "2026-09-27T01:00:00Z", LastStateChange: "2026-09-27T01:00:01Z",
			Labels: map[string]string{"alertname": "HighCPU"}, Annotations: map[string]string{"summary": "高负载"},
			Enrichment:    map[string]string{"owner": "ops"},
			Correlations:  []analysis.RenderedCorrelation{{ViewKey: "shop", DisplayName: "商城"}},
			RelatedAlerts: []analysis.RenderedRelatedAlert{{ID: "41", Severity: "warning", Title: "IO 高", State: "Resolved", StartsAt: "2026-09-26T23:00:00Z"}},
		},
		Integrations:  []analysis.RenderedIntegration{{Kind: "metrics", Name: "thanos-prod"}},
		ModelContract: analysis.ModelContract{ModelID: "fixture", ContextBudgetTokens: 4096, MaxOutputTokens: 1024},
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseInput(canonical)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := BuildInitialMessages(parsed)
	if err != nil {
		t.Fatal(err)
	}
	var expected, rendered map[string]any
	occurrenceBytes, _ := json.Marshal(input.Occurrence)
	if err := json.Unmarshal(occurrenceBytes, &expected); err != nil {
		t.Fatal(err)
	}
	user := strings.TrimPrefix(messages[len(messages)-1].Content, "请分析以下告警：\n")
	if err := json.Unmarshal([]byte(user), &rendered); err != nil {
		t.Fatal(err)
	}
	actualBytes, _ := json.Marshal(rendered["告警"])
	expectedBytes, _ := json.Marshal(expected)
	if string(actualBytes) != string(expectedBytes) {
		t.Fatalf("Quoin occurrence lost facts: got %s, want %s", actualBytes, expectedBytes)
	}
	if strings.Contains(user, "contextBudgetTokens") || strings.Contains(user, "toolCatalog") {
		t.Fatal("execution metadata leaked into the model prompt")
	}
	legacy, err := BuildPriorInitialMessages(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(legacy[len(legacy)-1].Content, `"relatedAlerts"`) || strings.Contains(legacy[len(legacy)-1].Content, `"severity"`) {
		t.Fatal("prior agent generation must keep its frozen model-message shape")
	}
}

func TestInvestigationQuoinHistoryReachesModel(t *testing.T) {
	input := investigation.Input{
		Messages: []investigation.MessageInput{{Role: "user", Content: "这条告警呢？"}},
		RecentOccurrences: []investigation.RenderedRecentOccurrence{{
			ID: "41", Severity: "critical", Title: "数据库连接超时", SourceKey: "am-prod",
			StartsAt: "2026-09-26T23:00:00Z", Labels: map[string]string{"alertname": "DBTimeout"},
		}},
		ModelContract: investigation.ModelContract{ModelID: "fixture", ContextBudgetTokens: 4096, MaxOutputTokens: 1024},
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseInvestigationInput(canonical)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := BuildInvestigationMessages(parsed)
	if err != nil {
		t.Fatal(err)
	}
	var expected, rendered map[string]any
	historyBytes, _ := json.Marshal(input.RecentOccurrences[0])
	if err := json.Unmarshal(historyBytes, &expected); err != nil {
		t.Fatal(err)
	}
	history := strings.TrimPrefix(messages[1].Content, "Quoin 平台近期收录过以下告警记录（含已恢复的；即时查询 ALERTS 为空不代表这些告警没发生过）：\n")
	if err := json.Unmarshal([]byte(history), &rendered); err != nil {
		t.Fatal(err)
	}
	actualBytes, _ := json.Marshal(rendered["近期告警记录"].([]any)[0])
	expectedBytes, _ := json.Marshal(expected)
	if string(actualBytes) != string(expectedBytes) {
		t.Fatalf("Quoin alert history lost facts: got %s, want %s", actualBytes, expectedBytes)
	}
	previous, err := BuildPriorInvestigationMessages(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(previous[1].Content, `"severity"`) || strings.Contains(previous[1].Content, `"title"`) {
		t.Fatal("prior investigation generation must retain its old history projection")
	}
}

func TestCurrentInputRejectsUnrecognizedFacts(t *testing.T) {
	tests := []struct {
		name  string
		input string
		parse func([]byte) error
	}{
		{"initial", `{"occurrence":{"id":"1","labels":{},"unexpectedFact":"lost"},"modelContract":{"modelId":"fixture"}}`, func(b []byte) error { _, err := ParseInput(b); return err }},
		{"investigation", `{"messages":[{"role":"user","content":"hello"}],"sources":[],"recentOccurrences":[{"id":"1","unexpectedFact":"lost"}],"modelContract":{"modelId":"fixture"}}`, func(b []byte) error { _, err := ParseInvestigationInput(b); return err }},
		{"inspection", `{"schemaKind":"inspection_analysis_v1","attemptId":1,"inspectionRunId":1,"plan":{"key":"p","params":{},"scope":{},"unexpectedFact":"lost"},"modelContract":{"modelId":"fixture"}}`, func(b []byte) error { _, err := ParseInspectionInput(b); return err }},
		{"knowledge", `{"schemaKind":"knowledge_extraction_v1","attemptId":1,"batchId":1,"sourceMaterialId":1,"text":"x","unexpectedFact":"lost","modelContract":{"modelId":"fixture"}}`, func(b []byte) error { _, err := ParseKnowledgeExtractionInput(b); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.parse([]byte(tt.input)); err == nil || !strings.Contains(err.Error(), "unexpectedFact") {
				t.Fatalf("unrecognized input must fail visibly, got %v", err)
			}
		})
	}
}
