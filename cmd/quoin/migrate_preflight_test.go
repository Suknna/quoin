package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	_ "modernc.org/sqlite"
)

// The stopped live deployment's recorded predecessor identity. The CLI tests
// pin these literals because the compiled binary must admit exactly the bytes
// the production database carries — no helper indirection may stand between the
// operator's data and the admission decision.
const (
	pinnedDeclarationPredecessorDigest = "3a95baf7b2ecab6a5f81fe084334fe953a5c23a3a71db3de1cc1d8c0ee107eb2"
	pinnedDirectMigrationID            = "20260910_direct_investigation_metrics_v1"
	pinnedDirectMigrationLedgerDigest  = "39255c7776f4318773e3fe288b84b95a25bd91f4c9885cd816a040993e84375c"
)

var (
	cliBinaryOnce sync.Once
	cliBinaryPath string
	cliBinaryErr  error
)

// builtQuoinBinary compiles the real quoin executable once and returns its
// path; every CLI case below executes this binary exactly as the deployment
// helper does, so argument parsing, the preflight peek, the authenticated open,
// and the stable exit-code contract are all exercised end to end.
func builtQuoinBinary(t *testing.T) string {
	t.Helper()
	cliBinaryOnce.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			cliBinaryErr = err
			return
		}
		directory, err := os.MkdirTemp("", "quoin-migrate-cli-bin")
		if err != nil {
			cliBinaryErr = err
			return
		}
		cliBinaryPath = filepath.Join(directory, "quoin")
		cliBinaryErr = exec.Command("go", "build", "-o", cliBinaryPath, ".").Run()
	})
	if cliBinaryErr != nil {
		t.Fatalf("build quoin binary: %v", cliBinaryErr)
	}
	return cliBinaryPath
}

// runQuoinMigrateCLI executes `quoin migrate preflight --config <path>` and
// returns the exit code with both streams verbatim: the deployment helper
// records the stable code from stderr and the JSON summary from stdout.
func runQuoinMigrateCLI(t *testing.T, binary, configPath string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, binary, "migrate", "preflight", "--config", configPath)
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	code := 0
	exitErr, ok := err.(*exec.ExitError)
	if err != nil && !ok {
		t.Fatalf("run migrate preflight: %v stderr=%s", err, stderr.String())
	}
	if ok {
		code = exitErr.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

// seedPredecessorDeployment materializes a stopped-stack data directory around
// the exact released predecessor DDL: schema_state, the optionally recorded
// direct migration ledger row, the root-key binding the authenticated open
// demands, and optionally a fully verified Upgrade maintenance window. The
// ledger is append-only in the released schema, so its state is fixed here.
func seedPredecessorDeployment(t *testing.T, schemaDigest, ledgerDigest string, withMaintenance bool) (configPath string) {
	t.Helper()
	const enteredAt = "2026-01-01T00:00:00Z"
	const backupAt = "2026-01-02T00:00:00Z"
	root := t.TempDir()
	dataDirectory := filepath.Join(root, "data")
	secrets := filepath.Join(root, "secrets")
	for _, directory := range []string{dataDirectory, secrets} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rootKeyFile := filepath.Join(secrets, "root-key")
	rootKey := bytes.Repeat([]byte{0x51}, 32)
	if err := os.WriteFile(rootKeyFile, rootKey, 0o600); err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(root, "component.yaml")
	config := fmt.Sprintf(`component: quoin
publicOrigin: https://quoin.test
stelePublicURL: https://quoin.test/stele/alerts
dataDirectory: %s
backupDirectory: %s
rootKeyFile: %s
runtimeTlsCertificateFile: %s
runtimeTlsPrivateKeyFile: %s
steleServiceTokenFile: %s
`, dataDirectory, filepath.Join(root, "backups"), rootKeyFile, filepath.Join(secrets, "runtime.crt"), filepath.Join(secrets, "runtime.key"), filepath.Join(secrets, "stele"))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile(filepath.Join("..", "..", "internal", "quoin", "upgrade", "testdata", "declaration-cutover-predecessor.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDirectory, "quoin.db")+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-01-01T00:00:00Z')`, schemaDigest); err != nil {
		t.Fatal(err)
	}
	if ledgerDigest != "" {
		if _, err := db.Exec(`INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`, pinnedDirectMigrationID, ledgerDigest, enteredAt); err != nil {
			t.Fatal(err)
		}
	}
	nonce, ciphertext, err := bootstrap.SealRootKeyVerifier(rootKey, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, nonce, ciphertext, enteredAt); err != nil {
		t.Fatal(err)
	}
	// Every real deployment persists the idle maintenance singleton; without it
	// the gate reads no row at all instead of the stable not-active code.
	if _, err := db.Exec(`INSERT INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`); err != nil {
		t.Fatal(err)
	}
	if !withMaintenance {
		return configPath
	}
	if _, err := db.Exec(`INSERT INTO users(username,display_name,role,enabled,password_phc,auth_revision,created_at,updated_at) VALUES('upgrade-admin','Upgrade Admin','admin',1,'x',1,?,?)`, enteredAt, enteredAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE maintenance_state SET active=1,reason='Upgrade',entered_at=?,entered_by_type='user',entered_by_id=1,row_version=row_version+1 WHERE id=1 AND active=0`, enteredAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(2,'BackupPreflight','pre_upgrade_backup','Safe','backup_verified',?)`, enteredAt); err != nil {
		t.Fatal(err)
	}
	digest := hex.EncodeToString(make([]byte, 32))
	if _, err := db.Exec(`INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,db_sha256,manifest_sha256,artifact_count,size_bytes,manifest_path,row_version,created_at,updated_at,started_at,completed_at,triggered_by) VALUES('succeeded','completed','upgrade','online',NULL,?,?,0,1234,'/backup/manifest.json',1,?,?,?,?,1)`, digest, digest, backupAt, backupAt, backupAt, backupAt); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func TestMigratePreflightCLIAcceptsReleasedPredecessor(t *testing.T) {
	binary := builtQuoinBinary(t)
	config := seedPredecessorDeployment(t, pinnedDeclarationPredecessorDigest, pinnedDirectMigrationLedgerDigest, true)
	code, stdout, stderr := runQuoinMigrateCLI(t, binary, config)
	if code != 0 {
		t.Fatalf("preflight rejected the released predecessor: exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"stage":"preflight-verified"`) || !strings.Contains(stdout, `"migrationHistory":1`) {
		t.Fatalf("summary did not admit the recorded predecessor: %s", stdout)
	}
}

func TestMigratePreflightCLIRejectsUnknownDigest(t *testing.T) {
	binary := builtQuoinBinary(t)
	// A database whose schema identity matches no released release and carries
	// no ledger must fail closed before the data directory is even locked; the
	// stable code travels on stderr for the deployment helper's report.
	config := seedPredecessorDeployment(t, hex.EncodeToString(make([]byte, 32)), "", false)
	code, _, stderr := runQuoinMigrateCLI(t, binary, config)
	if code != 1 || !strings.Contains(stderr, "schema_digest_mismatch") {
		t.Fatalf("unknown digest exit=%d stderr=%s", code, stderr)
	}
}

func TestMigratePreflightCLIRejectsTamperedLedgerDigest(t *testing.T) {
	binary := builtQuoinBinary(t)
	config := seedPredecessorDeployment(t, pinnedDeclarationPredecessorDigest, strings.Repeat("0", 64), true)
	code, _, stderr := runQuoinMigrateCLI(t, binary, config)
	if code != 1 || !strings.Contains(stderr, "schema_history_present") {
		t.Fatalf("tampered ledger exit=%d stderr=%s", code, stderr)
	}
}

func TestMigratePreflightCLIRequiresVerifiedUpgradeMaintenance(t *testing.T) {
	binary := builtQuoinBinary(t)
	// Exact released predecessor identity, but no active Upgrade maintenance
	// and no upgrade backup: the operational gate must still hold the line even
	// when the schema admission would pass.
	config := seedPredecessorDeployment(t, pinnedDeclarationPredecessorDigest, pinnedDirectMigrationLedgerDigest, false)
	code, _, stderr := runQuoinMigrateCLI(t, binary, config)
	if code != 1 || !strings.Contains(stderr, "upgrade_maintenance_not_active") {
		t.Fatalf("missing maintenance exit=%d stderr=%s", code, stderr)
	}
}
