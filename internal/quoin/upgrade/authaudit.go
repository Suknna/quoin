package upgrade

// 认证审计阶段 1（docs/auth-audit-implementation-plan.md）的发布转换：把逐字
// 节捕获于 testdata/auth-audit-predecessor.sql 的最后一个前置 schema 转换到
// 当前认证审计规范。转换分三步，全部位于同一独占事务：
//
//  1. 管理员归一（见 normalizeAdminTopologyOn，对所有前置发布统一执行）：
//     目标规范带 role='admin' 唯一部分索引与 admin 保护触发器，未归一的副本
//     要么撞索引要么静默违反不变量，因此归一必须发生在重建之前。
//  2. 规范重建（rebuildCanonicalSchema）：新增列（initialized 等）按目标
//     默认值落位——迁移后的全部用户 initialized=0，必须补初始化；认证引导
//     容量（auth_flows、user_contacts 等）保持空表，不伪造历史。
//  3. 审计保留单例种子：旧数据保守起步（cleanup_enabled=0），清理与保留期
//     由用户确认后开启；全新引导（bootstrap）才以启用态落库。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

const (
	// authAuditPredecessorSchemaDigest is the released pre-audit canonical
	// schema identity, pinned to the byte-exact capture in
	// testdata/auth-audit-predecessor.sql.
	authAuditPredecessorSchemaDigest = "d67fc107b5f8a978eae6b609393f6ec5563978234dc5167da7c1d021315b6269"
	authAuditMigrationID             = "20260915_auth_audit_v1"

	// unifiedAdminUsername is the single administrator's canonical login after
	// migration. Renaming applies only to the retained administrator; no other
	// account is ever renamed.
	unifiedAdminUsername = "admin"
)

// Options carries the operator decisions the migration cannot infer. The zero
// value is valid for every deterministic deployment.
type Options struct {
	// RetainedAdminID names the administrator kept when the predecessor holds
	// more than one enabled administrator. Zero means unspecified: rejected
	// whenever the choice is ambiguous, and cross-checked against the actual
	// administrators whenever present.
	RetainedAdminID int64
}

// Stable error codes surfaced by `quoin migrate` for the administrator
// topology; the deployment helper records them verbatim in its report.
var (
	// ErrAdminMissing blocks predecessors that cannot yield the enabled single
	// administrator the canonical schema guarantees: no administrator at all,
	// or every administrator disabled.
	ErrAdminMissing = errors.New("admin_missing")
	// ErrRetainedAdminSelectionRequired blocks migrations of predecessors with
	// several enabled administrators until the operator names the retained
	// stable id explicitly.
	ErrRetainedAdminSelectionRequired = errors.New("retained_admin_selection_required")
	// ErrRetainedAdminUnknown rejects a selection that is not one of the
	// enabled administrators.
	ErrRetainedAdminUnknown = errors.New("retained_admin_unknown")
	// ErrAdminUsernameConflict blocks the login unification when another
	// account already owns the unified username; the migration never renames
	// that account to make room.
	ErrAdminUsernameConflict = errors.New("admin_username_conflict")
)

// adminNormalization is the planned (and, after application, observed) single
// administrator consolidation of one predecessor database.
type adminNormalization struct {
	retainedID      int64
	renamed         bool
	demotedIDs      []int64
	revokedSessions int64
}

// planAdminNormalization validates the administrator topology read-only. It
// runs in PreflightWithOptions before the old image is retired and again under
// the exclusive migration transaction, so both surfaces fail on the same
// stable codes before any byte is rewritten.
func planAdminNormalization(ctx context.Context, conn *sql.Conn, options Options) (adminNormalization, error) {
	rows, err := conn.QueryContext(ctx, `SELECT id,username,enabled FROM users WHERE role='admin' ORDER BY id`)
	if err != nil {
		return adminNormalization{}, err
	}
	type administrator struct {
		id       int64
		username string
		enabled  int
	}
	var admins []administrator
	for rows.Next() {
		var admin administrator
		if err := rows.Scan(&admin.id, &admin.username, &admin.enabled); err != nil {
			rows.Close()
			return adminNormalization{}, err
		}
		admins = append(admins, admin)
	}
	if err := rows.Close(); err != nil {
		return adminNormalization{}, err
	}
	if len(admins) == 0 {
		return adminNormalization{}, fmt.Errorf("%w: predecessor has no administrator account and migration cannot fabricate one", ErrAdminMissing)
	}
	var enabled []administrator
	for _, admin := range admins {
		if admin.enabled == 1 {
			enabled = append(enabled, admin)
		}
	}
	if len(enabled) == 0 {
		return adminNormalization{}, fmt.Errorf("%w: every predecessor administrator is disabled; the retained administrator must stay enabled", ErrAdminMissing)
	}
	var retained administrator
	if len(enabled) == 1 {
		retained = enabled[0]
		if options.RetainedAdminID != 0 && options.RetainedAdminID != retained.id {
			return adminNormalization{}, fmt.Errorf("%w: retained id %d is not the single enabled administrator %d", ErrRetainedAdminUnknown, options.RetainedAdminID, retained.id)
		}
	} else {
		if options.RetainedAdminID == 0 {
			return adminNormalization{}, fmt.Errorf("%w: %d enabled administrators; pass the retained stable id explicitly", ErrRetainedAdminSelectionRequired, len(enabled))
		}
		found := false
		for _, admin := range enabled {
			if admin.id == options.RetainedAdminID {
				retained, found = admin, true
				break
			}
		}
		if !found {
			return adminNormalization{}, fmt.Errorf("%w: retained id %d is not one of the %d enabled administrators", ErrRetainedAdminUnknown, options.RetainedAdminID, len(enabled))
		}
	}
	plan := adminNormalization{retainedID: retained.id}
	if retained.username != unifiedAdminUsername {
		var conflicts int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE username=? AND id<>?`, unifiedAdminUsername, retained.id).Scan(&conflicts); err != nil {
			return plan, err
		}
		if conflicts != 0 {
			return plan, fmt.Errorf("%w: another account owns the unified login %q; rename it in the predecessor release first", ErrAdminUsernameConflict, unifiedAdminUsername)
		}
		plan.renamed = true
	}
	for _, admin := range admins {
		if admin.id != retained.id {
			plan.demotedIDs = append(plan.demotedIDs, admin.id)
		}
	}
	return plan, nil
}

// normalizeAdminTopologyOn applies the plan on the predecessor schema before
// the canonical rebuild creates the single-admin unique index and the
// admin-protecting triggers. The retained administrator keeps id, password and
// history; every other administrator becomes an operator preserving its
// enabled state; every still-active predecessor session is revoked because no
// migrated account may resume a pre-audit session. Released triggers shape the
// writes: a role change is a security change and must advance auth_revision
// exactly once, and row_version must increment by exactly one.
func normalizeAdminTopologyOn(ctx context.Context, conn *sql.Conn, options Options) (adminNormalization, error) {
	plan, err := planAdminNormalization(ctx, conn, options)
	if err != nil {
		return plan, err
	}
	now := migrationNow()
	if plan.renamed {
		if err := renameRetainedAdminLogin(ctx, conn, plan.retainedID, now); err != nil {
			return plan, err
		}
	}
	for _, id := range plan.demotedIDs {
		if _, err := conn.ExecContext(ctx, `UPDATE users SET role='operator',auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=? AND role='admin'`, now, id); err != nil {
			return plan, err
		}
	}
	result, err := conn.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE revoked_at IS NULL`, now)
	if err != nil {
		return plan, err
	}
	revoked, err := result.RowsAffected()
	if err != nil {
		return plan, err
	}
	plan.revokedSessions = revoked
	return plan, nil
}

// renameRetainedAdminLogin performs the one sanctioned rewrite of the released
// username-immutability invariant (trg_users_username_immutable, schema
// 12.22). The preflight conflict check already proved no other account owns
// the unified login; inside this exclusive transaction the trigger is lifted
// for exactly this single UPDATE and restored from its captured bytes before
// anything commits.
func renameRetainedAdminLogin(ctx context.Context, conn *sql.Conn, retainedID int64, now string) error {
	var triggerSQL sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT sql FROM main.sqlite_master WHERE type='trigger' AND name='trg_users_username_immutable'`).Scan(&triggerSQL); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if triggerSQL.Valid {
		if _, err := conn.ExecContext(ctx, `DROP TRIGGER main.trg_users_username_immutable`); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE users SET username=?,row_version=row_version+1,updated_at=? WHERE id=?`, unifiedAdminUsername, now, retainedID); err != nil {
		return err
	}
	if triggerSQL.Valid {
		if _, err := conn.ExecContext(ctx, triggerSQL.String); err != nil {
			return fmt.Errorf("restore username immutability: %w", err)
		}
	}
	return nil
}

// migrateAuthAuditOn converts the pre-audit predecessor. Beyond the shared
// pre-rebuild normalization the conversion is additive: new columns take their
// canonical defaults (migrated users re-onboard with initialized=0) and new
// auth capacity starts empty; only the retention singletons are seeded so the
// old database starts conservative (cleanup disabled) until the operator opts in.
func migrateAuthAuditOn(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	stored, report, err := beginReleasedSchemaMigration(ctx, conn, authAuditMigrationID, authAuditPredecessorSchemaDigest)
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	report.LegacySchemaDigest = stored
	if err := rebuildCanonicalSchema(ctx, conn, nil, false); err != nil {
		return report, err
	}
	if err := seedAuditRetentionSingletons(ctx, conn); err != nil {
		return report, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`, authAuditMigrationID, migrationDigest(authAuditMigrationID), migrationNow()); err != nil {
		return report, err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	if _, err := conn.ExecContext(ctx, `UPDATE schema_state SET schema_digest=?,upgraded_at=? WHERE id=1`, hex.EncodeToString(digest[:]), migrationNow()); err != nil {
		return report, err
	}
	return report, nil
}

// seedAuditRetentionSingletons plants both audit retention singletons with
// their conservative old-data values: six natural months, cleanup disabled,
// no permit. The fresh-bootstrap path (not this migration) is what enables
// cleanup from the start.
func seedAuditRetentionSingletons(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO audit_retention(id,retention_months,cleanup_enabled,row_version) VALUES(1,6,0,1)`); err != nil {
		return fmt.Errorf("seed audit retention singleton: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO audit_cleanup_permits(id,active,cutoff_at,upper_event_id,acquired_at) VALUES(1,0,'',0,'')`); err != nil {
		return fmt.Errorf("seed audit cleanup permit singleton: %w", err)
	}
	return nil
}
