package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/backup"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/test/support"
)

func TestRestoreBackupArgumentRemovesOnlyRestoreFlag(t *testing.T) {
	backup, remaining := restoreBackupArgument([]string{"--config", "/tmp/component.yaml", "--backup", "42"})
	if backup != "42" || !reflect.DeepEqual(remaining, []string{"--config", "/tmp/component.yaml"}) {
		t.Fatalf("backup=%q remaining=%q", backup, remaining)
	}
	backup, remaining = restoreBackupArgument([]string{"--backup=43", "--config", "/tmp/component.yaml"})
	if backup != "43" || !reflect.DeepEqual(remaining, []string{"--config", "/tmp/component.yaml"}) {
		t.Fatalf("backup=%q remaining=%q", backup, remaining)
	}
}

func TestRestoreBackupArgumentRejectsMissingValue(t *testing.T) {
	backup, remaining := restoreBackupArgument([]string{"--config", "/tmp/component.yaml", "--backup"})
	if backup != "" || remaining != nil {
		t.Fatalf("backup=%q remaining=%q", backup, remaining)
	}
}

func TestTrimTerminalPasswordRemovesOnlyEnterSequence(t *testing.T) {
	if got := trimTerminalPassword([]byte("  secret value  \r\n")); got != "  secret value  " {
		t.Fatalf("trimTerminalPassword=%q", got)
	}
}

// restoreCLIFixture prepares one deployment with a single administrator and a
// verified offline backup, then closes the database so the restore command can
// take the exclusive data-directory lock. The returned config path points at
// the generated strict component configuration.
func restoreCLIFixture(t *testing.T) (configPath, backupID, dataDirectory, rootKeyFile string) {
	t.Helper()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.example.test",
		DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"),
		RootKeyFile: filepath.Join(secrets, "root-key"), RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile: filepath.Join(secrets, "runtime-tls.key"), RuntimeClientCAFile: filepath.Join(secrets, "stele-service-token"),
	}
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatalf("bootstrap secrets: %v", err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateFirstAdmin(context.Background(), "admin", "Restore Admin", "original-password-123"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(config.DataDirectory, "artifacts", "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.DataDirectory, "artifacts", "blobs", "snapshot.blob"), []byte("snapshot artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	backups, err := backup.NewService(database.SQL, backup.Config{DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, ArtifactDirectory: filepath.Join(config.DataDirectory, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	run, err := backups.RunOffline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Close the backup service's owned reader pool before the write pool so
	// SQLite checkpoints and removes the WAL sidecar; the restore subprocess
	// refuses a data directory that still carries one.
	if err := backups.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(root, "quoin.yaml")
	body := "component: quoin\npublicOrigin: " + config.PublicOrigin + "\ndataDirectory: " + config.DataDirectory + "\nbackupDirectory: " + config.BackupDirectory + "\nrootKeyFile: " + config.RootKeyFile + "\nruntimeTlsCertificateFile: " + config.RuntimeTLSCertificateFile + "\nruntimeTlsPrivateKeyFile: " + config.RuntimeTLSPrivateKeyFile + "\nruntimeClientCaFile: " + config.RuntimeClientCAFile + "\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, run.ID, config.DataDirectory, config.RootKeyFile
}

func TestRestoreCLIPrintsOneTimeCredentialOnceOnPTY(t *testing.T) {
	binary := recoverCLIBinary(t)
	configPath, backupID, dataDirectory, rootKeyFile := restoreCLIFixture(t)
	output, waitErr := runRecoverOnPTY(t, binary, []string{"restore", "--backup", backupID, "--config", configPath}, "admin\n")
	if waitErr != nil {
		t.Fatalf("restore failed: %v\n%s", waitErr, output)
	}
	// The PTY line discipline ends lines with \r\n.
	output = strings.ReplaceAll(output, "\r\n", "\n")
	marker := "shown only once):"
	index := strings.Index(output, marker)
	if index < 0 {
		t.Fatalf("missing credential banner: %s", output)
	}
	rest := strings.Split(strings.TrimPrefix(output[index+len(marker):], "\n"), "\n")
	token := strings.TrimSpace(rest[0])
	if token == "" || strings.Contains(token, " ") {
		t.Fatalf("no credential token line found in: %q", rest)
	}
	if strings.Count(output, token) != 1 {
		t.Fatal("the one-time credential must appear exactly once — never in the structured log line")
	}
	if !strings.Contains(output, "restore.completed") {
		t.Fatalf("expected the secret-free completion log line: %s", output)
	}

	database, err := bootstrap.OpenDatabase(context.Background(), dataDirectory, rootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	var active int
	var reason string
	if err := database.SQL.QueryRowContext(context.Background(), `SELECT active,reason FROM maintenance_state WHERE id=1`).Scan(&active, &reason); err != nil {
		t.Fatal(err)
	}
	if active != 1 || reason != "Restore" {
		t.Fatalf("maintenance active=%d reason=%q", active, reason)
	}
	var initialized, pendingFlows int
	if err := database.SQL.QueryRowContext(context.Background(), `SELECT initialized FROM users WHERE username='admin'`).Scan(&initialized); err != nil || initialized != 0 {
		t.Fatalf("restored admin must sit at the unified initialization entry: initialized=%d err=%v", initialized, err)
	}
	if err := database.SQL.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sqlite_master WHERE name IN ('auth_flows','auth_challenges','auth_delivery_settings')`).Scan(&pendingFlows); err != nil || pendingFlows != 0 {
		t.Fatalf("retired auth tables must stay absent: count=%d err=%v", pendingFlows, err)
	}
	// The printed temporary password signs in and starts the unified
	// initialization flow directly.
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	result, err := service.LoginWithPassword(context.Background(), "admin", token, "test")
	if err != nil {
		t.Fatalf("the printed temporary password must sign in: %v", err)
	}
	if !result.User.PasswordChangeRequired {
		t.Fatalf("the restore credential must stay restricted: %+v", result.User)
	}
}

func TestRestoreCLIWithoutTTYFailsClosed(t *testing.T) {
	binary := recoverCLIBinary(t)
	configPath, backupID, dataDirectory, _ := restoreCLIFixture(t)
	original, err := os.ReadFile(filepath.Join(dataDirectory, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(binary, "restore", "--backup", backupID, "--config", configPath).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "attached TTY") {
		t.Fatalf("run without a TTY must fail with the TTY requirement: err=%v output=%s", err, output)
	}
	current, err := os.ReadFile(filepath.Join(dataDirectory, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, current) {
		t.Fatal("failed run must not touch the live database")
	}
}
