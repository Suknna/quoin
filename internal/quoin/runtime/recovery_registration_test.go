package runtime_test

// T35 recovery registration coverage: the helper-owned begin/resume state
// machine, the frozen fence digests, the recovery rotation through
// pending→confirmed→current/retiring, and the rejection matrix.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

func digestOf(parts ...string) string {
	sum := sha256.Sum256([]byte(join(parts)))
	return hex.EncodeToString(sum[:])
}

func join(parts []string) string {
	out := ""
	for _, part := range parts {
		out += part
	}
	return out
}

// recoveryService bootstraps a real database with the lintel slot already
// registered and first-authenticated (the pre-recovery production state).
func recoveryService(t *testing.T) (*qruntime.Service, *sql.DB) {
	t.Helper()
	root := t.TempDir()
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.test",
		DataDirectory:             filepath.Join(root, "data"),
		RootKeyFile:               filepath.Join(root, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(root, "tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(root, "tls.key"),
		SteleServiceTokenFile:     filepath.Join(root, "stele"),
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	service := qruntime.NewService(database.SQL)
	ctx := context.Background()
	var session [32]byte
	_, handle, _, err := service.PrepareRegistration(ctx, "lintel", 1, session)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, generation, err := service.RevealToken(handle, session)
	if err != nil {
		t.Fatal(err)
	}
	longTerm, _, err := service.Register(ctx, "lintel", raw, generation, "boot-old", release, release)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := service.Adjudicate(ctx, longTerm, "lintel", "boot-old", 1, release, release, catalog.Digest(), catalog.Digest())
	if err != nil || !decision.Accepted {
		t.Fatalf("old credential hello: decision=%+v err=%v", decision, err)
	}
	return service, database.SQL
}

func TestTicket35RecoveryRegistrationRotatesAndResumes(t *testing.T) {
	service, database := recoveryService(t)
	ctx := context.Background()
	fence := qruntime.LintelRecoveryFence{
		Backend: "compose", Disposition: "exclusively_reattached",
		DispositionDigest: digestOf("compose", "exclusively_reattached", "lintel-bind"),
		FenceReportDigest: digestOf("fence-report"),
	}
	begin, err := service.BeginLintelRecoveryRegistration(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if !begin.NeedsRegistration || begin.RegistrationToken == "" || begin.ReplacementGeneration != 2 {
		t.Fatalf("begin=%+v", begin)
	}
	var active int
	var reason string
	if err := database.QueryRow(`SELECT active,reason FROM maintenance_state WHERE id=1`).Scan(&active, &reason); err != nil {
		t.Fatal(err)
	}
	if active != 1 || reason != "LintelRecovery" {
		t.Fatalf("maintenance active=%d reason=%s", active, reason)
	}
	var blocking int
	if err := database.QueryRow(`SELECT COUNT(*) FROM maintenance_items WHERE maintenance_revision=? AND safe_state='Blocking'`, begin.MaintenanceRevision).Scan(&blocking); err != nil {
		t.Fatal(err)
	}
	if blocking != 3 {
		t.Fatalf("frozen blocking items=%d, want 3", blocking)
	}

	// A second begin with the same fence is idempotent on the revision and
	// may re-mint the unconsumed token (still generation 2).
	resume, err := service.BeginLintelRecoveryRegistration(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if resume.MaintenanceRevision != begin.MaintenanceRevision || !resume.NeedsRegistration || resume.ReplacementGeneration != 2 {
		t.Fatalf("re-begin=%+v", resume)
	}
	// Different digests on the frozen revision conflict.
	other := fence
	other.FenceReportDigest = digestOf("different-fence")
	if _, err := service.BeginLintelRecoveryRegistration(ctx, other); !errors.Is(err, qruntime.ErrLintelRecoveryFrozenFence) {
		t.Fatalf("re-fence error=%v, want frozen fence conflict", err)
	}

	longTerm, generation, err := service.Register(ctx, "lintel", resume.RegistrationToken, begin.ReplacementGeneration, "boot-new", release, release)
	if err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Fatalf("generation=%d", generation)
	}
	var state string
	var currentGen, retiringGen int64
	var oldRetired sql.NullString
	if err := database.QueryRow(`
		SELECT s.state,
		       (SELECT generation FROM runtime_credentials WHERE id=s.current_credential_id),
		       (SELECT generation FROM runtime_credentials WHERE id=s.retiring_credential_id),
		       (SELECT retired_at FROM runtime_credentials WHERE generation=1 AND slot='lintel')
		FROM runtime_slots s WHERE s.slot='lintel'`).Scan(&state, &currentGen, &retiringGen, &oldRetired); err != nil {
		t.Fatal(err)
	}
	if state != "registered" || currentGen != 2 || retiringGen != 1 || oldRetired.Valid {
		t.Fatalf("after rotation state=%s current=%d retiring=%d oldRetired=%v", state, currentGen, retiringGen, oldRetired.Valid)
	}
	// The superseded one-time token from the first begin can never mint.
	if _, _, err := service.Register(ctx, "lintel", begin.RegistrationToken, begin.ReplacementGeneration, "boot-new", release, release); err == nil {
		t.Fatal("superseded recovery token unexpectedly registered")
	}
	// The consumed one-time token can never mint twice.
	if _, _, err := service.Register(ctx, "lintel", begin.RegistrationToken, begin.ReplacementGeneration, "boot-new", release, release); err == nil {
		t.Fatal("replayed recovery registration unexpectedly succeeded")
	}
	// Resume after registration: no second token, same replacement.
	afterRegister, err := service.BeginLintelRecoveryRegistration(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if afterRegister.NeedsRegistration || afterRegister.RegistrationToken != "" || afterRegister.ReplacementGeneration != 2 {
		t.Fatalf("resume after register=%+v", afterRegister)
	}
	// The replacement's first Hello authenticates generation 2.
	decision, err := service.Adjudicate(ctx, longTerm, "lintel", "boot-new", 1, release, release, catalog.Digest(), catalog.Digest())
	if err != nil || !decision.Accepted || !decision.MarkedFirstAuthenticated {
		t.Fatalf("replacement hello: decision=%+v err=%v", decision, err)
	}
	var firstAuth sql.NullString
	if err := database.QueryRow(`SELECT first_authenticated_at FROM runtime_credentials WHERE slot='lintel' AND generation=2`).Scan(&firstAuth); err != nil {
		t.Fatal(err)
	}
	if !firstAuth.Valid {
		t.Fatal("replacement first_authenticated_at missing")
	}
}

func TestTicket35RecoveryBeginRejectsInvalidInputsAndStates(t *testing.T) {
	service, database := recoveryService(t)
	ctx := context.Background()
	for _, fence := range []qruntime.LintelRecoveryFence{
		{Backend: "nomad", Disposition: "retired", DispositionDigest: digestOf("d"), FenceReportDigest: digestOf("f")},
		{Backend: "compose", Disposition: "wiped", DispositionDigest: digestOf("d"), FenceReportDigest: digestOf("f")},
		{Backend: "compose", Disposition: "retired", DispositionDigest: "not-a-digest", FenceReportDigest: digestOf("f")},
	} {
		if _, err := service.BeginLintelRecoveryRegistration(ctx, fence); !errors.Is(err, qruntime.ErrLintelRecoveryFence) {
			t.Fatalf("fence=%+v error=%v, want fence rejection", fence, err)
		}
	}
	// A different active maintenance blocks the recovery begin.
	if _, err := database.Exec(`UPDATE maintenance_state SET active=1,reason='RootKeyRebind',entered_at='2026-01-01T00:00:00Z',entered_by_type='system',entered_by_id=0,row_version=row_version+1 WHERE id=1 AND active=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginLintelRecoveryRegistration(ctx, qruntime.LintelRecoveryFence{Backend: "helm", Disposition: "retired", DispositionDigest: digestOf("d"), FenceReportDigest: digestOf("f")}); !errors.Is(err, qruntime.ErrLintelRecoveryState) {
		t.Fatalf("error=%v, want state rejection", err)
	}
}
