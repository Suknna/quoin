package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/inspection"
	"github.com/Suknna/quoin/internal/quoin/testfixture"
	"github.com/Suknna/quoin/test/plugins/synthetic"
)

type syntheticCollectionCaller struct {
	requests []plugins.PlatformRequest
}

func (caller *syntheticCollectionCaller) Call(_ context.Context, request plugins.PlatformRequest) (*plugins.PlatformResponse, error) {
	caller.requests = append(caller.requests, request)
	return &plugins.PlatformResponse{StatusCode: 200, Body: []byte(`{"value":"ok"}`)}, nil
}

// The runtime worker must select the new plugin's declared internal tool, not
// silently execute metrics_collect as the old host switch did. This test runs
// the actual local scan/dispatch/commit path rather than calling ToolEntry
// directly (the webhook → queue → Relay → Agent side is exercised in the
// separate same-DB high-level acceptance test).
func TestLocalSyntheticCollectionUsesDeclaredTool(t *testing.T) {
	stubLocalCollectTool(t) // existing metrics probe qualifies the fixture connection
	db, service := newLocalInspectionFixture(t)
	registry := plugins.NewRegistry()
	thanos, ok := plugins.Default().Plugin("thanos")
	if !ok {
		t.Fatal("builtin metrics plugin not assembled")
	}
	for _, declared := range []plugins.Plugin{thanos, synthetic.Plugin()} {
		if err := registry.Register(declared); err != nil {
			t.Fatal(err)
		}
	}
	enabled := []string{"thanos", "synthetic-plugin"}
	catalogs, err := attempt.BuildCatalogs(registry, enabled)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Inspections.UsePluginRegistry(registry, enabled); err != nil {
		t.Fatal(err)
	}
	service.Inspections.Attempts().Catalogs = catalogs
	service.LocalToolEntries = catalogs.Handlers
	caller := &syntheticCollectionCaller{}
	previous := newLocalToolCaller
	newLocalToolCaller = func(*RuntimeService, localConnection) plugins.PlatformCaller { return caller }
	t.Cleanup(func() { newLocalToolCaller = previous })
	_ = testfixture.SeedHTTPConnectionPair(t, db, "synthetic-local", synthetic.ConnectionKindValue, time.Now())
	if _, err := service.Inspections.CreatePlan(localInspectionAdminContext(t), 1, "synthetic-local-plan", inspection.PlanInput{
		PlanKey: "synthetic-local-plan", DisplayName: "Synthetic", Enabled: true,
		ConnectionName: "synthetic-local", PluginID: "synthetic-plugin", TemplateID: synthetic.TemplateID,
		Params: map[string]any{"expression": "echo"}, ScopeKind: "integration", Timezone: "UTC",
	}); err != nil {
		t.Fatal(err)
	}
	run, err := service.Inspections.CreatePlanRun(localInspectionAdminContext(t), 1, "synthetic-local-run", "synthetic-local-plan")
	if err != nil {
		t.Fatal(err)
	}
	service.runLocalExecutionPass(context.Background())
	if len(caller.requests) != 1 || caller.requests[0].Method != "GET" || caller.requests[0].Path != "/collect" || caller.requests[0].Query.Get("expression") != "echo" {
		t.Fatalf("declared synthetic collection was not dispatched: %+v", caller.requests)
	}
	var state, resultJSON string
	if err := db.QueryRow(`SELECT state FROM inspection_runs WHERE id=?`, run.RunID).Scan(&state); err != nil || state != "Completed" {
		t.Fatalf("synthetic run=%s err=%v", state, err)
	}
	if err := db.QueryRow(`SELECT e.result_json FROM evidence e JOIN inspection_check_results x ON e.id=x.evidence_id WHERE x.run_id=?`, run.RunID).Scan(&resultJSON); err != nil || resultJSON == "" {
		t.Fatalf("synthetic Evidence result=%q err=%v", resultJSON, err)
	}
	var frozen struct {
		Expression string `json:"expression"`
		Value      string `json:"value"`
		EvidenceAt string `json:"evidenceAt"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &frozen); err != nil || run.EvidenceAt == nil || frozen.EvidenceAt != *run.EvidenceAt || frozen.Expression != "echo" || frozen.Value != "ok" {
		t.Fatalf("synthetic result %q differs from plugin-owned frozen check: %+v err=%v", resultJSON, frozen, err)
	}
	var grantPurpose string
	if err := db.QueryRow(`SELECT g.purpose FROM attempt_connection_grants g JOIN execution_attempts a ON a.id=g.attempt_id WHERE a.scope_type='run_check' AND a.scope_id=?`, run.RunID).Scan(&grantPurpose); err != nil || grantPurpose != synthetic.CollectionGrantPurpose {
		t.Fatalf("synthetic grant=%q err=%v", grantPurpose, err)
	}
}
