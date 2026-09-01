package app

import (
	"context"
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
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
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
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.example.com", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(secrets, "root-key"), RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(secrets, "runtime-tls.key"), SteleServiceTokenFile: filepath.Join(secrets, "stele-service-token")}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authService, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authService.CreateFirstAdmin(ctx, "admin", "Admin", "Correct horse battery staple 2026!"); err != nil {
		t.Fatal(err)
	}
	var adminID int64
	if err := database.SQL.QueryRow(`SELECT id FROM users WHERE username='admin'`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	service, err := backup.NewService(database.SQL, backup.Config{DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, ArtifactDirectory: filepath.Join(config.DataDirectory, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.RunOffline(ctx)
	if err != nil || created.Status != "succeeded" {
		t.Fatalf("create archive=%+v err=%v", created, err)
	}
	checks := 0
	application := &apiServer{backups: service, backupAuthorize: func(context.Context, string, string) (auth.Session, error) {
		checks++
		if checks >= 3 {
			return auth.Session{}, errors.New("session revoked")
		}
		return auth.Session{User: auth.User{ID: adminID, Role: "admin"}}, nil
	}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/backups/"+created.ID+"/download", nil)
	request.SetPathValue("backupId", created.ID)
	request.AddCookie(&http.Cookie{Name: "__Host-quoin-session", Value: "test-session"})
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
	application.db = database.SQL
	application.backupAuthorize = func(context.Context, string, string) (auth.Session, error) {
		return auth.Session{User: auth.User{ID: adminID, Role: "admin"}}, nil
	}
	blocked := httptest.NewRecorder()
	application.downloadBackup(blocked, request)
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("maintenance response=%d, want %d", blocked.Code, http.StatusServiceUnavailable)
	}
}

func TestDownloadBackupContentLengthMakesTruncatedTransportObservable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.example.com", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(secrets, "root-key"), RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(secrets, "runtime-tls.key"), SteleServiceTokenFile: filepath.Join(secrets, "stele-service-token")}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service, err := backup.NewService(database.SQL, backup.Config{DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, ArtifactDirectory: filepath.Join(config.DataDirectory, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.RunOffline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	application := &apiServer{backups: service, backupAuthorize: func(context.Context, string, string) (auth.Session, error) {
		return auth.Session{User: auth.User{ID: 1, Role: "admin"}}, nil
	}}
	application.backupCopy = func(writer io.Writer, reader io.Reader) (int64, error) {
		buffer := make([]byte, 1)
		count, _ := reader.Read(buffer)
		if count > 0 {
			_, _ = writer.Write(buffer[:count])
		}
		return int64(count), errors.New("transport disconnected")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/backups/{backupId}/download", application.downloadBackup)
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
