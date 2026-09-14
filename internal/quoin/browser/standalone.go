package browser

// Standalone browser identities (ADR-0004: the browser plugin is an optional,
// business-view-independent integration). A standalone identity carries a
// NULL business_system_id and a stable user-facing identity_key; its
// authentication revision, manual login, tunnel and publish lifecycle reuse
// the same durable operation machinery as the historical business-bound
// identity — only the locator differs (identity_key instead of systemKey).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/config"
)

// standaloneKeyPattern is the closed user-facing key vocabulary. The key is
// permanent: operations, tunnels and history reference it, so it is never
// reused for another identity.
var standaloneKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,63}$`)

// StandaloneConfigureInput is one create-or-edit revision command for a
// standalone identity. An empty IdentityKey asks Quoin to generate a stable
// key from the name.
type StandaloneConfigureInput struct {
	IdentityKey        string
	Name               string
	StartURL           string
	Probe              ProbeConfig
	ExpectedRowVersion *int64
	ClientCommandID    string
}

func validateStandaloneConfigure(input StandaloneConfigureInput) error {
	if input.Name == "" || input.ClientCommandID == "" || input.Probe.JourneyID == "" || input.Probe.Version < 1 || len(input.Probe.Params) == 0 {
		return ErrInvalid
	}
	parsed, err := url.ParseRequestURI(input.StartURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%w: start URL", ErrInvalid)
	}
	var object map[string]any
	if json.Unmarshal(input.Probe.Params, &object) != nil {
		return fmt.Errorf("%w: authentication probe params", ErrInvalid)
	}
	if fieldErrors := config.ValidateJourneyReferenceVersion(input.Probe.JourneyID, input.Probe.Version, "authentication_probe", object, "authenticationProbe"); len(fieldErrors) != 0 {
		return fmt.Errorf("%w: authentication probe reference", ErrInvalid)
	}
	if input.IdentityKey != "" && !standaloneKeyPattern.MatchString(input.IdentityKey) {
		return fmt.Errorf("%w: identityKey", ErrInvalid)
	}
	return nil
}

// standaloneSlug derives a candidate stable key from the identity name.
func standaloneSlug(name string) string {
	slug := strings.ToLower(strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		default:
			return '-'
		}
	}, strings.TrimSpace(name)))
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return ""
	}
	if len(slug) > 64 {
		slug = slug[:64]
	}
	if !standaloneKeyPattern.MatchString(slug) {
		return ""
	}
	return slug
}

// ConfigureStandalone commits an immutable revision of one standalone
// identity. Creation starts in AuthenticationRequired; an edit bumps the
// revision and re-probes only when a published profile already exists —
// exactly the business-bound Configure semantics, located by identity_key.
func (service *Service) ConfigureStandalone(ctx context.Context, actorID int64, input StandaloneConfigureInput) (Identity, *Operation, error) {
	if err := validateStandaloneConfigure(input); err != nil {
		return Identity{}, nil, err
	}
	_, version, digest, err := service.Catalog()
	if err != nil {
		return Identity{}, nil, fmt.Errorf("read journey catalog: %w", err)
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return Identity{}, nil, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Identity{}, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	identityKey := input.IdentityKey
	if identityKey == "" {
		identityKey = standaloneSlug(input.Name)
		if identityKey == "" {
			return Identity{}, nil, fmt.Errorf("%w: name produces no usable identityKey", ErrInvalid)
		}
	}
	command := "configure_standalone_browser_identity"
	replayed, _, err := replayCommand(ctx, conn, actorID, input.ClientCommandID, command, commandDigest(identityKey, input.Name, input.StartURL, input.Probe.JourneyID, input.Probe.Version, string(input.Probe.Params), input.ExpectedRowVersion))
	if err != nil {
		return Identity{}, nil, err
	}
	if replayed {
		identity, lookupErr := service.standaloneIdentityOn(ctx, conn, identityKey)
		return identity, nil, lookupErr
	}
	var identityID, rowVersion, newRevisionID int64
	var currentProfile sql.NullInt64
	err = conn.QueryRowContext(ctx, `SELECT id,current_profile_generation_id,row_version FROM browser_identities WHERE identity_key=?`, identityKey).Scan(&identityID, &currentProfile, &rowVersion)
	now := service.now().UTC().Format(time.RFC3339Nano)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Creation: a generated key must stay unique; explicit keys collide
		// deterministically instead of being silently rewritten.
		result, insertErr := conn.ExecContext(ctx, `INSERT INTO browser_identity_revisions (business_system_id,revision,name,start_url,probe_journey_id,probe_journey_version,probe_params_json,journey_catalog_digest,journey_catalog_version,created_by,created_at) VALUES (NULL,1,?,?,?,?,?,?,?,?,?)`, input.Name, input.StartURL, input.Probe.JourneyID, input.Probe.Version, string(input.Probe.Params), digest, version, actorID, now)
		if insertErr != nil {
			return Identity{}, nil, insertErr
		}
		newRevisionID, _ = result.LastInsertId()
		result, insertErr = conn.ExecContext(ctx, `INSERT INTO browser_identities (business_system_id,identity_key,current_revision_id,current_profile_generation_id,state,created_at) VALUES (NULL,?,?,NULL,'AuthenticationRequired',?)`, identityKey, newRevisionID, now)
		if insertErr != nil {
			if input.IdentityKey == "" && strings.Contains(insertErr.Error(), "ux_browser_identities_identity_key") {
				// Generated-key collision: retry with a numeric suffix keeps
				// creation honest without rewriting an existing identity.
				return Identity{}, nil, fmt.Errorf("%w: identityKey generation collided; retry with an explicit key", ErrConflict)
			}
			return Identity{}, nil, insertErr
		}
		identityID, _ = result.LastInsertId()
	case err != nil:
		return Identity{}, nil, err
	default:
		// Edit revision: the same optimistic-concurrency and idle-identity
		// fences as the business-bound path.
		if input.ExpectedRowVersion == nil || *input.ExpectedRowVersion != rowVersion {
			return Identity{}, nil, &RowVersionError{Current: rowVersion}
		}
		var active int
		if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_operations WHERE identity_id=? AND (state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect') OR stop_confirmed_at IS NULL)`, identityID).Scan(&active); err != nil {
			return Identity{}, nil, err
		}
		if active != 0 {
			return Identity{}, nil, ErrConflict
		}
		var revision int64
		// Revisions of a standalone identity carry a NULL business_system_id, so
		// history is only reachable through the current-revision pointer; the
		// frozen trigger otherwise rejects an owner mismatch.
		if err = conn.QueryRowContext(ctx, `SELECT r.revision+1 FROM browser_identities i JOIN browser_identity_revisions r ON r.id=i.current_revision_id WHERE i.identity_key=?`, identityKey).Scan(&revision); err != nil {
			return Identity{}, nil, err
		}
		result, insertErr := conn.ExecContext(ctx, `INSERT INTO browser_identity_revisions (business_system_id,revision,name,start_url,probe_journey_id,probe_journey_version,probe_params_json,journey_catalog_digest,journey_catalog_version,created_by,created_at) VALUES (NULL,?,?,?,?,?,?,?,?,?,?)`, revision, input.Name, input.StartURL, input.Probe.JourneyID, input.Probe.Version, string(input.Probe.Params), digest, version, actorID, now)
		if insertErr != nil {
			return Identity{}, nil, insertErr
		}
		newRevisionID, _ = result.LastInsertId()
		if _, err = conn.ExecContext(ctx, `UPDATE browser_identities SET current_revision_id=?,row_version=row_version+1 WHERE id=? AND row_version=?`, newRevisionID, identityID, rowVersion); err != nil {
			return Identity{}, nil, err
		}
	}
	var probeOperationID int64
	if currentProfile.Valid {
		result, insertErr := conn.ExecContext(ctx, `INSERT INTO browser_operations (identity_id,identity_revision_id,profile_generation_id,owner_attempt_id,kind,actor_user_id,actor_session_id,verification_manifest_item_id,clone_identity,state,journey_catalog_digest,journey_catalog_version,journey_id,journey_version,probe_phase,requested_at) VALUES (?,?,?,NULL,'authentication_probe',NULL,NULL,NULL,NULL,'Queued',?,?,?,?,?,?,?)`, identityID, newRevisionID, currentProfile.Int64, digest, version, input.Probe.JourneyID, input.Probe.Version, "revision_change", now)
		if insertErr != nil {
			return Identity{}, nil, insertErr
		}
		probeOperationID, _ = result.LastInsertId()
	}
	if err = recordCommand(ctx, conn, actorID, input.ClientCommandID, command, commandDigest(identityKey, input.Name, input.StartURL, input.Probe.JourneyID, input.Probe.Version, string(input.Probe.Params), input.ExpectedRowVersion), "browser_identity", identityID, now); err != nil {
		return Identity{}, nil, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Identity{}, nil, err
	}
	committed = true
	if err := conn.Close(); err != nil {
		return Identity{}, nil, err
	}
	identity, err := service.standaloneIdentityOn(ctx, service.db, identityKey)
	if err != nil {
		return Identity{}, nil, err
	}
	if probeOperationID == 0 {
		return identity, nil, nil
	}
	operation, err := service.operationOn(ctx, service.db, probeOperationID)
	if err != nil {
		return Identity{}, nil, err
	}
	if service.Dispatch != nil {
		_ = service.Dispatch(ctx, probeOperationID)
	}
	return identity, &operation, nil
}

// ListStandaloneIdentities returns every plugin-owned browser identity in
// stable identity_key order.
func (service *Service) ListStandaloneIdentities(ctx context.Context) ([]Identity, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT identity_key FROM browser_identities WHERE identity_key IS NOT NULL ORDER BY identity_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The production pool has one connection; release the list before loading details.
	if err := rows.Close(); err != nil {
		return nil, err
	}
	items := make([]Identity, 0, len(keys))
	for _, key := range keys {
		identity, err := service.standaloneIdentityOn(ctx, service.db, key)
		if err != nil {
			return nil, err
		}
		items = append(items, identity)
	}
	return items, nil
}

func (service *Service) GetStandaloneIdentity(ctx context.Context, identityKey string) (Identity, error) {
	return service.standaloneIdentityOn(ctx, service.db, identityKey)
}

// standaloneIdentityOn projects one standalone identity by its stable key.
func (service *Service) standaloneIdentityOn(ctx context.Context, db sqlQueryer, identityKey string) (Identity, error) {
	var identity Identity
	var params string
	var profileID sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT i.id,i.identity_key,i.state,i.row_version,r.id,r.revision,r.name,r.start_url,r.probe_journey_id,r.probe_journey_version,r.probe_params_json,r.journey_catalog_digest,r.journey_catalog_version,r.created_at,i.current_profile_generation_id FROM browser_identities i JOIN browser_identity_revisions r ON r.id=i.current_revision_id WHERE i.identity_key=?`, identityKey).Scan(&identity.ID, &identity.IdentityKey, &identity.State, &identity.RowVersion, &identity.Revision.ID, &identity.Revision.Number, &identity.Revision.Name, &identity.Revision.StartURL, &identity.Revision.Probe.JourneyID, &identity.Revision.Probe.Version, &params, &identity.Revision.CatalogDigest, &identity.Revision.CatalogVersion, &identity.Revision.CreatedAt, &profileID)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrNotFound
	}
	if err != nil {
		return Identity{}, err
	}
	identity.Revision.Probe.Params = json.RawMessage(params)
	if profileID.Valid {
		profile, err := service.profileOn(ctx, db, profileID.Int64)
		if err != nil {
			return Identity{}, err
		}
		identity.Profile = &profile
	}
	var opID int64
	err = db.QueryRowContext(ctx, `SELECT id FROM browser_operations WHERE identity_id=? AND (state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect') OR stop_confirmed_at IS NULL) ORDER BY id DESC LIMIT 1`, identity.ID).Scan(&opID)
	if err == nil {
		op, e := service.operationOn(ctx, db, opID)
		if e != nil {
			return Identity{}, e
		}
		identity.CurrentOperation = &op
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Identity{}, err
	}
	identity.LastProbe, err = latestProbeOn(ctx, db, identity.ID)
	if err != nil {
		return Identity{}, err
	}
	return identity, nil
}
