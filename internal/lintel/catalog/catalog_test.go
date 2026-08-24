package catalog

import (
	"encoding/json"
	"testing"
)

func TestCatalogIsStableAndClosed(t *testing.T) {
	first, second := Digest(), Digest()
	if first != second || len(first) != 64 {
		t.Fatalf("digest is not stable SHA-256: %q / %q", first, second)
	}
	var document struct {
		Journeys map[string]struct {
			Purpose       string         `json:"purpose"`
			Version       int            `json:"version"`
			EvidenceKinds []string       `json:"evidence_kinds"`
			ParamsSchema  map[string]any `json:"params_schema"`
		} `json:"journeys"`
	}
	if err := json.Unmarshal(Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	probe, ok := document.Journeys["authentication.url-prefix.v1"]
	if !ok || probe.Purpose != "authentication_probe" || probe.Version != 1 || len(probe.EvidenceKinds) != 0 {
		t.Fatalf("invalid authentication probe entry: %#v", probe)
	}
	if probe.ParamsSchema["additionalProperties"] != false {
		t.Fatalf("probe params must be closed: %#v", probe.ParamsSchema)
	}
}
