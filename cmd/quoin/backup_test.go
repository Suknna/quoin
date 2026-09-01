package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// TestOfflineBackupSubprocess proves the operator-facing binary acquires the
// same data lock and writes the normal Backup aggregate, not a host-side copy.
func TestOfflineBackupSubprocess(t *testing.T) {
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.example.test", DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backups"), RootKeyFile: filepath.Join(secrets, "root-key"), RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"), RuntimeTLSPrivateKeyFile: filepath.Join(secrets, "runtime-tls.key"), SteleServiceTokenFile: filepath.Join(secrets, "stele-service-token")}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "quoin.yaml")
	body := fmt.Sprintf("component: quoin\npublicOrigin: %s\ndataDirectory: %s\nbackupDirectory: %s\nrootKeyFile: %s\nruntimeTlsCertificateFile: %s\nruntimeTlsPrivateKeyFile: %s\nsteleServiceTokenFile: %s\n", config.PublicOrigin, config.DataDirectory, config.BackupDirectory, config.RootKeyFile, config.RuntimeTLSCertificateFile, config.RuntimeTLSPrivateKeyFile, config.SteleServiceTokenFile)
	if err = os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "quoin")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, output)
	}
	if output, err := exec.Command(binary, "backup", "--offline", "--config", configPath).CombinedOutput(); err != nil {
		t.Fatalf("offline backup: %v\n%s", err, output)
	}
	database, err = bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var status, mode string
	if err = database.SQL.QueryRow(`SELECT status,execution_mode FROM backups ORDER BY id DESC LIMIT 1`).Scan(&status, &mode); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || mode != "offline" {
		t.Fatalf("backup status=%s mode=%s", status, mode)
	}
}
