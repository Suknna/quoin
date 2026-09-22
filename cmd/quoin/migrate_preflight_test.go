package main

// `quoin migrate [preflight]` CLI 契约（首发语义）：产品从未发布,不存在前驱
// 转换——preflight 只承认"恰好等于当前 canonical schema"的数据库,任何
// digest 分歧都以稳定码拒绝且没有迁移路径。用例执行真实编译的 quoin 二
// 进制,覆盖参数解析、只读 peek、认证打开与稳定退出码契约。

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	_ "modernc.org/sqlite"
)

var (
	cliBinaryOnce sync.Once
	cliBinaryPath string
	cliBinaryErr  error
)

// builtQuoinBinary compiles the real quoin executable once; every CLI case
// executes this binary exactly as the deployment helper does.
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

// runQuoinCLI executes an arbitrary `quoin` invocation and returns the exit
// code with both streams verbatim.
func runQuoinCLI(t *testing.T, binary string, arguments ...string) (int, string, string) {
	t.Helper()
	command := exec.Command(binary, arguments...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			return exitError.ExitCode(), stdout.String(), stderr.String()
		}
		t.Fatalf("run quoin %v: %v", arguments, err)
	}
	return 0, stdout.String(), stderr.String()
}

func runQuoinMigrateCLI(t *testing.T, binary, configPath string) (int, string, string) {
	t.Helper()
	return runQuoinCLI(t, binary, append([]string{"migrate", "preflight"}, "--config", configPath)...)
}

func currentSchemaDigest() string {
	digest := sha256.Sum256([]byte(gencontracts.SchemaSQL))
	return hex.EncodeToString(digest[:])
}

// seedFreshDeployment builds a data directory holding a fresh canonical
// database under the given schema digest, with an optional verified upgrade
// window (maintenance + pre-upgrade backup owned by the seeded admin).
func seedFreshDeployment(t *testing.T, schemaDigest string, withMaintenance bool) string {
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
	configPath := filepath.Join(root, "component.yaml")
	config := fmt.Sprintf(`component: quoin
publicOrigin: https://quoin.test
stelePublicURL: https://quoin.test/stele/webhook/alertmanager
dataDirectory: %s
backupDirectory: %s
rootKeyFile: %s
runtimeTlsCertificateFile: %s
runtimeTlsPrivateKeyFile: %s
runtimeClientCaFile: %s
`, dataDirectory, filepath.Join(root, "backups"), rootKeyFile, filepath.Join(secrets, "runtime.crt"), filepath.Join(secrets, "runtime.key"), filepath.Join(secrets, "stele"))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDirectory, "quoin.db")+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(gencontracts.SchemaSQL)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,?)`, schemaDigest, enteredAt); err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := bootstrap.SealRootKeyVerifier(rootKey, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, nonce, ciphertext, enteredAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,auth_revision,created_at,updated_at) VALUES(1,'upgrade-admin','Upgrade Admin','admin',1,1,'phc-fixture',1,?,?)`, enteredAt, enteredAt); err != nil {
		t.Fatal(err)
	}
	if !withMaintenance {
		return configPath
	}
	if _, err := db.Exec(`UPDATE maintenance_state SET active=1,reason='Upgrade',entered_at=?,entered_by_type='user',entered_by_id=1,row_version=row_version+1 WHERE id=1 AND active=0`, enteredAt); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := db.QueryRow(`SELECT row_version FROM maintenance_state WHERE id=1`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?,?,'pre_upgrade_backup','Safe','backup_verified',?)`, revision, "BackupPreflight", enteredAt); err != nil {
		t.Fatal(err)
	}
	digest := hex.EncodeToString(make([]byte, 32))
	if _, err := db.Exec(`INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,db_sha256,manifest_sha256,artifact_count,size_bytes,manifest_path,row_version,created_at,updated_at,started_at,completed_at,triggered_by) VALUES('succeeded','completed','upgrade','online',NULL,?,?,0,1234,'/backup/manifest.json',1,?,?,?,?,1)`, digest, digest, backupAt, backupAt, backupAt, backupAt); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func TestSchemaMismatchReasonAdmitsOnlyCurrentCanonical(t *testing.T) {
	if reason, mismatch := schemaMismatchReason("v1", currentSchemaDigest()); mismatch || reason != "" {
		t.Fatalf("current canonical schema flagged: mismatch=%v reason=%q", mismatch, reason)
	}
	if _, mismatch := schemaMismatchReason("v2", currentSchemaDigest()); !mismatch {
		t.Fatal("non-v1 schema version accepted")
	}
	if reason, mismatch := schemaMismatchReason("v1", strings.Repeat("0", 64)); !mismatch || reason != "schema_digest_mismatch" {
		t.Fatalf("foreign digest not rejected: mismatch=%v reason=%q", mismatch, reason)
	}
}

func TestMigratePreflightCLIAcceptsCurrentCanonicalSchema(t *testing.T) {
	binary := builtQuoinBinary(t)
	config := seedFreshDeployment(t, currentSchemaDigest(), true)
	code, stdout, stderr := runQuoinMigrateCLI(t, binary, config)
	if code != 0 {
		t.Fatalf("preflight rejected the canonical schema: exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"stage":"preflight-verified"`) || !strings.Contains(stdout, `"migrationHistory":0`) {
		t.Fatalf("summary did not verify the canonical schema: %s", stdout)
	}
}

func TestMigratePreflightCLIRejectsForeignDigest(t *testing.T) {
	binary := builtQuoinBinary(t)
	// A database whose schema identity matches no released build fails closed
	// before the data directory is even locked; the stable code travels on
	// stderr for the deployment helper's report.
	config := seedFreshDeployment(t, strings.Repeat("a", 64), true)
	code, _, stderr := runQuoinMigrateCLI(t, binary, config)
	if code != 1 || !strings.Contains(stderr, "schema_digest_mismatch") {
		t.Fatalf("foreign digest exit=%d stderr=%s", code, stderr)
	}
}

func TestMigratePreflightCLIRequiresVerifiedUpgradeMaintenance(t *testing.T) {
	binary := builtQuoinBinary(t)
	// Exact current schema identity, but no active Upgrade maintenance: the
	// operational gate holds the line even when the schema admission passes.
	config := seedFreshDeployment(t, currentSchemaDigest(), false)
	code, _, stderr := runQuoinMigrateCLI(t, binary, config)
	if code != 1 || !strings.Contains(stderr, "upgrade_maintenance_not_active") {
		t.Fatalf("missing maintenance exit=%d stderr=%s", code, stderr)
	}
}
