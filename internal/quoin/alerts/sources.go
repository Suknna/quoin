package alerts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// Sources owns the alert source and credential lifecycle commands behind the
// frozen schema triggers (DATA-ALERT-009/010, SEC-REVEAL-*, HTTP-COMMAND-*).

// CreateSourceResult carries the non-secret outcome of creating a source with
// its first credential generation; the raw bearer never leaves the caller's
// reveal path.
type CreateSourceResult struct {
	SourceID     int64
	CredentialID int64
	SourceKey    string
}

// CreateSource creates the logical source and its first Active credential in
// one transaction, persisting only the bearer digest. The audit event records
// the acting admin (HTTP-COMMAND-008).
func (service *Service) CreateSource(ctx context.Context, sourceKey, protocol string, bearerDigest []byte, actorID int64, timestamp string) (CreateSourceResult, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return CreateSourceResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return CreateSourceResult{}, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	result, err := conn.ExecContext(ctx, `INSERT INTO alert_sources(source_key, protocol, enabled, created_at) VALUES(?,?,1,?)`, sourceKey, protocol, timestamp)
	if err != nil {
		return CreateSourceResult{}, err
	}
	sourceID, err := result.LastInsertId()
	if err != nil {
		return CreateSourceResult{}, err
	}
	credentialResult, err := conn.ExecContext(ctx, `INSERT INTO alert_source_credentials(source_id, digest, state, created_at) VALUES(?,?,'Active',?)`, sourceID, bearerDigest, timestamp)
	if err != nil {
		return CreateSourceResult{}, err
	}
	credentialID, err := credentialResult.LastInsertId()
	if err != nil {
		return CreateSourceResult{}, err
	}
	if err := service.recordAudit(ctx, conn, "user", actorID, "alert_source.create", "success", "alert_source", sourceID, timestamp); err != nil {
		return CreateSourceResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return CreateSourceResult{}, err
	}
	return CreateSourceResult{SourceID: sourceID, CredentialID: credentialID, SourceKey: sourceKey}, nil
}

// RotateCredential creates a new Active generation superseding the current
// Active one (max two accepted generations per source, schema enforced).
func (service *Service) RotateCredential(ctx context.Context, sourceKey string, newDigest []byte, actorID int64, timestamp string) (CreateSourceResult, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return CreateSourceResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return CreateSourceResult{}, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	var sourceID int64
	var currentID int64
	err = conn.QueryRowContext(ctx, `SELECT id FROM alert_sources WHERE source_key=?`, sourceKey).Scan(&sourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return CreateSourceResult{}, errNotFound
	}
	if err != nil {
		return CreateSourceResult{}, err
	}
	err = conn.QueryRowContext(ctx, `SELECT id FROM alert_source_credentials WHERE source_id=? AND state='Active' ORDER BY id DESC LIMIT 1`, sourceID).Scan(&currentID)
	if errors.Is(err, sql.ErrNoRows) {
		return CreateSourceResult{}, fmt.Errorf("no active credential to rotate")
	}
	if err != nil {
		return CreateSourceResult{}, err
	}
	result, err := conn.ExecContext(ctx, `INSERT INTO alert_source_credentials(source_id, digest, state, supersedes_credential_id, created_at) VALUES(?,?,'Active',?,?)`,
		sourceID, newDigest, currentID, timestamp)
	if err != nil {
		return CreateSourceResult{}, err
	}
	credentialID, err := result.LastInsertId()
	if err != nil {
		return CreateSourceResult{}, err
	}
	if err := service.recordAudit(ctx, conn, "user", actorID, "alert_source.rotate", "success", "alert_source_credential", credentialID, timestamp); err != nil {
		return CreateSourceResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return CreateSourceResult{}, err
	}
	return CreateSourceResult{SourceID: sourceID, CredentialID: credentialID, SourceKey: sourceKey}, nil
}

// RetireCredential performs the explicit Active|PendingRetirement -> Retired
// transition (DATA-ALERT-009). expectedRowVersion is checked in the UPDATE.
func (service *Service) RetireCredential(ctx context.Context, sourceKey string, credentialID int64, expectedRowVersion int64, actorID int64, timestamp string) (bool, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return false, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	result, err := conn.ExecContext(ctx, `UPDATE alert_source_credentials SET state='Retired', retired_at=?, row_version=row_version+1 WHERE id=? AND source_id=(SELECT id FROM alert_sources WHERE source_key=?) AND row_version=? AND state IN ('Active','PendingRetirement')`,
		timestamp, credentialID, sourceKey, expectedRowVersion)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		// Either wrong row version or already retired.
		return false, nil
	}
	if err := service.recordAudit(ctx, conn, "user", actorID, "alert_source.credential_retire", "success", "alert_source_credential", credentialID, timestamp); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, err
	}
	return true, nil
}

var errNotFound = errors.New("not found")

// SetSourceEnabled toggles the logical source with row-version fencing.
func (service *Service) SetSourceEnabled(ctx context.Context, sourceKey string, enabled bool, expectedRowVersion int64, actorID int64, timestamp string) (bool, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return false, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	value := 0
	if enabled {
		value = 1
	}
	var disabledAt any
	if !enabled {
		disabledAt = timestamp
	}
	var sourceID int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM alert_sources WHERE source_key=?`, sourceKey).Scan(&sourceID); err != nil {
		return false, err
	}
	result, err := conn.ExecContext(ctx, `UPDATE alert_sources SET enabled=?, disabled_at=?, row_version=row_version+1 WHERE source_key=? AND row_version=?`,
		value, disabledAt, sourceKey, expectedRowVersion)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return false, nil
	}
	if err := service.recordAudit(ctx, conn, "user", actorID, "alert_source.set_enabled", "success", "alert_source", sourceID, timestamp); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, err
	}
	return true, nil
}

// ListCredentials returns the credential history (no digests).
func (service *Service) ListCredentials(ctx context.Context, sourceKey string) ([]CredentialSummary, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT c.id, c.state, c.row_version, c.created_at, c.supersedes_credential_id, c.first_used_at, c.pending_retirement_at, c.retired_at FROM alert_source_credentials c JOIN alert_sources s ON s.id=c.source_id WHERE s.source_key=? ORDER BY c.id DESC`, sourceKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	credentials := []CredentialSummary{}
	for rows.Next() {
		var summary CredentialSummary
		var id int64
		var supersedes, firstUsed, pendingRetirement, retired sql.NullString
		if err := rows.Scan(&id, &summary.State, &summary.RowVersion, &summary.CreatedAt, &supersedes, &firstUsed, &pendingRetirement, &retired); err != nil {
			return nil, err
		}
		summary.ID = strconv.FormatInt(id, 10)
		if supersedes.Valid {
			summary.SupersedesCredentialID = &supersedes.String
		}
		if firstUsed.Valid {
			summary.FirstUsedAt = &firstUsed.String
		}
		if pendingRetirement.Valid {
			summary.PendingRetirementAt = &pendingRetirement.String
		}
		if retired.Valid {
			summary.RetiredAt = &retired.String
		}
		credentials = append(credentials, summary)
	}
	return credentials, rows.Err()
}

type CredentialSummary struct {
	ID                     string  `json:"id"`
	State                  string  `json:"state"`
	RowVersion             int64   `json:"rowVersion"`
	CreatedAt              string  `json:"createdAt"`
	SupersedesCredentialID *string `json:"supersedesCredentialId,omitempty"`
	FirstUsedAt            *string `json:"firstUsedAt,omitempty"`
	PendingRetirementAt    *string `json:"pendingRetirementAt,omitempty"`
	RetiredAt              *string `json:"retiredAt,omitempty"`
}

// RecordRevealAudit appends the non-secret reveal audit event (HTTP-COMMAND-
// 008): no handle, no bearer, just actor, target credential, and outcome.
func (service *Service) RecordRevealAudit(ctx context.Context, actorID, credentialID int64, outcome string, timestamp string) error {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	if err := service.recordAudit(ctx, conn, "user", actorID, "alert_source.credential_reveal", outcome, "alert_source_credential", credentialID, timestamp); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}
