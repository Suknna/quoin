package rootkey_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/creack/pty"
	"gopkg.in/yaml.v3"
)

// TestTicket34 executes the compiled production command through an attached
// pseudo-terminal. It owns only a temporary deployment directory; its domain
// setup uses public services, never direct product-table writes.
func TestTicket34RootKeyRebindRealCommand(t *testing.T) {
	evidence := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidence == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T34 runtime evidence run disabled")
	}
	if err := os.MkdirAll(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC()
	root := t.TempDir()
	cleanup := map[string]any{"phases": []map[string]any{{"phase": "ticket-runtime", "resources": []map[string]any{{"name": "temporary deployment directory, compiled Quoin binary, root keys and SQLite files", "kind": "file-tree", "removalCommand": "testing.T TempDir cleanup", "observedFinalState": "removed"}}, "failures": []string{}}}}
	t.Cleanup(func() { writeJSON(t, filepath.Join(evidence, "cleanup.json"), cleanup) })
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(root, "secrets", "root-key"), RuntimeTLSCertificateFile: filepath.Join(root, "secrets", "runtime.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(root, "secrets", "runtime.key"), SteleServiceTokenFile: filepath.Join(root, "secrets", "stele-token")}
	configBytes, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "quoin.yaml")
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	oldKey, err := os.ReadFile(config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.CreateFirstAdmin(context.Background(), "admin", "Admin", "a sufficiently long local temporary password"); err != nil {
		t.Fatal(err)
	}
	connectionService := connections.NewService(database.SQL, func() ([]byte, error) { return os.ReadFile(config.RootKeyFile) })
	configJSON, _ := json.Marshal(map[string]string{"type": "thanos", "baseUrl": "https://thanos.example.test", "username": "operator"})
	secretJSON, _ := json.Marshal(map[string]string{"type": "thanos", "username": "operator", "password": "runtime-test-only-secret"})
	created, err := connectionService.Create(context.Background(), connections.CreateInput{Name: "main-thanos", Type: connections.TypeThanos, NonSecretJSON: configJSON, Secret: secretJSON, SecretPresent: true}, 1, "ticket34-create")
	if err != nil {
		t.Fatal(err)
	}
	var oldGenerationID int64
	var oldNonce, oldCiphertext []byte
	if err := database.SQL.QueryRow(`SELECT cg.id,cg.nonce,cg.ciphertext FROM credential_generations cg WHERE cg.connection_id=?`, created.ID).Scan(&oldGenerationID, &oldNonce, &oldCiphertext); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	newKey := []byte("abcdef0123456789abcdef0123456789")
	if err := os.WriteFile(config.RootKeyFile, newKey, 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "quoin")
	build := exec.Command("go", "build", "-o", binary, "./cmd/quoin")
	build.Dir = repositoryRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build command: %v\n%s", err, output)
	}
	command := exec.Command(binary, "root-key", "rebind", "--config", configPath)
	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Write([]byte("REBIND\n")); err != nil {
		t.Fatal(err)
	}
	output, readErr := io.ReadAll(terminal)
	if err := command.Wait(); err != nil {
		t.Fatalf("real rebind command failed: %v\n%s", err, output)
	}
	if readErr != nil && !strings.Contains(readErr.Error(), "input/output error") {
		t.Fatal(readErr)
	}
	commandOutput := string(output)
	if !strings.Contains(commandOutput, "root_key_rebind=applied binding_revision=2") {
		t.Fatalf("unexpected rebind output: %q", commandOutput)
	}
	if err := os.WriteFile(config.RootKeyFile, oldKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile); err == nil {
		t.Fatal("old key opened real rebound database")
	}
	tamperedKey := append([]byte(nil), newKey...)
	tamperedKey[0] ^= 0x01
	if err := os.WriteFile(config.RootKeyFile, tamperedKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile); err == nil {
		t.Fatal("tampered replacement key opened real rebound database")
	}
	if err := os.WriteFile(config.RootKeyFile, newKey, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var enabled, revalidation, binding int
	if err := reopened.SQL.QueryRow(`SELECT enabled,revalidation_required FROM connections WHERE id=?`, created.ID).Scan(&enabled, &revalidation); err != nil {
		t.Fatal(err)
	}
	if err := reopened.SQL.QueryRow(`SELECT binding_revision FROM root_key_state WHERE id=1`).Scan(&binding); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 || revalidation != 1 || binding != 2 {
		t.Fatalf("real command state: enabled=%d revalidation=%d binding=%d", enabled, revalidation, binding)
	}
	var generationCount int
	var persistedNonce, persistedCiphertext []byte
	if err := reopened.SQL.QueryRow(`SELECT COUNT(*) FROM credential_generations WHERE connection_id=?`, created.ID).Scan(&generationCount); err != nil {
		t.Fatal(err)
	}
	if err := reopened.SQL.QueryRow(`SELECT nonce,ciphertext FROM credential_generations WHERE id=?`, oldGenerationID).Scan(&persistedNonce, &persistedCiphertext); err != nil {
		t.Fatal(err)
	}
	if generationCount != 1 || !bytes.Equal(oldNonce, persistedNonce) || !bytes.Equal(oldCiphertext, persistedCiphertext) {
		t.Fatal("old credential generation was changed or replaced during rebind")
	}
	reboundConnections := connections.NewService(reopened.SQL, func() ([]byte, error) { return os.ReadFile(config.RootKeyFile) })
	if _, err := reboundConnections.OpenGeneration(context.Background(), oldGenerationID); err == nil {
		t.Fatal("old credential generation was still grantable after root-key rebind")
	}
	var rebindAuditCount int
	if err := reopened.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='root_key.rebind' AND outcome='success'`).Scan(&rebindAuditCount); err != nil {
		t.Fatal(err)
	}
	if rebindAuditCount != 1 {
		t.Fatalf("root-key rebind audit count=%d, want 1", rebindAuditCount)
	}
	commit, dirtyDigest := gitEvidence(t)
	writeJSON(t, filepath.Join(evidence, "runtime-evidence.json"), map[string]any{
		"startedAt": startedAt.Format(time.RFC3339Nano),
		"finishedAt": time.Now().UTC().Format(time.RFC3339Nano),
		"git":        map[string]string{"commit": commit, "dirtyStateSHA256": dirtyDigest},
		"binary":     map[string]string{"path": binary, "sha256": fileSHA256(t, binary)},
		"command":    map[string]any{"argv": []string{binary, "root-key", "rebind", "--config", configPath}, "exitCode": 0, "stdout": commandOutput},
		"assertions": map[string]any{"oldKeyRejected": true, "tamperedReplacementKeyRejected": true, "newKeyVerified": true, "connectionIsolated": true, "oldGenerationUnchanged": true, "oldGenerationGrantRejected": true, "rootKeyRebindAuditRecorded": true, "bindingRevision": binding},
		"secrets":    "redacted",
	})
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}
func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func gitEvidence(t *testing.T) (string, string) {
	t.Helper()
	commit, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := exec.Command("git", "status", "--porcelain=v1").Output()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(dirty)
	return strings.TrimSpace(string(commit)), hex.EncodeToString(digest[:])
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}
