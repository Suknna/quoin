package journey

import (
	"fmt"

	"github.com/Suknna/quoin/internal/lintel/catalog"
)

// StatusMarkerID is the stable Journey ID of the status-marker Playwright
// Journey. The executable source is playwright-runner.mjs, not an interpreted
// action document.
const StatusMarkerID = "page.status-marker.v1"

// StatusMarkerSourceDigest is SHA-256(playwright-runner.mjs). The catalog
// generator/gate computes the same value directly from that sole source file.
const StatusMarkerSourceDigest = "c4efc3f95352c04bffb7f5c2b6b56cade322852f36606e4e1816f9e41d1ecd4f"

// ValidateExecutableJourney closes a frozen catalog binding against the single
// versioned Playwright implementation shipped by this Lintel build.
func ValidateExecutableJourney(journeyID string, version int64) error {
	if journeyID != StatusMarkerID || version != 2 {
		return fmt.Errorf("journey %q version %d has no executable Playwright implementation", journeyID, version)
	}
	document, err := catalog.Document()
	if err != nil {
		return err
	}
	journeys, _ := document["journeys"].(map[string]any)
	entry, ok := journeys[journeyID].(map[string]any)
	if !ok {
		return fmt.Errorf("journey %q is not in the embedded catalog", journeyID)
	}
	declared, _ := entry["steps_digest"].(string)
	if declared != StatusMarkerSourceDigest {
		return fmt.Errorf("journey %q catalog digest does not match its Playwright source", journeyID)
	}
	return nil
}
