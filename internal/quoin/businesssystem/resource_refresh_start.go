package businesssystem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

// StartResourceRefresh creates one complete durable discovery work graph for an
// immediate or scheduled cycle. The active-run and scheduled-run indexes are the
// cross-process dedupe authority; callers may safely retry the same command.
// The root, its children, snapshots, and grants commit atomically, so scheduler
// dispatch always has real Queued children to pick up.
func (service *Service) StartResourceRefresh(ctx context.Context, principalID int64, clientCommandID, systemKey, triggerKind string, scheduledFor *string) (ResourceRefreshRunDetail, error) {
	if triggerKind != "manual" && triggerKind != "schedule" {
		return ResourceRefreshRunDetail{}, fmt.Errorf("invalid resource refresh trigger kind %q", triggerKind)
	}
	if (triggerKind == "manual") != (scheduledFor == nil) {
		return ResourceRefreshRunDetail{}, fmt.Errorf("resource refresh trigger and scheduled time disagree")
	}
	digest := auth.DigestCommand("business_system.resource_refresh.start", map[string]any{"systemKey": systemKey, "triggerKind": triggerKind, "scheduledFor": scheduledFor})
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	// Check maintenance after owning the writer lock, so an upgrade fence and a
	// scheduler tick cannot both admit new discovery work.
	var maintenanceActive int
	// Older databases may not materialize their singleton until upgrade work
	// begins; an absent row means normal operation, while an explicit active row
	// always fences admission.
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE((SELECT active FROM maintenance_state WHERE id=1),0)`).Scan(&maintenanceActive); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	if maintenanceActive != 0 {
		return ResourceRefreshRunDetail{}, fmt.Errorf("resource refresh admission is blocked by maintenance")
	}
	if record, found, err := auth.LookupCommandOn(ctx, conn, principalID, clientCommandID); err != nil {
		return ResourceRefreshRunDetail{}, err
	} else if found {
		if record.RequestDigest != digest {
			return ResourceRefreshRunDetail{}, ErrCommandReused
		}
		var detail ResourceRefreshRunDetail
		if err := decodeStored(record.ResultPayload, &detail); err != nil {
			return ResourceRefreshRunDetail{}, err
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return ResourceRefreshRunDetail{}, err
		}
		committed = true
		return detail, nil
	}
	var systemID, versionID int64
	var contractID sql.NullInt64
	err = conn.QueryRowContext(ctx, `SELECT b.id,v.id,v.label_contract_version_id FROM business_systems b JOIN business_system_config_versions v ON v.id=b.current_config_version_id WHERE b.key=? AND b.enabled=1 AND v.state='published'`, systemKey).Scan(&systemID, &versionID, &contractID)
	if errors.Is(err, sql.ErrNoRows) {
		return ResourceRefreshRunDetail{}, ErrNotFound
	}
	if err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	// Scheduler uses system actor 0, which is intentionally not a users row.
	// The nullable creator column records that distinction without fabricating a
	// principal merely to satisfy a foreign key.
	var createdBy any = principalID
	if principalID == 0 {
		createdBy = nil
	}
	result, err := conn.ExecContext(ctx, `INSERT INTO resource_refresh_runs(business_system_id,config_version_id,label_contract_version_id,trigger_kind,scheduled_for,state,row_version,created_by,created_at) VALUES(?,?,?,?,?,'Queued',1,?,?)`, systemID, versionID, nullableContractID(contractID), triggerKind, scheduledFor, createdBy, service.nowText())
	if err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	runID, _ := result.LastInsertId()
	now := service.nowText()
	// The scope trigger permits children only beneath a Running root. Creating all
	// children before commit guarantees the dispatcher cannot observe an orphaned
	// root that would otherwise remain Running forever.
	if _, err = conn.ExecContext(ctx, `UPDATE resource_refresh_runs SET state='Running',evidence_at=?,row_version=2 WHERE id=?`, now, runID); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	if err := createResourceRefreshAttempts(ctx, conn, runID, versionID, now); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	state, rowVersion := "Running", int64(2)
	// A declaration with no discoveries is accurately complete without runtime
	// dispatch. It has no child work and therefore satisfies the terminal fence.
	if _, err = conn.ExecContext(ctx, `UPDATE resource_refresh_runs SET state='Completed',row_version=3 WHERE id=? AND NOT EXISTS (SELECT 1 FROM execution_attempts WHERE scope_type='resource_refresh_run' AND scope_id=?)`, runID, runID); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	var childCount int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE scope_type='resource_refresh_run' AND scope_id=?`, runID).Scan(&childCount); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	if childCount == 0 {
		state, rowVersion = "Completed", 3
	}
	detail := ResourceRefreshRunDetail{ID: fmt.Sprint(runID), BusinessSystemID: fmt.Sprint(systemID), ConfigVersionID: fmt.Sprint(versionID), TriggerKind: triggerKind, State: state, RowVersion: rowVersion, CreatedAt: now, EvidenceAt: &now}
	if contractID.Valid {
		detail.LabelContractVersionID = fmt.Sprint(contractID.Int64)
	}
	if err = auth.RecordCommand(ctx, conn, principalID, clientCommandID, "business_system.resource_refresh.start", digest, "committed", "resource_refresh_run", runID, encode(detail)); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	committed = true
	return detail, nil
}
func nullableContractID(id sql.NullInt64) any {
	if id.Valid {
		return id.Int64
	}
	return nil
}
