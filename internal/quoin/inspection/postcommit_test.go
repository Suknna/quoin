package inspection

// Post-commit fact emission coverage for the inspection authority
// transactions (ADR-0014, issue #110): the due window, a committed check
// result (success and gap) and the sealed report each persist one bounded
// fact inside their committing transaction — rows exist immediately after
// the command returns, before any dispatcher pass.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/pluginevents"
)

func TestInspectionFactsEmittedInAuthorityTransactions(t *testing.T) {
	h := newTestHarness(t)
	subscriber := &inspectionFactRecorder{}
	registry := plugins.NewRegistry()
	if err := registry.Register(plugins.Plugin{
		ID: "hooky", Version: "v1-test",
		PostCommitSubscriptions: []plugins.PostCommitSubscription{
			{EventType: plugins.FactInspectionDailyWindowDue},
			{EventType: plugins.FactInspectionCheckEvidenceCommitted},
			{EventType: plugins.FactInspectionReportSealed},
		},
		PostCommitHandler: subscriber,
	}); err != nil {
		t.Fatal(err)
	}
	h.service.SetPostCommitPublisher(pluginevents.NewPublisher(registry, []string{"hooky"}))

	// A real collection success commits its check fact (with an evidence
	// reference); an explicit gap commits its fact without one.
	h.seedPlan(t, "plan-ok")
	h.seedPlan(t, "plan-gap")
	successRun, err := h.service.CreatePlanRun(commandContext(t), h.principal, "facts-run-ok", "plan-ok")
	if err != nil {
		t.Fatal(err)
	}
	successAttempt := h.promqlAttemptID(t, successRun.RunID)
	h.dispatchPromQL(t, successAttempt)
	if err := h.service.CommitPluginProposal(context.Background(), successAttempt, "plinth-boot", 1, pluginSuccessProposal(t, h, successAttempt, successRun.RunID, "success")); err != nil {
		t.Fatal(err)
	}
	gapRun, err := h.service.CreatePlanRun(commandContext(t), h.principal, "facts-run-gap", "plan-gap")
	if err != nil {
		t.Fatal(err)
	}
	gapAttempt := h.promqlAttemptID(t, gapRun.RunID)
	h.dispatchPromQL(t, gapAttempt)
	if err := h.service.CommitPluginProposal(context.Background(), gapAttempt, "plinth-boot", 1, pluginSuccessProposal(t, h, gapAttempt, gapRun.RunID, "gap")); err != nil {
		t.Fatal(err)
	}
	okRefs := h.eventRow(t, plugins.FactInspectionCheckEvidenceCommitted, 2, 0)
	for _, name := range []string{"runId", "checkResultId", "attemptId", "evidenceId"} {
		if !strings.Contains(okRefs, name) {
			t.Fatalf("success check fact misses ref %q: %s", name, okRefs)
		}
	}
	if !strings.Contains(okRefs, `"status":"ok"`) {
		t.Fatalf("success check fact labels: %s", okRefs)
	}
	gapRefs := h.eventRow(t, plugins.FactInspectionCheckEvidenceCommitted, 2, 1)
	if strings.Contains(gapRefs, "evidenceId") {
		t.Fatalf("gap check fact must not fabricate an evidence reference: %s", gapRefs)
	}
	if !strings.Contains(gapRefs, `"status":"gap"`) {
		t.Fatalf("gap check fact labels: %s", gapRefs)
	}

	// The trigger boundary commits the due fact with the frozen window.
	h.seedDailyConfig(t, dailyTestConfigInput("facts-daily", "plan-ok"))
	boundary := time.Date(2026, time.September, 28, 6, 0, 0, 0, time.UTC)
	h.pinDailyNow(t, boundary)
	if err := h.service.CreateScheduledDailyReport(context.Background(), DailyReportConfig{ConfigKey: "facts-daily", Timezone: "UTC"}, boundary); err != nil {
		t.Fatal(err)
	}
	dueRefs := h.eventRow(t, plugins.FactInspectionDailyWindowDue, 1, 0)
	for _, fragment := range []string{"configId", "reportId", "2026-09-27", "windowStart"} {
		if !strings.Contains(dueRefs, fragment) {
			t.Fatalf("due fact misses %q: %s", fragment, dueRefs)
		}
	}

	// Past the cutoff the seal commits its fact with the immutable version.
	h.pinDailyNow(t, boundary.Add(2*time.Hour))
	if err := h.service.SealDueDailyReports(context.Background(), boundary.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	sealedRefs := h.eventRow(t, plugins.FactInspectionReportSealed, 1, 0)
	for _, fragment := range []string{"reportId", "versionId", "facts-daily"} {
		if !strings.Contains(sealedRefs, fragment) {
			t.Fatalf("sealed fact misses %q: %s", fragment, sealedRefs)
		}
	}

	// No subscriber is invoked by the domain commands themselves: delivery is
	// strictly the dispatcher's post-commit work.
	if subscriber.calls != 0 {
		t.Fatalf("subscriber ran without a dispatcher pass: %d", subscriber.calls)
	}
}

// eventRow returns the refs_json of one event of the given type (ordered by
// commit id, 0-based ordinal) and fails unless exactly want rows exist.
func (h *testHarness) eventRow(t *testing.T, eventType string, want, ordinal int) string {
	t.Helper()
	var rows int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM plugin_events WHERE event_type=?`, eventType).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != want {
		t.Fatalf("%s event rows=%d, want %d", eventType, rows, want)
	}
	var refsJSON string
	if err := h.db.QueryRow(`SELECT refs_json FROM plugin_events WHERE event_type=? ORDER BY id LIMIT 1 OFFSET ?`, eventType, ordinal).Scan(&refsJSON); err != nil {
		t.Fatal(err)
	}
	return refsJSON
}

// inspectionFactRecorder is the fake subscriber of this package's tests.
type inspectionFactRecorder struct {
	calls int
}

func (recorder *inspectionFactRecorder) HandlePostCommitFact(context.Context, plugins.PostCommitFact) error {
	recorder.calls++
	return nil
}
