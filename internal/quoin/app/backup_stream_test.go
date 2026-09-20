package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/backup"
	"github.com/Suknna/quoin/test/support"
)

func TestAuthorizedBackupReaderStopsAtTheFirstRevocationCheck(t *testing.T) {
	checks := 0
	revoked := errors.New("session revoked")
	reader := &authorizedBackupReader{
		reader: strings.NewReader(strings.Repeat("x", 64*1024)),
		check: func() error {
			checks++
			if checks == 2 {
				return revoked
			}
			return nil
		},
	}
	written, err := io.Copy(io.Discard, reader)
	if !errors.Is(err, revoked) {
		t.Fatalf("copy error=%v, want revocation", err)
	}
	if written <= 0 || written > 32*1024 || checks != 2 {
		t.Fatalf("written=%d checks=%d, want one bounded chunk then revoke", written, checks)
	}
}

func TestDownloadBackupRecordsFailedTerminalAuditWhenSessionRevokesMidTransfer(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.example.com", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(secrets, "root-key"), RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(secrets, "runtime-tls.key"), RuntimeClientCAFile: filepath.Join(secrets, "stele-service-token")}
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, authService, initialPassword := newScenarioAuth(t, config.DataDirectory, config.RootKeyFile)
	defer database.Close()
	// The administrator is initialized through the real flow and the session
	// comes from the real two-step login: audited handler calls need the
	// guard-shaped execution metadata of a genuine session.
	scenarioInitializeAdmin(t, authService, initialPassword, "Correct horse battery staple 2026!")
	bearer := scenarioLogin(t, authService, "admin", "Correct horse battery staple 2026!")
	session, err := authService.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatal(err)
	}
	adminID := session.User.ID
	service, err := backup.NewService(database.SQL, backup.Config{DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, ArtifactDirectory: filepath.Join(config.DataDirectory, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	// Registered after the database defer, so the owned reader pool closes
	// before the write pool and the WAL sidecar is checkpointed away.
	defer service.Close()
	created, err := service.RunOffline(ctx)
	if err != nil || created.Status != "succeeded" {
		t.Fatalf("create archive=%+v err=%v", created, err)
	}
	checks := 0
	application := &apiServer{backups: service, db: database.SQL, backupAuthorize: func(context.Context, string, string) (auth.Session, error) {
		checks++
		if checks >= 3 {
			return auth.Session{}, errors.New("session revoked")
		}
		return auth.Session{User: auth.User{ID: adminID, Role: "admin"}}, nil
	}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/backups/"+created.ID+"/download", nil)
	request.SetPathValue("backupId", created.ID)
	request.AddCookie(&http.Cookie{Name: "__Host-quoin-session", Value: "test-session"})
	// Direct handler invocation bypasses the admission guard, so the request
	// must carry the same execution metadata the guard builds from the real
	// session; audited operations refuse a context without it.
	request = request.WithContext(scenarioRequestContext(t, session))
	response := httptest.NewRecorder()
	application.downloadBackup(response, request)
	if response.Code != http.StatusOK || response.Body.Len() == 0 {
		t.Fatalf("status=%d bytes=%d", response.Code, response.Body.Len())
	}
	var started, failed, completed int
	if err := database.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='backup.download_started'`).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if err := database.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='backup.download_failed'`).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if err := database.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='backup.download_completed'`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if started != 1 || failed != 1 || completed != 0 {
		t.Fatalf("audit started=%d failed=%d completed=%d", started, failed, completed)
	}

	// Maintenance is rechecked after initial authentication and denies a fresh
	// transfer before the archive can be prepared or streamed.
	if _, err := database.SQL.Exec(`UPDATE maintenance_state SET active=1,reason='Upgrade',entered_at='2026-01-01T00:00:00Z',entered_by_type='system',row_version=row_version+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	application.backupAuthorize = func(context.Context, string, string) (auth.Session, error) {
		return auth.Session{User: auth.User{ID: adminID, Role: "admin"}}, nil
	}
	blocked := httptest.NewRecorder()
	application.downloadBackup(blocked, request)
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("maintenance response=%d, want %d", blocked.Code, http.StatusServiceUnavailable)
	}
	var maintenanceBody struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(blocked.Body.Bytes(), &maintenanceBody); err != nil || maintenanceBody.Code != "unavailable" {
		t.Fatalf("maintenance body=%q err=%v; want unavailable", blocked.Body.String(), err)
	}
}

func TestDownloadBackupContentLengthMakesTruncatedTransportObservable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.example.com", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(secrets, "root-key"), RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(secrets, "runtime-tls.key"), RuntimeClientCAFile: filepath.Join(secrets, "stele-service-token")}
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, authService, initialPassword := newScenarioAuth(t, config.DataDirectory, config.RootKeyFile)
	defer database.Close()
	scenarioInitializeAdmin(t, authService, initialPassword, "Correct horse battery staple 2026!")
	bearer := scenarioLogin(t, authService, "admin", "Correct horse battery staple 2026!")
	session, err := authService.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatal(err)
	}
	service, err := backup.NewService(database.SQL, backup.Config{DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, ArtifactDirectory: filepath.Join(config.DataDirectory, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	// Same ownership order as the other download fixture: the owned reader
	// pool closes before the write pool.
	defer service.Close()
	created, err := service.RunOffline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	application := &apiServer{backups: service, backupAuthorize: func(context.Context, string, string) (auth.Session, error) {
		return auth.Session{User: auth.User{ID: session.User.ID, Role: "admin"}}, nil
	}}
	application.backupCopy = func(writer io.Writer, reader io.Reader) (int64, error) {
		buffer := make([]byte, 1)
		count, _ := reader.Read(buffer)
		if count > 0 {
			_, _ = writer.Write(buffer[:count])
		}
		return int64(count), errors.New("transport disconnected")
	}
	// The audited handler refuses a bare request context, so the server-side
	// handler injects the guard-shaped metadata of the real session before
	// dispatching (the transport round trip itself stays unchanged).
	downloadContext := scenarioRequestContext(t, session)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/backups/{backupId}/download", func(writer http.ResponseWriter, request *http.Request) {
		application.downloadBackup(writer, request.WithContext(downloadContext))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/backups/"+created.ID+"/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: "__Host-quoin-session", Value: "test-session"})
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength < 2 {
		t.Fatalf("status=%d contentLength=%d", response.StatusCode, response.ContentLength)
	}
	_, readErr := io.ReadAll(response.Body)
	if readErr == nil {
		t.Fatal("truncated archive read unexpectedly succeeded")
	}
}
