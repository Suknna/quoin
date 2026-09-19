package maintenance

// The offline verification gate must be an actual SQLite read-only open, not
// a code discipline: SQLite itself rejects any write attempt on the mode=ro
// connection — including INSERTs issued through QueryContext with RETURNING
// (auth-audit plan stage 2, "使用SQLite实际只读连接"). These tests are
// internal because the gate is deliberately unexported: it is the minimal
// controlled raw-open authority of the offline boundary, never a package-wide
// exemption.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

func readonlyGateFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.test",
		DataDirectory:             filepath.Join(root, "data"),
		RootKeyFile:               filepath.Join(root, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(root, "tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(root, "tls.key"),
		RuntimeClientCAFile:     filepath.Join(root, "stele"),
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(config.DataDirectory, "quoin.db")
}

func TestReadOnlyGateRejectsExecWrites(t *testing.T) {
	ctx := context.Background()
	path := readonlyGateFixture(t)
	ro, err := openReadOnlyDatabase(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) SELECT row_version,'Integrity','readonly-gate','Safe','probe','2026-01-01T00:00:00Z' FROM maintenance_state WHERE id=1`); err == nil {
		t.Fatal("the read-only open accepted an ExecContext write")
	}
	var total int
	if err := ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_items`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("a write landed through the read-only open: items=%d", total)
	}
}

func TestReadOnlyGateRejectsQueryWritesWithReturning(t *testing.T) {
	ctx := context.Background()
	path := readonlyGateFixture(t)
	ro, err := openReadOnlyDatabase(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	// The INSERT travels through QueryContext, so the failure can only come
	// from SQLite's read-only mode — not from hiding the Exec surface.
	rows, err := ro.QueryContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) SELECT row_version,'Integrity','readonly-gate','Safe','probe','2026-01-01T00:00:00Z' FROM maintenance_state WHERE id=1 RETURNING id`)
	if err != nil {
		return // rejected at prepare/step time: the guarantee holds
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("the read-only open executed an INSERT..RETURNING through QueryContext")
	}
	if err := rows.Err(); err == nil {
		t.Fatal("the read-only open completed a write through QueryContext without an error")
	}
	var total int
	if err := ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_items`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("a write landed through the read-only open: items=%d", total)
	}
}

func TestVerifyOfflineDatabaseAcceptsAHealthyDatabase(t *testing.T) {
	ctx := context.Background()
	path := readonlyGateFixture(t)
	// The verification gate reads successfully through the same read-only
	// open the write tests just proved non-mutating.
	if err := verifyOfflineDatabase(ctx, path); err != nil {
		t.Fatalf("healthy database failed the offline gate: %v", err)
	}
}
