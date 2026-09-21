package app

// sensitive=1 Artifact 下载的会话围栏测试（CONTEXT「敏感内容下载」：活动
// 流绑定该 Session，撤销、禁用或降级时立即中止剩余发送）。与备份下载同
// 一 authorizedBackupReader 模式：每 32KiB 重新验会话与角色。Fixture 按
// schema 的合法 attachment 配对播种（artifacts.kind='attachment' 闭包到
// source_materials.kind='text_attachment'，并由 text_attachments 完成关联
// ——触发器 trg_artifacts_owner_closure 的权威形状），不做任何 schema 改写。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/auth"
	_ "modernc.org/sqlite"
)

// chunkFenceWriter 在首个正文块真正写出之后才触发撤销——围栏的契约是
// 「授权失效前最多再放行一个有界块」，所以必须先有一个块被服务，再验
// 证剩余发送被切断。WriteHeader 只记录状态，不触发撤销（响应头按
// HTTP-FILE-004 先于正文发出是合法语义）。
type chunkFenceWriter struct {
	header  http.Header
	status  int
	body    bytes.Buffer
	revoke  func()
	revoked bool
}

func (writer *chunkFenceWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = http.Header{}
	}
	return writer.header
}

func (writer *chunkFenceWriter) WriteHeader(status int) {
	writer.status = status
}

func (writer *chunkFenceWriter) Write(body []byte) (int, error) {
	written, err := writer.body.Write(body)
	if !writer.revoked && writer.body.Len() > 0 {
		writer.revoked = true
		writer.revoke()
	}
	return written, err
}

// seedSensitiveArtifact 按生产播种形状注册一个产物：source_materials →
// artifact_blobs → artifacts → text_attachments（DATA-ARTIFACT-003 的所有权
// 顺序与触发器要求的合法 attachment 配对）。
func seedAttachmentArtifact(t *testing.T, db *sql.DB, storeDir string, body []byte, sensitive int) {
	t.Helper()
	sum := sha256.Sum256(body)
	shaHex := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(filepath.Join(storeDir, "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "blobs", shaHex+".blob"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	material, err := db.Exec(`INSERT INTO source_materials(kind,digest,size_bytes,content,created_at) VALUES('text_attachment',?,?,NULL,?)`, shaHex, len(body), now)
	if err != nil {
		t.Fatal(err)
	}
	materialID, err := material.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := db.Exec(`INSERT INTO artifact_blobs(sha256,size_bytes,storage_key,created_at) VALUES(?,?,?,?)`, shaHex, len(body), "blobs/"+shaHex+".blob", now)
	if err != nil {
		t.Fatal(err)
	}
	blobID, err := blob.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := db.Exec(`INSERT INTO artifacts(blob_id,kind,media_type,sensitive,retention_kind,owner_type,owner_id,expires_at,created_at) VALUES(?, 'attachment','text/plain',?,'long_term','source_material',?,NULL,?)`, blobID, sensitive, materialID, now)
	if err != nil {
		t.Fatal(err)
	}
	artifactID, err := inserted.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO text_attachments(source_material_id,artifact_id,original_filename,size_bytes,digest,uploaded_at) VALUES(?,?,?,?,?,?)`, materialID, artifactID, "fence-body.txt", len(body), shaHex, now); err != nil {
		t.Fatal(err)
	}
}

func newFenceFixture(t *testing.T) (*apiServer, *sql.DB, *auth.Service, auth.Session, string, string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/fence.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewService(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetReader(fixtureReadOnlyPool(t, db)); err != nil {
		t.Fatal(err)
	}
	// 真实初始化 + 登录拿到真实 bearer（与 authScenario 同一路径）。
	initial, err := auth.GenerateInitialPassword()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureBootstrapAdmin(context.Background(), initial, time.Now().UTC().Add(auth.InitialPasswordLifetime)); err != nil {
		t.Fatal(err)
	}
	login, err := service.LoginWithPassword(context.Background(), "admin", initial, "Test on Linux")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Authenticate(context.Background(), login.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(context.Background(), session, initial, "Root admin passphrase 2026!"); err != nil {
		t.Fatal(err)
	}
	login, err = service.LoginWithPassword(context.Background(), "admin", "Root admin passphrase 2026!", "Test on Linux")
	if err != nil {
		t.Fatal(err)
	}
	session, err = service.Authenticate(context.Background(), login.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	storeDir := t.TempDir()
	store, err := artifact.NewStore(db, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetReader(fixtureReadOnlyPool(t, db)); err != nil {
		t.Fatal(err)
	}
	application := &apiServer{auth: service, db: db, artifacts: store}
	return application, db, service, session, login.Bearer, storeDir
}

func fenceDownloadRequest(t *testing.T, session auth.Session, bearer string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/1/content", nil)
	request.SetPathValue("artifactId", "1")
	request.AddCookie(&http.Cookie{Name: "__Host-quoin-session", Value: bearer})
	// 直接调用 handler 绕过了准入守卫：请求必须携带守卫为真实会话构建的
	// execution metadata，否则下载审计按 ADR-0006 拒绝裸上下文。
	return request.WithContext(scenarioRequestContext(t, session))
}

// 首个正文块真正发出后，通过真实服务（Logout 走 runner 落 revoked_at 并
// 提交审计）撤销会话：剩余发送必须立即中止（CONTEXT「敏感内容下载」）。
func TestSensitiveArtifactDownloadAbortsOnSessionRevocation(t *testing.T) {
	application, _, service, session, bearer, storeDir := newFenceFixture(t)
	body := []byte(strings.Repeat("sensitive-payload-", 8192)) // 约 148 KiB，远超单块 32 KiB
	seedAttachmentArtifact(t, application.db, storeDir, body, 1)

	writer := &chunkFenceWriter{revoke: func() {
		if err := service.Logout(context.Background(), session); err != nil {
			t.Errorf("logout (revoke): %v", err)
		}
	}}
	application.downloadArtifactContent(writer, fenceDownloadRequest(t, session, bearer))
	if writer.status != http.StatusOK {
		t.Fatalf("status=%d, want 200 (head goes out before the body fence)", writer.status)
	}
	delivered := writer.body.Len()
	if delivered == 0 {
		t.Fatal("no body byte was served; the fixture must let the first bounded chunk pass before revoking")
	}
	if delivered > 32*1024 {
		t.Fatalf("fence let %d bytes pass after revocation, want at most one bounded chunk", delivered)
	}
	if delivered >= len(body) {
		t.Fatalf("revoked session still received the full %d bytes; fence never fired", delivered)
	}
	// 撤销已真实落库：同一 bearer 不再通过认证。
	if _, err := service.Authenticate(context.Background(), bearer); err == nil {
		t.Fatal("bearer must stop authenticating after the served logout revocation")
	}
}

// 唯一内置管理员不能降级；安全修订变化仍须使在途下载会话失效。
func TestSensitiveArtifactDownloadAbortsOnSecurityRevisionChange(t *testing.T) {
	application, _, service, session, bearer, storeDir := newFenceFixture(t)
	body := []byte(strings.Repeat("demotion-payload-", 8192))
	seedAttachmentArtifact(t, application.db, storeDir, body, 1)

	writer := &chunkFenceWriter{revoke: func() {
		otherLogin, err := service.LoginWithPassword(context.Background(), "admin", "Root admin passphrase 2026!", "Other device")
		if err != nil {
			t.Error(err)
			return
		}
		otherSession, err := service.Authenticate(context.Background(), otherLogin.Bearer)
		if err != nil {
			t.Error(err)
			return
		}
		if err := service.ChangePassword(context.Background(), otherSession, "Root admin passphrase 2026!", "Changed admin passphrase 2027!"); err != nil {
			t.Errorf("change password: %v", err)
		}
	}}
	application.downloadArtifactContent(writer, fenceDownloadRequest(t, session, bearer))
	if writer.status != http.StatusOK {
		t.Fatalf("status=%d, want 200", writer.status)
	}
	delivered := writer.body.Len()
	if delivered == 0 || delivered > 32*1024 || delivered >= len(body) {
		t.Fatalf("demotion fence delivered=%d of %d, want one bounded chunk then abort", delivered, len(body))
	}
	// 降级后同一 bearer 的会话已随修订失效（Authenticate 的修订连接条件）。
	if _, err := service.Authenticate(context.Background(), bearer); err == nil {
		t.Fatal("demoted admin's session must no longer authenticate at the old revision")
	}
}

// 非敏感产物不走会话围栏（CONTEXT 的敏感条款只覆盖 sensitive=1 产物与备
// 份）：会话中途撤销时普通产物的既有语义不受影响。
func TestNonSensitiveArtifactDownloadUnaffected(t *testing.T) {
	application, db, _, session, bearer, storeDir := newFenceFixture(t)
	body := []byte("ordinary evidence body")
	seedAttachmentArtifact(t, db, storeDir, body, 0)

	writer := &chunkFenceWriter{revoke: func() {
		if _, err := db.Exec(`UPDATE sessions SET revoked_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), session.ID); err != nil {
			t.Errorf("revoke session: %v", err)
		}
	}}
	application.downloadArtifactContent(writer, fenceDownloadRequest(t, session, bearer))
	if writer.status != http.StatusOK || writer.body.Len() != len(body) {
		t.Fatalf("non-sensitive download=(status=%d,bytes=%d), want (200,%d)", writer.status, writer.body.Len(), len(body))
	}
}
