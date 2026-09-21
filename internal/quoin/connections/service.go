// Package connections owns the typed connection domain on the Quoin side
// (T07): connection/revision/generation persistence, enable/disable fences
// and the connection-probe attempt/grant closure. Metrics probes
// are executed by the Plinth supervisor over the control stream; model
// provider probes arrive with T08.
//
// Every active mutation runs through the family's execution runner
// (runner.go) so the automatic audit — and for create/rotate the durable
// command ledger — commits in the same transaction as the domain change
// (ADR-0006). Queries run through the injected read-only reader seam.
package connections

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/connections/modelprovider"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

const (
	// TypePrometheus and TypeThanos deliberately remain distinct connection
	// identities even though both use the Prometheus HTTP query protocol.
	// Business declarations select one explicitly; no global metrics default
	// exists.
	TypePrometheus    = "prometheus"
	TypeThanos        = "thanos"
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

// PostEnableHook runs inside the enable transaction after the connection row
// advanced but before commit. The guarded execution.Executor keeps the hook
// from escaping or ending the runner-owned transaction; a hook error rolls
// the whole enable back, so enablement-coupled invariants stay atomic.
type PostEnableHook func(ctx context.Context, tx execution.Executor, name string) error

type Service struct {
	db *sql.DB
	// reader is the read seam for every query path; production injects the
	// real read-only pool (SetReader). It defaults to the write pool until
	// the deployment composition wires the dedicated reader.
	reader   audit.Reader
	now      func() time.Time
	rootKey  RootKeyProvider
	commands *commandRunner
	// postEnableInTx 在启用事务提交前运行（ADR-0004 默认基础计划等启用耦合
	// 副作用必须与启用原子提交）；由 app 层通过 SetPostEnableInTx 注册。
	postEnableInTx PostEnableHook
}

func NewService(db *sql.DB, rootKey RootKeyProvider) *Service {
	service := &Service{db: db, rootKey: rootKey, now: time.Now}
	service.commands = newCommandRunner(db, service.now)
	return service
}

// SetReader injects the read-only query capability (bootstrap.Database.Reader,
// opened in real SQLite read-only mode). Queries never mutate state, so they
// must not depend on the write pool. A nil reader keeps the current seam.
func (service *Service) SetReader(reader audit.Reader) error {
	if reader == nil {
		return errors.New("connections: read-only reader is required")
	}
	service.reader = reader
	return nil
}

// read serves pure read paths from the injected read-only pool. Unwired it
// returns the zero-value execution.Reader, which fails closed — the writer
// database is never a read fallback.
func (service *Service) read() audit.Reader {
	if service.reader != nil {
		return service.reader
	}
	return execution.Reader{}
}

// Reader serves the app layer's read-only dispatch lookups (the injected
// bootstrap read-only pool).
func (service *Service) Reader() audit.Reader { return service.read() }

// SetPostEnableInTx registers a hook invoked inside the enable transaction
// after the connection row advanced but before commit. A hook error rolls the
// whole enable back, so enablement-coupled invariants stay atomic.
func (service *Service) SetPostEnableInTx(hook PostEnableHook) {
	service.postEnableInTx = hook
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
	case TypePrometheus, TypeThanos:
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
		authType, authTypePresent := document["authType"].(string)
		username, usernamePresent := document["username"]
		// Pre-authType Thanos revisions may retain an optional username without
		// a password. Preserve their original no-auth behavior rather than
		// inferring Basic Auth from the username alone. Public requests declare
		// authType explicitly through the OpenAPI variant.
		if authType == "" {
			authType = "none"
		}
		if authType != "none" && authType != "basic" && authType != "bearer" {
			return nil, fmt.Errorf("%w: authType must be none, basic or bearer", ErrValidation)
		}
		if authType == "basic" {
			value, ok := username.(string)
			if !usernamePresent || !ok || value == "" {
				return nil, fmt.Errorf("%w: basic auth requires username", ErrValidation)
			}
		} else if authTypePresent && usernamePresent && username != "" {
			return nil, fmt.Errorf("%w: username only applies to basic auth", ErrValidation)
		}
	case TypeModelProvider:
		baseURL, _ := document["baseUrl"].(string)
		chat, _ := document["chatModelId"].(string)
		// Embeddings are an optional capability. Chat-only providers remain
		// suitable for analysis and investigation while knowledge workflows
		// stay unavailable until a separately qualified embedding model exists.
		if baseURL == "" || chat == "" {
			return nil, fmt.Errorf("%w: baseUrl and chatModelId are required", ErrValidation)
		}
		if embed, exists := document["embeddingModelId"]; exists {
			if value, ok := embed.(string); !ok || value == "" {
				return nil, fmt.Errorf("%w: embeddingModelId must be a non-empty string when configured", ErrValidation)
			}
		}
	default:
		return nil, fmt.Errorf("%w: unknown connection type", ErrValidation)
	}
	// Reject credential material in the revision projection. Auth mode and
	// basic username are safe to retain; the password, bearer token or API
	// key exists solely in the encrypted credential generation.
	for _, forbidden := range []string{"password", "bearerToken", "apiKey"} {
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
	Prometheus    *metricsSecretJSON       `json:"prometheus,omitempty"`
	Thanos        *metricsSecretJSON       `json:"thanos,omitempty"`
	ModelProvider *modelProviderSecretJSON `json:"model_provider,omitempty"`
}

// metricsSecretJSON supports the closed metrics auth modes: no carrier for
// "none", username/password for "basic", and a bearer token for "bearer".
// Authentication mode is non-secret revision metadata; these values are never
// included in API projections or command digests.
type metricsSecretJSON struct {
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	BearerToken string `json:"bearerToken,omitempty"`
}

// validateMetricsCredential prevents programmatic callers from persisting a
// credential mode that cannot produce the declared HTTP authentication. The
// public adapter applies the same rules before this service boundary.
func validateMetricsCredential(connectionType string, config, secret []byte) error {
	if connectionType != TypePrometheus && connectionType != TypeThanos {
		return nil
	}
	var projection struct {
		AuthType string `json:"authType"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(config, &projection); err != nil {
		return err
	}
	var carrier struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		BearerToken string `json:"bearerToken"`
	}
	if len(secret) > 0 {
		if err := json.Unmarshal(secret, &carrier); err != nil {
			return fmt.Errorf("%w: metrics credential is not valid JSON", ErrValidation)
		}
	}
	// An omitted authType is the historical Thanos form: do not reinterpret
	// it or reject its secret carrier. New HTTP input is explicit and reaches
	// one of the closed cases below.
	if projection.AuthType == "" {
		return nil
	}
	switch projection.AuthType {
	case "none":
		if carrier.Username != "" || carrier.Password != "" || carrier.BearerToken != "" {
			return fmt.Errorf("%w: no-auth metrics connection must not carry credentials", ErrValidation)
		}
	case "basic":
		// A pre-authType direct service fixture may contain the historical
		// username-only Thanos shape. HTTP requests cannot reach this branch
		// because splitConfig requires password; retain it only so an existing
		// encrypted generation can be rotated instead of becoming unreadable.
		if projection.Username == "" || carrier.BearerToken != "" || (carrier.Username != "" && carrier.Username != projection.Username) {
			return fmt.Errorf("%w: basic metrics credentials are incomplete", ErrValidation)
		}
	case "bearer":
		if carrier.BearerToken == "" || carrier.Username != "" || carrier.Password != "" {
			return fmt.Errorf("%w: bearer metrics credentials are incomplete", ErrValidation)
		}
	default:
		return fmt.Errorf("%w: unsupported metrics auth type", ErrValidation)
	}
	return nil
}

type modelProviderSecretJSON struct {
	APIKey string `json:"apiKey"`
}

// Create persists a new connection with revision 1 and generation 1 as a
// durable, replayable command (DATA-COMMAND-002): the runner's transaction
// holds the replay lookup, the validation rejections, the sealed credential
// generation and the automatic audit row. The secret is sealed with the
// current root binding and never enters the ledger — the digest covers only
// non-secret semantic fields plus secret presence, and the replay payload is
// the non-secret Summary.
func (service *Service) Create(ctx context.Context, input CreateInput, createdBy int64, clientCommandID string) (Summary, error) {
	digest := auth.DigestCommand(opCreate, map[string]any{
		"name": input.Name, "type": input.Type,
		"nonSecret": string(input.NonSecretJSON), "secretPresent": input.SecretPresent,
	})
	outcome, err := execution.Run(ctx, service.commands.runner, service.commands.create, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     createdBy,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (Summary, execution.Change, error) {
		if err := requireActor(ctx, createdBy); err != nil {
			return Summary{}, execution.Changed, err
		}
		config, err := validateConfig(input.Type, input.NonSecretJSON)
		if err != nil {
			return Summary{}, execution.Changed, rejectionOf(ErrValidation, codeValidation, err.Error(), 0)
		}
		if err := validateSecret(input.Type, input.Secret); err != nil {
			return Summary{}, execution.Changed, rejectionOf(ErrValidation, codeValidation, err.Error(), 0)
		}
		if err := validateMetricsCredential(input.Type, config, input.Secret); err != nil {
			return Summary{}, execution.Changed, rejectionOf(ErrValidation, codeValidation, err.Error(), 0)
		}
		now := timestampOf(service.now)
		var bindingRevision int
		if err := tx.QueryRowContext(ctx, `SELECT binding_revision FROM root_key_state WHERE id=1`).Scan(&bindingRevision); err != nil {
			return Summary{}, execution.Changed, err
		}
		insert, err := tx.ExecContext(ctx, `INSERT INTO connections(name,type,enabled,revalidation_required,created_at) VALUES(?,?,0,0,?)`, input.Name, input.Type, now)
		if err != nil {
			if isUnique(err) {
				return Summary{}, execution.Changed, rejectionOf(ErrNameTaken, codeNameTaken, "connection name already exists", 0)
			}
			return Summary{}, execution.Changed, err
		}
		connectionID, err := insert.LastInsertId()
		if err != nil {
			return Summary{}, execution.Changed, err
		}
		revision, err := tx.ExecContext(ctx, `INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_by,created_at) VALUES(?,1,?,?,?)`, connectionID, string(config), createdBy, now)
		if err != nil {
			return Summary{}, execution.Changed, err
		}
		revisionID, err := revision.LastInsertId()
		if err != nil {
			return Summary{}, execution.Changed, err
		}
		generationID, err := service.insertGeneration(ctx, tx, connectionID, input.Type, bindingRevision, input.Secret, createdBy, now)
		if err != nil {
			return Summary{}, execution.Changed, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE connections SET current_revision_id=?,current_credential_generation_id=?,row_version=row_version+1 WHERE id=?`, revisionID, generationID, connectionID); err != nil {
			return Summary{}, execution.Changed, err
		}
		summary, err := getSummaryOn(ctx, tx, input.Name)
		return summary, execution.Changed, err
	}, func(summary Summary) int64 { return summary.ID })
	if err != nil {
		return Summary{}, domainError(err)
	}
	return outcome.Result, nil
}

// insertGeneration seals and stores credential generation seq for the
// connection; returns the new row id. The current encryption envelopes are
// reused unchanged; the sealed bytes never appear in any audit or ledger
// payload.
func (service *Service) insertGeneration(ctx context.Context, tx execution.Executor, connectionID int64, connectionType string, bindingRevision int, secret []byte, createdBy int64, now string) (int64, error) {
	var nextSeq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation_seq),0)+1 FROM credential_generations WHERE connection_id=?`, connectionID).Scan(&nextSeq); err != nil {
		return 0, err
	}
	var envelope *envelopeWire
	if len(secret) == 0 {
		// A model provider requires an API key. Prometheus-compatible
		// connections may use no auth, but still seal an explicit empty
		// carrier to preserve independent, auditable credential generations.
		if connectionType != TypePrometheus && connectionType != TypeThanos {
			return 0, fmt.Errorf("%w: %s requires a secret", ErrValidation, connectionType)
		}
		secret = []byte("{}")
	}
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
	insert, err := tx.ExecContext(ctx,
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
	case TypePrometheus:
		payload.Prometheus = &metricsSecretJSON{Username: carrier["username"], Password: carrier["password"], BearerToken: carrier["bearerToken"]}
	case TypeThanos:
		payload.Thanos = &metricsSecretJSON{Username: carrier["username"], Password: carrier["password"], BearerToken: carrier["bearerToken"]}
	case TypeModelProvider:
		payload.ModelProvider = &modelProviderSecretJSON{APIKey: carrier["apiKey"]}
	}
	return payload
}

// getSummaryOn reads one summary row through the given query seam (the
// guarded runner transaction inside audited operations, the read-only reader
// on direct query paths).
func getSummaryOn(ctx context.Context, reader audit.Reader, name string) (Summary, error) {
	row := reader.QueryRowContext(ctx, `
		SELECT c.id,c.name,c.type,c.enabled,c.revalidation_required,
		       COALESCE(c.current_revision_id,0),COALESCE(c.current_credential_generation_id,0),c.row_version,c.created_at,
		       COALESCE((SELECT config_json FROM connection_revisions WHERE id=c.current_revision_id),'{}')
		FROM connections c WHERE c.name=?`, name)
	return scanSummary(row)
}

// Get returns the connection summary by stable name through the read seam.
func (service *Service) Get(ctx context.Context, name string) (Summary, error) {
	return getSummaryOn(ctx, service.read(), name)
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

// List returns one keyset page ordered by name through the read seam.
func (service *Service) List(ctx context.Context, after string, limit int) ([]Summary, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := service.reader.QueryContext(ctx, `
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
// provider (DATA-CONN-003/006) as one audited mutation. The qualified probe
// result must close onto the current immutable revision/generation pair; the
// enablement-coupled post-enable hook commits atomically or not at all.
func (service *Service) Enable(ctx context.Context, name string, expectedRowVersion int64, qualifiedProbeResultID int64, createdBy int64) (Summary, error) {
	summary, err := execution.Execute(ctx, service.commands.runner, service.commands.enable, func(tx *execution.Tx) (Summary, error) {
		if err := requireActor(ctx, createdBy); err != nil {
			return Summary{}, err
		}
		var id int64
		var connectionType string
		var enabled, revalidation int
		var rowVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT id,type,enabled,revalidation_required,row_version FROM connections WHERE name=?`, name).Scan(&id, &connectionType, &enabled, &revalidation, &rowVersion); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Summary{}, rejectionOf(ErrNotFound, codeNotFound, "connection does not exist", 0)
			}
			return Summary{}, err
		}
		if rowVersion != expectedRowVersion {
			return Summary{}, &versionRejection{current: rowVersion, id: id, rejection: execution.Rejection{Code: codeRowVersion, Detail: "connection was modified concurrently", ObjectID: id}}
		}
		if enabled == 1 && revalidation == 0 {
			// Semantic no-op (HTTP-COMMAND-011): the row is untouched and the
			// runner still records the command as the durable fact.
			return getSummaryOn(ctx, tx, name)
		}
		if connectionType == TypeModelProvider || connectionType == TypePrometheus || connectionType == TypeThanos {
			if qualifiedProbeResultID == 0 {
				return Summary{}, rejectionOf(ErrValidation, codeValidation, fmt.Sprintf("%s enable requires an explicit passed probe result", connectionType), id)
			}
			var probeType string
			var outcome string
			var probeRevisionID, probeGenerationID int64
			var currentRevisionID, currentGenerationID int64
			if err := tx.QueryRowContext(ctx, `SELECT connection_type,outcome,connection_revision_id,credential_generation_id FROM connection_probe_results WHERE id=?`, qualifiedProbeResultID).Scan(&probeType, &outcome, &probeRevisionID, &probeGenerationID); err != nil {
				return Summary{}, rejectionOf(ErrValidation, codeValidation, "unknown probe result", id)
			}
			if err := tx.QueryRowContext(ctx, `SELECT current_revision_id,current_credential_generation_id FROM connections WHERE id=?`, id).Scan(&currentRevisionID, &currentGenerationID); err != nil {
				return Summary{}, err
			}
			// Rotation deliberately leaves an already enabled metrics connection in
			// revalidation-required state. A passed real probe over its new immutable
			// revision/generation pair is the event that clears that state; rejecting
			// it because the flag is set would make recovery impossible. Old results
			// cannot qualify because their frozen pair no longer matches.
			if probeType != connectionType || outcome != "passed" || probeRevisionID != currentRevisionID || probeGenerationID != currentGenerationID {
				return Summary{}, rejectionOf(ErrActiveConflict, codeActiveConflict, "probe result does not close onto the current pair", id)
			}
			// The explicit qualification event must close onto the row version
			// the enabling UPDATE produces (trigger checks
			// q.enabled_row_version = NEW.row_version AFTER the update): insert
			// it first against row_version+1, then advance the row in the same
			// transaction. Metrics and model providers share this immutable proof;
			// type-specific SQL closure verifies the matching real probe child.
			if _, err := tx.ExecContext(ctx, `INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(?,?,?,?,?)`, id, rowVersion+1, qualifiedProbeResultID, createdBy, timestampOf(service.now)); err != nil {
				return Summary{}, err
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE connections SET enabled=1,revalidation_required=0,row_version=row_version+1 WHERE id=? AND row_version=?`, id, rowVersion)
		if err != nil {
			if isUnique(err) {
				return Summary{}, rejectionOf(ErrSingleEnabled, codeSingleEnabled, "another enabled connection of this type already exists", id)
			}
			return Summary{}, err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return Summary{}, &versionRejection{current: rowVersion, id: id, rejection: execution.Rejection{Code: codeRowVersion, Detail: "connection was modified concurrently", ObjectID: id}}
		}
		if service.postEnableInTx != nil {
			if err := service.postEnableInTx(ctx, tx, name); err != nil {
				return Summary{}, err
			}
		}
		return getSummaryOn(ctx, tx, name)
	}, func(summary Summary) int64 { return summary.ID })
	if err != nil {
		return Summary{}, domainError(err)
	}
	return summary, nil
}

// Disable blocks new dispatches; already accepted attempts finish. During a
// RootKeyRebind it is the explicit choice to retain the connection disabled.
// The state flip is one audited mutation under the row version fence.
func (service *Service) Disable(ctx context.Context, name string, expectedRowVersion int64) (Summary, error) {
	summary, err := execution.Execute(ctx, service.commands.runner, service.commands.disable, func(tx *execution.Tx) (Summary, error) {
		var id, rowVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT id,row_version FROM connections WHERE name=?`, name).Scan(&id, &rowVersion); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Summary{}, rejectionOf(ErrNotFound, codeNotFound, "connection does not exist", 0)
			}
			return Summary{}, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE connections SET enabled=0,row_version=row_version+1 WHERE name=? AND row_version=?`, name, expectedRowVersion)
		if err != nil {
			return Summary{}, err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return Summary{}, &versionRejection{current: rowVersion, id: id, rejection: execution.Rejection{Code: codeRowVersion, Detail: "connection was modified concurrently", ObjectID: id}}
		}
		if err := markRootKeyRebindConnectionSafe(ctx, tx, name, "disabled"); err != nil {
			return Summary{}, err
		}
		return getSummaryOn(ctx, tx, name)
	}, func(summary Summary) int64 { return summary.ID })
	if err != nil {
		return Summary{}, domainError(err)
	}
	return summary, nil
}

func markRootKeyRebindConnectionSafe(ctx context.Context, tx execution.Executor, name, detailCode string) error {
	var active int
	var reason string
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &revision); err != nil {
		return err
	}
	if active == 0 || reason != "RootKeyRebind" {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE maintenance_items SET safe_state='Safe',detail_code=?,updated_at=? WHERE maintenance_revision=? AND kind='Connection' AND object_key=? AND safe_state='Blocking'`, detailCode, time.Now().UTC().Format(time.RFC3339Nano), revision, name)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return fmt.Errorf("root key rebind checklist item missing or already completed for connection %q", name)
	}
	return nil
}

// openGenerationOn is the sole decryption stage for a credential generation.
// It is reachable only from the audited FulfillGrant runner transaction
// (RUNTIME-GRANT-002): the sealed execution.Executor keeps this sensitive
// read inside the runner-owned, guarded write window — not on the arbitrary
// read-only seam. There is deliberately no public, unguarded reveal entry
// point.
func (service *Service) openGenerationOn(ctx context.Context, conn execution.Executor, generationID int64) (*typedSecretJSON, error) {
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

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// Rotate creates the next revision and credential generation as one durable,
// replayable command that atomically switches the current pair (T09,
// HTTP-COMMAND-013): enabled model providers are disabled first (v1:
// disable-then-switch) and every rotated connection requires a fresh passed
// probe before enabling again (revalidation_required). Grants already frozen
// into a running attempt keep their snapshot until the attempt ends (grant
// 冻结语义，见 grant.go 头注）——轮换只截断新 grant 的下发，不追溯在途
// attempt。The manual rotate audit is replaced by the runner's automatic
// audit row in the same transaction.
func (service *Service) Rotate(ctx context.Context, name string, expectedRowVersion int64, input CreateInput, createdBy int64, clientCommandID string) (Summary, error) {
	digest := auth.DigestCommand(opRotate, map[string]any{
		"name": name, "type": input.Type,
		"nonSecret": string(input.NonSecretJSON), "secretPresent": input.SecretPresent,
	})
	outcome, err := execution.Run(ctx, service.commands.runner, service.commands.rotate, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     createdBy,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (Summary, execution.Change, error) {
		if err := requireActor(ctx, createdBy); err != nil {
			return Summary{}, execution.Changed, err
		}
		config, err := validateConfig(input.Type, input.NonSecretJSON)
		if err != nil {
			return Summary{}, execution.Changed, rejectionOf(ErrValidation, codeValidation, err.Error(), 0)
		}
		if err := validateSecret(input.Type, input.Secret); err != nil {
			return Summary{}, execution.Changed, rejectionOf(ErrValidation, codeValidation, err.Error(), 0)
		}
		var id int64
		var connectionType string
		var rowVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT id,type,row_version FROM connections WHERE name=?`, name).Scan(&id, &connectionType, &rowVersion); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Summary{}, execution.Changed, rejectionOf(ErrNotFound, codeNotFound, "connection does not exist", 0)
			}
			return Summary{}, execution.Changed, err
		}
		if rowVersion != expectedRowVersion {
			return Summary{}, execution.Changed, &versionRejection{current: rowVersion, id: id, rejection: execution.Rejection{Code: codeRowVersion, Detail: "connection was modified concurrently", ObjectID: id}}
		}
		if connectionType != input.Type {
			return Summary{}, execution.Changed, rejectionOf(ErrTypeMismatch, codeTypeMismatch, "rotation must keep the connection type", id)
		}
		now := timestampOf(service.now)
		var bindingRevision int
		if err := tx.QueryRowContext(ctx, `SELECT binding_revision FROM root_key_state WHERE id=1`).Scan(&bindingRevision); err != nil {
			return Summary{}, execution.Changed, err
		}
		var nextRevisionSeq int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision_seq),0)+1 FROM connection_revisions WHERE connection_id=?`, id).Scan(&nextRevisionSeq); err != nil {
			return Summary{}, execution.Changed, err
		}
		revision, err := tx.ExecContext(ctx, `INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_by,created_at) VALUES(?,?,?,?,?)`, id, nextRevisionSeq, string(config), createdBy, now)
		if err != nil {
			return Summary{}, execution.Changed, err
		}
		revisionID, err := revision.LastInsertId()
		if err != nil {
			return Summary{}, execution.Changed, err
		}
		generationID, err := service.insertGeneration(ctx, tx, id, input.Type, bindingRevision, input.Secret, createdBy, now)
		if err != nil {
			return Summary{}, execution.Changed, err
		}
		// A new revision/generation has no qualifying probe yet. Disable every
		// rotated typed connection before switching the pair so the immutable
		// qualification trigger cannot let the old proof authorize new credentials.
		// The later exact-pair passed probe is required to enable it again.
		if _, err := tx.ExecContext(ctx, `UPDATE connections SET current_revision_id=?,current_credential_generation_id=?,enabled=0,revalidation_required=1,row_version=row_version+1 WHERE id=?`, revisionID, generationID, id); err != nil {
			return Summary{}, execution.Changed, err
		}
		// Re-entry under the current root binding closes only this frozen
		// RootKeyRebind checklist item. The connection remains revalidation-required
		// until the later normal-mode verification/enable path succeeds.
		if err := markRootKeyRebindConnectionSafe(ctx, tx, name, "reentered_with_current_root_key"); err != nil {
			return Summary{}, execution.Changed, err
		}
		summary, err := getSummaryOn(ctx, tx, name)
		return summary, execution.Changed, err
	}, func(summary Summary) int64 { return summary.ID })
	if err != nil {
		return Summary{}, domainError(err)
	}
	return outcome.Result, nil
}

// DiscoveryView is the non-secret model discovery outcome for the HTTP
// adapter. Provider errors are never echoed: detail carries a stable,
// secret-free hint.
type DiscoveryView struct {
	Available bool
	Models    []modelprovider.DiscoveredModel
	Detail    string
}

// discoveryAudit is the durable, secret-free audit payload of one discovery
// execution. The API key exists only in request memory and never enters any
// ledger or audit row.
type discoveryAudit struct {
	Available  bool `json:"available"`
	ModelCount int  `json:"modelCount"`
}

// DiscoverProviderModels probes the upstream /v1/models endpoint with the
// form's Base URL and API key (input helper only, never a qualification).
// The external network call runs OUTSIDE any transaction; the audited
// mutation records the durable, secret-free outcome afterwards
// (connection.model_discovery). A context without execution metadata fails
// closed.
func (service *Service) DiscoverProviderModels(ctx context.Context, baseURL, apiKey string) (DiscoveryView, error) {
	if _, err := execution.Require(ctx); err != nil {
		return DiscoveryView{}, err
	}
	view := DiscoveryView{}
	models, err := modelprovider.DiscoverUpstream(ctx, baseURL, apiKey)
	switch {
	case err != nil:
		view.Detail = "暂时无法从该地址读取模型列表；可以直接手工填写模型 ID。"
	case len(models) == 0:
		view.Detail = "上游未返回任何模型；可以直接手工填写模型 ID。"
	default:
		view.Available = true
		view.Models = models
	}
	// The upstream conversation itself is not transactional; only the durable
	// discovery fact is recorded, classified by the runner with its automatic
	// audit row (no state change, no ledger row — the request owns no
	// client command id).
	if _, err := execution.Execute(ctx, service.commands.runner, service.commands.modelDiscovery, func(tx *execution.Tx) (discoveryAudit, error) {
		return discoveryAudit{Available: view.Available, ModelCount: len(view.Models)}, nil
	}, func(discoveryAudit) int64 { return 0 }); err != nil {
		return DiscoveryView{}, err
	}
	return view, nil
}

// timestampOf is the shared UTC RFC3339Nano formatting for domain rows.
func timestampOf(now func() time.Time) string {
	return now().UTC().Format(time.RFC3339Nano)
}

// locator renders a decimal domain locator (probe result ids in HTTP bodies).
func locator(id int64) string { return strconv.FormatInt(id, 10) }
