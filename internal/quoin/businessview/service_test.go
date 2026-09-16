package businessview

// 业务视图命令面测试：共享执行器下的创建幂等/并发前提/整体提交更新/确定性
// 拒绝，以及自动审计、重放不重复成功、缺失执行上下文与会话证明失败关闭。

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/auth"
	_ "github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/testfixture"
	_ "modernc.org/sqlite"
)

// newTestSQLDB 打开 fixture 写库并在同一文件上建立真实只读池（与生产组合
// 一致：execution.OpenReadOnly 的 mode=ro + query_only，可写池绝不充当读源）。
// 两个池都由测试持有并在结束时关闭。
func newTestSQLDB(t *testing.T) (*sql.DB, execution.Reader) {
	t.Helper()
	dbPath := t.TempDir() + "/views.db"
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gen.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return db, reader
}

func newViewHarness(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	db, reader := newTestSQLDB(t)
	now := "2026-09-13T00:00:00Z"
	// 远期会话过期：fixture 会话在测试窗口内始终未过期，撤销/漂移用例再
	// 单独改变过期或撤销事实。
	idle, absolute := "2036-09-13T00:00:00Z", "2036-09-20T00:00:00Z"
	for _, statement := range []string{
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'fixture',1,'` + now + `','` + now + `')`,
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(2,'op','Op','operator',1,1,'fixture',1,'` + now + `','` + now + `')`,
		`INSERT INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,zeroblob(12),zeroblob(16),'` + now + `')`,
		`INSERT INTO connections(id,name,type,enabled,row_version,current_revision_id,current_credential_generation_id,created_at) VALUES(1,'fixture-metrics','thanos',1,1,NULL,NULL,'` + now + `')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,randomblob(32),1,'fixture','` + now + `','` + now + `','` + idle + `','` + absolute + `')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(2,2,randomblob(32),1,'fixture','` + now + `','` + now + `','` + idle + `','` + absolute + `')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	// 与生产组合一致（app.configureReadOnly）：真实只读池经共享执行器验证后
	// 装配服务；可写池绝不充当读源。
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	service, err := NewServiceWithReader(reader, runner)
	if err != nil {
		t.Fatal(err)
	}
	return service, db
}

// sessionContext 注入带会话证明引用的合法执行元数据。真实入口（HTTP 准入）
// 统一接线前，测试显式提供 actor、session 与 correlation——服务自身绝不
// 静默合成身份或退化为无会话复核。
func sessionContext(t *testing.T, principalID int64, session execution.SessionRef, correlation string) context.Context {
	t.Helper()
	return testfixture.UserContext(t, principalID, session, correlation)
}

// adminContext 是管理员（用户 1，会话 1，签发 revision 1）的上下文。
func adminContext(t *testing.T, correlation string) context.Context {
	t.Helper()
	return sessionContext(t, 1, execution.SessionRef{ID: 1, AuthRevision: 1}, correlation)
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// assertNothingPersisted 钳制干净拒绝：业务、台账、审计都没有痕迹。
func assertNothingPersisted(t *testing.T, db *sql.DB) {
	t.Helper()
	for name, query := range map[string]string{
		"business_views":  `SELECT COUNT(*) FROM business_views`,
		"client_commands": `SELECT COUNT(*) FROM client_commands`,
		"audit_events":    `SELECT COUNT(*) FROM audit_events`,
	} {
		if got := countRows(t, db, query); got != 0 {
			t.Fatalf("%s rows after clean rejection = %d, want 0", name, got)
		}
	}
}

func TestBusinessViewCreateUpdateRead(t *testing.T) {
	service, db := newViewHarness(t)
	ctx := adminContext(t, "corr-create-read")
	view, err := service.CreateView(ctx, 1, "create-view-0001", ViewInput{
		ViewKey: "checkout-prod", DisplayName: "结算生产", Description: "结算域生产候选",
		ConnectionName: "fixture-metrics", LabelConditions: map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.Scope.ConnectionName != "fixture-metrics" || view.Scope.LabelConditions["env"] != "prod" || view.RowVersion != 1 {
		t.Fatalf("created view = %+v", view)
	}
	// key 退役不复用。
	if _, err := service.CreateView(ctx, 1, "create-view-0002", ViewInput{ViewKey: "checkout-prod", DisplayName: "重复"}); err == nil {
		t.Fatal("duplicate key must be rejected")
	}
	updated, err := service.UpdateView(ctx, 1, "update-view-0001", ViewInput{
		ViewKey: "checkout-prod", DisplayName: "结算生产 v2", ConnectionName: "fixture-metrics",
	}, view.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RowVersion != 2 || updated.DisplayName != "结算生产 v2" {
		t.Fatalf("updated view = %+v", updated)
	}
	if _, err := service.UpdateView(ctx, 1, "update-view-0002", ViewInput{ViewKey: "checkout-prod", DisplayName: "并发"}, view.RowVersion); err == nil {
		t.Fatal("stale row version must conflict")
	}
	if _, err := service.CreateView(ctx, 1, "create-view-0003", ViewInput{ViewKey: "BAD_KEY", DisplayName: "非法"}); err == nil {
		t.Fatal("malformed key must be rejected")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM business_views WHERE view_key='BAD_KEY'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("rejected creation must not persist")
	}
	list, err := service.ListViews(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	// 跨来源候选集合：connection 可空。
	if _, err := service.CreateView(ctx, 1, "create-view-0004", ViewInput{ViewKey: "all-sources", DisplayName: "全部来源"}); err != nil {
		t.Fatal(err)
	}
	cross, err := service.GetView(ctx, "all-sources")
	if err != nil || cross.Scope.ConnectionName != "" {
		t.Fatalf("cross-source view = %+v err=%v", cross, err)
	}
	var rejection *ConflictError
	if _, err := service.GetView(ctx, "missing"); !errors.As(err, &rejection) || rejection.Code != "not_found" {
		t.Fatalf("missing view must surface its typed rejection, got %v", err)
	}
}

// 自动审计：成功命令同事务产生一条 execute 审计与目标引用；幂等重放返回
// 原结果与原关联且不新增审计；换摘要重用命令键确定性冲突且不留新痕迹。
func TestBusinessViewAutomaticAuditAndReplay(t *testing.T) {
	service, db := newViewHarness(t)
	ctx := adminContext(t, "corr-audit-0001")
	input := ViewInput{
		ViewKey: "checkout-prod", DisplayName: "结算生产",
		ConnectionName: "fixture-metrics", LabelConditions: map[string]string{"env": "prod"},
	}
	view, err := service.CreateView(ctx, 1, "create-audit-0001", input)
	if err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events`); got != 1 {
		t.Fatalf("audit events after create = %d, want 1", got)
	}
	var (
		actorType, action, outcome, phase, commandID, correlation string
		actorID, refID                                            int64
		refType, initiatorType                                    string
		initiatorID                                               int64
	)
	if err := db.QueryRow(`SELECT actor_type,actor_id,action,outcome,phase,client_command_id,correlation_id,domain_ref_type,domain_ref_id,initiator_type,initiator_id FROM audit_events`).Scan(
		&actorType, &actorID, &action, &outcome, &phase, &commandID, &correlation, &refType, &refID, &initiatorType, &initiatorID); err != nil {
		t.Fatal(err)
	}
	if actorType != "user" || actorID != 1 || action != CommandCreate || outcome != "success" ||
		phase != "execute" || commandID != "create-audit-0001" || correlation != "corr-audit-0001" ||
		refType != ObjectBusinessView || refID != view.viewID || initiatorType != "user" || initiatorID != 1 {
		t.Fatalf("audit row = actor %s/%d action=%s outcome=%s phase=%s cmd=%s corr=%s ref=%s/%d initiator %s/%d",
			actorType, actorID, action, outcome, phase, commandID, correlation, refType, refID, initiatorType, initiatorID)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_event_targets WHERE target_type=? AND target_id=?`, ObjectBusinessView, view.viewID); got != 1 {
		t.Fatalf("audit targets = %d, want 1", got)
	}

	// 幂等重放：同一 (principal, client_command_id) 与同一请求摘要返回原结果，
	// 不产生新的业务成功审计。
	replayed, err := service.CreateView(ctx, 1, "create-audit-0001", input)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ViewKey != view.ViewKey || replayed.RowVersion != view.RowVersion {
		t.Fatalf("replayed view = %+v, want %+v", replayed, view)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events`); got != 1 {
		t.Fatalf("audit events after replay = %d, want 1 (no duplicate success)", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM business_views`); got != 1 {
		t.Fatalf("business views after replay = %d, want 1", got)
	}

	// 命令键重用（同 ID 不同请求）：确定性拒绝，且不新增持久化痕迹。
	var reuseConflict *ConflictError
	if _, err := service.CreateView(ctx, 1, "create-audit-0001", ViewInput{ViewKey: "other-view", DisplayName: "其它"}); !errors.As(err, &reuseConflict) || reuseConflict.Code != "command_reused" {
		t.Fatalf("reused command id must surface command_reused, got %v", err)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events`); got != 1 {
		t.Fatalf("audit events after reuse conflict = %d, want 1", got)
	}

	// 确定性业务拒绝：拒绝事实与命令台账持久化，业务修改不落库。
	if _, err := service.CreateView(ctx, 1, "create-audit-0002", input); err == nil {
		t.Fatal("duplicate key must be rejected")
	}
	var rejection *ConflictError
	if _, err := service.CreateView(ctx, 1, "create-audit-0002", input); !errors.As(err, &rejection) || rejection.Code != "view_exists" {
		t.Fatalf("replayed rejection must surface view_exists, got %v", err)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM business_views`); got != 1 {
		t.Fatalf("rejected creation persisted a view: %d", got)
	}
	var ledgerOutcome string
	if err := db.QueryRow(`SELECT outcome FROM client_commands WHERE client_command_id='create-audit-0002'`).Scan(&ledgerOutcome); err != nil {
		t.Fatal(err)
	}
	if ledgerOutcome != "rejected_known" {
		t.Fatalf("rejection ledger outcome = %q", ledgerOutcome)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events WHERE outcome='rejected'`); got != 1 {
		t.Fatalf("rejected audit events = %d, want 1", got)
	}
}

// 并发前提拒绝：过期 row_version 的更新以拒绝事实持久化，业务行保持原状；
// 重放同一拒绝命令返回同一结果且不重复审计。
func TestBusinessViewUpdateConflictRecordsRejectionOnce(t *testing.T) {
	service, db := newViewHarness(t)
	ctx := adminContext(t, "corr-conflict-0001")
	view, err := service.CreateView(ctx, 1, "create-conflict-0001", ViewInput{ViewKey: "checkout-prod", DisplayName: "结算生产"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateView(ctx, 1, "update-conflict-0000", ViewInput{ViewKey: "checkout-prod", DisplayName: "结算生产 v2"}, view.RowVersion); err != nil {
		t.Fatal(err)
	}
	staleInput := ViewInput{ViewKey: "checkout-prod", DisplayName: "并发写"}
	var conflict *ConflictError
	if _, err := service.UpdateView(ctx, 1, "update-conflict-0001", staleInput, view.RowVersion); !errors.As(err, &conflict) || conflict.Code != "row_version_conflict" {
		t.Fatalf("stale update must surface row_version_conflict, got %v", err)
	}
	if got := countRows(t, db, `SELECT row_version FROM business_views WHERE view_key='checkout-prod'`); got != 2 {
		t.Fatalf("row_version after rejected update = %d, want 2", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events WHERE outcome='rejected' AND action=?`, CommandUpdate); got != 1 {
		t.Fatalf("rejected update audit events = %d, want 1", got)
	}
	// 重放被拒绝的命令：从台账返回同一拒绝，不再新增审计行。
	if _, err := service.UpdateView(ctx, 1, "update-conflict-0001", staleInput, view.RowVersion); err == nil {
		t.Fatal("replayed rejection must stay rejected")
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM audit_events`); got != 3 {
		t.Fatalf("audit events after rejection replay = %d, want 3 (two successes, one rejected)", got)
	}
}

// 失败关闭：缺失执行元数据（真实入口尚未接线时的集成缺口）必须拒绝命令，
// 不合成系统身份、不写台账、不写审计。
func TestBusinessViewCommandsWithoutMetadataFailClosed(t *testing.T) {
	service, db := newViewHarness(t)
	ctx := context.Background()
	if _, err := service.CreateView(ctx, 1, "create-no-meta", ViewInput{ViewKey: "checkout-prod", DisplayName: "结算生产"}); !errors.Is(err, execution.ErrMissingContext) {
		t.Fatalf("missing metadata must fail with ErrMissingContext, got %v", err)
	}
	if _, err := service.UpdateView(ctx, 1, "update-no-meta", ViewInput{ViewKey: "checkout-prod", DisplayName: "结算生产"}, 1); !errors.Is(err, execution.ErrMissingContext) {
		t.Fatalf("missing metadata must fail with ErrMissingContext, got %v", err)
	}
	assertNothingPersisted(t, db)
}

// 会话复核（auth.VerifyExecutionSession，要求 admin）：非管理员主体即使
// 持有自身的有效会话也被干净拒绝，不产生任何业务、台账或审计痕迹。
func TestBusinessViewAuthorizeRejectsNonAdmin(t *testing.T) {
	service, db := newViewHarness(t)
	ctx := sessionContext(t, 2, execution.SessionRef{ID: 2, AuthRevision: 1}, "corr-operator-0001")
	if _, err := service.CreateView(ctx, 2, "create-operator-0001", ViewInput{ViewKey: "checkout-prod", DisplayName: "结算生产"}); !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("operator session must surface ErrActorChanged, got %v", err)
	}
	assertNothingPersisted(t, db)
}

// 撤销竞态封闭：准入之后会话被撤销的命令在事务内复核时失败，不落任何痕迹。
func TestBusinessViewRevokedSessionClosesAdmissionRace(t *testing.T) {
	service, db := newViewHarness(t)
	if _, err := db.Exec(`UPDATE sessions SET revoked_at='2026-09-13T01:00:00Z' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	ctx := adminContext(t, "corr-revoked-0001")
	if _, err := service.CreateView(ctx, 1, "create-revoked-0001", ViewInput{ViewKey: "checkout-prod", DisplayName: "结算生产"}); !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("revoked session must surface ErrActorChanged, got %v", err)
	}
	assertNothingPersisted(t, db)
}

// 会话证明引用缺失（例如伪造或老版本入口未携带）不得退化为仅查 users 行：
// 一律 ErrActorChanged，不产生任何痕迹。
func TestBusinessViewCommandsWithoutSessionProofFailClosed(t *testing.T) {
	service, db := newViewHarness(t)
	ctx := sessionContext(t, 1, execution.SessionRef{}, "corr-no-session-0001")
	if _, err := service.CreateView(ctx, 1, "create-no-session-0001", ViewInput{ViewKey: "checkout-prod", DisplayName: "结算生产"}); !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("missing session proof must surface ErrActorChanged, got %v", err)
	}
	assertNothingPersisted(t, db)
}

// 口令轮换推进 auth_revision 后，携带旧 revision 的会话证明立即失效。
// （schema 触发器要求 auth_revision 变更伴随真实的用户安全变更与
// row_version 前进——与真实改密事务同一形状。）
func TestBusinessViewAuthRevisionDriftClosesRace(t *testing.T) {
	service, db := newViewHarness(t)
	if _, err := db.Exec(`UPDATE users SET password_phc='rotated',auth_revision=auth_revision+1,row_version=row_version+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	ctx := adminContext(t, "corr-drift-0001")
	if _, err := service.CreateView(ctx, 1, "create-drift-0001", ViewInput{ViewKey: "checkout-prod", DisplayName: "结算生产"}); !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("stale auth revision must surface ErrActorChanged, got %v", err)
	}
	assertNothingPersisted(t, db)
}

// 过期会话（空闲过期窗口已过）即使未显式撤销也被拒绝。活动窗口触发器禁止
// 把既有会话的过期时间回拨，故用替换行的方式构造过期事实。
func TestBusinessViewExpiredSessionIsRejected(t *testing.T) {
	service, db := newViewHarness(t)
	for _, statement := range []string{
		`DELETE FROM sessions WHERE id=1`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,randomblob(32),1,'fixture','2026-09-13T00:00:00Z','2026-09-12T00:00:00Z','2026-09-12T12:00:00Z','2036-09-20T00:00:00Z')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	ctx := adminContext(t, "corr-expired-0001")
	if _, err := service.CreateView(ctx, 1, "create-expired-0001", ViewInput{ViewKey: "checkout-prod", DisplayName: "结算生产"}); !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("expired session must surface ErrActorChanged, got %v", err)
	}
	assertNothingPersisted(t, db)
}

// writableAdapter 是任意实现 audit.Reader 的适配器：包着可写连接的读形状，
// 正是注入边界必须结构性拒绝的第二条写通道。
type writableAdapter struct{ db *sql.DB }

func (a writableAdapter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return a.db.QueryContext(ctx, query, args...)
}

func (a writableAdapter) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return a.db.QueryRowContext(ctx, query, args...)
}

// 默认构造（应用初始装配，configureReadOnly 之前）绝不把可写 db 当作读源：
// 读路径保持零值可信只读面，所有读取失败关闭；构造本身不 panic。
func TestDefaultServiceReadsFailClosed(t *testing.T) {
	_, db := newViewHarness(t)
	service := NewService(db)
	if _, err := service.ListViews(context.Background()); err == nil {
		t.Fatal("unwired default service must fail closed on reads")
	}
	if _, err := service.GetView(context.Background(), "any"); err == nil {
		t.Fatal("unwired default service must fail closed on single reads")
	}
}

// 读能力注入只接受 execution.OpenReadOnly 产生的可信类型：裸可写 db、任意
// audit.Reader 适配器与 nil 一并被拒绝，不存在可写兼容通道；可信读源装配
// 后读路径真实可用。
func TestReaderInjectionAcceptsOnlyTrustedReadOnly(t *testing.T) {
	db, reader := newTestSQLDB(t)
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	if _, err := NewServiceWithReader(db, runner); err == nil {
		t.Fatal("raw writable database must not be accepted as a reader")
	}
	if _, err := NewServiceWithReader(nil, runner); err == nil {
		t.Fatal("nil reader must be rejected")
	}
	if _, err := NewServiceWithReader(writableAdapter{db: db}, runner); err == nil {
		t.Fatal("arbitrary audit.Reader adapters must not be accepted as a reader")
	}
	if _, err := NewServiceWithReader(reader, runner); err != nil {
		t.Fatalf("trusted OpenReadOnly reader must be accepted: %v", err)
	}
}

// SetReader 把真实只读能力转发给共享执行器验证并采纳其读面：默认构造的
// 服务注入前读失败关闭，注入后读路径真实可用（空 schema 上查询成功、缺失
// 视图返回类型化 not_found）；可写 db 仍被拒绝。
func TestSetReaderWiresReadOnlyCapability(t *testing.T) {
	db, reader := newTestSQLDB(t)
	service := NewService(db)
	if err := service.SetReader(db); err == nil {
		t.Fatal("writable db must not be accepted as reader")
	}
	if _, err := service.ListViews(context.Background()); err == nil {
		t.Fatal("service must stay fail closed before the reader is wired")
	}
	if err := service.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	views, err := service.ListViews(context.Background())
	if err != nil || len(views) != 0 {
		t.Fatalf("wired empty list = %+v err=%v", views, err)
	}
	var rejection *ConflictError
	if _, err := service.GetView(context.Background(), "missing"); !errors.As(err, &rejection) || rejection.Code != "not_found" {
		t.Fatalf("wired missing view must surface its typed rejection, got %v", err)
	}
}

// seedAlertSourceForView 在 fixture 库里插入一个告警源，供 alertSourceKeys
// 引用校验使用。
func seedAlertSourceForView(t *testing.T, db *sql.DB, sourceKey string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,row_version,created_at) VALUES(?,'alertmanager',1,1,'2026-09-13T00:00:00Z')`, sourceKey); err != nil {
		t.Fatal(err)
	}
}

// 告警归属来源约束：alertSourceKeys 显式声明 AM 告警源（与 Prom connection
// 身份严格区分）；非空时必须至少一个精确标签条件且逐 key 校验存在；空
// alertSourceKeys 是合法普通视图（不参与告警归属），不破坏原保存行为。
func TestBusinessViewAlertSourceKeysScope(t *testing.T) {
	service, db := newViewHarness(t)
	ctx := adminContext(t, "corr-alert-scope-0001")
	seedAlertSourceForView(t, db, "am-prod")
	seedAlertSourceForView(t, db, "am-edge")

	view, err := service.CreateView(ctx, 1, "create-scope-0001", ViewInput{
		ViewKey: "payments-prod", DisplayName: "支付生产",
		AlertSourceKeys: []string{"am-edge", "am-prod", "am-prod"},
		LabelConditions: map[string]string{"service": "payments"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 去重且稳定序存储，投影回读一致。
	got, err := service.GetView(ctx, "payments-prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Scope.AlertSourceKeys) != 2 || got.Scope.AlertSourceKeys[0] != "am-edge" || got.Scope.AlertSourceKeys[1] != "am-prod" {
		t.Fatalf("alert source keys must dedupe and sort, got %+v", got.Scope.AlertSourceKeys)
	}
	if got.Scope.LabelConditions["service"] != "payments" || view.RowVersion != 1 {
		t.Fatalf("scoped view = %+v", got)
	}

	// 普通（非告警）视图保持原语义：空 alertSourceKeys + 空标签条件可保存。
	if _, err := service.CreateView(ctx, 1, "create-scope-0002", ViewInput{ViewKey: "plain-view", DisplayName: "普通视图"}); err != nil {
		t.Fatalf("plain view without alert scope must still save: %v", err)
	}

	// 参与告警归属必须有标签条件：拒绝空标签兜底视图。
	var rejection *ConflictError
	if _, err := service.CreateView(ctx, 1, "create-scope-0003", ViewInput{
		ViewKey: "catch-all", DisplayName: "兜底", AlertSourceKeys: []string{"am-prod"},
	}); !errors.As(err, &rejection) || rejection.Code != "malformed_scope" {
		t.Fatalf("scoped view without label conditions must be rejected, got %v", err)
	}

	// 未知告警源 key 拒绝；不存在“跨身份”兜底。
	if _, err := service.CreateView(ctx, 1, "create-scope-0004", ViewInput{
		ViewKey: "ghost-scope", DisplayName: "幽灵", AlertSourceKeys: []string{"no-such-source"},
		LabelConditions: map[string]string{"service": "payments"},
	}); !errors.As(err, &rejection) || rejection.Code != "unknown_alert_source" {
		t.Fatalf("unknown alert source key must be rejected, got %v", err)
	}

	// 更新可整体改来源约束；row_version 前提不变。
	updated, err := service.UpdateView(ctx, 1, "update-scope-0001", ViewInput{
		ViewKey: "payments-prod", DisplayName: "支付生产 v2",
		AlertSourceKeys: []string{"am-prod"}, LabelConditions: map[string]string{"service": "payments", "env": "prod"},
	}, got.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Scope.AlertSourceKeys) != 1 || updated.Scope.AlertSourceKeys[0] != "am-prod" || updated.RowVersion != 2 {
		t.Fatalf("updated scoped view = %+v", updated)
	}

	// 连接身份与告警源身份互不替换：connectionName 不进 alertSourceKeys。
	if _, err := service.CreateView(ctx, 1, "create-scope-0005", ViewInput{
		ViewKey: "mixed-scope", DisplayName: "混合", ConnectionName: "fixture-metrics",
		AlertSourceKeys: []string{"am-prod"}, LabelConditions: map[string]string{"service": "payments"},
	}); err != nil {
		t.Fatal(err)
	}
	mixed, err := service.GetView(ctx, "mixed-scope")
	if err != nil || mixed.Scope.ConnectionName != "fixture-metrics" || len(mixed.Scope.AlertSourceKeys) != 1 {
		t.Fatalf("connection and alert-source scopes must stay separate: %+v err=%v", mixed.Scope, err)
	}
}
