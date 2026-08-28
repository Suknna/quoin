// Browser identity and Journey-catalog binding projections for run_check
// browser children. These are read-only projections of the same frozen facts
// the Config Verification admission path uses; no second authority.
package inspection

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	quoinconfig "github.com/Suknna/quoin/internal/quoin/config"
)

type browserIdentity struct {
	IdentityID          int64
	RevisionID          int64
	ProfileGenerationID sql.NullInt64
	Generation          sql.NullInt64
	StartURL            string
	ProbeJourneyID      string
	ProbeVersion        int64
	ProbeParams         string
}

func loadBrowserIdentity(ctx context.Context, conn *sql.Conn, systemID int64) (browserIdentity, bool, error) {
	var identity browserIdentity
	err := conn.QueryRowContext(ctx, `
		SELECT i.id, r.id, i.current_profile_generation_id, g.generation, r.start_url,
		       r.probe_journey_id, r.probe_journey_version, COALESCE(r.probe_params_json,'{}')
		FROM browser_identities i
		JOIN browser_identity_revisions r ON r.id=i.current_revision_id
		LEFT JOIN browser_profile_generations g ON g.id=i.current_profile_generation_id
		WHERE i.business_system_id=?`, systemID).
		Scan(&identity.IdentityID, &identity.RevisionID, &identity.ProfileGenerationID, &identity.Generation,
			&identity.StartURL, &identity.ProbeJourneyID, &identity.ProbeVersion, &identity.ProbeParams)
	if errors.Is(err, sql.ErrNoRows) {
		// A system without any browser identity can still run its plan: every
		// browser check settles as an authentication_required gap.
		return identity, false, nil
	}
	if err != nil {
		return identity, false, fmt.Errorf("browser identity for system %d: %w", systemID, err)
	}
	return identity, true, nil
}

// resolveJourneyBinding reads the embedded Journey Catalog for the frozen
// journey implementation version (CFG-JOURNEY-002 provenance).
func resolveJourneyBinding(journeyID string) (digest, catalogVersion string, version int64, err error) {
	document, catalogVersion, digest, err := quoinconfig.JourneyCatalog()
	if err != nil {
		return "", "", 0, err
	}
	journeys, _ := document["journeys"].(map[string]any)
	entry, ok := journeys[journeyID].(map[string]any)
	if !ok {
		return "", "", 0, fmt.Errorf("browser check references journey %q missing from the embedded catalog", journeyID)
	}
	number, _ := entry["version"].(float64)
	if number < 1 {
		return "", "", 0, fmt.Errorf("journey %q has no integer version in the embedded catalog", journeyID)
	}
	return digest, catalogVersion, int64(number), nil
}
