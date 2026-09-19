package main

import (
	"bytes"
	"context"
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
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
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
	return runQuoinCLI(t, binary, append([]string{"migrate", "preflight"}, "--config", configPath)...)
}

// runQuoinCLI executes an arbitrary `quoin` invocation and returns the exit
// code with both streams verbatim.
func runQuoinCLI(t *testing.T, binary string, arguments ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	code := 0
	exitErr, ok := err.(*exec.ExitError)
	if err != nil && !ok {
		t.Fatalf("run quoin %v: %v stderr=%s", arguments, err, stderr.String())
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
	accounts := []seedAccount{}
	if withMaintenance {
		accounts = append(accounts, seedAccount{id: 1, username: "upgrade-admin", role: "admin", enabled: 1})
	}
	configPath, _ = seedPredecessorDeploymentWithAccounts(t, "declaration-cutover-predecessor.sql", schemaDigest, ledgerDigest, accounts, withMaintenance)
	return configPath
}

// seedAccount is one predecessor user row; enabled accounts get active
// sessions that satisfy the released issue trigger.
type seedAccount struct {
	id       int64
	username string
	role     string
	enabled  int
	sessions int
}

// seedPredecessorDeploymentWithAccounts is the general form of
// seedPredecessorDeployment for any released predecessor fixture, with an
// explicit account population for the administrator-topology cases.
func seedPredecessorDeploymentWithAccounts(t *testing.T, schemaFile, schemaDigest, ledgerDigest string, accounts []seedAccount, withMaintenance bool) (configPath, dataDirectory string) {
	t.Helper()
	const enteredAt = "2026-01-01T00:00:00Z"
	const backupAt = "2026-01-02T00:00:00Z"
	root := t.TempDir()
	dataDirectory = filepath.Join(root, "data")
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
runtimeClientCaFile: %s
`, dataDirectory, filepath.Join(root, "backups"), rootKeyFile, filepath.Join(secrets, "runtime.crt"), filepath.Join(secrets, "runtime.key"), filepath.Join(secrets, "stele"))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile(filepath.Join("..", "..", "internal", "quoin", "upgrade", "testdata", schemaFile))
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
	for _, account := range accounts {
		if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,enabled,password_phc,auth_revision,created_at,updated_at) VALUES(?,?,?,?,?,?,1,?,?)`,
			account.id, account.username, account.username+" display", account.role, account.enabled, "phc-"+account.username, enteredAt, enteredAt); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < account.sessions; i++ {
			token := sha256.Sum256([]byte(account.username + "/" + string(rune('a'+i))))
			if _, err := db.Exec(`INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,?,?,?,?,?,?)`,
				account.id, token[:], 1, "fixture", enteredAt, enteredAt, enteredAt, enteredAt); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !withMaintenance {
		return configPath, dataDirectory
	}
	if len(accounts) == 0 {
		t.Fatal("a verified upgrade window requires a user to own the upgrade backup")
	}
	enteredBy := int64(0)
	enteredByType := "deployment_helper"
	if len(accounts) != 0 {
		enteredBy = accounts[0].id
		enteredByType = "user"
	}
	if _, err := db.Exec(`UPDATE maintenance_state SET active=1,reason='Upgrade',entered_at=?,entered_by_type=?,entered_by_id=?,row_version=row_version+1 WHERE id=1 AND active=0`, enteredAt, enteredByType, enteredBy); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(2,'BackupPreflight','pre_upgrade_backup','Safe','backup_verified',?)`, enteredAt); err != nil {
		t.Fatal(err)
	}
	digest := hex.EncodeToString(make([]byte, 32))
	if _, err := db.Exec(`INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,db_sha256,manifest_sha256,artifact_count,size_bytes,manifest_path,row_version,created_at,updated_at,started_at,completed_at,triggered_by) VALUES('succeeded','completed','upgrade','online',NULL,?,?,0,1234,'/backup/manifest.json',1,?,?,?,?,?)`, digest, digest, backupAt, backupAt, backupAt, backupAt, enteredBy); err != nil {
		t.Fatal(err)
	}
	return configPath, dataDirectory
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

// The pre-audit predecessor fixture is the byte-exact schema captured before
// the auth-audit canonical change; the CLI must admit exactly these bytes.
const pinnedAuthAuditPredecessorDigest = "d67fc107b5f8a978eae6b609393f6ec5563978234dc5167da7c1d021315b6269"

func seedAuthAuditPredecessorDeployment(t *testing.T, accounts []seedAccount, withMaintenance bool) (configPath, dataDirectory string) {
	t.Helper()
	return seedPredecessorDeploymentWithAccounts(t, "auth-audit-predecessor.sql", pinnedAuthAuditPredecessorDigest, "", accounts, withMaintenance)
}

func TestMigratePreflightCLIAcceptsAuthAuditPredecessor(t *testing.T) {
	binary := builtQuoinBinary(t)
	config, _ := seedAuthAuditPredecessorDeployment(t, []seedAccount{{id: 1, username: "upgrade-admin", role: "admin", enabled: 1}}, true)
	code, stdout, stderr := runQuoinMigrateCLI(t, binary, config)
	if code != 0 {
		t.Fatalf("preflight rejected the pre-audit predecessor: exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"stage":"preflight-verified"`) {
		t.Fatalf("summary did not admit the pre-audit predecessor: %s", stdout)
	}
}

func TestMigratePreflightCLIRequiresRetainedAdminSelection(t *testing.T) {
	binary := builtQuoinBinary(t)
	// Two enabled administrators make the retention choice ambiguous: the
	// read-only preflight on the OLD image must reject it with the stable code
	// before the operator retires the predecessor release.
	config, _ := seedAuthAuditPredecessorDeployment(t, []seedAccount{
		{id: 1, username: "alice", role: "admin", enabled: 1},
		{id: 2, username: "bob", role: "admin", enabled: 1},
	}, true)
	code, _, stderr := runQuoinMigrateCLI(t, binary, config)
	if code != 1 || !strings.Contains(stderr, "retained_admin_selection_required") {
		t.Fatalf("ambiguous admins exit=%d stderr=%s", code, stderr)
	}
}

func TestMigrateCLIRejectsAmbiguousAdminSelectionAtomically(t *testing.T) {
	binary := builtQuoinBinary(t)
	config, dataDirectory := seedAuthAuditPredecessorDeployment(t, []seedAccount{
		{id: 1, username: "alice", role: "admin", enabled: 1, sessions: 1},
		{id: 2, username: "bob", role: "admin", enabled: 1},
	}, true)
	code, _, stderr := runQuoinCLI(t, binary, "migrate", "--config", config)
	if code != 1 || !strings.Contains(stderr, "retained_admin_selection_required") {
		t.Fatalf("ambiguous migrate exit=%d stderr=%s", code, stderr)
	}
	// The rejection must leave the predecessor exactly as it was: digest,
	// administrators, active session, active maintenance window, no ledger.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDirectory, "quoin.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var digest string
	if err := db.QueryRow(`SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&digest); err != nil || digest != pinnedAuthAuditPredecessorDigest {
		t.Fatalf("digest after rejection=%q err=%v", digest, err)
	}
	var admins, activeSessions, ledger, active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&admins); err != nil || admins != 2 {
		t.Fatalf("admin rows=%d err=%v", admins, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE revoked_at IS NULL`).Scan(&activeSessions); err != nil || activeSessions != 1 {
		t.Fatalf("active sessions=%d err=%v", activeSessions, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM migration_ledger`).Scan(&ledger); err != nil || ledger != 0 {
		t.Fatalf("ledger rows=%d err=%v", ledger, err)
	}
	if err := db.QueryRow(`SELECT active FROM maintenance_state WHERE id=1`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("maintenance after rejection=%d err=%v", active, err)
	}
}

func TestMigrateCLIRetainsExplicitSelectedAdmin(t *testing.T) {
	binary := builtQuoinBinary(t)
	config, dataDirectory := seedAuthAuditPredecessorDeployment(t, []seedAccount{
		{id: 1, username: "alice", role: "admin", enabled: 1, sessions: 2},
		{id: 2, username: "bob", role: "admin", enabled: 1},
	}, true)
	code, stdout, stderr := runQuoinCLI(t, binary, "migrate", "--config", config, "--retain-admin-id", "1")
	if code != 0 {
		t.Fatalf("explicit migrate failed: exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"stage":"migrated"`) {
		t.Fatalf("summary=%s", stdout)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDirectory, "quoin.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	target := sha256.Sum256([]byte(gencontracts.SchemaSQL))
	var digest string
	if err := db.QueryRow(`SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if digest != hex.EncodeToString(target[:]) {
		t.Fatalf("migrated digest=%s want %x", digest, target)
	}
	var retainedUsername, retainedRole string
	var retainedEnabled, retainedInitialized int
	if err := db.QueryRow(`SELECT username,role,enabled,initialized FROM users WHERE id=1`).Scan(&retainedUsername, &retainedRole, &retainedEnabled, &retainedInitialized); err != nil {
		t.Fatal(err)
	}
	if retainedUsername != "admin" || retainedRole != "admin" || retainedEnabled != 1 || retainedInitialized != 0 {
		t.Fatalf("retained admin=%q/%s/%d/%d", retainedUsername, retainedRole, retainedEnabled, retainedInitialized)
	}
	var bobRole, bobUsername string
	var bobEnabled, bobInitialized int
	if err := db.QueryRow(`SELECT username,role,enabled,initialized FROM users WHERE id=2`).Scan(&bobUsername, &bobRole, &bobEnabled, &bobInitialized); err != nil {
		t.Fatal(err)
	}
	if bobUsername != "bob" || bobRole != "operator" || bobEnabled != 1 || bobInitialized != 0 {
		t.Fatalf("demoted admin=%q/%s/%d/%d", bobUsername, bobRole, bobEnabled, bobInitialized)
	}
	var admins, activeSessions, maintenanceActive int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&admins); err != nil || admins != 1 {
		t.Fatalf("admin rows=%d err=%v", admins, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE revoked_at IS NULL`).Scan(&activeSessions); err != nil || activeSessions != 0 {
		t.Fatalf("active sessions after migrate=%d err=%v", activeSessions, err)
	}
	if err := db.QueryRow(`SELECT active FROM maintenance_state WHERE id=1`).Scan(&maintenanceActive); err != nil || maintenanceActive != 0 {
		t.Fatalf("maintenance after migrate=%d err=%v", maintenanceActive, err)
	}
}

func TestMigrateCLIRejectsNonAdminRetainedSelection(t *testing.T) {
	binary := builtQuoinBinary(t)
	config, _ := seedAuthAuditPredecessorDeployment(t, []seedAccount{
		{id: 1, username: "alice", role: "admin", enabled: 1},
		{id: 2, username: "bob", role: "operator", enabled: 1},
	}, true)
	code, _, stderr := runQuoinCLI(t, binary, "migrate", "--config", config, "--retain-admin-id", "2")
	if code != 1 || !strings.Contains(stderr, "retained_admin_unknown") {
		t.Fatalf("non-admin selection exit=%d stderr=%s", code, stderr)
	}
}
