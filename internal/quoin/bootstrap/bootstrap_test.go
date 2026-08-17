package bootstrap_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

func TestSecretBootstrapIsIdempotentAndRejectsPartialState(t *testing.T) {
	config := configFor(t.TempDir())
	created, err := bootstrap.BootstrapSecrets(config)
	if err != nil || !created {
		t.Fatalf("first bootstrap: created=%v err=%v", created, err)
	}
	created, err = bootstrap.BootstrapSecrets(config)
	if err != nil || created {
		t.Fatalf("idempotent bootstrap: created=%v err=%v", created, err)
	}
	if err := os.Remove(config.RuntimeTLSPrivateKeyFile); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.BootstrapSecrets(config); err == nil {
		t.Fatal("partial secret state was accepted")
	}
}

func TestDatabaseBindsRootKeyAndCurrentSchema(t *testing.T) {
	ctx := context.Background()
	config := configFor(t.TempDir())
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	wrongKey := filepath.Join(filepath.Dir(config.RootKeyFile), "wrong-root-key")
	if err := os.WriteFile(wrongKey, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, wrongKey); err == nil {
		t.Fatal("database accepted the wrong root key")
	}
	database, err = bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var integrity string
	if err := database.SQL.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check=%q err=%v", integrity, err)
	}
}

func configFor(root string) contract.QuoinConfig {
	secrets := filepath.Join(root, "secrets")
	return contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.test",
		DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backup"),
		RootKeyFile: filepath.Join(secrets, "root-key"), RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile: filepath.Join(secrets, "runtime-tls.key"), SteleServiceTokenFile: filepath.Join(secrets, "stele-service-token")}
}
