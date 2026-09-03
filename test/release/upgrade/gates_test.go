package upgrade_test

// Real-binary gate proofs that need no container platform: the first-release
// schema gate's unsupported-version rejection with a synthetic non-release
// fixture (mechanism evidence only — it proves the gate, never an N-1
// migration), and the structural no-Ready-during-migration lock exclusion.

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

func gateFixture(t *testing.T) (binary, configPath, dataDir, rootKey string) {
	t.Helper()
	workRoot := t.TempDir()
	binaryPath := filepath.Join(workRoot, "quoin")
	build := exec.Command("go", "build", "-trimpath", "-o", binaryPath, "./cmd/quoin")
	build.Dir = repoRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build quoin: %v: %s", err, output)
	}
	root := filepath.Join(workRoot, "run")
	secrets := filepath.Join(root, "secrets")
	dataDir = filepath.Join(root, "data")
	rootKey = filepath.Join(secrets, "root-key")
	configPath = filepath.Join(workRoot, "component.yaml")
	lines := []string{
		"component: quoin",
		"publicOrigin: https://quoin.test",
		"dataDirectory: " + dataDir,
		"backupDirectory: " + filepath.Join(root, "backups"),
		"rootKeyFile: " + rootKey,
		"runtimeTlsCertificateFile: " + filepath.Join(secrets, "runtime-tls.crt"),
		"runtimeTlsPrivateKeyFile: " + filepath.Join(secrets, "runtime-tls.key"),
		"steleServiceTokenFile: " + filepath.Join(secrets, "stele-service-token"),
	}
	if err := os.WriteFile(configPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runBinary(binaryPath, "secrets", "bootstrap", "--config", configPath); err != nil {
		t.Fatalf("secrets bootstrap: %v", err)
	}
	// A real fresh v1 database with a real administrator.
	database, err := bootstrap.OpenDatabase(context.Background(), dataDir, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewService(database.SQL)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := service.CreateFirstAdmin(context.Background(), "admin", "Gate Admin", "gate-password-123"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return binaryPath, configPath, dataDir, rootKey
}

type gateRunError struct{ output string }

func (gate *gateRunError) Error() string { return gate.output }

func runBinary(binary string, arguments ...string) error {
	command := exec.Command(binary, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		return &gateRunError{output: string(output)}
	}
	return nil
}

func repoRoot(t *testing.T) string {
	t.Helper()
	absolute, err := filepath.Abs(filepath.Join(".", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

// tamperSchema mutates the persisted schema identity through the same
// embedded SQLite driver the product uses.
func tamperSchema(t *testing.T, dataDir, statement string) error {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "quoin.db")+"?mode=rw")
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(statement)
	return err
}

// TestSchemaGateRejectsUnsupportedVersion proves the first-release gate with
// a synthetic non-release fixture: a database whose persisted schema
// identity claims a different version is rejected by the real migrate
// command with the stable unsupported_schema_version code, and serve fails
// closed on the same fixture. This is mechanism evidence only — it never
// constitutes N-1 migration evidence.
func TestSchemaGateRejectsUnsupportedVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("release gate test builds the real binary")
	}
	binary, configPath, dataDir, _ := gateFixture(t)
	if err := tamperSchema(t, dataDir, "UPDATE schema_state SET schema_version='v0.9.0' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := runBinary(binary, "migrate", "preflight", "--config", configPath); err == nil || !strings.Contains(err.Error(), "unsupported_schema_version") {
		t.Fatalf("migrate preflight did not reject with the stable code: %v", err)
	}
	// The unified schema gate is structural: serve rejects the same fixture
	// rather than running against an unknown schema.
	if err := runBinary(binary, "serve", "--config", configPath); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("serve did not reject the synthetic schema: %v", err)
	}
}

// TestNoReadyDuringMigrationHoldingExclusiveLock proves structurally that no
// Quoin can become Ready while the exclusive migration step holds the data
// directory lock: the serve binary exits with the stable lock rejection
// before binding any listener. After the lock is released the same binary
// reaches normal readiness on the real ops listener.
func TestNoReadyDuringMigrationHoldingExclusiveLock(t *testing.T) {
	if testing.Short() {
		t.Skip("release gate test builds the real binary")
	}
	binary, configPath, dataDir, rootKey := gateFixture(t)
	lock, err := bootstrap.OpenDatabase(context.Background(), dataDir, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	// The exclusive migration window: serve must fail fast, never Ready.
	if serveErr := runBinary(binary, "serve", "--config", configPath); serveErr == nil || !strings.Contains(serveErr.Error(), "state directory is already owned") {
		t.Fatalf("serve failure was not the stable lock rejection: %v", serveErr)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	// With the lock free, the same binary serves and reports Ready on the
	// fixed internal ops port. The host may already use :8080/:9090, so the
	// real serve and its probe run inside one isolated network namespace.
	script := `
set -e
ip link set lo up
cd "$(dirname "$0")"
./quoin-link serve --config component.yaml > serve-ns.log 2>&1 &
SERVE_PID=$!
cleanup() { kill "$SERVE_PID" 2>/dev/null || true; wait "$SERVE_PID" 2>/dev/null || true; }
trap cleanup EXIT
for _ in $(seq 1 200); do
  if curl -sf -m 2 http://127.0.0.1:9090/readyz > readyz.json 2>/dev/null; then
    cat readyz.json
    exit 0
  fi
  sleep 0.3
done
cat serve-ns.log 2>/dev/null || true; echo NO-READYZ
exit 1
`
	run := filepath.Join(t.TempDir(), "run-serve.sh")
	if err := os.WriteFile(run, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(binary, filepath.Join(filepath.Dir(run), "quoin-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(run), "component.yaml"), []byte(readConfigWithAbsolutePaths(t, configPath)), 0o600); err != nil {
		t.Fatal(err)
	}
	isolated := exec.Command("unshare", "-n", "bash", run)
	isolated.Dir = filepath.Dir(run)
	output, err := isolated.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated serve never reached readiness: %v: %s", err, output)
	}
	if !strings.Contains(string(output), `"mode":"normal"`) || !strings.Contains(string(output), `"reason":"ready"`) {
		t.Fatalf("isolated readyz was not normal/ready: %s", output)
	}
}

// readConfigWithAbsolutePaths rewrites the fixture config into the isolated
// run directory with absolute secret/data paths (temp dirs may be symlinks).
func readConfigWithAbsolutePaths(t *testing.T, configPath string) string {
	t.Helper()
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(body), "\n")
	for index, line := range lines {
		for _, key := range []string{"dataDirectory", "backupDirectory", "rootKeyFile", "runtimeTlsCertificateFile", "runtimeTlsPrivateKeyFile", "steleServiceTokenFile"} {
			if strings.HasPrefix(line, key+": ") {
				value := strings.TrimPrefix(line, key+": ")
				if filepath.IsAbs(value) {
					continue
				}
				resolved, err := filepath.Abs(filepath.Join(filepath.Dir(absolute), value))
				if err == nil {
					lines[index] = key + ": " + resolved
				}
			}
		}
	}
	return strings.Join(lines, "\n")
}
