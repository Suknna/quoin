package alerts

import (
	"context"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// newTestService builds a fresh bootstrap database + alert service in a temp
// dir, returning teardown.
func newTestService(t *testing.T) (*Service, *bootstrap.Database, func()) {
	t.Helper()
	root := t.TempDir()
	secrets := root + "/secrets"
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.test",
		DataDirectory: root + "/data", BackupDirectory: root + "/backup",
		RootKeyFile:               secrets + "/root-key",
		RuntimeTLSCertificateFile: secrets + "/runtime-tls.crt",
		RuntimeTLSPrivateKeyFile:  secrets + "/runtime-tls.key",
		SteleServiceTokenFile:     secrets + "/stele-service-token",
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	return NewService(database.SQL), database, func() { database.Close() }
}

func seedSource(t *testing.T, service *Service, ctx context.Context, key string) (int64, int64) {
	t.Helper()
	digest := make([]byte, 32)
	for index := range digest {
		digest[index] = byte(index)
	}
	result, err := service.CreateSource(ctx, key, "alertmanager", digest, 1, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	return result.SourceID, result.CredentialID
}
