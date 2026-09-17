package attempt

// The legacy fallback pins (ADR-0004 历史快照). The frozen NULL-catalog
// documents are FIXED BYTES: their integrity is pinned by SHA-256 digests,
// independent of the compiled implementations. A legitimate future tool
// change never edits these documents — the compatibility test below is
// where a drifted tool moves from "matches the installed executor" to an
// explicit drift-denial assertion, while the historical bytes stay exactly
// what the original dispatch rendered.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestLegacyCatalogDocumentsAreByteFrozen(t *testing.T) {
	for _, tc := range []struct {
		agentVersion string
		frozen       string
		wantDigest   string
	}{
		{PreviousAgentVersion, legacyInitialAnalysisCatalogJSON, "487fa17d8eb82d0363af6d2e131bc4b49cc43fde0e3924d2035297f69614aabd"},
		{"investigation-v1", legacyInvestigationCatalogJSON, "b6cdda30dff77535364c4e736fa0fff211f5872e74da081c3e81274ef2f6110f"},
	} {
		sum := sha256.Sum256([]byte(tc.frozen))
		if got := hex.EncodeToString(sum[:]); got != tc.wantDigest {
			t.Fatalf("agent %s legacy document digest %s != pinned %s: the historical snapshot is fixed data and must never be rewritten", tc.agentVersion, got, tc.wantDigest)
		}
		var catalog FrozenCatalog
		if err := json.Unmarshal([]byte(tc.frozen), &catalog); err != nil {
			t.Fatalf("agent %s legacy document is not valid JSON: %v", tc.agentVersion, err)
		}
		if catalog.AgentVersion != tc.agentVersion {
			t.Fatalf("agent %s legacy document carries agentVersion %q", tc.agentVersion, catalog.AgentVersion)
		}
		if len(catalog.Tools) == 0 {
			t.Fatalf("agent %s legacy document carries no tools", tc.agentVersion)
		}
	}
}

// TestLegacyToolsCompatibilityIsExplicit walks every tool of the frozen
// fallback and asserts its CURRENT pairing with the assembled
// implementations. Today every historical tool matches; when an
// implementation legitimately changes, that tool's assertion flips to an
// explicit drift-denial here — the frozen documents above never move, and
// the drifted tool is denied at authorization instead of reinterpreted.
func TestLegacyToolsCompatibilityIsExplicit(t *testing.T) {
	catalogs := DefaultCatalogs()
	for _, tc := range []struct {
		agentVersion string
		frozen       string
	}{
		{PreviousAgentVersion, legacyInitialAnalysisCatalogJSON},
		{"investigation-v1", legacyInvestigationCatalogJSON},
	} {
		var catalog FrozenCatalog
		if err := json.Unmarshal([]byte(tc.frozen), &catalog); err != nil {
			t.Fatal(err)
		}
		for _, tool := range catalog.Tools {
			t.Run(tc.agentVersion+"/"+tool.Name, func(t *testing.T) {
				// Current pairing: the installed implementation still matches
				// the frozen contract byte-for-byte. A future change to this
				// tool flips this assertion to an explicit denial:
				//
				//   if _, err := catalogs.InstalledDefinition(tool); err == nil {
				//       t.Fatal("<tool> drifted; the frozen legacy attempt must be denied explicitly")
				//   }
				if _, err := catalogs.InstalledDefinition(tool); err != nil {
					t.Fatalf("frozen tool no longer matches the installed executor — record the explicit drift denial: %v", err)
				}
			})
		}
	}
}
