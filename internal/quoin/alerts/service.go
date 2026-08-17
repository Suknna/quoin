package alerts

import (
	"context"
	"database/sql"
	"strconv"
)

// Service is the alert-domain facade over the frozen SQLite authority.
type Service struct {
	db *sql.DB
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// recordAudit writes a narrow append-only audit event inside the caller's
// transaction (CONTEXT audit semantics), with the standard target child row
// matching auth's insertAudit.
func (service *Service) recordAudit(ctx context.Context, conn *sql.Conn, actorType string, actorID int64, action, outcome, targetType string, targetID int64, timestamp string) error {
	result, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type, actor_id, action, outcome, domain_ref_type, domain_ref_id, created_at) VALUES(?,?,?,?,?,?,?)`,
		actorType, actorID, action, outcome, targetType, targetID, timestamp)
	if err != nil {
		return err
	}
	auditID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO audit_event_targets(audit_event_id, target_type, target_id) VALUES(?,?,?)`, auditID, targetType, targetID)
	return err
}

// ListSources returns the admin-facing alert source list.
func (service *Service) ListSources(ctx context.Context) ([]SourceSummary, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT id, source_key, protocol, enabled, row_version, created_at, disabled_at FROM alert_sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := []SourceSummary{}
	for rows.Next() {
		var summary SourceSummary
		var id int64
		var enabled int
		var disabledAt sql.NullString
		if err := rows.Scan(&id, &summary.Key, &summary.Protocol, &enabled, &summary.RowVersion, &summary.CreatedAt, &disabledAt); err != nil {
			return nil, err
		}
		summary.ID = strconv.FormatInt(id, 10)
		summary.Enabled = enabled == 1
		if disabledAt.Valid {
			summary.DisabledAt = &disabledAt.String
		}
		sources = append(sources, summary)
	}
	return sources, rows.Err()
}

type SourceSummary struct {
	ID         string  `json:"id"`
	Key        string  `json:"key"`
	Protocol   string  `json:"protocol"`
	Enabled    bool    `json:"enabled"`
	RowVersion int64   `json:"rowVersion"`
	CreatedAt  string  `json:"createdAt"`
	DisabledAt *string `json:"disabledAt"`
}

// GetSource returns one alert source with its credential count.
func (service *Service) GetSource(ctx context.Context, sourceKey string) (SourceDetail, error) {
	var detail SourceDetail
	var id int64
	var enabled int
	var disabledAt sql.NullString
	err := service.db.QueryRowContext(ctx, `SELECT id, source_key, protocol, enabled, row_version, created_at, disabled_at, (SELECT COUNT(*) FROM alert_source_credentials c WHERE c.source_id = alert_sources.id) FROM alert_sources WHERE source_key=?`, sourceKey).
		Scan(&id, &detail.Key, &detail.Protocol, &enabled, &detail.RowVersion, &detail.CreatedAt, &disabledAt, &detail.CredentialCount)
	if err != nil {
		return SourceDetail{}, err
	}
	detail.ID = strconv.FormatInt(id, 10)
	detail.Enabled = enabled == 1
	if disabledAt.Valid {
		detail.DisabledAt = &disabledAt.String
	}
	return detail, nil
}

type SourceDetail struct {
	SourceSummary
	CredentialCount int `json:"credentialCount"`
}

// LookupDigestForBearer authenticates a Stele-attached bearer against the
// active credential set and returns the source and credential ids for the
// delivery transaction. This is the Stele path; the HTTP reveal path does not
// call it.
func (service *Service) LookupDigestForBearer(ctx context.Context, bearerDigest []byte) (sourceID, credentialID int64, ok bool, err error) {
	err = service.db.QueryRowContext(ctx, `SELECT c.source_id, c.id FROM alert_source_credentials c JOIN alert_sources s ON s.id = c.source_id WHERE c.digest = ? AND s.enabled = 1 AND c.state IN ('Active','PendingRetirement')`, bearerDigest).
		Scan(&sourceID, &credentialID)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return sourceID, credentialID, true, nil
}

// CredentialSnapshot returns the frozen non-secret credential digest snapshot
// Stele caches (RUNTIME-STELE-002): active + pending-retirement generations
// only, retired generations absent.
func (service *Service) CredentialSnapshot(ctx context.Context) (version uint64, sources []SnapshotSource, err error) {
	var maxID int64
	if err := service.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM alert_source_credentials`).Scan(&maxID); err != nil {
		return 0, nil, err
	}
	rows, err := service.db.QueryContext(ctx, `SELECT s.id, s.source_key, s.protocol, s.enabled, c.id, c.digest FROM alert_sources s JOIN alert_source_credentials c ON c.source_id = s.id WHERE c.state IN ('Active','PendingRetirement') ORDER BY s.id, c.id`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	bySource := map[int64]*SnapshotSource{}
	order := []int64{}
	for rows.Next() {
		var sourceID, credentialID int64
		var sourceKey, protocol string
		var enabled int
		var digest []byte
		if err := rows.Scan(&sourceID, &sourceKey, &protocol, &enabled, &credentialID, &digest); err != nil {
			return 0, nil, err
		}
		entry := bySource[sourceID]
		if entry == nil {
			entry = &SnapshotSource{SourceID: sourceID, SourceKey: sourceKey, Protocol: protocol, Enabled: enabled == 1}
			bySource[sourceID] = entry
			order = append(order, sourceID)
		}
		entry.Credentials = append(entry.Credentials, CredentialDigest{ID: credentialID, Digest: digest})
	}
	sources = make([]SnapshotSource, 0, len(order))
	for _, sourceID := range order {
		sources = append(sources, *bySource[sourceID])
	}
	return uint64(maxID), sources, nil
}

type SnapshotSource struct {
	SourceID    int64              `json:"sourceId"`
	SourceKey   string             `json:"sourceKey"`
	Protocol    string             `json:"protocol"`
	Enabled     bool               `json:"enabled"`
	Credentials []CredentialDigest `json:"credentials"`
}

type CredentialDigest struct {
	ID     int64  `json:"id"`
	Digest []byte `json:"digest"`
}

// CountFiringOccurrences supports the snapshot/list query used by the UI.
func (service *Service) CountFiringOccurrences(ctx context.Context) (int64, error) {
	var count int64
	err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_occurrences WHERE state='Firing'`).Scan(&count)
	return count, err
}
