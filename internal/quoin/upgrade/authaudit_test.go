package upgrade

// 认证审计阶段 1（docs/auth-audit-implementation-plan.md）迁移测试。前置库就是
// 规范变更前逐字节捕获的发布 schema（testdata/auth-audit-predecessor.sql），
// digest 断言即准入证明。所有断言都在真实约束与触发器下运行：管理员的归一在
// 规范重建（唯一 admin 唯一索引、admin 保护触发器）之前完成，历史行逐列保留。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	// Bootstrap registers the sha256 SQLite scalar required by the frozen schema
	// triggers. The test runs the released schema, not a hand-built facsimile.
	_ "github.com/Suknna/quoin/internal/quoin/bootstrap"
	_ "modernc.org/sqlite"
)

// The predecessor identity is pinned verbatim: it is the sha256 of the exact
// schema.sql bytes captured before the auth-audit canonical change. If either
// the fixture or the constant drifts, no operator is ever told a foreign
// database is the known predecessor.
const pinnedAuthAuditPredecessorDigest = "d67fc107b5f8a978eae6b609393f6ec5563978234dc5167da7c1d021315b6269"

func TestAuthAuditPredecessorIdentityIsPinned(t *testing.T) {
	if authAuditPredecessorSchemaDigest != pinnedAuthAuditPredecessorDigest {
		t.Fatalf("auth-audit predecessor digest drifted: %s", authAuditPredecessorSchemaDigest)
	}
	if authAuditMigrationID != "20260915_auth_audit_v1" {
		t.Fatalf("auth-audit migration id drifted: %s", authAuditMigrationID)
	}
}

func authAuditPredecessorFixture(t *testing.T) *sql.DB {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "auth-audit-predecessor.sql"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(schema)
	if actual := hex.EncodeToString(digest[:]); actual != authAuditPredecessorSchemaDigest {
		t.Fatalf("predecessor fixture digest=%s want=%s", actual, authAuditPredecessorSchemaDigest)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "auth-audit-predecessor.db")+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-01-01T00:00:00Z')`, authAuditPredecessorSchemaDigest); err != nil {
		t.Fatal(err)
	}
	return db
}

// predecessorAccount seeds a real predecessor user row (and, for enabled
// accounts, active sessions that satisfy the released issue trigger).
type predecessorAccount struct {
	id       int64
	username string
	role     string
	enabled  int
	sessions int
}

func seedPredecessorAccounts(t *testing.T, db *sql.DB, accounts []predecessorAccount) {
	t.Helper()
	const now = "2026-01-01T00:00:00Z"
	for _, account := range accounts {
		if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,enabled,password_phc,auth_revision,created_at,updated_at) VALUES(?,?,?,?,?,?,1,?,?)`,
			account.id, account.username, account.username+" display", account.role, account.enabled, "phc-"+account.username, now, now); err != nil {
			t.Fatalf("seed user %s: %v", account.username, err)
		}
		for i := 0; i < account.sessions; i++ {
			token := sha256.Sum256([]byte(account.username + "/" + string(rune('a'+i))))
			if _, err := db.Exec(`INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,?,?,?,?,?,?)`,
				account.id, token[:], 1, "fixture", now, now, now, now); err != nil {
				t.Fatalf("seed session for %s: %v", account.username, err)
			}
		}
	}
}

// seedAuthAuditUpgradeWindow materializes the operator state the operational
// gate demands on the predecessor schema: an active Upgrade maintenance whose
// only checklist item is Safe plus a succeeded upgrade backup inside the
// window. triggeredBy must be a real user id (released backups FK); every
// insert satisfies the released CHECK constraints.
func seedAuthAuditUpgradeWindow(t *testing.T, db *sql.DB, triggeredBy int64) int64 {
	t.Helper()
	const enteredAt = "2026-01-01T00:00:00Z"
	const backupAt = "2026-01-02T00:00:00Z"
	if _, err := db.Exec(`INSERT INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE maintenance_state SET active=1,reason='Upgrade',entered_at=?,entered_by_type='user',entered_by_id=?,row_version=row_version+1,exited_at=NULL,exited_by_type=NULL,exited_by_id=NULL WHERE id=1 AND active=0`, enteredAt, triggeredBy); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := db.QueryRow(`SELECT row_version FROM maintenance_state WHERE id=1`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?,'BackupPreflight','pre_upgrade_backup','Safe','backup_verified',?)`, revision, enteredAt); err != nil {
		t.Fatal(err)
	}
	digest := hex.EncodeToString(make([]byte, 32))
	if _, err := db.Exec(`INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,db_sha256,manifest_sha256,artifact_count,size_bytes,manifest_path,row_version,created_at,updated_at,started_at,completed_at,triggered_by) VALUES('succeeded','completed','upgrade','online',NULL,?,?,0,1234,'/backup/manifest.json',1,?,?,?,?,?)`, digest, digest, backupAt, backupAt, backupAt, backupAt, triggeredBy); err != nil {
		t.Fatal(err)
	}
	return revision
}

func assertSingleAdminEndState(t *testing.T, db *sql.DB, retained predecessorAccount, demoted []predecessorAccount, expectRevoked int64, expectRenamed bool) {
	t.Helper()
	role, username, enabled, password, initialized := "", "", -1, "", -1
	if err := db.QueryRow(`SELECT role,username,enabled,password_phc,initialized FROM users WHERE id=?`, retained.id).Scan(&role, &username, &enabled, &password, &initialized); err != nil {
		t.Fatal(err)
	}
	if role != "admin" || enabled != 1 {
		t.Fatalf("retained admin id=%d role=%s enabled=%d", retained.id, role, enabled)
	}
	wantUsername := retained.username
	if expectRenamed {
		wantUsername = "admin"
	}
	if username != wantUsername {
		t.Fatalf("retained admin username=%q want=%q", username, wantUsername)
	}
	if password != "phc-"+retained.username {
		t.Fatalf("retained admin password digest drifted: %q", password)
	}
	if initialized != 0 {
		t.Fatalf("migrated admin initialized=%d want=0 (supplementary onboarding required)", initialized)
	}
	for _, account := range demoted {
		role, username, enabled, initialized := "", "", -1, -1
		if err := db.QueryRow(`SELECT role,username,enabled,initialized FROM users WHERE id=?`, account.id).Scan(&role, &username, &enabled, &initialized); err != nil {
			t.Fatal(err)
		}
		if role != "operator" || username != account.username || enabled != account.enabled {
			t.Fatalf("demoted admin id=%d role=%s username=%q enabled=%d want operator/%s/%d", account.id, role, username, enabled, account.username, account.enabled)
		}
		if initialized != 0 {
			t.Fatalf("demoted admin id=%d initialized=%d want=0", account.id, initialized)
		}
	}
	var admins int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&admins); err != nil || admins != 1 {
		t.Fatalf("admin rows=%d err=%v", admins, err)
	}
	// 规范重建后所有迁移用户（含普通操作员）initialized=0：旧口令不发正式会话，
	// 必须走补初始化；空库引导语义绝不套用到迁移库。
	var uninitialized int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE initialized<>0`).Scan(&uninitialized); err != nil || uninitialized != 0 {
		t.Fatalf("users initialized<>0=%d err=%v", uninitialized, err)
	}
	var active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE revoked_at IS NULL`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("active sessions after migration=%d err=%v", active, err)
	}
	if expectRevoked > 0 {
		var total int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&total); err != nil || int64(total) != expectRevoked {
			t.Fatalf("sessions=%d want revoked=%d", total, expectRevoked)
		}
	}
}

func TestAuthAuditMigrationRetainsTheSingleAdmin(t *testing.T) {
	db := authAuditPredecessorFixture(t)
	retained := predecessorAccount{id: 3, username: "ops-admin", role: "admin", enabled: 1, sessions: 2}
	operator := predecessorAccount{id: 7, username: "case-operator", role: "operator", enabled: 1, sessions: 1}
	seedPredecessorAccounts(t, db, []predecessorAccount{retained, operator})
	seedAuthAuditUpgradeWindow(t, db, retained.id)
	result, err := MigrateWithOptions(context.Background(), db, Options{})
	if err != nil {
		t.Fatalf("single-admin migration rejected: %v", err)
	}
	if result.SchemaVersion != "v1" {
		t.Fatalf("schema version=%q", result.SchemaVersion)
	}
	assertSingleAdminEndState(t, db, retained, nil, 3, true)
	var ledger int
	if err := db.QueryRow(`SELECT COUNT(*) FROM migration_ledger WHERE migration_id=?`, authAuditMigrationID).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows=%d err=%v", ledger, err)
	}
	target := sha256.Sum256([]byte(gen.SchemaSQL))
	var stored string
	if err := db.QueryRow(`SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != hex.EncodeToString(target[:]) {
		t.Fatalf("schema digest=%s want %x", stored, target)
	}
	// 新引导容量为空：补初始化从零开始，不伪造历史。
	// install_credentials 仅保留历史迁移权威，运行期不再读写，故不在此断言。
	var flows, contacts, challenges int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM auth_flows),(SELECT COUNT(*) FROM user_contacts),(SELECT COUNT(*) FROM auth_challenges)`).Scan(&flows, &contacts, &challenges); err != nil {
		t.Fatal(err)
	}
	if flows != 0 || contacts != 0 || challenges != 0 {
		t.Fatalf("auth bootstrap capacity must start empty: flows=%d contacts=%d challenges=%d", flows, contacts, challenges)
	}
	// 审计保留单例已按旧数据保守值种子化：清理关闭，等待用户确认。
	var retentionMonths, cleanupEnabled int
	if err := db.QueryRow(`SELECT retention_months,cleanup_enabled FROM audit_retention WHERE id=1`).Scan(&retentionMonths, &cleanupEnabled); err != nil || retentionMonths != 6 || cleanupEnabled != 0 {
		t.Fatalf("audit retention singleton=%d/%d err=%v", retentionMonths, cleanupEnabled, err)
	}
	var permitActive int
	var cutoff string
	if err := db.QueryRow(`SELECT active,cutoff_at FROM audit_cleanup_permits WHERE id=1`).Scan(&permitActive, &cutoff); err != nil || permitActive != 0 {
		t.Fatalf("cleanup permit singleton active=%d cutoff=%q err=%v", permitActive, cutoff, err)
	}
	// AUTOINCREMENT 高水位与外键完整性保持迁移前事实。
	var sequence int
	if err := db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='users'`).Scan(&sequence); err != nil || sequence < 7 {
		t.Fatalf("users sequence=%d err=%v", sequence, err)
	}
	if rows, err := db.Query(`PRAGMA foreign_key_check`); err != nil {
		t.Fatal(err)
	} else if rows.Next() {
		rows.Close()
		t.Fatal("foreign key violation after migration")
	} else if err := rows.Err(); err != nil {
		t.Fatal(err)
	} else {
		rows.Close()
	}
}

func TestAuthAuditMigrationRequiresExplicitRetainedAdminSelection(t *testing.T) {
	db := authAuditPredecessorFixture(t)
	seedPredecessorAccounts(t, db, []predecessorAccount{
		{id: 1, username: "alice", role: "admin", enabled: 1, sessions: 1},
		{id: 2, username: "bob", role: "admin", enabled: 1, sessions: 1},
	})
	seedAuthAuditUpgradeWindow(t, db, 1)
	if _, err := MigrateWithOptions(context.Background(), db, Options{}); !errors.Is(err, ErrRetainedAdminSelectionRequired) {
		t.Fatalf("ambiguous admins err=%v", err)
	}
	// 拒绝必须原子：库保持前置状态，修正选项后可安全重试。
	var digest string
	if err := db.QueryRow(`SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&digest); err != nil || digest != authAuditPredecessorSchemaDigest {
		t.Fatalf("digest after rejection=%q err=%v", digest, err)
	}
	var admins, ledgerRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&admins); err != nil || admins != 2 {
		t.Fatalf("admin rows after rejection=%d err=%v", admins, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM migration_ledger`).Scan(&ledgerRows); err != nil || ledgerRows != 0 {
		t.Fatalf("ledger rows after rejection=%d err=%v", ledgerRows, err)
	}
	if _, err := MigrateWithOptions(context.Background(), db, Options{RetainedAdminID: 1}); err != nil {
		t.Fatalf("retry with explicit selection rejected: %v", err)
	}
	assertSingleAdminEndState(t, db, predecessorAccount{id: 1, username: "alice", role: "admin", enabled: 1}, []predecessorAccount{{id: 2, username: "bob", role: "admin", enabled: 1}}, 2, true)
}

func TestAuthAuditMigrationDemotesUnselectedAdminsPreservingEnabledState(t *testing.T) {
	db := authAuditPredecessorFixture(t)
	retained := predecessorAccount{id: 1, username: "alice", role: "admin", enabled: 1, sessions: 1}
	second := predecessorAccount{id: 2, username: "bob", role: "admin", enabled: 1, sessions: 2}
	disabled := predecessorAccount{id: 3, username: "carol", role: "admin", enabled: 0}
	seedPredecessorAccounts(t, db, []predecessorAccount{retained, second, disabled})
	seedAuthAuditUpgradeWindow(t, db, retained.id)
	result, err := MigrateWithOptions(context.Background(), db, Options{RetainedAdminID: 1})
	if err != nil {
		t.Fatalf("explicit-selection migration rejected: %v", err)
	}
	if len(result.DemotedAdminIDs) != 2 || result.DemotedAdminIDs[0] != 2 || result.DemotedAdminIDs[1] != 3 {
		t.Fatalf("demoted ids=%v want [2 3]", result.DemotedAdminIDs)
	}
	if result.RetainedAdminID != 1 || result.RevokedSessionCount != 3 {
		t.Fatalf("report=%+v", result)
	}
	assertSingleAdminEndState(t, db, retained, []predecessorAccount{second, disabled}, 3, true)
}

func TestAuthAuditMigrationRejectsSelectionThatIsNotAnEnabledAdmin(t *testing.T) {
	for _, tc := range []struct {
		name      string
		selection int64
	}{
		{"operator", 2},
		{"absent", 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := authAuditPredecessorFixture(t)
			seedPredecessorAccounts(t, db, []predecessorAccount{
				{id: 1, username: "alice", role: "admin", enabled: 1},
				{id: 2, username: "bob", role: "operator", enabled: 1},
			})
			seedAuthAuditUpgradeWindow(t, db, 1)
			if _, err := MigrateWithOptions(context.Background(), db, Options{RetainedAdminID: tc.selection}); !errors.Is(err, ErrRetainedAdminUnknown) {
				t.Fatalf("selection %d err=%v", tc.selection, err)
			}
			var digest string
			if err := db.QueryRow(`SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&digest); err != nil || digest != authAuditPredecessorSchemaDigest {
				t.Fatalf("digest after rejection=%q err=%v", digest, err)
			}
		})
	}
}

// 单 admin 且显式给出匹配 ID 的组合必须成功：显式选择与确定性保留同义。
func TestAuthAuditMigrationHonorsMatchingExplicitSelectionForSingleAdmin(t *testing.T) {
	db := authAuditPredecessorFixture(t)
	retained := predecessorAccount{id: 5, username: "admin", role: "admin", enabled: 1, sessions: 1}
	seedPredecessorAccounts(t, db, []predecessorAccount{retained})
	seedAuthAuditUpgradeWindow(t, db, retained.id)
	if _, err := MigrateWithOptions(context.Background(), db, Options{RetainedAdminID: 5}); err != nil {
		t.Fatalf("matching explicit selection rejected: %v", err)
	}
	// 登录名已是 admin，无需改名。
	assertSingleAdminEndState(t, db, retained, nil, 1, false)
}

func TestAuthAuditMigrationBlocksAdminUsernameConflictWithoutRenamingOthers(t *testing.T) {
	db := authAuditPredecessorFixture(t)
	seedPredecessorAccounts(t, db, []predecessorAccount{
		{id: 1, username: "alice", role: "admin", enabled: 1},
		{id: 2, username: "admin", role: "operator", enabled: 1},
	})
	seedAuthAuditUpgradeWindow(t, db, 1)
	if _, err := MigrateWithOptions(context.Background(), db, Options{RetainedAdminID: 1}); !errors.Is(err, ErrAdminUsernameConflict) {
		t.Fatalf("username conflict err=%v", err)
	}
	// 其他用户绝不被静默改名；库保持前置状态。
	var operatorName, role string
	if err := db.QueryRow(`SELECT username,role FROM users WHERE id=2`).Scan(&operatorName, &role); err != nil || operatorName != "admin" || role != "operator" {
		t.Fatalf("operator row mutated: username=%q role=%q err=%v", operatorName, role, err)
	}
	var adminName string
	if err := db.QueryRow(`SELECT username FROM users WHERE id=1`).Scan(&adminName); err != nil || adminName != "alice" {
		t.Fatalf("admin row mutated: %q err=%v", adminName, err)
	}
}

func TestAuthAuditMigrationRequiresAnAdministrator(t *testing.T) {
	t.Run("no-administrator", func(t *testing.T) {
		db := authAuditPredecessorFixture(t)
		// A non-admin account can own the backup row (released FK); the
		// migration must still refuse to fabricate the missing administrator.
		seedPredecessorAccounts(t, db, []predecessorAccount{
			{id: 1, username: "clerk", role: "operator", enabled: 1},
		})
		seedAuthAuditUpgradeWindow(t, db, 1)
		if _, err := MigrateWithOptions(context.Background(), db, Options{}); !errors.Is(err, ErrAdminMissing) {
			t.Fatalf("no-administrator err=%v", err)
		}
	})
	t.Run("only-disabled-admin", func(t *testing.T) {
		db := authAuditPredecessorFixture(t)
		seedPredecessorAccounts(t, db, []predecessorAccount{
			{id: 1, username: "sleepy", role: "admin", enabled: 0},
			{id: 2, username: "op", role: "operator", enabled: 1},
		})
		seedAuthAuditUpgradeWindow(t, db, 2)
		if _, err := MigrateWithOptions(context.Background(), db, Options{RetainedAdminID: 1}); !errors.Is(err, ErrAdminMissing) {
			t.Fatalf("disabled-only err=%v", err)
		}
	})
}

// Preflight 是旧镜像上的只读验证：管理员拓扑拒绝必须零写入。
func TestPreflightWithOptionsRejectsAmbiguousAdminTopologyWithoutWrites(t *testing.T) {
	db := authAuditPredecessorFixture(t)
	seedPredecessorAccounts(t, db, []predecessorAccount{
		{id: 1, username: "alice", role: "admin", enabled: 1},
		{id: 2, username: "bob", role: "admin", enabled: 1},
	})
	seedAuthAuditUpgradeWindow(t, db, 1)
	snap := func() string {
		t.Helper()
		var state string
		if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM migration_ledger)||'/'||(SELECT schema_digest FROM schema_state WHERE id=1)||'/'||(SELECT COUNT(*) FROM users WHERE role='admin')||'/'||(SELECT COUNT(*) FROM sessions WHERE revoked_at IS NULL)||'/'||(SELECT active FROM maintenance_state WHERE id=1)`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := snap()
	if _, err := PreflightWithOptions(context.Background(), db, Options{}); !errors.Is(err, ErrRetainedAdminSelectionRequired) {
		t.Fatalf("preflight err=%v", err)
	}
	if after := snap(); after != before {
		t.Fatalf("preflight mutated the database: before=%s after=%s", before, after)
	}
	// 显式选择修正后同一只读验证通过。
	if _, err := PreflightWithOptions(context.Background(), db, Options{RetainedAdminID: 2}); err != nil {
		t.Fatalf("preflight with selection rejected: %v", err)
	}
}

func TestPreflightWithOptionsAcceptsSingleAdminPredecessor(t *testing.T) {
	db := authAuditPredecessorFixture(t)
	seedPredecessorAccounts(t, db, []predecessorAccount{
		{id: 1, username: "upgrade-admin", role: "admin", enabled: 1},
		{id: 2, username: "op", role: "operator", enabled: 1},
	})
	seedAuthAuditUpgradeWindow(t, db, 1)
	result, err := PreflightWithOptions(context.Background(), db, Options{})
	if err != nil {
		t.Fatalf("single-admin preflight rejected: %v", err)
	}
	if result.SchemaVersion != "v1" || result.MigrationHistory != 0 {
		t.Fatalf("preflight result=%+v", result)
	}
	// 旧 API 包装仍然可用且等价。
	if _, err := Preflight(context.Background(), db); err != nil {
		t.Fatalf("legacy Preflight wrapper rejected: %v", err)
	}
}

// 前置库可能经不同历史发布路径到达本 digest；准入只接受真实迁移事实：
// 每一行都必须是已知迁移 ID 且携带其派生 digest，且不得已有本迁移行。
func TestAuthAuditPredecessorGateAdmitsOnlyAuthenticHistory(t *testing.T) {
	seedLedger := func(t *testing.T, rows ...[2]string) *sql.DB {
		t.Helper()
		db := authAuditPredecessorFixture(t)
		for _, row := range rows {
			if _, err := db.Exec(`INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`, row[0], row[1], "2026-01-01T00:00:00Z"); err != nil {
				t.Fatal(err)
			}
		}
		return db
	}
	authentic := func(id string) [2]string { return [2]string{id, migrationDigest(id)} }
	cases := []struct {
		name    string
		rows    [][2]string
		wantErr error
	}{
		{"empty", nil, nil},
		{"plugin-only", [][2]string{authentic(pluginRegistryMigrationID)}, nil},
		{"metrics-only", [][2]string{authentic(legacyMetricsBusinessMigrationID)}, nil},
		{"direct-and-declaration", [][2]string{authentic(directInvestigationMetricsMigrationID), authentic(declarationCutoverMigrationID)}, nil},
		{"multi-release-chain", [][2]string{authentic(directInvestigationMetricsMigrationID), authentic(declarationCutoverMigrationID), authentic(pluginRegistryMigrationID)}, nil},
		{"tampered-digest", [][2]string{{pluginRegistryMigrationID, digest64()}}, ErrSchemaHistoryPresent},
		{"unknown-id", [][2]string{{"20260101_future_migration", migrationDigest("20260101_future_migration")}}, ErrSchemaHistoryPresent},
		{"this-migration-present", [][2]string{authentic(authAuditMigrationID)}, ErrSchemaHistoryPresent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := seedLedger(t, tc.rows...)
			err := gateError(t, db)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("authentic history rejected: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err=%v want %v", err, tc.wantErr)
			}
		})
	}
}

// 旧前置发布（声明切换版）同样在规范重建前归一管理员：目标规范已带唯一 admin
// 索引，未归一的多管理员副本绝不允许静默失败或绕过。
func TestReleasedSchemaMigrationNormalizesAdminsBeforeCanonicalRebuild(t *testing.T) {
	newDeclarationDeployment := func(t *testing.T) (*sql.DB, predecessorAccount, predecessorAccount) {
		t.Helper()
		db := declarationCutoverFixture(t)
		retained := predecessorAccount{id: 1, username: "alice", role: "admin", enabled: 1, sessions: 1}
		second := predecessorAccount{id: 2, username: "bob", role: "admin", enabled: 1}
		seedPredecessorAccounts(t, db, []predecessorAccount{retained, second})
		seedAuthAuditUpgradeWindow(t, db, retained.id)
		return db, retained, second
	}
	t.Run("missing-selection-blocks", func(t *testing.T) {
		db, _, _ := newDeclarationDeployment(t)
		if _, err := MigrateWithOptions(context.Background(), db, Options{}); !errors.Is(err, ErrRetainedAdminSelectionRequired) {
			t.Fatalf("ambiguous admins err=%v", err)
		}
		var digest string
		if err := db.QueryRow(`SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&digest); err != nil || digest != declarationCutoverSchemaDigest {
			t.Fatalf("digest after rejection=%q err=%v", digest, err)
		}
	})
	t.Run("explicit-selection-normalizes", func(t *testing.T) {
		db, retained, second := newDeclarationDeployment(t)
		if _, err := MigrateWithOptions(context.Background(), db, Options{RetainedAdminID: retained.id}); err != nil {
			t.Fatalf("declaration migration rejected: %v", err)
		}
		assertSingleAdminEndState(t, db, retained, []predecessorAccount{second}, 1, true)
		var declarationLedger int
		if err := db.QueryRow(`SELECT COUNT(*) FROM migration_ledger WHERE migration_id=?`, declarationCutoverMigrationID).Scan(&declarationLedger); err != nil || declarationLedger != 1 {
			t.Fatalf("declaration ledger rows=%d err=%v", declarationLedger, err)
		}
	})
}
