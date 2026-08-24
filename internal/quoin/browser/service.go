// Package browser owns the durable Browser Identity and Browser Operation
// authority.  Runtime process state remains in Lintel; this package only
// commits the immutable configuration, lifecycle fences and their audit
// projections defined by the frozen SQLite schema.
package browser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"time"

	"github.com/Suknna/quoin/internal/lintel/catalog"
	quoinconfig "github.com/Suknna/quoin/internal/quoin/config"
)

var (
	ErrNotFound       = errors.New("browser identity or operation not found")
	ErrConflict       = errors.New("browser lifecycle conflict")
	ErrRowVersion     = errors.New("expected row version does not match")
	ErrInvalid        = errors.New("invalid browser identity input")
	ErrRuntimeOffline = errors.New("lintel runtime unavailable")
	ErrSessionRevoked = errors.New("manual-login session is no longer valid")
)

type RowVersionError struct{ Current int64 }

func (e *RowVersionError) Error() string { return ErrRowVersion.Error() }
func (e *RowVersionError) Unwrap() error { return ErrRowVersion }

type ProbeConfig struct {
	JourneyID string          `json:"journeyId"`
	Version   int64           `json:"journeyVersion"`
	Params    json.RawMessage `json:"params"`
}

type ConfigureInput struct {
	SystemKey          string
	Name               string
	StartURL           string
	Probe              ProbeConfig
	ExpectedRowVersion *int64
	ClientCommandID    string
}

type Revision struct {
	ID             int64       `json:"id,string"`
	Number         int64       `json:"revision"`
	Name           string      `json:"name"`
	StartURL       string      `json:"startUrl"`
	Probe          ProbeConfig `json:"authenticationProbe"`
	CatalogDigest  string      `json:"catalogDigest"`
	CatalogVersion string      `json:"catalogVersion"`
	CreatedAt      string      `json:"createdAt"`
}

type Profile struct {
	ID               int64  `json:"id,string"`
	Generation       int64  `json:"generation"`
	RevisionID       int64  `json:"identityRevisionId,string"`
	ChromiumRevision string `json:"chromiumRevision"`
	ManifestDigest   string `json:"profileManifestDigest"`
	ProbeJourneyID   string `json:"probeJourneyId"`
	ProbeVersion     int64  `json:"probeJourneyVersion"`
	CatalogDigest    string `json:"probeCatalogDigest"`
	CatalogVersion   string `json:"probeCatalogVersion"`
	PublishedAt      string `json:"publishedAt"`
}

type ProbeResult struct {
	Phase          string  `json:"phase"`
	Result         string  `json:"result"`
	JourneyID      string  `json:"journeyId"`
	CatalogDigest  string  `json:"catalogDigest"`
	CatalogVersion string  `json:"catalogVersion"`
	ObservedAt     string  `json:"observedAt"`
	JourneyVersion int64   `json:"journeyVersion"`
	ReasonCode     *string `json:"reasonCode,omitempty"`
}

type Operation struct {
	ID                    int64         `json:"id,string"`
	IdentityID            int64         `json:"identityId,string"`
	RevisionID            int64         `json:"identityRevisionId,string"`
	RowVersion            int64         `json:"rowVersion"`
	ProfileGenerationID   *int64        `json:"profileGenerationId,omitempty,string"`
	Kind                  string        `json:"kind"`
	State                 string        `json:"state"`
	RequestedAt           string        `json:"requestedAt"`
	CatalogDigest         string        `json:"-"`
	CatalogVersion        string        `json:"-"`
	ActorUserID           *int64        `json:"-"`
	ActorSessionID        *int64        `json:"actorSessionId,omitempty,string"`
	StartedAt             *string       `json:"startedAt,omitempty"`
	StartDispatchedAt     *string       `json:"startDispatchedAt,omitempty"`
	ReconnectDeadline     *string       `json:"reconnectDeadline,omitempty"`
	EndedAt               *string       `json:"endedAt,omitempty"`
	TerminalReason        *string       `json:"terminalReason,omitempty"`
	StopConfirmedAt       *string       `json:"stopConfirmedAt,omitempty"`
	StopConfirmationBasis *string       `json:"stopConfirmationBasis,omitempty"`
	CleanupStateHash      *string       `json:"cleanupStateHash,omitempty"`
	CanAttach             bool          `json:"canAttach"`
	CanPublish            bool          `json:"canPublish"`
	CanCancel             bool          `json:"canCancel"`
	StartedByUsername     string        `json:"startedByUsername,omitempty"`
	ProbeResults          []ProbeResult `json:"probeResults,omitempty"`
}

type Identity struct {
	ID               int64        `json:"id,string"`
	RowVersion       int64        `json:"rowVersion"`
	State            string       `json:"state"`
	Revision         Revision     `json:"currentRevision"`
	Profile          *Profile     `json:"currentProfile"`
	LastProbe        *ProbeResult `json:"lastProbe"`
	CurrentOperation *Operation   `json:"currentOperation"`
}

type Service struct {
	db      *sql.DB
	now     func() time.Time
	Catalog func() (document []byte, version, digest string, err error)
	// Dispatch is injected by app.RuntimeService after its live Lintel channel
	// is available. The database transaction always commits before dispatch.
	Dispatch func(context.Context, int64) error
}

func NewService(db *sql.DB) *Service {
	return &Service{
		db:  db,
		now: time.Now,
		Catalog: func() ([]byte, string, string, error) {
			return catalog.Bytes(), catalog.Version, catalog.Digest(), nil
		},
	}
}

// DB exposes the authority connection only to Quoin's colocated runtime dispatcher.
func (service *Service) DB() *sql.DB { return service.db }

func validateConfigure(input ConfigureInput) error {
	if input.SystemKey == "" || input.Name == "" || input.ClientCommandID == "" || input.Probe.JourneyID == "" || input.Probe.Version < 1 || len(input.Probe.Params) == 0 {
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
	if fieldErrors := quoinconfig.ValidateJourneyReferenceVersion(input.Probe.JourneyID, input.Probe.Version, "authentication_probe", object, "authenticationProbe"); len(fieldErrors) != 0 {
		return fmt.Errorf("%w: authentication probe reference", ErrInvalid)
	}
	return nil
}

// Configure commits an immutable revision. A configured identity starts in
// AuthenticationRequired until a manual login publishes a verified profile.
func (service *Service) Configure(ctx context.Context, actorID int64, input ConfigureInput) (Identity, *Operation, error) {
	if err := validateConfigure(input); err != nil {
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
	var systemID int64
	if err = conn.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, input.SystemKey).Scan(&systemID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Identity{}, nil, ErrNotFound
		}
		return Identity{}, nil, err
	}
	replayed, _, err := replayCommand(ctx, conn, actorID, input.ClientCommandID, "configure_browser_identity", commandDigest(input.SystemKey, input.Name, input.StartURL, input.Probe.JourneyID, input.Probe.Version, string(input.Probe.Params), input.ExpectedRowVersion))
	if err != nil {
		return Identity{}, nil, err
	}
	if replayed {
		identity, lookupErr := service.identityOn(ctx, conn, input.SystemKey)
		return identity, nil, lookupErr
	}
	var identityID, rowVersion, newRevisionID int64
	var currentProfile sql.NullInt64
	err = conn.QueryRowContext(ctx, `SELECT id,current_profile_generation_id,row_version FROM browser_identities WHERE business_system_id=?`, systemID).Scan(&identityID, &currentProfile, &rowVersion)
	now := service.now().UTC().Format(time.RFC3339Nano)
	if errors.Is(err, sql.ErrNoRows) {
		result, insertErr := conn.ExecContext(ctx, `INSERT INTO browser_identity_revisions (business_system_id,revision,name,start_url,probe_journey_id,probe_journey_version,probe_params_json,journey_catalog_digest,journey_catalog_version,created_by,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, systemID, 1, input.Name, input.StartURL, input.Probe.JourneyID, input.Probe.Version, string(input.Probe.Params), digest, version, actorID, now)
		if insertErr != nil {
			return Identity{}, nil, insertErr
		}
		newRevisionID, _ = result.LastInsertId()
		result, insertErr = conn.ExecContext(ctx, `INSERT INTO browser_identities (business_system_id,current_revision_id,current_profile_generation_id,state,created_at) VALUES (?,?,NULL,'AuthenticationRequired',?)`, systemID, newRevisionID, now)
		if insertErr != nil {
			return Identity{}, nil, insertErr
		}
		identityID, _ = result.LastInsertId()
	} else if err != nil {
		return Identity{}, nil, err
	} else {
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
		if err = conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM browser_identity_revisions WHERE business_system_id=?`, systemID).Scan(&revision); err != nil {
			return Identity{}, nil, err
		}
		result, insertErr := conn.ExecContext(ctx, `INSERT INTO browser_identity_revisions (business_system_id,revision,name,start_url,probe_journey_id,probe_journey_version,probe_params_json,journey_catalog_digest,journey_catalog_version,created_by,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, systemID, revision, input.Name, input.StartURL, input.Probe.JourneyID, input.Probe.Version, string(input.Probe.Params), digest, version, actorID, now)
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
	if err = recordCommand(ctx, conn, actorID, input.ClientCommandID, "configure_browser_identity", commandDigest(input.SystemKey, input.Name, input.StartURL, input.Probe.JourneyID, input.Probe.Version, string(input.Probe.Params), input.ExpectedRowVersion), "browser_identity", identityID, now); err != nil {
		return Identity{}, nil, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Identity{}, nil, err
	}
	committed = true
	// Test and production DB pools may intentionally have one connection; the
	// transaction connection must be returned before reading the committed view.
	if err := conn.Close(); err != nil {
		return Identity{}, nil, err
	}
	identity, err := service.identityOn(ctx, service.db, input.SystemKey)
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
	// A disconnected Lintel leaves the durable global-FIFO item queued; its
	// attach path drains it. A best-effort immediate dispatch is only a latency
	// optimization and cannot alter the committed authority.
	if service.Dispatch != nil {
		_ = service.Dispatch(ctx, probeOperationID)
	}
	return identity, &operation, nil
}

// StartManualLogin reserves the identity before asking Lintel to create a
// headed browser. This makes duplicate login starts structurally impossible.
func (service *Service) StartManualLogin(ctx context.Context, systemKey string, actorUserID, actorSessionID, expectedRowVersion int64, clientCommandID string) (Operation, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Operation{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if clientCommandID == "" || actorUserID < 1 || actorSessionID < 1 {
		return Operation{}, ErrInvalid
	}
	// Authenticate again inside the same write transaction that reserves the
	// identity. This makes a concurrently committed Session revocation win over
	// a request that passed the HTTP authentication middleware earlier.
	var sessionUserID, issuedRevision, currentRevision, enabled int64
	var revoked sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT s.user_id,s.auth_revision_at_issue,u.auth_revision,u.enabled,s.revoked_at FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.id=?`, actorSessionID).Scan(&sessionUserID, &issuedRevision, &currentRevision, &enabled, &revoked); err != nil {
		return Operation{}, ErrSessionRevoked
	}
	if sessionUserID != actorUserID || revoked.Valid || enabled != 1 || issuedRevision != currentRevision {
		return Operation{}, ErrSessionRevoked
	}
	replayed, resultID, err := replayCommand(ctx, conn, actorUserID, clientCommandID, "start_browser_manual_login", commandDigest(systemKey, expectedRowVersion))
	if err != nil {
		return Operation{}, err
	}
	if replayed {
		return service.operationOn(ctx, conn, resultID)
	}
	var identityID, revisionID, rowVersion int64
	var digest, version string
	err = conn.QueryRowContext(ctx, `SELECT i.id,i.current_revision_id,i.row_version,r.journey_catalog_digest,r.journey_catalog_version FROM browser_identities i JOIN business_systems s ON s.id=i.business_system_id JOIN browser_identity_revisions r ON r.id=i.current_revision_id WHERE s.key=?`, systemKey).Scan(&identityID, &revisionID, &rowVersion, &digest, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, err
	}
	if rowVersion != expectedRowVersion {
		return Operation{}, &RowVersionError{Current: rowVersion}
	}
	now := service.now().UTC().Format(time.RFC3339Nano)
	result, err := conn.ExecContext(ctx, `INSERT INTO browser_operations (identity_id,identity_revision_id,profile_generation_id,owner_attempt_id,kind,actor_user_id,actor_session_id,verification_manifest_item_id,clone_identity,state,journey_catalog_digest,journey_catalog_version,journey_id,journey_version,probe_phase,requested_at) VALUES (?,?,NULL,NULL,'manual_login',?,?,NULL,NULL,'Queued',?,?,NULL,NULL,NULL,?)`, identityID, revisionID, actorUserID, actorSessionID, digest, version, now)
	if err != nil {
		// The partial unique index is the concurrency authority. Map its
		// deterministic collision to the closed domain error rather than
		// leaking SQLite text through the HTTP surface.
		if strings.Contains(err.Error(), "browser_operations.identity_id") {
			return Operation{}, ErrConflict
		}
		return Operation{}, err
	}
	id, _ := result.LastInsertId()
	if err = recordCommand(ctx, conn, actorUserID, clientCommandID, "start_browser_manual_login", commandDigest(systemKey, expectedRowVersion), "browser_operation", id, now); err != nil {
		return Operation{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Operation{}, err
	}
	committed = true
	if err := conn.Close(); err != nil {
		return Operation{}, err
	}
	op, err := service.operationOn(ctx, service.db, id)
	if err != nil {
		return Operation{}, err
	}
	// A disconnected Lintel is not a failed user command: the durable FIFO
	// record is the authority and a later stream attach dispatches it. A send
	// failure likewise leaves it queued for the dispatcher to retry.
	if service.Dispatch == nil || service.Dispatch(ctx, id) != nil {
		return op, nil
	}
	return service.operationOn(ctx, service.db, id)
}

// ValidateTunnel confirms every durable fence before Quoin relays a single
// RFB byte. The Lintel control binding is part of the operation assignment.
func (service *Service) ValidateTunnel(ctx context.Context, operationID, identityID, actorUserID, actorSessionID int64, bootID string, epoch uint64) bool {
	var matched int
	err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_operations WHERE id=? AND identity_id=? AND kind='manual_login' AND state IN ('Starting','Running','AwaitingReconnect') AND actor_user_id=? AND actor_session_id=? AND lintel_boot_id=? AND lintel_connection_epoch<=?`, operationID, identityID, actorUserID, actorSessionID, bootID, epoch).Scan(&matched)
	return err == nil && matched == 1
}

// InventoryItem is Quoin's expected on-disk profile identity sent to Lintel at
// every boot before it may start Browser Operations.
type InventoryItem struct {
	IdentityID, ProfileGenerationID, Generation int64
	ChromiumRevision, ManifestDigest            string
}

func (service *Service) ExpectedInventory(ctx context.Context) ([]InventoryItem, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT g.identity_id,g.id,g.generation,g.chromium_revision,g.profile_manifest_digest FROM browser_profile_generations g JOIN browser_identities i ON i.current_profile_generation_id=g.id ORDER BY g.identity_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []InventoryItem
	for rows.Next() {
		var item InventoryItem
		if err := rows.Scan(&item.IdentityID, &item.ProfileGenerationID, &item.Generation, &item.ChromiumRevision, &item.ManifestDigest); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (service *Service) GetIdentity(ctx context.Context, systemKey string) (Identity, error) {
	return service.identityOn(ctx, service.db, systemKey)
}
func (service *Service) GetOperation(ctx context.Context, systemKey string, id int64) (Operation, error) {
	var matched int
	err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_operations o JOIN browser_identities i ON i.id=o.identity_id JOIN business_systems s ON s.id=i.business_system_id WHERE s.key=? AND o.id=?`, systemKey, id).Scan(&matched)
	if err != nil {
		return Operation{}, err
	}
	if matched != 1 {
		return Operation{}, ErrNotFound
	}
	return service.operationOn(ctx, service.db, id)
}

func (service *Service) Cancel(ctx context.Context, systemKey string, operationID, actorID, expectedVersion int64, clientCommandID string) (Operation, error) {
	if clientCommandID == "" {
		return Operation{}, ErrInvalid
	}
	digest := commandDigest(systemKey, operationID, expectedVersion)
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Operation{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	replayed, resultID, err := replayCommand(ctx, conn, actorID, clientCommandID, "cancel_browser_operation", digest)
	if err != nil {
		return Operation{}, err
	}
	if replayed {
		return service.operationOn(ctx, conn, resultID)
	}
	var matches int
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_operations o JOIN browser_identities i ON i.id=o.identity_id JOIN business_systems s ON s.id=i.business_system_id WHERE s.key=? AND o.id=?`, systemKey, operationID).Scan(&matches); err != nil {
		return Operation{}, err
	}
	if matches != 1 {
		return Operation{}, ErrNotFound
	}
	op, err := service.operationOn(ctx, conn, operationID)
	if err != nil {
		return Operation{}, err
	}
	if op.ActorUserID == nil || *op.ActorUserID != actorID {
		return Operation{}, ErrNotFound
	}
	if op.RowVersion != expectedVersion {
		return Operation{}, &RowVersionError{Current: op.RowVersion}
	}
	if op.State == "Succeeded" || op.State == "Failed" || op.State == "Cancelled" || op.State == "Interrupted" {
		return op, nil
	}
	now := service.now().UTC().Format(time.RFC3339Nano)
	if op.StartedAt == nil {
		// A zero started_at alone does not prove dispatch did not occur. The
		// persisted Start fence is the authority for choosing cleanup semantics.
		var startDispatched sql.NullString
		if err = conn.QueryRowContext(ctx, `SELECT start_dispatched_at FROM browser_operations WHERE id=?`, operationID).Scan(&startDispatched); err != nil {
			return Operation{}, err
		}
		if !startDispatched.Valid {
			_, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='Cancelled',ended_at=?,terminal_reason='cancelled',stop_confirmed_at=?,stop_confirmation_basis='not_dispatched',row_version=row_version+1 WHERE id=? AND row_version=?`, now, now, operationID, expectedVersion)
		} else {
			_, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='Cancelled',ended_at=?,terminal_reason='cancelled',row_version=row_version+1 WHERE id=? AND row_version=?`, now, operationID, expectedVersion)
		}
	} else {
		_, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='Cancelled',ended_at=?,terminal_reason='cancelled',row_version=row_version+1 WHERE id=? AND row_version=?`, now, operationID, expectedVersion)
	}
	if err != nil {
		return Operation{}, err
	}
	if err = recordCommand(ctx, conn, actorID, clientCommandID, "cancel_browser_operation", digest, "browser_operation", operationID, now); err != nil {
		return Operation{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Operation{}, err
	}
	committed = true
	if err := conn.Close(); err != nil {
		return Operation{}, err
	}
	return service.operationOn(ctx, service.db, operationID)
}

func commandDigest(values ...any) string {
	encoded, _ := json.Marshal(values)
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:])
}
func replayCommand(ctx context.Context, db sqlQueryer, actorID int64, commandID, commandType, digest string) (bool, int64, error) {
	var storedType, storedDigest string
	var resultID sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT command_type,request_digest,result_object_id FROM client_commands WHERE principal_type='user' AND principal_id=? AND client_command_id=?`, actorID, commandID).Scan(&storedType, &storedDigest, &resultID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if storedType != commandType || storedDigest != digest || !resultID.Valid {
		return false, 0, ErrConflict
	}
	return true, resultID.Int64, nil
}
func recordCommand(ctx context.Context, db *sql.Conn, actorID int64, commandID, commandType, digest, objectType string, objectID int64, now string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO client_commands(principal_type,principal_id,client_command_id,command_type,request_digest,outcome,result_object_type,result_object_id,created_at) VALUES('user',?,?,?,?, 'committed',?,?,?)`, actorID, commandID, commandType, digest, objectType, objectID, now)
	return err
}
func recordCommandDB(ctx context.Context, db *sql.DB, actorID int64, commandID, commandType, digest, objectType string, objectID int64, now string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO client_commands(principal_type,principal_id,client_command_id,command_type,request_digest,outcome,result_object_type,result_object_id,created_at) VALUES('user',?,?,?,?, 'committed',?,?,?)`, actorID, commandID, commandType, digest, objectType, objectID, now)
	return err
}

func (service *Service) identityOn(ctx context.Context, db sqlQueryer, systemKey string) (Identity, error) {
	var identity Identity
	var params string
	var profileID sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT i.id,i.state,i.row_version,r.id,r.revision,r.name,r.start_url,r.probe_journey_id,r.probe_journey_version,r.probe_params_json,r.journey_catalog_digest,r.journey_catalog_version,r.created_at,i.current_profile_generation_id FROM browser_identities i JOIN business_systems s ON s.id=i.business_system_id JOIN browser_identity_revisions r ON r.id=i.current_revision_id WHERE s.key=?`, systemKey).Scan(&identity.ID, &identity.State, &identity.RowVersion, &identity.Revision.ID, &identity.Revision.Number, &identity.Revision.Name, &identity.Revision.StartURL, &identity.Revision.Probe.JourneyID, &identity.Revision.Probe.Version, &params, &identity.Revision.CatalogDigest, &identity.Revision.CatalogVersion, &identity.Revision.CreatedAt, &profileID)
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

type sqlQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (service *Service) profileOn(ctx context.Context, db sqlQueryer, id int64) (Profile, error) {
	var p Profile
	err := db.QueryRowContext(ctx, `SELECT id,generation,identity_revision_id,chromium_revision,profile_manifest_digest,probe_journey_id,probe_journey_version,probe_catalog_digest,probe_catalog_version,published_at FROM browser_profile_generations WHERE id=?`, id).Scan(&p.ID, &p.Generation, &p.RevisionID, &p.ChromiumRevision, &p.ManifestDigest, &p.ProbeJourneyID, &p.ProbeVersion, &p.CatalogDigest, &p.CatalogVersion, &p.PublishedAt)
	if err != nil {
		return Profile{}, err
	}
	return p, nil
}
func (service *Service) operationOn(ctx context.Context, db sqlQueryer, id int64) (Operation, error) {
	var op Operation
	var profile, actor, session sql.NullInt64
	var dispatched, started, reconnect, ended, reason, stopped, basis, cleanup sql.NullString
	err := db.QueryRowContext(ctx, `SELECT o.id,o.identity_id,o.identity_revision_id,o.profile_generation_id,o.kind,o.state,o.journey_catalog_digest,o.journey_catalog_version,o.requested_at,o.actor_user_id,o.actor_session_id,o.row_version,o.start_dispatched_at,o.started_at,o.reconnect_deadline,o.ended_at,o.terminal_reason,o.stop_confirmed_at,o.stop_confirmation_basis,hex(o.cleanup_state_hash),COALESCE(u.username,'') FROM browser_operations o LEFT JOIN users u ON u.id=o.actor_user_id WHERE o.id=?`, id).Scan(&op.ID, &op.IdentityID, &op.RevisionID, &profile, &op.Kind, &op.State, &op.CatalogDigest, &op.CatalogVersion, &op.RequestedAt, &actor, &session, &op.RowVersion, &dispatched, &started, &reconnect, &ended, &reason, &stopped, &basis, &cleanup, &op.StartedByUsername)
	if err != nil {
		return Operation{}, err
	}
	if profile.Valid {
		v := profile.Int64
		op.ProfileGenerationID = &v
	}
	if actor.Valid {
		v := actor.Int64
		op.ActorUserID = &v
	}
	if session.Valid {
		v := session.Int64
		op.ActorSessionID = &v
	}
	for _, x := range []struct {
		s sql.NullString
		p **string
	}{{dispatched, &op.StartDispatchedAt}, {started, &op.StartedAt}, {reconnect, &op.ReconnectDeadline}, {ended, &op.EndedAt}, {reason, &op.TerminalReason}, {stopped, &op.StopConfirmedAt}, {basis, &op.StopConfirmationBasis}, {cleanup, &op.CleanupStateHash}} {
		if x.s.Valid {
			v := x.s.String
			*x.p = &v
		}
	}
	rows, err := queryProbes(ctx, db, id)
	if err != nil {
		return Operation{}, err
	}
	op.ProbeResults = rows
	op.CanAttach = op.Kind == "manual_login" && (op.State == "Running" || op.State == "AwaitingReconnect")
	op.CanPublish = op.Kind == "manual_login" && op.State == "Running"
	op.CanCancel = op.Kind == "manual_login" && (op.State == "Queued" || op.State == "WaitingForCapacity" || op.State == "Starting" || op.State == "Running" || op.State == "AwaitingReconnect")
	return op, nil
}

func latestProbeOn(ctx context.Context, db sqlQueryer, identityID int64) (*ProbeResult, error) {
	var operationID int64
	err := db.QueryRowContext(ctx, `SELECT o.id FROM browser_probe_results p JOIN browser_operations o ON o.id=p.operation_id WHERE o.identity_id=? ORDER BY p.observed_at DESC,p.id DESC LIMIT 1`, identityID).Scan(&operationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	probes, err := queryProbes(ctx, db, operationID)
	if err != nil || len(probes) == 0 {
		return nil, err
	}
	return &probes[len(probes)-1], nil
}

func queryProbes(ctx context.Context, db sqlQueryer, operationID int64) ([]ProbeResult, error) {
	// sqlQueryer intentionally remains minimal for read projections; concrete
	// DB/Conn implementations are the only callers that can enumerate probes.
	switch queryer := db.(type) {
	case *sql.DB:
		return readProbes(ctx, queryer, operationID)
	case *sql.Conn:
		return readProbes(ctx, queryer, operationID)
	default:
		return nil, nil
	}
}

type probeRowsQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readProbes(ctx context.Context, db probeRowsQueryer, operationID int64) ([]ProbeResult, error) {
	rows, err := db.QueryContext(ctx, `SELECT phase,result,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,reason_code,observed_at FROM browser_probe_results WHERE operation_id=? ORDER BY probe_seq`, operationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var probes []ProbeResult
	for rows.Next() {
		var probe ProbeResult
		if err := rows.Scan(&probe.Phase, &probe.Result, &probe.JourneyID, &probe.JourneyVersion, &probe.CatalogDigest, &probe.CatalogVersion, &probe.ReasonCode, &probe.ObservedAt); err != nil {
			return nil, err
		}
		probes = append(probes, probe)
	}
	return probes, rows.Err()
}
