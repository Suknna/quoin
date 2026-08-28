package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestCatalogPreservesFrozenEntries is the in-repo CFG-JOURNEY-005 build gate:
// a retained stable ID keeps its params_schema JCS bytes exactly, and every
// ordinary journey entry declares a closed non-empty evidence kind set that
// includes structured (CFG-JOURNEY-001).
func TestCatalogPreservesFrozenEntries(t *testing.T) {
	document := Bytes()
	var parsed struct {
		CatalogVersion string `json:"catalog_version"`
		Journeys       map[string]struct {
			EvidenceKinds []string        `json:"evidence_kinds"`
			ParamsSchema  json.RawMessage `json:"params_schema"`
			Purpose       string          `json:"purpose"`
			StepsDigest   string          `json:"steps_digest"`
			Version       int64           `json:"version"`
		} `json:"journeys"`
	}
	if err := json.Unmarshal(document, &parsed); err != nil {
		t.Fatalf("catalog is not JSON: %v", err)
	}
	if parsed.CatalogVersion != Version {
		t.Fatalf("catalog_version drifted from the build constant")
	}
	probe, ok := parsed.Journeys["authentication.url-prefix.v1"]
	if !ok {
		t.Fatalf("the frozen authentication probe entry must be retained")
	}
	if string(probe.ParamsSchema) != `{"additionalProperties":false,"properties":{"authenticatedUrlPrefix":{"format":"uri","type":"string"}},"required":["authenticatedUrlPrefix"],"type":"object"}` {
		t.Fatalf("retained journey params_schema bytes changed: %s", probe.ParamsSchema)
	}
	if probe.Purpose != "authentication_probe" || probe.Version != 1 || len(probe.EvidenceKinds) != 0 {
		t.Fatalf("authentication probe contract drifted: %#v", probe)
	}
	marker, ok := parsed.Journeys["page.status-marker.v1"]
	if !ok {
		t.Fatalf("the status-marker journey entry is missing")
	}
	if marker.Purpose != "journey" || marker.Version != 2 {
		t.Fatalf("status-marker purpose/version wrong: %#v", marker)
	}
	if len(marker.EvidenceKinds) != 1 || marker.EvidenceKinds[0] != "structured" {
		t.Fatalf("ordinary journey must declare exactly the structured evidence kind: %#v", marker.EvidenceKinds)
	}
	source, err := os.ReadFile(filepath.Join("..", "browser", "journey", "playwright-runner.mjs"))
	if err != nil {
		t.Fatalf("read the sole Playwright Journey source: %v", err)
	}
	sum := sha256.Sum256(source)
	if marker.StepsDigest != hex.EncodeToString(sum[:]) {
		t.Fatalf("journey catalog source digest drifted: catalog=%s source=%x", marker.StepsDigest, sum)
	}
}
