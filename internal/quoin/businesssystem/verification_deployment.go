package businesssystem

import (
	"context"
	"database/sql"
)

// RunDeploymentAcceptanceVerification creates the deployment_acceptance
// ConfigVerificationRun bound to a frozen Deployment Acceptance manifest item
// (VERIFY-CONFIG-001): it reuses the real PromQL/Journey execution machinery
// against the manifest-frozen current published config pointers instead of a
// draft. The invocation owns idempotency — one non-terminal run per manifest
// item — so there is no client command ledger here.
func (service *Service) RunDeploymentAcceptanceVerification(ctx context.Context, principalID, businessSystemID, versionID, contractVersionID, manifestItemID int64) (int64, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	// Idempotent per manifest item: one deployment_acceptance run per frozen
	// item, terminal or not — the invocation owns the retry semantics.
	var existing int64
	err = conn.QueryRowContext(ctx, `SELECT id FROM config_verification_runs
		WHERE purpose='deployment_acceptance' AND verification_manifest_item_id=? ORDER BY id LIMIT 1`, manifestItemID).Scan(&existing)
	if err == nil {
		return existing, service.commitOn(ctx, conn, &committed)
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	now := service.nowText()
	insert, err := conn.ExecContext(ctx, `INSERT INTO config_verification_runs(
		purpose,business_system_id,config_version_id,label_contract_version_id,verification_manifest_item_id,state,row_version,created_by,created_at)
		VALUES('deployment_acceptance',?,?,?,?,'Queued',1,?,?)`,
		businessSystemID, versionID, contractVersionID, manifestItemID, principalID, now)
	if err != nil {
		return 0, mapVerificationAbort(err)
	}
	runID, err := insert.LastInsertId()
	if err != nil {
		return 0, err
	}
	var browserCount int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM config_checks c JOIN config_plans p ON p.id=c.plan_id
		WHERE p.config_version_id=? AND c.kind='browser'`, versionID).Scan(&browserCount); err != nil {
		return 0, err
	}
	var promQLCount int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM config_checks c JOIN config_plans p ON p.id=c.plan_id
		WHERE p.config_version_id=? AND c.kind='promql'`, versionID).Scan(&promQLCount); err != nil {
		return 0, err
	}
	if promQLCount > 0 || browserCount > 0 {
		// Same discipline as prepublish: the parent transition and every
		// child insert share the creation transaction.
		if _, err := conn.ExecContext(ctx, `UPDATE config_verification_runs SET state='Running', evidence_at=?, row_version=2 WHERE id=? AND state='Queued'`, now, runID); err != nil {
			return 0, mapVerificationAbort(err)
		}
		if promQLCount > 0 {
			if _, err := createPromQLVerificationAttempts(ctx, conn, runID, versionID, contractVersionID, now); err != nil {
				return 0, err
			}
		}
		if browserCount > 0 {
			if _, err := createBrowserVerificationAttempts(ctx, conn, runID, versionID, contractVersionID, businessSystemID, now); err != nil {
				return 0, err
			}
			if _, err := admitNextJourneyChildOn(ctx, conn, now); err != nil {
				return 0, err
			}
		}
	} else {
		// The deterministic no-check completion path: traverse Running to
		// Passed; the frozen Passed trigger accepts the empty check set.
		if _, err := conn.ExecContext(ctx, `UPDATE config_verification_runs SET state='Running', evidence_at=?, row_version=2 WHERE id=? AND state='Queued'`, now, runID); err != nil {
			return 0, mapVerificationAbort(err)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE config_verification_runs SET state='Passed', row_version=3 WHERE id=? AND state='Running'`, runID); err != nil {
			return 0, mapVerificationAbort(err)
		}
	}
	if err := service.audit(ctx, conn, principalID, "config_verification.deployment_acceptance_run", runID, now); err != nil {
		return 0, err
	}
	if err := service.commitOn(ctx, conn, &committed); err != nil {
		return 0, err
	}
	return runID, nil
}

func (service *Service) commitOn(ctx context.Context, conn *sql.Conn, committed *bool) error {
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	*committed = true
	return nil
}
