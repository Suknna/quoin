package deployment_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/verification/deployment"
)

const adminPassword = "Correct horse battery staple 2026!"

type harness struct {
	t         *testing.T
	db        *sql.DB
	auth      *auth.Service
	service   *deployment.Service
	now       time.Time
	sessionID int64
	userID    int64
	advanceBy time.Duration
}

// advance moves the deterministic service clock forward for the next call.
func (h *harness) advance(delta time.Duration) { h.advanceBy += delta }

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newBoundHarness(t, &contract.DeploymentBinding{
		ReleaseVersion:          "v1.2.3",
		ReleaseSubjectDigest:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeploymentConfigDigest:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Backend:                 "compose",
		Architecture:            "linux/amd64",
		BrowserChromiumRevision: "1200.0.6099.109",
	})
}

func newBoundHarness(t *testing.T, binding *contract.DeploymentBinding) *harness {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.example.test",
		DataDirectory:             filepath.Join(root, "data"),
		BackupDirectory:           filepath.Join(root, "backup"),
		RootKeyFile:               filepath.Join(secrets, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(secrets, "runtime-tls.key"),
		SteleServiceTokenFile:     filepath.Join(secrets, "stele-service-token"),
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	authService, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authService.CreateFirstAdmin(ctx, "admin", "Quoin Admin", adminPassword); err != nil {
		t.Fatal(err)
	}
	login, _, err := authService.Login(ctx, "admin", adminPassword, "harness")
	if err != nil {
		t.Fatal(err)
	}
	session, err := authService.Authenticate(ctx, login.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	harness := &harness{t: t, db: database.SQL, auth: authService, now: started, sessionID: session.ID, userID: session.User.ID}
	harness.service = deployment.NewService(database.SQL, func() time.Time { return harness.now.Add(harness.advanceBy) }, binding, config.PublicOrigin)
	return harness
}

func (h *harness) start() int64 {
	h.t.Helper()
	invocation, err := h.service.Start(context.Background(), h.sessionID, h.userID, "cmd-start")
	if err != nil {
		h.t.Fatalf("start invocation: %v", err)
	}
	return invocation
}

func (h *harness) mustExec(query string, arguments ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(query, arguments...); err != nil {
		h.t.Fatalf("exec %s: %v", query, err)
	}
}

func (h *harness) queryInt(query string, arguments ...any) int64 {
	h.t.Helper()
	var value int64
	if err := h.db.QueryRow(query, arguments...).Scan(&value); err != nil {
		h.t.Fatalf("query %s: %v", query, err)
	}
	return value
}
