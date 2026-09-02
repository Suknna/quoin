package maintenance_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/maintenance"
)

func TestRootKeyRebindAtomicallyIsolatesAndRetainsOldEnvelope(t *testing.T) {
	ctx := context.Background()
	config, originalKey, database := rebindFixture(t)
	service := connections.NewService(database.SQL, func() ([]byte, error) { return os.ReadFile(config.RootKeyFile) })
	input := rebindThanosInput("old-secret")
	created, err := service.Create(ctx, input, 1, "create-before-rebind")
	if err != nil {
		t.Fatal(err)
	}
	var oldNonce, oldCiphertext []byte
	if err := database.SQL.QueryRow(`SELECT nonce,ciphertext FROM credential_generations WHERE id=?`, created.CurrentGenerationID).Scan(&oldNonce, &oldCiphertext); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	newKey := []byte("abcdef0123456789abcdef0123456789")
	if err := os.WriteFile(config.RootKeyFile, newKey, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := maintenance.RebindRootKey(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if result.BindingRevision != 2 || result.ConnectionCount != 1 || result.AlreadyRebound {
		t.Fatalf("unexpected rebind result: %+v", result)
	}
	// Same replacement key repeats the committed operation without a second
	// binding advance; a current key on inactive maintenance is rejected below.
	if replay, err := maintenance.RebindRootKey(ctx, config.DataDirectory, config.RootKeyFile); err != nil || !replay.AlreadyRebound || replay.BindingRevision != 2 {
		t.Fatalf("unexpected replay: result=%+v err=%v", replay, err)
	}
	if err := os.WriteFile(config.RootKeyFile, originalKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile); err == nil {
		t.Fatal("old root key unexpectedly authenticated rebound database")
	}
	if err := os.WriteFile(config.RootKeyFile, newKey, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	var enabled, revalidation, revision int
	var nonce, ciphertext []byte
	if err := reopened.SQL.QueryRow(`SELECT enabled,revalidation_required,current_credential_generation_id FROM connections WHERE name='main-thanos'`).Scan(&enabled, &revalidation, new(int64)); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 || revalidation != 1 {
		t.Fatalf("connection was not isolated: enabled=%d revalidation=%d", enabled, revalidation)
	}
	if err := reopened.SQL.QueryRow(`SELECT binding_revision FROM root_key_state WHERE id=1`).Scan(&revision); err != nil || revision != 2 {
		t.Fatalf("unexpected binding revision: %d err=%v", revision, err)
	}
	if err := reopened.SQL.QueryRow(`SELECT nonce,ciphertext FROM credential_generations WHERE id=?`, created.CurrentGenerationID).Scan(&nonce, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if string(nonce) != string(oldNonce) || string(ciphertext) != string(oldCiphertext) {
		t.Fatal("rebind rewrote immutable historical credential envelope")
	}
	if _, err := connections.NewService(reopened.SQL, func() ([]byte, error) { return newKey, nil }).OpenGeneration(ctx, created.CurrentGenerationID); err == nil {
		t.Fatal("old binding generation was still decryptable/grantable")
	}
	state, err := maintenance.NewService(reopened.SQL).State(ctx)
	if err != nil || !state.Active || state.Reason != "RootKeyRebind" || len(state.Items) != 2 {
		t.Fatalf("unexpected maintenance state: %+v err=%v", state, err)
	}
	if state.Items[0].SafeState != "Blocking" && state.Items[1].SafeState != "Blocking" {
		t.Fatal("connection re-entry was not explicitly required")
	}
	if _, err := maintenance.NewService(reopened.SQL).Exit(ctx, maintenance.ExitRequest{ActorID: 1, ExpectedRowVersion: state.RowVersion, ExpectedReason: state.Reason, ClientCommandID: "cannot-exit-before-reentry"}); !errors.Is(err, maintenance.ErrConflict) {
		t.Fatalf("exit without explicit connection action err=%v, want conflict", err)
	}

	newService := connections.NewService(reopened.SQL, func() ([]byte, error) { return newKey, nil })
	current, err := newService.Get(ctx, "main-thanos")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := newService.Rotate(ctx, "main-thanos", current.RowVersion, rebindThanosInput("new-secret"), 1, "reenter-after-rebind")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.CurrentGenerationID == created.CurrentGenerationID || !rotated.RevalidationRequired {
		t.Fatalf("re-entry did not atomically create current generation: %+v", rotated)
	}
	var rotationAudit int
	if err := reopened.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='connection.rotate' AND client_command_id='reenter-after-rebind' AND outcome='success'`).Scan(&rotationAudit); err != nil || rotationAudit != 1 {
		t.Fatalf("re-entry audit missing: count=%d err=%v", rotationAudit, err)
	}
	state, err = maintenance.NewService(reopened.SQL).State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.NewService(reopened.SQL).Exit(ctx, maintenance.ExitRequest{ActorID: 1, ExpectedRowVersion: state.RowVersion, ExpectedReason: state.Reason, ClientCommandID: "exit-after-reentry"}); err != nil {
		t.Fatal(err)
	}
	var grants int
	if err := reopened.SQL.QueryRow(`SELECT COUNT(*) FROM tool_call_connection_grants`).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("rebind created active grants: count=%d err=%v", grants, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.RebindRootKey(ctx, config.DataDirectory, config.RootKeyFile); !errors.Is(err, maintenance.ErrRootKeyAlreadyCurrent) {
		t.Fatalf("current root key rebind err=%v, want already-current", err)
	}
}

func TestRootKeyRebindRollsBackEveryStateChangeWhenAuditCannotCommit(t *testing.T) {
	ctx := context.Background()
	config, originalKey, database := rebindFixture(t)
	service := connections.NewService(database.SQL, func() ([]byte, error) { return os.ReadFile(config.RootKeyFile) })
	created, err := service.Create(ctx, rebindThanosInput("old-secret"), 1, "create-for-rollback")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL.Exec(`CREATE TRIGGER fail_root_key_rebind_audit BEFORE INSERT ON audit_events WHEN NEW.action='root_key.rebind' BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.RootKeyFile, []byte("abcdef0123456789abcdef0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.RebindRootKey(ctx, config.DataDirectory, config.RootKeyFile); err == nil {
		t.Fatal("rebind unexpectedly committed when its mandatory audit failed")
	}
	if err := os.WriteFile(config.RootKeyFile, originalKey, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var binding, active, enabled, revalidation, generations int
	if err := reopened.SQL.QueryRow(`SELECT binding_revision FROM root_key_state WHERE id=1`).Scan(&binding); err != nil {
		t.Fatal(err)
	}
	if err := reopened.SQL.QueryRow(`SELECT active FROM maintenance_state WHERE id=1`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := reopened.SQL.QueryRow(`SELECT enabled,revalidation_required FROM connections WHERE id=?`, created.ID).Scan(&enabled, &revalidation); err != nil {
		t.Fatal(err)
	}
	if err := reopened.SQL.QueryRow(`SELECT COUNT(*) FROM credential_generations WHERE connection_id=?`, created.ID).Scan(&generations); err != nil {
		t.Fatal(err)
	}
	if binding != 1 || active != 0 || enabled != 0 || revalidation != 0 || generations != 1 {
		t.Fatalf("partial rebind state survived: binding=%d active=%d enabled=%d revalidation=%d generations=%d", binding, active, enabled, revalidation, generations)
	}
}

func TestRootKeyRebindExplicitDisableClosesChecklist(t *testing.T) {
	ctx := context.Background()
	config, _, database := rebindFixture(t)
	service := connections.NewService(database.SQL, func() ([]byte, error) { return os.ReadFile(config.RootKeyFile) })
	created, err := service.Create(ctx, rebindThanosInput("old-secret"), 1, "create-for-disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	newKey := []byte("abcdef0123456789abcdef0123456789")
	if err := os.WriteFile(config.RootKeyFile, newKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.RebindRootKey(ctx, config.DataDirectory, config.RootKeyFile); err != nil {
		t.Fatal(err)
	}
	reopened, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	service = connections.NewService(reopened.SQL, func() ([]byte, error) { return newKey, nil })
	current, err := service.Get(ctx, created.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Disable(ctx, current.Name, current.RowVersion); err != nil {
		t.Fatal(err)
	}
	state, err := maintenance.NewService(reopened.SQL).State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range state.Items {
		if item.Kind == "Connection" && (item.SafeState != "Safe" || item.DetailCode != "disabled") {
			t.Fatalf("disabled connection checklist not safe: %+v", item)
		}
	}
}

func TestRootKeyRebindRejectsTamperedReplacementVerifier(t *testing.T) {
	ctx := context.Background()
	config, _, database := rebindFixture(t)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	newKey := []byte("abcdef0123456789abcdef0123456789")
	if err := os.WriteFile(config.RootKeyFile, newKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.RebindRootKey(ctx, config.DataDirectory, config.RootKeyFile); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(config.DataDirectory, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TRIGGER trg_root_key_state_revision_forward; UPDATE root_key_state SET verifier_ciphertext=zeroblob(16) WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile); err == nil {
		t.Fatal("tampered new verifier unexpectedly authenticated")
	}
}

func rebindFixture(t *testing.T) (contract.QuoinConfig, []byte, *bootstrap.Database) {
	t.Helper()
	root := t.TempDir()
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test", DataDirectory: filepath.Join(root, "data"), RootKeyFile: filepath.Join(root, "root-key"), RuntimeTLSCertificateFile: filepath.Join(root, "tls.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(root, "tls.key"), SteleServiceTokenFile: filepath.Join(root, "stele")}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL.Exec(`INSERT INTO users(id,username,display_name,role,enabled,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,'$argon2id$phc',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	return config, key, database
}

func rebindThanosInput(password string) connections.CreateInput {
	config, _ := json.Marshal(map[string]string{"type": "thanos", "baseUrl": "https://thanos.example.test", "username": "operator"})
	secret, _ := json.Marshal(map[string]string{"type": "thanos", "username": "operator", "password": password})
	return connections.CreateInput{Name: "main-thanos", Type: connections.TypeThanos, NonSecretJSON: config, Secret: secret, SecretPresent: true}
}
