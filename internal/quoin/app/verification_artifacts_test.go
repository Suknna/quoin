package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

func TestVerificationArtifactAdapterCommits(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	config := contract.QuoinConfig{Component: "quoin", PublicOrigin: "https://quoin.example.test",
		DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backup"),
		RootKeyFile: filepath.Join(root, "secrets", "root-key"), RuntimeTLSCertificateFile: filepath.Join(root, "secrets", "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile: filepath.Join(root, "secrets", "runtime-tls.key"), SteleServiceTokenFile: filepath.Join(root, "secrets", "stele-service-token")}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := artifact.NewStore(database.SQL, filepath.Join(root, "data", "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	authService, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authService.CreateFirstAdmin(ctx, "admin", "Quoin Admin", "Correct horse battery staple 2026!"); err != nil {
		t.Fatal(err)
	}
	login, _, err := authService.Login(ctx, "admin", "Correct horse battery staple 2026!", "test")
	if err != nil {
		t.Fatal(err)
	}
	session, err := authService.Authenticate(ctx, login.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	digest := repeat6("a")
	if _, err := database.SQL.Exec(`INSERT INTO verification_invocation_manifests(
		admin_session_id,principal_user_id,release_subject_digest,catalog_digest,result_profile_digest,
		deployment_config_digest,public_origin_digest,applicable_set_digest,item_count,item_set_digest,
		manifest_digest,canonical_input_digest,started_at,deadline_at,created_at)
		VALUES(?,?,?,?,?,?,?,?,1,?,?,?, '2026-09-03T00:00:00Z','2026-09-03T08:00:00Z','2026-09-03T00:00:00Z')`,
		session.ID, session.User.ID, digest, digest, digest, digest, digest, digest, digest, digest, digest); err != nil {
		t.Fatal(err)
	}
	adapter := verificationArtifacts{store: store, db: database.SQL}
	artifactID, bodyDigest, err := adapter.CommitVerificationArtifact(ctx, "verification_bundle", "application/json", "long_term", 1, []byte(`{"a":1}`), time.Time{})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if artifactID <= 0 || len(bodyDigest) != 64 {
		t.Fatalf("artifact=%d digest=%s", artifactID, bodyDigest)
	}
}

func repeat6(seed string) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = seed[i%len(seed)]
	}
	return string(out)
}

func repeat(char byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = char
	}
	return string(out)
}
