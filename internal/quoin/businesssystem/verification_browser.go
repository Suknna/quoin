package businesssystem

// Config Verification browser-check execution (T23, CFG-VERIFYRUN-001/002,
// DATA-CONFIG-007, DATA-BROWSER-003/006): each browser check freezes one
// inspection_collection child Attempt (dispatched to Lintel) plus its journey
// Browser Operation. The identity's readiness decides the admission shape:
// a busy identity settles as a local identity_busy gap, an identity without
// any published profile settles as a deterministic authentication_required
// gap, and an identity with a profile executes the real versioned Journey.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	quoinconfig "github.com/Suknna/quoin/internal/quoin/config"
)

const journeyVerificationSchemaKind = "inspection_collection_v1"

type journeyCatalogRef struct {
	Digest  string `json:"digest"`
	Version string `json:"version"`
}

type journeyVerificationBinding struct {
	ID      string            `json:"id"`
	Version int64             `json:"version"`
	Params  json.RawMessage   `json:"params"`
	Catalog journeyCatalogRef `json:"catalog"`
}

type journeyVerificationIdentity struct {
	IdentityID          int64  `json:"identityId"`
	IdentityRevisionID  int64  `json:"identityRevisionId"`
	ProfileGenerationID int64  `json:"profileGenerationId"`
	ProfileGeneration   int64  `json:"profileGeneration"`
	StartURL            string `json:"startUrl"`
}

// journeyVerificationInput is the frozen inspection_collection_v1 shape of a
// browser check child. The operationId is non-null exactly when the child
// owns a journey operation (RUNTIME-TASK-011 binds the dispatch payload to
// the started operation).
type journeyVerificationInput struct {
	SchemaKind          string                      `json:"schemaKind"`
	AttemptID           int64                       `json:"attemptId"`
	OperationID         *int64                      `json:"operationId"`
	Identity            journeyVerificationIdentity `json:"identity"`
	Journey             journeyVerificationBinding  `json:"journey"`
	AuthenticationProbe journeyVerificationBinding  `json:"authenticationProbe"`
	PlanKey             string                      `json:"planKey"`
	CheckKey            string                      `json:"checkKey"`
}

// browserVerificationCheck projects one browser check with its frozen params.
// marshalJourneyVerificationInput produces the wire's canonical sorted-key JSON.
// The Browser Start payload is built from maps in browser.Service, so freezing
// the same map shape here makes its content digest a byte-for-byte binding —
// not merely a semantically equivalent projection.
func marshalJourneyVerificationInput(input journeyVerificationInput) ([]byte, error) {
	binding := func(value journeyVerificationBinding) map[string]any {
		return map[string]any{
			"id": value.ID, "version": value.Version, "params": json.RawMessage(value.Params),
			"catalog": map[string]any{"digest": value.Catalog.Digest, "version": value.Catalog.Version},
		}
	}
	return json.Marshal(map[string]any{
		"schemaKind": input.SchemaKind, "attemptId": input.AttemptID, "operationId": input.OperationID,
		"identity": map[string]any{
			"identityId": input.Identity.IdentityID, "identityRevisionId": input.Identity.IdentityRevisionID,
			"profileGenerationId": input.Identity.ProfileGenerationID, "profileGeneration": input.Identity.ProfileGeneration,
			"startUrl": input.Identity.StartURL,
		},
		"journey": binding(input.Journey), "authenticationProbe": binding(input.AuthenticationProbe),
		"planKey": input.PlanKey, "checkKey": input.CheckKey,
	})
}

type browserVerificationCheck struct {
	PlanKey   string
	CheckKey  string
	JourneyID string
	Params    string
}

type browserIdentitySnapshot struct {
	IdentityID          int64
	RevisionID          int64
	ProfileGenerationID sql.NullInt64
	Generation          sql.NullInt64
	State               string
	StartURL            string
	ProbeJourneyID      string
	ProbeVersion        int64
	ProbeParams         string
}

// createBrowserVerificationAttempts appends every browser check's child
// Attempt and journey operation to the Run creation transaction. Local gaps
// (busy or profile-less identity) settle inside the same transaction so the
// run can never hold the active fence without a converging path.
func createBrowserVerificationAttempts(ctx context.Context, conn *sql.Conn, runID, configVersionID, contractID, systemID int64, now string) (int, error) {
	document, catalogVersion, catalogDigest, err := quoinconfig.JourneyCatalog()
	if err != nil {
		return 0, err
	}
	journeys, _ := document["journeys"].(map[string]any)
	checks, err := browserVerificationChecks(ctx, conn, configVersionID)
	if err != nil {
		return 0, err
	}
	if len(checks) == 0 {
		return 0, nil
	}
	identity, err := loadBrowserIdentitySnapshot(ctx, conn, systemID)
	if err != nil {
		return 0, err
	}
	identityBusy := false
	if identity.ProfileGenerationID.Valid && identity.Generation.Valid {
		if err := conn.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM browser_operations WHERE identity_id=? AND (state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect') OR stop_confirmed_at IS NULL))`,
			identity.IdentityID).Scan(&identityBusy); err != nil {
			return 0, err
		}
	}
	needsConvergence := false
	count := 0
	for _, check := range checks {
		entry, ok := journeys[check.JourneyID].(map[string]any)
		if !ok {
			return 0, fmt.Errorf("browser check %s/%s references journey %q missing from the embedded catalog", check.PlanKey, check.CheckKey, check.JourneyID)
		}
		version, _ := entry["version"].(float64)
		if version < 1 {
			return 0, fmt.Errorf("journey %q has no integer version in the embedded catalog", check.JourneyID)
		}
		insert, err := conn.ExecContext(ctx, `
			INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,plan_key,check_key,state,quoin_release_version,created_at)
			VALUES('inspection_collection','config_verification_run',?,?,?,'Queued',?,?)`,
			runID, check.PlanKey, check.CheckKey, attempt.ReleaseVersion(), now)
		if err != nil {
			return 0, err
		}
		attemptID, err := insert.LastInsertId()
		if err != nil {
			return 0, err
		}
		params := json.RawMessage(check.Params)
		if len(params) == 0 || string(params) == "null" {
			params = json.RawMessage("{}")
		}
		probeParams := json.RawMessage(identity.ProbeParams)
		if len(probeParams) == 0 {
			probeParams = json.RawMessage("{}")
		}
		input := journeyVerificationInput{
			SchemaKind: journeyVerificationSchemaKind, AttemptID: attemptID,
			Identity: journeyVerificationIdentity{
				IdentityID:         identity.IdentityID,
				IdentityRevisionID: identity.RevisionID,
				StartURL:           identity.StartURL,
			},
			Journey: journeyVerificationBinding{
				ID: check.JourneyID, Version: int64(version), Params: params,
				Catalog: journeyCatalogRef{Digest: catalogDigest, Version: catalogVersion},
			},
			AuthenticationProbe: journeyVerificationBinding{
				ID: identity.ProbeJourneyID, Version: identity.ProbeVersion, Params: probeParams,
				Catalog: journeyCatalogRef{Digest: catalogDigest, Version: catalogVersion},
			},
			PlanKey: check.PlanKey, CheckKey: check.CheckKey,
		}
		if _, dispatched, err := admitLocalGapIfAny(ctx, conn, runID, attemptID, identity, identityBusy, input, now); err != nil {
			return 0, err
		} else if !dispatched {
			// Defer parent convergence until all local children are appended;
			// the parent projection is a single transaction-level calculation.
			needsConvergence = true
		}
		count++
	}
	if needsConvergence {
		if err := convergeVerificationRunOn(ctx, conn, runID); err != nil {
			return 0, err
		}
	}
	return count, nil
}
