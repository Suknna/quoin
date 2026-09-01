package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/app"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/backup"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// TestBackupCommandsOverSameOriginHandler exercises the real cookie, CSRF and
// durable command ledger path rather than calling a handler directly.
func TestBackupCommandsOverSameOriginHandler(t *testing.T) {
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
	if _, err = authService.CreateFirstAdmin(ctx, "admin", "Admin", "Correct horse battery staple 2026!"); err != nil {
		t.Fatal(err)
	}
	backupService, err := backup.NewService(database.SQL, backup.Config{DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, ArtifactDirectory: filepath.Join(config.DataDirectory, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	application := app.NewAPIServer(authService, database.SQL, config.RootKeyFile)
	application.SetBackupService(backupService)
	handler, err := app.NewHandler(application, config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	headers := map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json"}
	login := mustPost(t, server, headers, "/api/v1/auth/login", `{"username":"admin","password":"Correct horse battery staple 2026!"}`, http.StatusOK)
	cookie := splitCookie(login.headers.Get("Set-Cookie"))
	mustDo(t, server, http.MethodPut, merge(headers, map[string]string{"Cookie": cookie}), "/api/v1/auth/password", `{"currentPassword":"Correct horse battery staple 2026!","newPassword":"A better personal passphrase 2027!"}`, http.StatusNoContent)
	first := mustPost(t, server, merge(headers, map[string]string{"Cookie": cookie}), "/api/v1/backups", `{"clientCommandId":"backup-http-command"}`, http.StatusAccepted)
	replay := mustPost(t, server, merge(headers, map[string]string{"Cookie": cookie}), "/api/v1/backups", `{"clientCommandId":"backup-http-command"}`, http.StatusAccepted)
	var a, b struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal([]byte(first.body), &a); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(replay.body), &b); err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.ID != b.ID {
		t.Fatalf("same command did not replay durable row: first=%s replay=%s", first.body, replay.body)
	}
	settings := mustDo(t, server, http.MethodGet, map[string]string{"Cookie": cookie}, "/api/v1/backups/settings", "", http.StatusOK)
	var projection struct {
		BackupTarget string `json:"backupTarget"`
	}
	if err := json.Unmarshal([]byte(settings.body), &projection); err != nil {
		t.Fatal(err)
	}
	if projection.BackupTarget != config.BackupDirectory {
		t.Fatalf("backupTarget=%q, want process-visible %q", projection.BackupTarget, config.BackupDirectory)
	}
}

func TestBackupListRejectsMalformedPagination(t *testing.T) {
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
	if _, err = authService.CreateFirstAdmin(ctx, "admin", "Admin", "Correct horse battery staple 2026!"); err != nil {
		t.Fatal(err)
	}
	backupService, err := backup.NewService(database.SQL, backup.Config{DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, ArtifactDirectory: filepath.Join(config.DataDirectory, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	application := app.NewAPIServer(authService, database.SQL, config.RootKeyFile)
	application.SetBackupService(backupService)
	handler, err := app.NewHandler(application, config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	headers := map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json"}
	login := mustPost(t, server, headers, "/api/v1/auth/login", `{"username":"admin","password":"Correct horse battery staple 2026!"}`, http.StatusOK)
	cookie := splitCookie(login.headers.Get("Set-Cookie"))
	mustDo(t, server, http.MethodPut, merge(headers, map[string]string{"Cookie": cookie}), "/api/v1/auth/password", `{"currentPassword":"Correct horse battery staple 2026!","newPassword":"A better personal passphrase 2027!"}`, http.StatusNoContent)
	for _, endpoint := range []string{"/api/v1/backups?limit=0", "/api/v1/backups?cursor=tampered"} {
		response := mustDo(t, server, http.MethodGet, map[string]string{"Cookie": cookie}, endpoint, "", http.StatusBadRequest)
		var body struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal([]byte(response.body), &body); err != nil {
			t.Fatal(err)
		}
		if body.Code != "malformed_request" {
			t.Fatalf("%s code=%q", endpoint, body.Code)
		}
	}
}

func TestBackupSettingsScheduleCronJSONPresenceAndNull(t *testing.T) {
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
	if _, err = authService.CreateFirstAdmin(ctx, "admin", "Admin", "Correct horse battery staple 2026!"); err != nil {
		t.Fatal(err)
	}
	backupService, err := backup.NewService(database.SQL, backup.Config{DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, ArtifactDirectory: filepath.Join(config.DataDirectory, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	application := app.NewAPIServer(authService, database.SQL, config.RootKeyFile)
	application.SetBackupService(backupService)
	handler, err := app.NewHandler(application, config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	headers := map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json"}
	login := mustPost(t, server, headers, "/api/v1/auth/login", `{"username":"admin","password":"Correct horse battery staple 2026!"}`, http.StatusOK)
	cookie := splitCookie(login.headers.Get("Set-Cookie"))
	mustDo(t, server, http.MethodPut, merge(headers, map[string]string{"Cookie": cookie}), "/api/v1/auth/password", `{"currentPassword":"Correct horse battery staple 2026!","newPassword":"A better personal passphrase 2027!"}`, http.StatusNoContent)
	requestHeaders := merge(headers, map[string]string{"Cookie": cookie})
	mustDo(t, server, http.MethodPut, requestHeaders, "/api/v1/backups/settings", `{"clientCommandId":"set-cron-1","expectedRowVersion":1,"scheduleCron":"*/5 * * * *"}`, http.StatusOK)
	settings, err := backupService.Settings(ctx)
	if err != nil || settings.ScheduleCron == nil || *settings.ScheduleCron != "*/5 * * * *" {
		t.Fatalf("set cron settings=%+v err=%v", settings, err)
	}
	mustDo(t, server, http.MethodPut, requestHeaders, "/api/v1/backups/settings", `{"clientCommandId":"preserve-cron-2","expectedRowVersion":2,"retentionCount":3}`, http.StatusOK)
	settings, err = backupService.Settings(ctx)
	if err != nil || settings.ScheduleCron == nil || *settings.ScheduleCron != "*/5 * * * *" {
		t.Fatalf("omitted cron changed settings=%+v err=%v", settings, err)
	}
	mustDo(t, server, http.MethodPut, requestHeaders, "/api/v1/backups/settings", `{"clientCommandId":"clear-cron-3","expectedRowVersion":3,"scheduleCron":null}`, http.StatusOK)
	settings, err = backupService.Settings(ctx)
	if err != nil || settings.ScheduleCron != nil {
		t.Fatalf("null cron did not clear settings=%+v err=%v", settings, err)
	}
}
