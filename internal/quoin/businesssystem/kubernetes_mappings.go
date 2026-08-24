package businesssystem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/config"
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

// CreateKubernetesConnectionMapping appends one Active mapping. The command
// ledger makes a network retry idempotent; the partial unique index keeps the
// current relationship singular without deleting its prior history.
func (service *Service) CreateKubernetesConnectionMapping(ctx context.Context, principalID int64, clientCommandID, systemKey string, connectionID int64) (KubernetesConnectionMapping, error) {
	commandDigest := auth.DigestCommand("business_system.kubernetes_connection.create", map[string]any{
		"systemKey": systemKey, "connectionId": connectionID,
	})
	record, found, err := auth.LookupCommand(ctx, service.db, principalID, clientCommandID)
	if err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if replayed, committed, replayErr := replayKubernetesMappingCommand(record, found, commandDigest); committed || replayErr != nil {
		return replayed, replayErr
	}

	conn, err := service.db.Conn(ctx)
	if err != nil {
		return KubernetesConnectionMapping{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return KubernetesConnectionMapping{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	record, found, err = auth.LookupCommandOn(ctx, conn, principalID, clientCommandID)
	if err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if replayed, alreadyCommitted, replayErr := replayKubernetesMappingCommand(record, found, commandDigest); replayErr != nil {
		return KubernetesConnectionMapping{}, replayErr
	} else if alreadyCommitted {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return KubernetesConnectionMapping{}, err
		}
		committed = true
		return replayed, nil
	}

	systemID, err := systemIDForKey(ctx, conn, systemKey)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return service.rejectKubernetesMapping(ctx, conn, principalID, clientCommandID, commandDigest, "business_system.kubernetes_connection.create", "business_system.kubernetes_connection.create", systemKey, 0, kubernetesMappingRejection{Code: "not_found", Detail: "目标业务系统不存在"}, &committed)
		}
		return KubernetesConnectionMapping{}, err
	}
	var connectionType string
	if err := conn.QueryRowContext(ctx, `SELECT type FROM connections WHERE id=?`, connectionID).Scan(&connectionType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.rejectKubernetesMapping(ctx, conn, principalID, clientCommandID, commandDigest, "business_system.kubernetes_connection.create", "business_system.kubernetes_connection.create", systemKey, systemID, kubernetesMappingRejection{Code: "not_found", Detail: "目标 Kubernetes 连接不存在", ObjectID: connectionID}, &committed)
		}
		return KubernetesConnectionMapping{}, err
	}
	if connectionType != "kubernetes" {
		return service.rejectKubernetesMapping(ctx, conn, principalID, clientCommandID, commandDigest, "business_system.kubernetes_connection.create", "business_system.kubernetes_connection.create", systemKey, systemID, kubernetesMappingRejection{Code: "invalid_connection_type", Detail: "目标连接不是 Kubernetes 连接", ObjectID: connectionID}, &committed)
	}

	now := service.nowText()
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO business_system_kubernetes_connections(business_system_id,connection_id,state,created_by,created_at)
		VALUES(?,?,'Active',?,?)`, systemID, connectionID, principalID, now)
	if err != nil {
		if strings.Contains(err.Error(), "ux_business_system_kubernetes_connection_active") || strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return service.rejectKubernetesMapping(ctx, conn, principalID, clientCommandID, commandDigest, "business_system.kubernetes_connection.create", "business_system.kubernetes_connection.create", systemKey, systemID, kubernetesMappingRejection{Code: "active_conflict", Detail: "该 Kubernetes 连接已映射到此业务系统", ObjectID: connectionID}, &committed)
		}
		return KubernetesConnectionMapping{}, err
	}
	mappingID, err := insert.LastInsertId()
	if err != nil {
		return KubernetesConnectionMapping{}, err
	}
	mapping, err := kubernetesConnectionMappingOn(ctx, conn, mappingID)
	if err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if err := service.audit(ctx, conn, principalID, "business_system.kubernetes_connection.create", systemID, now); err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if err := auth.RecordCommand(ctx, conn, principalID, clientCommandID, "business_system.kubernetes_connection.create", commandDigest, "committed", "business_system_kubernetes_connection", mappingID, encode(mapping)); err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return KubernetesConnectionMapping{}, err
	}
	committed = true
	return mapping, nil
}

// RetireKubernetesConnectionMapping closes an Active binding once. The schema
// trigger rejects every other update shape, while this command's row-version
// fence reports a stale Admin view before it can overwrite current state.
func (service *Service) RetireKubernetesConnectionMapping(ctx context.Context, principalID int64, clientCommandID, systemKey string, mappingID, expectedRowVersion int64) (KubernetesConnectionMapping, error) {
	commandDigest := auth.DigestCommand("business_system.kubernetes_connection.retire", map[string]any{
		"systemKey": systemKey, "mappingId": mappingID, "expectedRowVersion": expectedRowVersion,
	})
	record, found, err := auth.LookupCommand(ctx, service.db, principalID, clientCommandID)
	if err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if replayed, committed, replayErr := replayKubernetesMappingCommand(record, found, commandDigest); committed || replayErr != nil {
		return replayed, replayErr
	}

	conn, err := service.db.Conn(ctx)
	if err != nil {
		return KubernetesConnectionMapping{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return KubernetesConnectionMapping{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	record, found, err = auth.LookupCommandOn(ctx, conn, principalID, clientCommandID)
	if err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if replayed, alreadyCommitted, replayErr := replayKubernetesMappingCommand(record, found, commandDigest); replayErr != nil {
		return KubernetesConnectionMapping{}, replayErr
	} else if alreadyCommitted {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return KubernetesConnectionMapping{}, err
		}
		committed = true
		return replayed, nil
	}

	systemID, err := systemIDForKey(ctx, conn, systemKey)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return service.rejectKubernetesMapping(ctx, conn, principalID, clientCommandID, commandDigest, "business_system.kubernetes_connection.retire", "business_system.kubernetes_connection.retire", systemKey, 0, kubernetesMappingRejection{Code: "not_found", Detail: "目标业务系统不存在", ObjectID: mappingID}, &committed)
		}
		return KubernetesConnectionMapping{}, err
	}
	now := service.nowText()
	update, err := conn.ExecContext(ctx, `
		UPDATE business_system_kubernetes_connections
		SET state='Retired',row_version=row_version+1,retired_by=?,retired_at=?
		WHERE id=? AND business_system_id=? AND state='Active' AND row_version=?`,
		principalID, now, mappingID, systemID, expectedRowVersion)
	if err != nil {
		return KubernetesConnectionMapping{}, err
	}
	affected, err := update.RowsAffected()
	if err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if affected == 0 {
		// Never turn a mapping owned by another Business System into a stale
		// version detail for this command target.
		var exists int
		lookupErr := conn.QueryRowContext(ctx, `SELECT 1 FROM business_system_kubernetes_connections WHERE id=? AND business_system_id=?`, mappingID, systemID).Scan(&exists)
		if errors.Is(lookupErr, sql.ErrNoRows) {
			return service.rejectKubernetesMapping(ctx, conn, principalID, clientCommandID, commandDigest, "business_system.kubernetes_connection.retire", "business_system.kubernetes_connection.retire", systemKey, systemID, kubernetesMappingRejection{Code: "not_found", Detail: "目标 Kubernetes 映射不存在", ObjectID: mappingID}, &committed)
		}
		if lookupErr != nil {
			return KubernetesConnectionMapping{}, lookupErr
		}
		mapping, lookupErr := kubernetesConnectionMappingOn(ctx, conn, mappingID)
		if lookupErr != nil {
			return KubernetesConnectionMapping{}, lookupErr
		}
		return service.rejectKubernetesMapping(ctx, conn, principalID, clientCommandID, commandDigest, "business_system.kubernetes_connection.retire", "business_system.kubernetes_connection.retire", systemKey, systemID, kubernetesMappingRejection{Code: "row_version_conflict", Detail: "该 Kubernetes 映射已发生变化，请刷新后重试", ObjectID: mappingID, CurrentVersion: int64Pointer(mapping.RowVersion)}, &committed)
	}
	mapping, err := kubernetesConnectionMappingOn(ctx, conn, mappingID)
	if err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if err := service.audit(ctx, conn, principalID, "business_system.kubernetes_connection.retire", systemID, now); err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if err := auth.RecordCommand(ctx, conn, principalID, clientCommandID, "business_system.kubernetes_connection.retire", commandDigest, "committed", "business_system_kubernetes_connection", mappingID, encode(mapping)); err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return KubernetesConnectionMapping{}, err
	}
	committed = true
	return mapping, nil
}

func systemIDForKey(ctx context.Context, conn *sql.Conn, systemKey string) (int64, error) {
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

func kubernetesConnectionMappingOn(ctx context.Context, conn *sql.Conn, mappingID int64) (KubernetesConnectionMapping, error) {
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

func validationError(path, reason, remediation string) error {
	return &config.ValidationError{Errors: []config.FieldError{{Path: path, Reason: reason, Remediation: remediation}}}
}

func int64Pointer(value int64) *int64    { return &value }
func stringPointer(value string) *string { return &value }

// kubernetesMappingRejection is the durable, non-secret payload for every
// deterministic mapping-command rejection. It is deliberately separate from
// transport errors: only facts the service has fully determined are committed.
type kubernetesMappingRejection struct {
	Code           string `json:"code"`
	Detail         string `json:"detail"`
	ObjectID       int64  `json:"objectId,omitempty"`
	CurrentVersion *int64 `json:"currentVersion,omitempty"`
}

func (rejection kubernetesMappingRejection) asError(systemKey string) error {
	switch rejection.Code {
	case "not_found":
		return ErrNotFound
	case "active_conflict", "row_version_conflict":
		return &ConflictError{Code: rejection.Code, Detail: rejection.Detail, SystemKey: systemKey, ObjectID: rejection.ObjectID, CurrentVersion: rejection.CurrentVersion}
	case "invalid_connection_type":
		return validationError("connectionId", rejection.Detail, "请选择一个 Kubernetes 类型的连接")
	default:
		return fmt.Errorf("unknown persisted Kubernetes mapping rejection %q", rejection.Code)
	}
}

func replayKubernetesMappingCommand(record auth.CommandRecord, found bool, digest string) (KubernetesConnectionMapping, bool, error) {
	if !found {
		return KubernetesConnectionMapping{}, false, nil
	}
	if record.RequestDigest != digest {
		return KubernetesConnectionMapping{}, true, ErrCommandReused
	}
	if record.Outcome == auth.OutcomeRejectedKnown {
		var rejection kubernetesMappingRejection
		if err := decodeStored(record.ResultPayload, &rejection); err != nil || rejection.Code == "" {
			return KubernetesConnectionMapping{}, true, errors.New("rejected Kubernetes mapping command has no result")
		}
		return KubernetesConnectionMapping{}, true, rejection.asError("")
	}
	return replayCommandResult(record, found, digest, func(mapping KubernetesConnectionMapping) bool { return mapping.ID > 0 })
}

// rejectKubernetesMapping closes a known business rejection atomically with
// its replay ledger row and audit event. Infrastructure errors intentionally
// never pass through this helper.
func (service *Service) rejectKubernetesMapping(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, digest, commandType, action, systemKey string, systemID int64, rejection kubernetesMappingRejection, committed *bool) (KubernetesConnectionMapping, error) {
	payload := encode(rejection)
	if err := auth.RecordCommand(ctx, conn, principalID, clientCommandID, commandType, digest, auth.OutcomeRejectedKnown, "business_system_kubernetes_connection", rejection.ObjectID, payload); err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('user',?,?,'rejected','business_system',?,?)`, principalID, action, systemID, service.nowText()); err != nil {
		return KubernetesConnectionMapping{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return KubernetesConnectionMapping{}, err
	}
	*committed = true
	return KubernetesConnectionMapping{}, rejection.asError(systemKey)
}
