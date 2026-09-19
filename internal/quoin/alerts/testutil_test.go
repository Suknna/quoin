package alerts

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/test/support"
)

// newTestService builds a fresh bootstrap database + alert service in a temp
// dir, returning teardown. It also seeds the built-in administrator (user 1)
// with one live session (session 1 at auth revision 1) so the management
// commands can verify a real session proof inside the runner transaction.
// The service assembles exactly like the composed application: the write
// pool stays runner-owned and the real read-only pool (bootstrap
// Database.Reader, opened by execution.OpenReadOnly) is installed through
// NewServiceWithReader — pure reads are served by the trusted reader, never
// by the writer.
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
		RuntimeClientCAFile:       secrets + "/stele-service-token",
	}
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	seedAdminSession(t, database.SQL)
	runner := execution.NewRunner(database.SQL, execution.NewRegistry(), nil)
	service, err := NewServiceWithReader(database.SQL, database.Reader, runner)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	return service, database, func() { database.Close() }
}

// seedAdminSession inserts the built-in administrator (user 1, admin, live at
// auth revision 1) and one unexpired session, mirroring the production
// identity facts the admission layer turns into execution metadata.
func seedAdminSession(t *testing.T, db *sql.DB) {
	t.Helper()
	now := "2026-09-14T00:00:00Z"
	idle, absolute := "2036-09-14T00:00:00Z", "2036-09-21T00:00:00Z"
	for _, statement := range []string{
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'fixture',1,'` + now + `','` + now + `')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,randomblob(32),1,'fixture','` + now + `','` + now + `','` + idle + `','` + absolute + `')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

// adminCommandContext attaches the execution metadata of the seeded
// administrator session (user 1, session 1, revision 1) onto ctx. The
// metadata is what the production admission middleware provides; the service
// itself never synthesizes identity.
func adminCommandContext(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	enriched, err := execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: "alerts-" + t.Name(),
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-" + t.Name()},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return enriched
}

func seedSource(t *testing.T, service *Service, ctx context.Context, key string) (int64, int64) {
	t.Helper()
	digest := make([]byte, 32)
	for index := range digest {
		digest[index] = byte(index)
	}
	result, _, err := service.CreateSource(adminCommandContext(t, ctx), "seed-"+key, key, "alertmanager", digest)
	if err != nil {
		t.Fatal(err)
	}
	return result.SourceID, result.CredentialID
}
