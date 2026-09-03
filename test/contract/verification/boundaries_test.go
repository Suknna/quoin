package verification

// TestBoundarySessionIdleExpiry proves the frozen session idle-expiry
// boundary over the real auth service and SQLite (fault.time cell
// session-idle-expiry): a session with a future idle deadline stays valid,
// a deadline elapsed to the boundary instant already rejects, and the
// expired terminal state stays stable.
//
// Idle deadlines only move forward in the product (frozen trigger), so the
// elapsed-side rows are materialized as genuinely aged session rows —
// exactly what a 13-hour-old session looks like on disk — and authenticated
// through the real Authenticate path. No wall-clock sleep.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

func TestBoundarySessionIdleExpiry(t *testing.T) {
	ctx := context.Background()
	config := boundaryConfig(t)
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	const password = "Correct horse battery staple 2026!"
	if _, err := service.CreateFirstAdmin(ctx, "admin", "Admin", password); err != nil {
		t.Fatal(err)
	}

	// before-boundary: a freshly issued session (idle deadline 12h ahead)
	// authenticates.
	login, _, err := service.Login(ctx, "admin", password, "boundary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, login.Bearer); err != nil {
		t.Fatalf("session rejected before the idle boundary: %v", err)
	}

	// at-boundary: the deadline elapsed one second ago — the boundary
	// instant itself already rejects.
	if bearer := agedSessionBearer(t, ctx, database.SQL, login, -time.Second); bearer != "" {
		if _, err := service.Authenticate(ctx, bearer); err == nil {
			t.Fatal("session accepted at the idle boundary")
		}
	}

	// after-boundary: a day-old deadline rejects, repeatedly and stably.
	stale := agedSessionBearer(t, ctx, database.SQL, login, -24*time.Hour)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := service.Authenticate(ctx, stale); err == nil {
			t.Fatalf("expired session resurrected on attempt %d", attempt)
		}
	}
}

// agedSessionBearer materializes a legitimately aged session row (created
// and last active 13h ago, idle deadline offset from now, absolute expiry
// still ahead) and returns its bearer.
func agedSessionBearer(t *testing.T, ctx context.Context, db *sql.DB, login auth.LoginResult, idleOffset time.Duration) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	now := time.Now().UTC()
	created := now.Add(-13 * time.Hour).Format(time.RFC3339Nano)
	idle := now.Add(idleOffset).Format(time.RFC3339Nano)
	absolute := now.Add(6 * 24 * time.Hour).Format(time.RFC3339Nano)
	_, err := db.ExecContext(ctx, `INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES((SELECT id FROM users WHERE username='admin'),?,?,'boundary-aged',?,?,?,?)`,
		digest[:], login.User.AuthRevision, created, created, idle, absolute)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func boundaryConfig(t *testing.T) contract.QuoinConfig {
	t.Helper()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	return contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.test",
		DataDirectory:             filepath.Join(root, "data"),
		RootKeyFile:               filepath.Join(secrets, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(secrets, "runtime-tls.key"),
		SteleServiceTokenFile:     filepath.Join(secrets, "stele-service-token"),
	}
}
