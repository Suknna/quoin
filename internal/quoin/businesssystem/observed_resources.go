package businesssystem

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type ObservedResourceSummary struct {
	ID                      string            `json:"id"`
	DiscoveryKey            string            `json:"discoveryKey"`
	IdentityLabels          map[string]string `json:"identityLabels"`
	ObservedAt              string            `json:"observedAt,omitempty"`
	Current                 bool              `json:"current"`
	Stale                   bool              `json:"stale"`
	LastSuccessfulRefreshAt *string           `json:"lastSuccessfulRefreshAt,omitempty"`
}
type ObservedResourceDetail struct {
	ObservedResourceSummary
	Labels    map[string]string `json:"labels"`
	CreatedAt string            `json:"createdAt"`
}

// ListObservedResources derives freshness from the configured interval and the
// immutable last-success fact; stale is never independently written as truth.
func (service *Service) ListObservedResources(ctx context.Context, systemKey string, current *bool, after int64, limit int) ([]ObservedResourceSummary, int64, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var systemID, interval int64
	if err := service.db.QueryRowContext(ctx, `SELECT id,resource_refresh_interval_seconds FROM business_systems WHERE key=?`, systemKey).Scan(&systemID, &interval); err != nil {
		if err == sql.ErrNoRows {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	query, args := `SELECT o.id,o.discovery_key,o.labels_json,o.observed_at,o.current,o.last_successful_refresh_at,
		COALESCE((SELECT json_group_object(name,value) FROM (SELECT name,value FROM observed_resource_identity_labels WHERE observed_resource_id=o.id ORDER BY name)),'{}')
		FROM observed_resources o WHERE o.business_system_id=? AND o.id>?`, []any{systemID, after}
	if current != nil {
		query += ` AND o.current=?`
		args = append(args, boolInt(*current))
	}
	query += ` ORDER BY o.id LIMIT ?`
	args = append(args, limit+1)
	rows, err := service.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]ObservedResourceSummary, 0, limit)
	var next int64
	for rows.Next() {
		var id int64
		var discovery, labelsJSON, observed, identityJSON string
		var currentInt int
		var last sql.NullString
		if err := rows.Scan(&id, &discovery, &labelsJSON, &observed, &currentInt, &last, &identityJSON); err != nil {
			return nil, 0, err
		}
		if len(items) == limit {
			next = id
			break
		}
		identity := map[string]string{}
		if err := json.Unmarshal([]byte(identityJSON), &identity); err != nil {
			return nil, 0, fmt.Errorf("decode observed resource identity: %w", err)
		}
		item := ObservedResourceSummary{ID: fmt.Sprint(id), DiscoveryKey: discovery, IdentityLabels: identity, ObservedAt: observed, Current: currentInt == 1}
		if last.Valid {
			item.LastSuccessfulRefreshAt = &last.String
			item.Stale = resourceStale(last.String, interval, service.now())
		}
		items = append(items, item)
	}
	return items, next, rows.Err()
}

func (service *Service) observedResourceIdentity(ctx context.Context, id int64) (map[string]string, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT name,value FROM observed_resource_identity_labels WHERE observed_resource_id=? ORDER BY name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[string]string{}
	for rows.Next() {
		var n, v string
		if err := rows.Scan(&n, &v); err != nil {
			return nil, err
		}
		labels[n] = v
	}
	return labels, rows.Err()
}
func resourceStale(last string, interval int64, now time.Time) bool {
	observed, err := time.Parse(time.RFC3339Nano, last)
	return err != nil || observed.Add(time.Duration(interval)*time.Second).Before(now)
}

func (service *Service) GetObservedResource(ctx context.Context, systemKey string, resourceID int64) (ObservedResourceDetail, error) {
	var systemID, interval int64
	if err := service.db.QueryRowContext(ctx, `SELECT id,resource_refresh_interval_seconds FROM business_systems WHERE key=?`, systemKey).Scan(&systemID, &interval); err != nil {
		if err == sql.ErrNoRows {
			return ObservedResourceDetail{}, ErrNotFound
		}
		return ObservedResourceDetail{}, err
	}
	var discovery, labelsJSON, observed, created string
	var current int
	var last sql.NullString
	if err := service.db.QueryRowContext(ctx, `SELECT discovery_key,labels_json,observed_at,current,last_successful_refresh_at,created_at FROM observed_resources WHERE id=? AND business_system_id=?`, resourceID, systemID).Scan(&discovery, &labelsJSON, &observed, &current, &last, &created); err != nil {
		if err == sql.ErrNoRows {
			return ObservedResourceDetail{}, ErrNotFound
		}
		return ObservedResourceDetail{}, err
	}
	labels := map[string]string{}
	if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
		return ObservedResourceDetail{}, err
	}
	identity, err := service.observedResourceIdentity(ctx, resourceID)
	if err != nil {
		return ObservedResourceDetail{}, err
	}
	summary := ObservedResourceSummary{ID: fmt.Sprint(resourceID), DiscoveryKey: discovery, IdentityLabels: identity, ObservedAt: observed, Current: current == 1}
	if last.Valid {
		summary.LastSuccessfulRefreshAt = &last.String
		summary.Stale = resourceStale(last.String, interval, service.now())
	}
	return ObservedResourceDetail{ObservedResourceSummary: summary, Labels: labels, CreatedAt: created}, nil
}
