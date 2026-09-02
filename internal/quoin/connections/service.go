// Package connections owns the typed connection domain on the Quoin side
// (T07): connection/revision/generation persistence, enable/disable fences
// and the connection-probe attempt/grant closure. Thanos/Kubernetes probes
// are executed by the Plinth supervisor over the control stream; model
// provider probes arrive with T08.
package connections

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"strconv"
	"time"
)

const (
	TypeThanos        = "thanos"
	TypeKubernetes    = "kubernetes"
	TypeModelProvider = "model_provider"
)

var (
	ErrNotFound       = errors.New("connection not found")
	ErrRowVersion     = errors.New("expected row version does not match")
	ErrValidation     = errors.New("connection input does not satisfy the domain rules")
	ErrNameTaken      = errors.New("connection name already exists")
	ErrTypeMismatch   = errors.New("connection type mismatch")
	ErrActiveConflict = errors.New("connection state conflicts with the request")
	ErrSingleEnabled  = errors.New("another enabled connection of this type already exists")
)

// RowVersionError carries the authoritative connections.row_version.
type RowVersionError struct {
	Current int64
	ID      int64
}

func (e *RowVersionError) Error() string { return ErrRowVersion.Error() }
func (e *RowVersionError) Unwrap() error { return ErrRowVersion }

// Summary is the ConnectionSummary projection (non-secret, typed config).
// Summary round-trips through JSON for the command-replay projection, so
// every field carries a stable tag (replays must restore the original ids).
type Summary struct {
	ID                   int64           `json:"id"`
	Name                 string          `json:"name"`
	Type                 string          `json:"type"`
	Enabled              bool            `json:"enabled"`
	RevalidationRequired bool            `json:"revalidationRequired"`
	CurrentRevisionID    int64           `json:"currentRevisionId"`
	CurrentGenerationID  int64           `json:"currentCredentialGenerationId"`
	RowVersion           int64           `json:"rowVersion"`
	Config               json.RawMessage `json:"config"`
	CreatedAt            string          `json:"createdAt,omitempty"`
}

// CreateInput is the validated ConnectionInput payload.
type CreateInput struct {
	Name          string
	Type          string
	NonSecretJSON json.RawMessage // server-generated typed projection
	Secret        []byte          // raw typed secret JSON (memory only)
	SecretPresent bool
}

// RootKeyProvider decrypts credential generations on demand (supervisor
// grant path); production binds the bootstrap root key.
type RootKeyProvider func() ([]byte, error)

type Service struct {
	db      *sql.DB
	now     func() time.Time
	rootKey RootKeyProvider
}

func NewService(db *sql.DB, rootKey RootKeyProvider) *Service {
	return &Service{db: db, rootKey: rootKey, now: time.Now}
}

// validateConfig checks the typed non-secret projection against the frozen
// per-type shapes (DATA-CONN-005) and returns normalized JSON.
func validateConfig(connectionType string, config json.RawMessage) (json.RawMessage, error) {
	var document map[string]any
	if err := json.Unmarshal(config, &document); err != nil {
		return nil, fmt.Errorf("%w: config is not valid JSON", ErrValidation)
	}
	if kind, _ := document["type"].(string); kind != connectionType {
		return nil, fmt.Errorf("%w: config type discriminator must be %q", ErrValidation, connectionType)
	}
	switch connectionType {
	case TypeThanos:
		baseURL, _ := document["baseUrl"].(string)
		if baseURL == "" {
			return nil, fmt.Errorf("%w: baseUrl is required", ErrValidation)
		}
		for _, field := range []string{"tlsCaPem", "tlsServerName"} {
			if value, ok := document[field].(string); !ok && document[field] != nil {
				return nil, fmt.Errorf("%w: %s must be a string", ErrValidation, field)
			} else if ok && len(value) > 1<<20 {
				return nil, fmt.Errorf("%w: %s too large", ErrValidation, field)
			}
		}
		if _, ok := document["tlsSkipVerify"].(bool); !ok && document["tlsSkipVerify"] != nil {
			return nil, fmt.Errorf("%w: tlsSkipVerify must be a boolean", ErrValidation)
		}
	case TypeKubernetes:
		// contextName/defaultNamespace are optional bounded strings.
		for _, field := range []string{"contextName", "defaultNamespace"} {
			if value, ok := document[field].(string); ok && len(value) > 253 {
				return nil, fmt.Errorf("%w: %s too long", ErrValidation, field)
			} else if !ok && document[field] != nil {
				return nil, fmt.Errorf("%w: %s must be a string", ErrValidation, field)
			}
		}
	case TypeModelProvider:
		baseURL, _ := document["baseUrl"].(string)
		chat, _ := document["chatModelId"].(string)
		embed, _ := document["embeddingModelId"].(string)
		if baseURL == "" || chat == "" || embed == "" {
			return nil, fmt.Errorf("%w: baseUrl, chatModelId and embeddingModelId are required", ErrValidation)
		}
	default:
		return nil, fmt.Errorf("%w: unknown connection type", ErrValidation)
	}
	// Reject any secret-shaped fields in the non-secret projection.
	for _, forbidden := range []string{"password", "kubeconfig", "apiKey"} {
		if _, exists := document[forbidden]; exists {
			return nil, fmt.Errorf("%w: %s must not appear in the non-secret projection", ErrValidation, forbidden)
		}
	}
	normalized, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

// validateSecret enforces the per-type secret carrier shape.
func validateSecret(connectionType string, secret []byte) error {
	if len(secret) == 0 {
		return nil
	}
	var document map[string]any
	if err := json.Unmarshal(secret, &document); err != nil {
		return fmt.Errorf("%w: secret is not valid JSON", ErrValidation)
	}
	if kind, _ := document["type"].(string); kind != connectionType {
		return fmt.Errorf("%w: secret type discriminator mismatch", ErrValidation)
	}
	return nil
}

type typedSecretJSON struct {
	Type          string                   `json:"type"`
	Thanos        *thanosSecretJSON        `json:"thanos,omitempty"`
	Kubernetes    *kubernetesSecretJSON    `json:"kubernetes,omitempty"`
	ModelProvider *modelProviderSecretJSON `json:"model_provider,omitempty"`
}

type thanosSecretJSON struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type kubernetesSecretJSON struct {
	Kubeconfig string `json:"kubeconfig"`
}

type modelProviderSecretJSON struct {
	APIKey string `json:"apiKey"`
}

// Create persists a new connection with revision 1 and generation 1 in one
// IMMEDIATE transaction. The secret is sealed with the current root binding.
func (service *Service) Create(ctx context.Context, input CreateInput, createdBy int64, clientCommandID string) (Summary, error) {
	config, err := validateConfig(input.Type, input.NonSecretJSON)
	if err != nil {
		return Summary{}, err
	}
	if err := validateSecret(input.Type, input.Secret); err != nil {
		return Summary{}, err
	}
	// Secret-input idempotency: the digest covers only non-secret semantic
	// fields plus secret presence — a replayed command id returns the
	// original result without comparing secret values (DATA-COMMAND-002).
	digest := auth.DigestCommand("connection.create", map[string]any{
		"name": input.Name, "type": input.Type,
		"nonSecret": string(input.NonSecretJSON), "secretPresent": input.SecretPresent,
	})
	if record, found, lookupErr := auth.LookupCommand(ctx, service.db, createdBy, clientCommandID); lookupErr == nil && found && record.RequestDigest == digest {
		var replayed Summary
		if err := json.Unmarshal([]byte(record.ResultPayload), &replayed); err == nil {
			return replayed, nil
		}
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return Summary{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return Summary{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	now := service.now().UTC().Format(time.RFC3339Nano)
	var bindingRevision int
	if err := conn.QueryRowContext(ctx, `SELECT binding_revision FROM root_key_state WHERE id=1`).Scan(&bindingRevision); err != nil {
		return Summary{}, err
	}
	insert, err := conn.ExecContext(ctx, `INSERT INTO connections(name,type,enabled,revalidation_required,created_at) VALUES(?,?,0,0,?)`, input.Name, input.Type, now)
	if err != nil {
		if isUnique(err) {
			return Summary{}, ErrNameTaken
		}
		return Summary{}, err
	}
	connectionID, err := insert.LastInsertId()
	if err != nil {
		return Summary{}, err
	}
	revision, err := conn.ExecContext(ctx, `INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_by,created_at) VALUES(?,1,?,?,?)`, connectionID, string(config), createdBy, now)
	if err != nil {
		return Summary{}, err
	}
	revisionID, err := revision.LastInsertId()
	if err != nil {
		return Summary{}, err
	}
	generationID, err := service.insertGeneration(ctx, conn, connectionID, input.Type, bindingRevision, input.Secret, createdBy, now)
	if err != nil {
		return Summary{}, err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE connections SET current_revision_id=?,current_credential_generation_id=?,row_version=row_version+1 WHERE id=?`, revisionID, generationID, connectionID); err != nil {
		return Summary{}, err
	}
	committed = true
	summary, summaryErr := service.getOn(ctx, conn, input.Name)
	if summaryErr != nil {
		return Summary{}, summaryErr
	}
	projection, err := json.Marshal(summary)
	if err != nil {
		return Summary{}, err
	}
	if err := auth.RecordCommand(ctx, conn, createdBy, clientCommandID, "connection.create", digest, "committed", "connection", connectionID, string(projection)); err != nil {
		return Summary{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return Summary{}, err
	}
	conn.Close() // release the single pool connection before re-reading
	return service.Get(ctx, input.Name)
}

// insertGeneration seals and stores credential generation seq for the
// connection; returns the new row id.
func (service *Service) insertGeneration(ctx context.Context, conn *sql.Conn, connectionID int64, connectionType string, bindingRevision int, secret []byte, createdBy int64, now string) (int64, error) {
	var nextSeq int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation_seq),0)+1 FROM credential_generations WHERE connection_id=?`, connectionID).Scan(&nextSeq); err != nil {
		return 0, err
	}
	var envelope *envelopeWire
	if len(secret) > 0 {
		rootKey, err := service.rootKey()
		if err != nil {
			return 0, err
		}
		typed := typedSecretFromRaw(connectionType, secret)
		wire, sealErr := sealEnvelope(rootKey, connectionID, nextSeq, connectionType, bindingRevision, typed)
		if sealErr != nil {
			return 0, sealErr
		}
		envelope = wire
	} else {
		// Kubernetes requires a kubeconfig; model provider requires an API
		// key; thanos may run without a secret (no basic auth).
		if connectionType != TypeThanos {
			return 0, fmt.Errorf("%w: %s requires a secret", ErrValidation, connectionType)
		}
		// Empty thanos secret: seal an explicit empty carrier so the
		// generation remains decryptable and audit-consistent.
		rootKey, err := service.rootKey()
		if err != nil {
			return 0, err
		}
		wire, sealErr := sealEnvelope(rootKey, connectionID, nextSeq, connectionType, bindingRevision, &typedSecretJSON{Type: connectionType})
		if sealErr != nil {
			return 0, sealErr
		}
		envelope = wire
	}
	insert, err := conn.ExecContext(ctx,
		`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_by,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		connectionID, nextSeq, envelopeVersion, bindingRevision, envelope.Nonce, envelope.Ciphertext, createdBy, now)
	if err != nil {
		return 0, err
	}
	return insert.LastInsertId()
}

func typedSecretFromRaw(connectionType string, secret []byte) *typedSecretJSON {
	var carrier map[string]string
	_ = json.Unmarshal(secret, &carrier)
	payload := &typedSecretJSON{Type: connectionType}
	switch connectionType {
	case TypeThanos:
		payload.Thanos = &thanosSecretJSON{Username: carrier["username"], Password: carrier["password"]}
	case TypeKubernetes:
		payload.Kubernetes = &kubernetesSecretJSON{Kubeconfig: carrier["kubeconfig"]}
	case TypeModelProvider:
		payload.ModelProvider = &modelProviderSecretJSON{APIKey: carrier["apiKey"]}
	}
	return payload
}

// getOn reads the summary on an open transaction connection.
func (service *Service) getOn(ctx context.Context, conn *sql.Conn, name string) (Summary, error) {
	row := conn.QueryRowContext(ctx, `
		SELECT c.id,c.name,c.type,c.enabled,c.revalidation_required,
		       COALESCE(c.current_revision_id,0),COALESCE(c.current_credential_generation_id,0),c.row_version,c.created_at,
		       COALESCE((SELECT config_json FROM connection_revisions WHERE id=c.current_revision_id),'{}')
		FROM connections c WHERE c.name=?`, name)
	return scanSummary(row)
}

// Get returns the connection summary by stable name.
// DB exposes the pool for read-only dispatch lookups in the app layer.
func (service *Service) DB() *sql.DB { return service.db }

func (service *Service) Get(ctx context.Context, name string) (Summary, error) {
	row := service.db.QueryRowContext(ctx, `
		SELECT c.id,c.name,c.type,c.enabled,c.revalidation_required,
		       COALESCE(c.current_revision_id,0),COALESCE(c.current_credential_generation_id,0),c.row_version,c.created_at,
		       COALESCE((SELECT config_json FROM connection_revisions WHERE id=c.current_revision_id),'{}')
		FROM connections c WHERE c.name=?`, name)
	var summary Summary
	var enabled, revalidation int
	var config string
	if err := row.Scan(&summary.ID, &summary.Name, &summary.Type, &enabled, &revalidation, &summary.CurrentRevisionID, &summary.CurrentGenerationID, &summary.RowVersion, &summary.CreatedAt, &config); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Summary{}, ErrNotFound
		}
		return Summary{}, err
	}
	summary.Enabled = enabled == 1
	summary.RevalidationRequired = revalidation == 1
	summary.Config = json.RawMessage(config)
	return summary, nil
}

// scanSummary reads one summary row (config is TEXT in SQLite).
func scanSummary(row *sql.Row) (Summary, error) {
	var summary Summary
	var enabled, revalidation int
	var config string
	if err := row.Scan(&summary.ID, &summary.Name, &summary.Type, &enabled, &revalidation, &summary.CurrentRevisionID, &summary.CurrentGenerationID, &summary.RowVersion, &summary.CreatedAt, &config); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Summary{}, ErrNotFound
		}
		return Summary{}, err
	}
	summary.Enabled = enabled == 1
	summary.RevalidationRequired = revalidation == 1
	summary.Config = json.RawMessage(config)
	return summary, nil
}

// List returns one keyset page ordered by name.
func (service *Service) List(ctx context.Context, after string, limit int) ([]Summary, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT c.id,c.name,c.type,c.enabled,c.revalidation_required,
		       COALESCE(c.current_revision_id,0),COALESCE(c.current_credential_generation_id,0),c.row_version,c.created_at,
		       COALESCE((SELECT config_json FROM connection_revisions WHERE id=c.current_revision_id),'{}')
		FROM connections c WHERE c.name>? ORDER BY c.name LIMIT ?`, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	summaries := []Summary{}
	for rows.Next() {
		var summary Summary
		var enabled, revalidation int
		var config string
		if err := rows.Scan(&summary.ID, &summary.Name, &summary.Type, &enabled, &revalidation, &summary.CurrentRevisionID, &summary.CurrentGenerationID, &summary.RowVersion, &summary.CreatedAt, &config); err != nil {
			return nil, false, err
		}
		summary.Enabled = enabled == 1
		summary.RevalidationRequired = revalidation == 1
		summary.Config = json.RawMessage(config)
		summaries = append(summaries, summary)
	}
	more := false
	if len(summaries) > limit {
		summaries = summaries[:limit]
		more = true
	}
	return summaries, more, rows.Err()
}

// Enable flips enabled=1 (clearing RevalidationRequired) under the row
// version fence and the single-enabled partial index for thanos/model
// provider (DATA-CONN-003/006).
func (service *Service) Enable(ctx context.Context, name string, expectedRowVersion int64, qualifiedProbeResultID int64, createdBy int64) (Summary, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return Summary{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return Summary{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var id int64
	var connectionType string
	var enabled, revalidation int
	var rowVersion int64
	if err := conn.QueryRowContext(ctx, `SELECT id,type,enabled,revalidation_required,row_version FROM connections WHERE name=?`, name).Scan(&id, &connectionType, &enabled, &revalidation, &rowVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Summary{}, ErrNotFound
		}
		return Summary{}, err
	}
	if rowVersion != expectedRowVersion {
		return Summary{}, &RowVersionError{Current: rowVersion, ID: id}
	}
	if enabled == 1 && revalidation == 0 {
		// Semantic no-op (HTTP-COMMAND-011).
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return Summary{}, err
		}
		committed = true
		conn.Close()
		return service.Get(ctx, name)
	}
	if connectionType == TypeModelProvider {
		if qualifiedProbeResultID == 0 {
			return Summary{}, fmt.Errorf("%w: model provider enable requires an explicit passed probe result", ErrValidation)
		}
		var probeType string
		var outcome string
		var probeRevisionID, probeGenerationID int64
		var currentRevisionID, currentGenerationID int64
		var revalidationProbe int
		if err := conn.QueryRowContext(ctx, `SELECT connection_type,outcome,connection_revision_id,credential_generation_id FROM connection_probe_results WHERE id=?`, qualifiedProbeResultID).Scan(&probeType, &outcome, &probeRevisionID, &probeGenerationID); err != nil {
			return Summary{}, fmt.Errorf("%w: unknown probe result", ErrValidation)
		}
		if err := conn.QueryRowContext(ctx, `SELECT current_revision_id,current_credential_generation_id,revalidation_required FROM connections WHERE id=?`, id).Scan(&currentRevisionID, &currentGenerationID, &revalidationProbe); err != nil {
			return Summary{}, err
		}
		if probeType != connectionType || outcome != "passed" || probeRevisionID != currentRevisionID || probeGenerationID != currentGenerationID || revalidationProbe != 0 {
			return Summary{}, fmt.Errorf("%w: probe result does not close onto the current pair", ErrActiveConflict)
		}
		// The explicit qualification event must close onto the row version
		// the enabling UPDATE produces (trigger checks
		// q.enabled_row_version = NEW.row_version AFTER the update): insert
		// it first against row_version+1, then advance the row in the same
		// transaction.
		if _, err := conn.ExecContext(ctx, `INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(?,?,?,?,?)`, id, rowVersion+1, qualifiedProbeResultID, createdBy, service.now().UTC().Format(time.RFC3339Nano)); err != nil {
			return Summary{}, err
		}
	}
	result, err := conn.ExecContext(ctx, `UPDATE connections SET enabled=1,revalidation_required=0,row_version=row_version+1 WHERE id=? AND row_version=?`, id, rowVersion)
	if err != nil {
		if isUnique(err) {
			return Summary{}, ErrSingleEnabled
		}
		return Summary{}, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return Summary{}, &RowVersionError{Current: rowVersion, ID: id}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return Summary{}, err
	}
	committed = true
	conn.Close()
	return service.Get(ctx, name)
}

// Disable blocks new dispatches; already accepted attempts finish. During a
// RootKeyRebind it is the explicit choice to retain the connection disabled.
func (service *Service) Disable(ctx context.Context, name string, expectedRowVersion int64) (Summary, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return Summary{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return Summary{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	result, err := conn.ExecContext(ctx, `UPDATE connections SET enabled=0,row_version=row_version+1 WHERE name=? AND row_version=?`, name, expectedRowVersion)
	if err != nil {
		return Summary{}, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		var current int64
		err := conn.QueryRowContext(ctx, `SELECT row_version FROM connections WHERE name=?`, name).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return Summary{}, ErrNotFound
		}
		return Summary{}, &RowVersionError{Current: current}
	}
	if err := markRootKeyRebindConnectionSafe(ctx, conn, name, "disabled"); err != nil {
		return Summary{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return Summary{}, err
	}
	committed = true
	if err := conn.Close(); err != nil {
		return Summary{}, err
	}
	return service.Get(ctx, name)
}

func markRootKeyRebindConnectionSafe(ctx context.Context, conn *sql.Conn, name, detailCode string) error {
	var active int
	var reason string
	var revision int64
	if err := conn.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &revision); err != nil {
		return err
	}
	if active == 0 || reason != "RootKeyRebind" {
		return nil
	}
	result, err := conn.ExecContext(ctx, `UPDATE maintenance_items SET safe_state='Safe',detail_code=?,updated_at=? WHERE maintenance_revision=? AND kind='Connection' AND object_key=? AND safe_state='Blocking'`, detailCode, time.Now().UTC().Format(time.RFC3339Nano), revision, name)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return fmt.Errorf("root key rebind checklist item missing or already completed for connection %q", name)
	}
	return nil
}

// OpenGeneration decrypts the current credential generation for the
// supervisor grant path (RUNTIME-GRANT-002).
func (service *Service) OpenGeneration(ctx context.Context, generationID int64) (*typedSecretJSON, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return service.openGenerationOn(ctx, conn, generationID)
}

func (service *Service) openGenerationOn(ctx context.Context, conn *sql.Conn, generationID int64) (*typedSecretJSON, error) {
	rootKey, err := service.rootKey()
	if err != nil {
		return nil, err
	}
	var connectionID, generationSeq, bindingRevision, envelopeVersion int
	var nonce, ciphertext []byte
	var connectionType string
	err = conn.QueryRowContext(ctx, `
		SELECT c.id,cg.generation_seq,cg.key_binding_revision,cg.envelope_version,cg.nonce,cg.ciphertext,c.type
		FROM credential_generations cg JOIN connections c ON c.id=cg.connection_id
		WHERE cg.id=?`, generationID).Scan(&connectionID, &generationSeq, &bindingRevision, &envelopeVersion, &nonce, &ciphertext, &connectionType)
	if err != nil {
		return nil, err
	}
	var currentBinding int
	if err := conn.QueryRowContext(ctx, `SELECT binding_revision FROM root_key_state WHERE id=1`).Scan(&currentBinding); err != nil {
		return nil, err
	}
	if bindingRevision != currentBinding {
		return nil, fmt.Errorf("credential binding revision %d does not match root binding %d", bindingRevision, currentBinding)
	}
	return openEnvelope(rootKey, int64(connectionID), int64(generationSeq), connectionType, bindingRevision, nonce, ciphertext)
}

func isUnique(err error) bool {
	return err != nil && contains(err.Error(), "UNIQUE constraint failed")
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func locator(id int64) string { return strconv.FormatInt(id, 10) }

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// Rotate creates the next revision and credential generation in one
// transaction and atomically switches the current pair (T09,
// HTTP-COMMAND-013): the old secret stops being handed out immediately;
// enabled model providers are disabled first (v1: disable-then-switch) and
// every rotated connection requires a fresh passed probe before enabling
// again (revalidation_required).
func (service *Service) Rotate(ctx context.Context, name string, expectedRowVersion int64, input CreateInput, createdBy int64, clientCommandID string) (Summary, error) {
	config, err := validateConfig(input.Type, input.NonSecretJSON)
	if err != nil {
		return Summary{}, err
	}
	if err := validateSecret(input.Type, input.Secret); err != nil {
		return Summary{}, err
	}
	digest := auth.DigestCommand("connection.rotate", map[string]any{
		"name": name, "type": input.Type,
		"nonSecret": string(input.NonSecretJSON), "secretPresent": input.SecretPresent,
	})
	if record, found, lookupErr := auth.LookupCommand(ctx, service.db, createdBy, clientCommandID); lookupErr == nil && found && record.RequestDigest == digest {
		var replayed Summary
		if err := json.Unmarshal([]byte(record.ResultPayload), &replayed); err == nil {
			return replayed, nil
		}
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return Summary{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return Summary{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var id int64
	var connectionType string
	var enabled, revalidation int
	var rowVersion int64
	if err := conn.QueryRowContext(ctx, `SELECT id,type,enabled,revalidation_required,row_version FROM connections WHERE name=?`, name).Scan(&id, &connectionType, &enabled, &revalidation, &rowVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Summary{}, ErrNotFound
		}
		return Summary{}, err
	}
	if rowVersion != expectedRowVersion {
		return Summary{}, &RowVersionError{ID: id, Current: rowVersion}
	}
	if connectionType != input.Type {
		return Summary{}, ErrTypeMismatch
	}
	now := service.now().UTC().Format(time.RFC3339Nano)
	var bindingRevision int
	if err := conn.QueryRowContext(ctx, `SELECT binding_revision FROM root_key_state WHERE id=1`).Scan(&bindingRevision); err != nil {
		return Summary{}, err
	}
	var nextRevisionSeq int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision_seq),0)+1 FROM connection_revisions WHERE connection_id=?`, id).Scan(&nextRevisionSeq); err != nil {
		return Summary{}, err
	}
	revision, err := conn.ExecContext(ctx, `INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_by,created_at) VALUES(?,?,?,?,?)`, id, nextRevisionSeq, string(config), createdBy, now)
	if err != nil {
		return Summary{}, err
	}
	revisionID, err := revision.LastInsertId()
	if err != nil {
		return Summary{}, err
	}
	generationID, err := service.insertGeneration(ctx, conn, id, input.Type, bindingRevision, input.Secret, createdBy, now)
	if err != nil {
		return Summary{}, err
	}
	// v1 semantics: an enabled model provider is disabled by the rotation;
	// every rotation marks the pair as requiring a fresh passed probe.
	nextEnabled := 0
	if enabled == 1 && connectionType != TypeModelProvider {
		nextEnabled = 1
	}
	if _, err := conn.ExecContext(ctx, `UPDATE connections SET current_revision_id=?,current_credential_generation_id=?,enabled=?,revalidation_required=1,row_version=row_version+1 WHERE id=?`, revisionID, generationID, nextEnabled, id); err != nil {
		return Summary{}, err
	}
	// Re-entry under the current root binding closes only this frozen
	// RootKeyRebind checklist item. The connection remains revalidation-required
	// until the later normal-mode verification/enable path succeeds.
	if err := markRootKeyRebindConnectionSafe(ctx, conn, name, "reentered_with_current_root_key"); err != nil {
		return Summary{}, err
	}
	summary, summaryErr := service.getOn(ctx, conn, name)
	if summaryErr != nil {
		return Summary{}, summaryErr
	}
	projection, err := json.Marshal(summary)
	if err != nil {
		return Summary{}, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,client_command_id,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('user',?,'connection.rotate',?,'success','connection',?,?)`, createdBy, clientCommandID, id, now); err != nil {
		return Summary{}, err
	}
	if err := auth.RecordCommand(ctx, conn, createdBy, clientCommandID, "connection.rotate", digest, "committed", "connection", id, string(projection)); err != nil {
		return Summary{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return Summary{}, err
	}
	committed = true
	conn.Close()
	return service.Get(ctx, name)
}
