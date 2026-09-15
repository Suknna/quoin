package businesssystem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// KubernetesConnectionMapping is the append-only management-plane binding
// between a Business System and a Kubernetes Connection. It is intentionally
// absent from business-system YAML and from model-visible tool arguments.
type KubernetesConnectionMapping struct {
	ID             int64   `json:"id,string"`
	ConnectionID   int64   `json:"connectionId,string"`
	ConnectionName string  `json:"connectionName"`
	State          string  `json:"state"`
	RowVersion     int64   `json:"rowVersion"`
	CreatedBy      string  `json:"createdBy"`
	CreatedAt      string  `json:"createdAt"`
	RetiredBy      *string `json:"retiredBy"`
	RetiredAt      *string `json:"retiredAt,omitempty"`
}

// ListKubernetesConnectionMappings returns the complete append-only mapping
// history in creation order so an Admin can distinguish current bindings from
// retired routing history.
func (service *Service) ListKubernetesConnectionMappings(ctx context.Context, systemKey string) ([]KubernetesConnectionMapping, error) {
	var systemID int64
	if err := service.db.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, systemKey).Scan(&systemID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return listKubernetesConnectionMappingsOn(ctx, service.db, systemID)
}

func systemIDForKey(ctx context.Context, conn execution.Executor, systemKey string) (int64, error) {
	var systemID int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, systemKey).Scan(&systemID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return systemID, nil
}

func listKubernetesConnectionMappingsOn(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, systemID int64) ([]KubernetesConnectionMapping, error) {
	rows, err := queryer.QueryContext(ctx, `
		SELECT m.id,m.connection_id,c.name,m.state,m.row_version,m.created_by,m.created_at,m.retired_by,m.retired_at
		FROM business_system_kubernetes_connections m
		JOIN connections c ON c.id=m.connection_id
		WHERE m.business_system_id=?
		ORDER BY m.id`, systemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	mappings := []KubernetesConnectionMapping{}
	for rows.Next() {
		mapping, err := scanKubernetesConnectionMapping(rows)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, mapping)
	}
	return mappings, rows.Err()
}

func kubernetesConnectionMappingOn(ctx context.Context, conn execution.Executor, mappingID int64) (KubernetesConnectionMapping, error) {
	row := conn.QueryRowContext(ctx, `
		SELECT m.id,m.connection_id,c.name,m.state,m.row_version,m.created_by,m.created_at,m.retired_by,m.retired_at
		FROM business_system_kubernetes_connections m
		JOIN connections c ON c.id=m.connection_id
		WHERE m.id=?`, mappingID)
	mapping, err := scanKubernetesConnectionMapping(row)
	if errors.Is(err, sql.ErrNoRows) {
		return KubernetesConnectionMapping{}, ErrNotFound
	}
	return mapping, err
}

func scanKubernetesConnectionMapping(row interface {
	Scan(...any) error
}) (KubernetesConnectionMapping, error) {
	var (
		mapping   KubernetesConnectionMapping
		createdBy sql.NullInt64
		retiredBy sql.NullInt64
		retiredAt sql.NullString
	)
	if err := row.Scan(&mapping.ID, &mapping.ConnectionID, &mapping.ConnectionName, &mapping.State, &mapping.RowVersion, &createdBy, &mapping.CreatedAt, &retiredBy, &retiredAt); err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if !createdBy.Valid {
		return KubernetesConnectionMapping{}, fmt.Errorf("mapping %d lacks required creator", mapping.ID)
	}
	mapping.CreatedBy = fmt.Sprint(createdBy.Int64)
	if retiredBy.Valid {
		value := fmt.Sprint(retiredBy.Int64)
		mapping.RetiredBy = &value
	}
	if retiredAt.Valid {
		mapping.RetiredAt = stringPointer(retiredAt.String)
	}
	return mapping, nil
}

func int64Pointer(value int64) *int64    { return &value }
func stringPointer(value string) *string { return &value }
