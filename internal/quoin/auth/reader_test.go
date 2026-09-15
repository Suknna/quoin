package auth_test

// The read seam fails closed: until SetReader installs a pool, every pure
// authentication read is rejected instead of silently reading (and
// contending) on the write database. ReadFlow is the canonical reviewer
// concern — the flow projection must never serve from the writer.

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

func TestReadsFailClosedWithoutReader(t *testing.T) {
	ctx := context.Background()
	// Deliberately NOT newAuthService: that fixture wires the read pool like
	// production. This test needs an UNWIRED service.
	config := testConfig(t)
	if created, err := bootstrap.BootstrapSecrets(config); err != nil || !created {
		t.Fatalf("bootstrap secrets: created=%v err=%v", created, err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetReader(nil); err == nil {
		t.Fatal("a nil reader must be rejected")
	}

	// Pure reads fail closed on the unwired service. The bearer must be
	// syntactically valid (32-byte base64) so the read actually reaches the
	// seam instead of being rejected at decode time.
	bearer := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if _, err := service.Authenticate(ctx, bearer); !errors.Is(err, auth.ErrReaderNotWired) {
		t.Fatalf("Authenticate without a reader must fail closed with ErrReaderNotWired, got %v", err)
	}
	// StartLogin's classification read fails through the same dead gate: the
	// gate's deterministic signature (canceled context on a zero-connection
	// handle) surfaces untranslated here — still strictly fail-closed, never
	// a writer read.
	if _, _, err := service.StartLogin(ctx, "admin", "x", "UA"); err == nil {
		t.Fatal("reads behind StartLogin must fail closed without a reader")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected failure class: %v", err)
	}

	// Wiring the pool turns the same reads live again — through the injected
	// handle, never through a hidden writer fallback.
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, "not-a-real-bearer-value"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("after wiring, reads must serve normally, got %v", err)
	}
}
