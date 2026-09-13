package businesssystem

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestResourceRefreshResultPreservesIdentityMappingsAcrossUpdates verifies the
// refresh-result boundary rather than hand-seeding rows: each refresh creates,
// binds, accepts, and commits its real discovery Attempt. A failed collection
// is incomplete evidence, so it must not withdraw the last complete projection.
func TestResourceRefreshResultPreservesIdentityMappingsAcrossUpdates(t *testing.T) {
	h := newHarness(t)
	version := h.mustUpload(t, validSystemYAML, h.principal, "cmd-result-upload-0001")
	if _, err := h.systems.Publish(context.Background(), h.principal, "cmd-result-publish-0002", "payments", versionID(t, version), nil); err != nil {
		t.Fatal(err)
	}

	commit := func(commandID string, outcome string, series []map[string]string) {
		t.Helper()
		run, err := h.systems.StartResourceRefresh(context.Background(), h.principal, commandID, "payments", "manual", nil)
		if err != nil {
			t.Fatalf("start refresh: %v", err)
		}
		queued, err := h.systems.QueuedResourceRefreshAttempts(context.Background())
		if err != nil || len(queued) != 1 {
			t.Fatalf("queued children = %v, %v; want exactly one", queued, err)
		}
		attemptID := queued[0]
		attempts := h.systems.ResourceRefreshAttempts()
		if err := attempts.BindToStream(context.Background(), attemptID, "result-boot", 1, time.Minute, "test"); err != nil {
			t.Fatalf("bind child: %v", err)
		}
		if err := attempts.Accept(context.Background(), attemptID, "result-boot", 1); err != nil {
			t.Fatalf("accept child: %v", err)
		}

		proposal := map[string]any{
			"schemaKind":           "resource_discovery_result_v1",
			"attemptId":            attemptID,
			"resourceRefreshRunId": resourceRefreshRunID(t, run),
			"discoveryKey":         "web-pods",
			"outcome":              outcome,
			"observedAt":           time.Now().UTC().Format(time.RFC3339Nano),
			"warnings":             []string{},
			"errors":               []string{},
			"gapReason":            nil,
		}
		if outcome == "success" {
			observations := make([]map[string]any, 0, len(series))
			for _, labels := range series {
				observations = append(observations, map[string]any{"labels": labels, "value": "1", "timestamp": 1})
			}
			proposal["series"] = observations
		} else {
			proposal["series"] = []any{}
			proposal["errors"] = []string{"metrics endpoint unavailable"}
			proposal["gapReason"] = "query_failed"
		}
		raw, err := json.Marshal(proposal)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.systems.CommitResourceRefreshProposal(context.Background(), attemptID, "result-boot", 1, raw); err != nil {
			t.Fatalf("commit refresh result: %v", err)
		}
	}

	commit("cmd-result-start-0003", "success", []map[string]string{
		{"job": "web", "instance": "one", "pod": "one-v1"},
		{"job": "web", "instance": "two", "pod": "two-v1"},
	})
	// The first series takes the conflict-update UPSERT path while the second is
	// new. Both mappings must remain attached to their own observed resource.
	commit("cmd-result-start-0004", "success", []map[string]string{
		{"job": "web", "instance": "one", "pod": "one-v2"},
		{"job": "web", "instance": "three", "pod": "three-v1"},
	})

	current := true
	resources, _, err := h.systems.ListObservedResources(context.Background(), "payments", &current, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ObservedResourceSummary{}
	for _, resource := range resources {
		got[resource.IdentityLabels["instance"]] = resource
	}
	if len(got) != 2 {
		t.Fatalf("current resources = %#v; want instances one and three", resources)
	}
	for instance, pod := range map[string]string{"one": "one-v2", "three": "three-v1"} {
		resource, ok := got[instance]
		if !ok || resource.IdentityLabels["job"] != "web" {
			t.Fatalf("identity mapping for %q = %#v; want job=web and instance=%q", instance, resource.IdentityLabels, instance)
		}
		detail, err := h.systems.GetObservedResource(context.Background(), "payments", mustResourceID(t, resource.ID))
		if err != nil || detail.Labels["pod"] != pod {
			t.Fatalf("updated labels for %q = %#v, %v; want pod=%q", instance, detail.Labels, err, pod)
		}
	}

	commit("cmd-result-start-0005", "error", nil)
	resources, _, err = h.systems.ListObservedResources(context.Background(), "payments", &current, 0, 50)
	if err != nil || len(resources) != 2 {
		t.Fatalf("failed refresh must preserve the complete projection: %#v, %v", resources, err)
	}
	for _, resource := range resources {
		if resource.IdentityLabels["job"] != "web" || (resource.IdentityLabels["instance"] != "one" && resource.IdentityLabels["instance"] != "three") {
			t.Fatalf("failed refresh changed identity mapping: %#v", resource)
		}
	}
}

func mustResourceID(t *testing.T, id string) int64 {
	t.Helper()
	var parsed int64
	if _, err := fmt.Sscan(id, &parsed); err != nil {
		t.Fatalf("parse observed resource ID %q: %v", id, err)
	}
	return parsed
}
