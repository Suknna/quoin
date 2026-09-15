// Package testfixture provides the shared SQLite test harness for domain
// modules migrating onto the execution runner (ADR-0006): the frozen schema,
// an initialized user with an active session, and the execution-metadata
// contexts the real admission entry will carry. Test-only: never import from
// production code.
package testfixture

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/execution"
	_ "modernc.org/sqlite"
)

// OpenDB opens one file-backed SQLite database with the frozen schema and
// the serialized-writer pool discipline the domain tests rely on.
func OpenDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "fixture.db")+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gen.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	return db
}

// SeedUser inserts one enabled, initialized user (past any forced password
// change) and returns its row id. Role is "admin" or "operator".
func SeedUser(t *testing.T, db *sql.DB, username, role string) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := db.Exec(`INSERT INTO users(username,display_name,role,enabled,initialized,password_phc,auth_revision,created_at,updated_at) VALUES(?,?,?,1,1,'fixture',1,?,?)`,
		username, username, role, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// SeedActiveSession inserts one unrevoked session for the user issued at the
// user's current auth revision with far-future idle/absolute expiry, and
// returns the session proof reference metadata carries.
func SeedActiveSession(t *testing.T, db *sql.DB, userID int64) execution.SessionRef {
	t.Helper()
	var revision int64
	if err := db.QueryRow(`SELECT auth_revision FROM users WHERE id=?`, userID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(10 * 365 * 24 * time.Hour).Format(time.RFC3339Nano)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := db.Exec(`INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,randomblob(32),?,'fixture',?,?,?,?)`,
		userID, revision, now, now, future, future)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return execution.SessionRef{ID: id, AuthRevision: revision}
}

// UserContext injects the execution metadata a wired admission entry would
// carry for the user's session: correlation, actor, HTTP source and the
// session proof reference. Domain services must fail closed without it —
// they never synthesize identities.
func UserContext(t *testing.T, userID int64, session execution.SessionRef, correlation string) context.Context {
	t.Helper()
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: userID},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-" + correlation},
		Session:       session,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// SessionlessContext injects metadata WITHOUT a session proof reference —
// for pinning the fail-closed behavior of session-verified operations.
func SessionlessContext(t *testing.T, userID int64, correlation string) context.Context {
	t.Helper()
	return UserContext(t, userID, execution.SessionRef{}, correlation)
}

// SystemContext injects the scheduler-origin system metadata a trusted
// background entry point creates for its own serialized operation window.
func SystemContext(t *testing.T, correlation string) context.Context {
	t.Helper()
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceScheduler, RequestID: "req-" + correlation},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// Count returns one integer count query result.
func Count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
