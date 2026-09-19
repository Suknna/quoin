package main

// `quoin admin recover` coverage: the strict argument handling, the attached
// TTY requirement (fail-closed without any database write), and the two
// operator-visible modes driven through a real PTY exactly like the
// deployment helper runs them. All data stays in per-test temporary
// directories; no live user store is touched.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/test/support"
	"github.com/creack/pty"
)

const (
	recoverCLIOldPassword = "Lost administrator passphrase 2026!"
	recoverCLINewPassword = "Recovered administrator passphrase 2027!"
)

func TestRecoveryModeArgument(t *testing.T) {
	mode, rest := recoveryModeArgument([]string{"--mode", "password", "--config", "/tmp/x.yaml"})
	if mode != "password" || len(rest) != 2 || rest[0] != "--config" || rest[1] != "/tmp/x.yaml" {
		t.Fatalf("unexpected split: mode=%q rest=%v", mode, rest)
	}
	mode, rest = recoveryModeArgument([]string{"--config", "/tmp/x.yaml", "--mode=factors"})
	if mode != "factors" || len(rest) != 2 || rest[1] != "/tmp/x.yaml" {
		t.Fatalf("unexpected inline split: mode=%q rest=%v", mode, rest)
	}
	mode, rest = recoveryModeArgument([]string{"--config", "/tmp/x.yaml"})
	if mode != "" || len(rest) != 2 {
		t.Fatalf("missing mode must pass through: mode=%q rest=%v", mode, rest)
	}
}

// recoverCLIBinary builds the real quoin binary once for all subprocess tests.
var (
	recoverCLIBinaryOnce sync.Once
	recoverCLIBinaryPath string
	recoverCLIBinaryErr  error
)

func recoverCLIBinary(t *testing.T) string {
	t.Helper()
	recoverCLIBinaryOnce.Do(func() {
		directory, err := os.MkdirTemp("", "quoin-admin-recover-bin")
		if err != nil {
			recoverCLIBinaryErr = err
			return
		}
		binary := filepath.Join(directory, "quoin")
		build := exec.Command("go", "build", "-o", binary, ".")
		build.Dir = "."
		var output bytes.Buffer
		build.Stderr = &output
		if err := build.Run(); err != nil {
			recoverCLIBinaryErr = fmt.Errorf("build binary: %v\n%s", err, output.String())
			return
		}
		recoverCLIBinaryPath = binary
	})
	if recoverCLIBinaryErr != nil {
		t.Fatal(recoverCLIBinaryErr)
	}
	return recoverCLIBinaryPath
}

// recoverCLIFixture prepares one deployment fixture: secrets, a fresh
// canonical database with the seeded administrator, and the YAML config path.
// The database handle is closed before the subprocess takes the exclusive
// data-directory lock.
func recoverCLIFixture(t *testing.T, initialized bool, password string) (configPath string, dataDirectory, rootKeyFile string) {
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
	seedRecoverCLIAdmin(t, database.SQL, initialized, password)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(root, "quoin.yaml")
	body := fmt.Sprintf("component: quoin\npublicOrigin: %s\ndataDirectory: %s\nbackupDirectory: %s\nrootKeyFile: %s\nruntimeTlsCertificateFile: %s\nruntimeTlsPrivateKeyFile: %s\nruntimeClientCaFile: %s\n",
		config.PublicOrigin, config.DataDirectory, config.BackupDirectory, config.RootKeyFile, config.RuntimeTLSCertificateFile, config.RuntimeTLSPrivateKeyFile, config.RuntimeClientCAFile)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, config.DataDirectory, config.RootKeyFile
}

func seedRecoverCLIAdmin(t *testing.T, db *sql.DB, initialized bool, password string) {
	t.Helper()
	phc, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	initializedValue := 0
	if initialized {
		initializedValue = 1
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := db.ExecContext(context.Background(), `INSERT INTO users(username,display_name,role,enabled,initialized,password_phc,password_change_required,created_at,updated_at) VALUES('admin','Administrator','admin',1,?,?,0,?,?)`, initializedValue, phc, now, now)
	if err != nil {
		t.Fatal(err)
	}
	userID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO user_contacts(user_id,channel,target,version,verified_at,created_at,updated_at) VALUES(?,'email','ops@quoin.test',1,?,?,?)`, userID, now, now, now); err != nil {
		t.Fatal(err)
	}
}

// runRecoverOnPTY drives the binary through a real pseudo-terminal like the
// deployment helper does, feeding input lines and collecting the merged
// stdout/stderr stream.
func runRecoverOnPTY(t *testing.T, binary string, arguments []string, input string) (string, error) {
	t.Helper()
	command := exec.Command(binary, arguments...)
	master, err := pty.Start(command)
	if err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(output, master)
		close(copied)
	}()
	if input != "" {
		if _, err := io.WriteString(master, input); err != nil {
			t.Fatal(err)
		}
	}
	waitErr := command.Wait()
	_ = master.Close()
	<-copied
	return output.String(), waitErr
}

func recoverCLIOpen(t *testing.T, dataDirectory, rootKeyFile string) *bootstrap.Database {
	t.Helper()
	database, err := bootstrap.OpenDatabase(context.Background(), dataDirectory, rootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func TestAdminRecoverPasswordModeOnPTY(t *testing.T) {
	binary := recoverCLIBinary(t)
	configPath, dataDirectory, rootKeyFile := recoverCLIFixture(t, true, recoverCLIOldPassword)
	output, waitErr := runRecoverOnPTY(t, binary, []string{"admin", "recover", "--config", configPath, "--mode", "password"},
		recoverCLINewPassword+"\n"+recoverCLINewPassword+"\n")
	if waitErr != nil {
		t.Fatalf("password recovery failed: %v\n%s", waitErr, output)
	}
	if !strings.Contains(output, "Administrator password reset") {
		t.Fatalf("unexpected output: %s", output)
	}
	database := recoverCLIOpen(t, dataDirectory, rootKeyFile)
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	flow, _, err := service.StartAuthentication(context.Background(), "admin", recoverCLINewPassword, "test")
	if err != nil {
		t.Fatalf("temporary password must start the unified initialization flow: %v", err)
	}
	if flow.Type != auth.FlowAdminInitialize || !flow.User.PasswordChangeRequired {
		t.Fatalf("unexpected flow after CLI recovery: %+v", flow)
	}
	if _, _, err := service.StartAuthentication(context.Background(), "admin", recoverCLIOldPassword, "test"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("old password must be rejected, got %v", err)
	}
}

func TestAdminRecoverPasswordMismatchNeverChanges(t *testing.T) {
	binary := recoverCLIBinary(t)
	configPath, dataDirectory, rootKeyFile := recoverCLIFixture(t, true, recoverCLIOldPassword)
	output, waitErr := runRecoverOnPTY(t, binary, []string{"admin", "recover", "--config", configPath, "--mode", "password"},
		recoverCLINewPassword+"\nDifferent password 2027!\n")
	if waitErr == nil || !strings.Contains(output, "do not match") {
		t.Fatalf("mismatched input must fail the run: err=%v output=%s", waitErr, output)
	}
	database := recoverCLIOpen(t, dataDirectory, rootKeyFile)
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.StartAuthentication(context.Background(), "admin", recoverCLIOldPassword, "test"); err != nil {
		t.Fatalf("failed run must leave the old credential intact: %v", err)
	}
}

func TestAdminRecoverFactorsModePrintsTokenOnceOnPTY(t *testing.T) {
	binary := recoverCLIBinary(t)
	configPath, dataDirectory, rootKeyFile := recoverCLIFixture(t, true, recoverCLIOldPassword)
	output, waitErr := runRecoverOnPTY(t, binary, []string{"admin", "recover", "--config", configPath, "--mode", "factors"}, "")
	if waitErr != nil {
		t.Fatalf("factors recovery failed: %v\n%s", waitErr, output)
	}
	// The PTY line discipline ends lines with \r\n.
	output = strings.ReplaceAll(output, "\r\n", "\n")
	marker := "shown only once):"
	index := strings.Index(output, marker)
	if index < 0 {
		t.Fatalf("missing credential banner: %s", output)
	}
	rest := output[index+len(marker):]
	lines := strings.Split(strings.TrimPrefix(rest, "\n"), "\n")
	token := strings.TrimSpace(lines[0])
	if token == "" || strings.Contains(token, " ") {
		t.Fatalf("no credential token line found in: %q", rest)
	}
	if strings.Count(output, token) != 1 {
		t.Fatal("the one-time credential must appear exactly once")
	}
	database := recoverCLIOpen(t, dataDirectory, rootKeyFile)
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	var initialized int
	if err := database.SQL.QueryRowContext(context.Background(), `SELECT initialized FROM users WHERE username='admin'`).Scan(&initialized); err != nil || initialized != 0 {
		t.Fatalf("factors recovery must reset the initialized state: value=%d err=%v", initialized, err)
	}
	flow, _, err := service.StartAuthentication(context.Background(), "admin", token, "test")
	if err != nil {
		t.Fatalf("the printed temporary password must start the unified initialization flow: %v", err)
	}
	if flow.Type != auth.FlowAdminInitialize {
		t.Fatalf("unexpected flow type %q", flow.Type)
	}
}

func TestAdminRecoverWithoutTTYFailsClosed(t *testing.T) {
	binary := recoverCLIBinary(t)
	configPath, dataDirectory, rootKeyFile := recoverCLIFixture(t, true, recoverCLIOldPassword)
	output, err := exec.Command(binary, "admin", "recover", "--config", configPath, "--mode", "factors").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "attached TTY") {
		t.Fatalf("run without a TTY must fail with the TTY requirement: err=%v output=%s", err, output)
	}
	// The failed run must not have touched the database at all.
	database := recoverCLIOpen(t, dataDirectory, rootKeyFile)
	var initialized, pendingFlows int
	if err := database.SQL.QueryRowContext(context.Background(), `SELECT initialized FROM users WHERE username='admin'`).Scan(&initialized); err != nil || initialized != 1 {
		t.Fatalf("failed run must preserve the account state: value=%d err=%v", initialized, err)
	}
	if err := database.SQL.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM auth_flows WHERE status='pending'`).Scan(&pendingFlows); err != nil || pendingFlows != 0 {
		t.Fatalf("failed run must not open flows: count=%d err=%v", pendingFlows, err)
	}
}

func TestAdminRecoverRejectsUnknownMode(t *testing.T) {
	binary := recoverCLIBinary(t)
	output, err := exec.Command(binary, "admin", "recover", "--config", "/dev/null", "--mode", "rotate").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "--mode must be password or factors") {
		t.Fatalf("unknown mode must print the usage contract: err=%v output=%s", err, output)
	}
}
