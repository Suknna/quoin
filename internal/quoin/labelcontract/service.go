// Package labelcontract owns the Label Contract aggregate (T16): immutable
// strict-YAML drafts and the atomic activation command whose single
// label_contract_activations INSERT is validated and applied entirely by the
// frozen SQL trigger (DATA-CONFIG-002/006). Zero enabled systems activate
// with an empty item set; per-system activation evidence remains the later
// Label Contract ticket's readiness surface.
package labelcontract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/config"
)

// ErrNotFound reports a missing contract version.
var ErrNotFound = errors.New("label contract version not found")

// ErrCommandReused reports a client command id replayed with a different
// request digest (HTTP-COMMAND-003).
var ErrCommandReused = errors.New("client command id reused with a different request")

// BadRequestError carries a deterministic request-shape rejection (400).
type BadRequestError struct{ Reason string }

func (err *BadRequestError) Error() string { return err.Reason }

// ConflictError carries the frozen conflict codes for stale activation
// preconditions (HTTP-ERROR-004/007).
type ConflictError struct {
	Code            string // row_version_conflict | current_pointer_conflict | invalid_state
	Detail          string
	CurrentVersion  *int64 // for current_pointer_conflict: the actual current contract id
	ObjectID        int64
	CurrentStateRow int64
}

func (err *ConflictError) Error() string { return err.Detail }

// Detail is the LabelContractDetail projection.
type Detail struct {
	ID            string         `json:"id"`
	Version       int64          `json:"version"`
	State         string         `json:"state"`
	RowVersion    int64          `json:"rowVersion"`
	ParserVersion string         `json:"parserVersion"`
	SchemaVersion string         `json:"schemaVersion"`
	CreatedAt     string         `json:"createdAt"`
	ActivatedAt   *string        `json:"activatedAt,omitempty"`
	YAMLBody      string         `json:"yamlBody"`
	ContractJSON  map[string]any `json:"contractJson"`
	Digest        string         `json:"digest"`
}

// ActivationItem is one enabled-system entry of the activation command; the
// business system key is resolved to its row id inside the transaction.
type ActivationItem struct {
	BusinessSystemKey                string
	ConfigVersionID                  int64
	VerificationRunID                int64
	ExpectedCurrentConfigVersionID   *int64
	ExpectedBusinessSystemRowVersion int64

	// businessSystemID is the transaction-local resolution of
	// BusinessSystemKey; it never enters the command digest.
	businessSystemID int64
}

// ActivateInput carries the multi-aggregate concurrency preconditions
// (DATA-CONFIG-005/006).
type ActivateInput struct {
	ContractVersion           int64
	ExpectedStateRowVersion   int64
	ExpectedCurrentContractID *int64
	ExpectedTargetRowVersion  int64
	Items                     []ActivationItem
}

// Service owns the Label Contract SQLite transactions.
type Service struct {
	db  *sql.DB
	now func() time.Time
}

// NewService builds the service on the shared database pool.
func NewService(db *sql.DB) *Service {
	return &Service{db: db, now: time.Now}
}

func (service *Service) nowText() string { return service.now().UTC().Format(time.RFC3339Nano) }

// CreateDraft parses the strict YAML once, persists the immutable draft row
// with its typed projection and returns the detail (DATA-CONFIG-003). The
// parse failure surfaces as *config.ValidationError for the 422 mapping.
func (service *Service) CreateDraft(ctx context.Context, principalID int64, clientCommandID string, body []byte, limits config.Limits) (Detail, error) {
	value, fieldErrors := config.ParseStrictYAML(body, limits, "document")
	if len(fieldErrors) != 0 {
		return Detail{}, &config.ValidationError{Errors: fieldErrors}
	}
	if fields := config.ValidateSchema(value, config.SchemaLabelContract); len(fields) != 0 {
		return Detail{}, &config.ValidationError{Errors: fields}
	}
	document, err := config.ExtractLabelContract(value)
	if err != nil {
		return Detail{}, &config.ValidationError{Errors: []config.FieldError{{Path: "document", Reason: err.Error()}}}
	}
	digest := document.Digest()
	commandDigest := auth.DigestCommand("label_contract.create", map[string]any{"digest": digest})
	if record, found, lookupErr := auth.LookupCommand(ctx, service.db, principalID, clientCommandID); lookupErr == nil && found {
		if record.RequestDigest != commandDigest {
			return Detail{}, ErrCommandReused
		}
		var replayed Detail
		if err := decodeStoredResult(record.ResultPayload, &replayed); err == nil && replayed.ID != "" {
			return replayed, nil
		}
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return Detail{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return Detail{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	now := service.nowText()
	var version int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM label_contracts`).Scan(&version); err != nil {
		return Detail{}, err
	}
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO label_contracts(version,yaml_body,contract_json,digest,parser_version,schema_version,state,row_version,created_at)
		VALUES(?,?,?,?,?,?, 'draft', 1, ?)`,
		version, string(body), document.CanonicalJSON(), digest, config.ParserVersion, config.SchemaVersion(config.SchemaLabelContract), now)
	if err != nil {
		return Detail{}, err
	}
	contractID, err := insert.LastInsertId()
	if err != nil {
		return Detail{}, err
	}
	if err := recordAudit(ctx, conn, principalID, "label_contract.create", contractID, now); err != nil {
		return Detail{}, err
	}
	detail, err := service.detailOn(ctx, conn, version)
	if err != nil {
		return Detail{}, err
	}
	if err := auth.RecordCommand(ctx, conn, principalID, clientCommandID, "label_contract.create", commandDigest, "committed", "label_contract", contractID, marshalResult(detail)); err != nil {
		return Detail{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return Detail{}, err
	}
	committed = true
	return detail, nil
}

// Activate submits the single activation INSERT; the AFTER trigger revalidates
// every precondition and atomically switches the contract pointer, the
// enabled systems' config pointers and the derived states (DATA-CONFIG-002).
// Any stale or invalid precondition aborts the whole statement.
func (service *Service) Activate(ctx context.Context, principalID int64, clientCommandID string, input ActivateInput) (Detail, error) {
	commandDigest := auth.DigestCommand("label_contract.activate", map[string]any{
		"version": input.ContractVersion,
		"state":   input.ExpectedStateRowVersion,
		"current": nullableID(input.ExpectedCurrentContractID),
		"target":  input.ExpectedTargetRowVersion,
		"items":   activationItemDigest(input.Items),
	})
	if record, found, lookupErr := auth.LookupCommand(ctx, service.db, principalID, clientCommandID); lookupErr == nil && found {
		if record.RequestDigest != commandDigest {
			return Detail{}, ErrCommandReused
		}
		var replayed Detail
		if err := decodeStoredResult(record.ResultPayload, &replayed); err == nil && replayed.ID != "" {
			return replayed, nil
		}
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return Detail{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return Detail{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var contractID int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM label_contracts WHERE version=?`, input.ContractVersion).Scan(&contractID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Detail{}, ErrNotFound
		}
		return Detail{}, err
	}
	// Resolve each item's business system key inside the serialized
	// transaction; unknown keys are deterministic request errors.
	for index := range input.Items {
		var systemID int64
		if err := conn.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, input.Items[index].BusinessSystemKey).Scan(&systemID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Detail{}, &BadRequestError{Reason: "compatibleVersions 引用了不存在的业务系统: " + input.Items[index].BusinessSystemKey}
			}
			return Detail{}, err
		}
		input.Items[index].businessSystemID = systemID
	}
	itemsJSON := buildItemsJSON(input.Items)
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO label_contract_activations(contract_id,expected_target_row_version,expected_state_row_version,expected_current_contract_id,items_json,created_at)
		VALUES(?,?,?,?,?,?)`,
		contractID, input.ExpectedTargetRowVersion, input.ExpectedStateRowVersion, nullableSQL(input.ExpectedCurrentContractID), itemsJSON, service.nowText()); err != nil {
		return Detail{}, mapActivationAbort(err, input)
	}
	if err := recordAudit(ctx, conn, principalID, "label_contract.activate", contractID, service.nowText()); err != nil {
		return Detail{}, err
	}
	detail, err := service.detailOn(ctx, conn, input.ContractVersion)
	if err != nil {
		return Detail{}, err
	}
	if err := auth.RecordCommand(ctx, conn, principalID, clientCommandID, "label_contract.activate", commandDigest, "committed", "label_contract", contractID, marshalResult(detail)); err != nil {
		return Detail{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return Detail{}, err
	}
	committed = true
	return detail, nil
}

// Current returns the active contract id and state row version, or nil when
// no contract has been activated yet (the upload form's provenance view).
func (service *Service) Current(ctx context.Context) (currentContractID *int64, stateRowVersion int64, err error) {
	var current sql.NullInt64
	if err := service.db.QueryRowContext(ctx, `SELECT current_contract_id,row_version FROM label_contract_state WHERE id=1`).Scan(&current, &stateRowVersion); err != nil {
		return nil, 0, err
	}
	if current.Valid {
		value := current.Int64
		currentContractID = &value
	}
	return currentContractID, stateRowVersion, nil
}

// List returns contract summaries newest-first with cursor pagination.
func (service *Service) List(ctx context.Context, cursor string, limit int) ([]Detail, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id,version,state,row_version,parser_version,schema_version,created_at,activated_at FROM label_contracts`
	args := []any{}
	if cursor != "" {
		if last, err := strconv.ParseInt(cursor, 10, 64); err == nil {
			query += ` WHERE version < ?`
			args = append(args, last)
		}
	}
	query += ` ORDER BY version DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := service.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []Detail{}
	for rows.Next() {
		detail, scanErr := scanSummary(rows)
		if scanErr != nil {
			return nil, false, scanErr
		}
		items = append(items, detail)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := false
	if len(items) > limit {
		items = items[:limit]
		more = true
	}
	return items, more, nil
}

// GetByVersion returns the full detail of one contract version.
func (service *Service) GetByVersion(ctx context.Context, version int64) (Detail, error) {
	return service.detailOn(ctx, nil, version)
}

func (service *Service) detailOn(ctx context.Context, conn *sql.Conn, version int64) (Detail, error) {
	query := `
		SELECT id,version,state,row_version,parser_version,schema_version,created_at,activated_at,yaml_body,contract_json,digest
		FROM label_contracts WHERE version=?`
	var (
		detail     Detail
		id         int64
		activated  sql.NullString
		contractJS string
	)
	var err error
	if conn != nil {
		err = conn.QueryRowContext(ctx, query, version).Scan(&id, &detail.Version, &detail.State, &detail.RowVersion, &detail.ParserVersion, &detail.SchemaVersion, &detail.CreatedAt, &activated, &detail.YAMLBody, &contractJS, &detail.Digest)
	} else {
		err = service.db.QueryRowContext(ctx, query, version).Scan(&id, &detail.Version, &detail.State, &detail.RowVersion, &detail.ParserVersion, &detail.SchemaVersion, &detail.CreatedAt, &activated, &detail.YAMLBody, &contractJS, &detail.Digest)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, err
	}
	detail.ID = strconv.FormatInt(id, 10)
	if activated.Valid {
		value := activated.String
		detail.ActivatedAt = &value
	}
	projection := map[string]any{}
	if err := decodeStoredResult(contractJS, &projection); err != nil {
		return Detail{}, fmt.Errorf("contract projection decode: %w", err)
	}
	detail.ContractJSON = projection
	return detail, nil
}
