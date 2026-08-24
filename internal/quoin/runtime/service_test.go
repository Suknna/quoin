package runtime_test

// Deterministic service-level coverage for the T06 slot authority: prepare
// fencing, one-time token consumption, Register transitions, handshake
// rejection matrix, epoch monotonicity, replacement and the Quoin/Lintel
// catalog digest agreement.

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

func newService(t *testing.T) *qruntime.Service {
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
	return qruntime.NewService(database.SQL)
}

const release = "v0.1.0-dev"

// prepareAndReveal runs the two-step reveal flow and returns the raw token.
func prepareAndReveal(t *testing.T, service *qruntime.Service, slot string, expectedRow int64, session [32]byte) (raw string, generation int64) {
	t.Helper()
	_, handle, available, err := service.PrepareRegistration(context.Background(), slot, expectedRow, session)
	if err != nil || !available {
		t.Fatalf("prepare %s: err=%v available=%v", slot, err, available)
	}
	raw, _, generation, err = service.RevealToken(handle, session)
	if err != nil {
		t.Fatalf("reveal %s: %v", slot, err)
	}
	return raw, generation
}

func TestPrepareRevealRegisterSingleConsumption(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	var session [32]byte
	view, handle, available, err := service.PrepareRegistration(ctx, "plinth", 1, session)
	if err != nil || !available {
		t.Fatalf("prepare: %v %v", err, available)
	}
	if view.State != qruntime.StateUnregistered || view.RowVersion != 2 {
		t.Fatalf("first prepare must keep unregistered and bump row_version: %+v", view)
	}
	raw, slot, generation, revealErr := service.RevealToken(handle, session)
	if revealErr != nil || slot != "plinth" || generation != 1 {
		t.Fatalf("reveal: %v slot=%s gen=%d", revealErr, slot, generation)
	}
	// Single successful consumption: a second reveal of the same handle fails.
	if _, _, _, err := service.RevealToken(handle, session); err != qruntime.ErrTokenGone {
		t.Fatalf("second reveal must be gone, got %v", err)
	}
	// Foreign sessions can never consume.
	if _, handle2, _, err := service.PrepareRegistration(ctx, "lintel", 1, session); err == nil {
		var stranger [32]byte
		stranger[0] = 9
		if _, _, _, err := service.RevealToken(handle2, stranger); err != qruntime.ErrTokenGone {
			t.Fatalf("foreign session reveal must be gone, got %v", err)
		}
	} else {
		t.Fatal(err)
	}
	// Register consumes the token once; replay is rejected.
	longTerm, gen, err := service.Register(ctx, "plinth", raw, generation, "boot-1", release, release)
	if err != nil || longTerm == "" || gen != 1 {
		t.Fatalf("register: %v %q %d", err, longTerm, gen)
	}
	if _, _, err := service.Register(ctx, "plinth", raw, generation, "boot-2", release, release); err == nil {
		t.Fatal("token replay must fail")
	}
	view, err = service.View(ctx, "plinth")
	if err != nil {
		t.Fatal(err)
	}
	if view.State != qruntime.StateRegistered || view.CurrentGeneration != 1 {
		t.Fatalf("slot must be registered with generation 1: %+v", view)
	}
}

func TestHandshakeRejectionMatrix(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	var session [32]byte
	digest := catalog.Digest()
	raw, generation := prepareAndReveal(t, service, "lintel", 1, session)
	longTerm, _, err := service.Register(ctx, "lintel", raw, generation, "boot-l", release, release)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		bearer     string
		slot       string
		release    string
		catalog    string
		wantReason string
	}{
		{"valid lintel handshake", longTerm, "lintel", release, digest, ""},
		{"unknown bearer", "AAAA", "lintel", release, digest, "TOKEN_INVALID"},
		{"version mismatch", longTerm, "lintel", "v0.0.1-other", digest, "VERSION_MISMATCH"},
		{"catalog mismatch", longTerm, "lintel", release, "deadbeef", "CATALOG_MISMATCH"},
		// A plinth bearer against the lintel slot points at a slot that is not
		// registered for this token: slot-state rejection wins.
		{"cross-slot token", longTerm, "plinth", release, "", "SLOT_REVOKED"},
	}
	// Each case uses its own boot so the epoch monotonicity bookkeeping
	// from an accepted case cannot stale-out a later rejected case.
	for index, testCase := range cases {
		boot := "boot-case-" + strconv.Itoa(index)
		decision, err := service.Adjudicate(ctx, testCase.bearer, testCase.slot, boot, 7, testCase.release, release, digest, testCase.catalog)
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if testCase.wantReason == "" {
			if !decision.Accepted {
				t.Fatalf("%s: expected accepted, got %s", testCase.name, decision.Reason)
			}
			service.AttachStream(testCase.slot, boot, 7)
		} else if decision.Accepted || decision.Reason != testCase.wantReason {
			t.Fatalf("%s: expected %s, got accepted=%v %s", testCase.name, testCase.wantReason, decision.Accepted, decision.Reason)
		}
	}
	// Same-boot epoch monotonicity (RUNTIME-CTRL-004): replaying the
	// accepted epoch is stale; a higher epoch on the same boot is accepted
	// (the accepted matrix case attached boot-case-0 at epoch 7).
	if decision, _ := service.Adjudicate(ctx, longTerm, "lintel", "boot-case-0", 7, release, release, digest, digest); decision.Accepted {
		t.Fatal("same-epoch reconnect must be EPOCH_STALE")
	}
	decision, err := service.Adjudicate(ctx, longTerm, "lintel", "boot-case-0", 8, release, release, digest, digest)
	if err != nil || !decision.Accepted {
		t.Fatalf("higher epoch must be accepted: err=%v accepted=%v reason=%s", err, decision.Accepted, decision.Reason)
	}
	// A NEW boot may restart at epoch 1.
	newBoot, err := service.Adjudicate(ctx, longTerm, "lintel", "boot-y", 1, release, release, digest, digest)
	if err != nil || !newBoot.Accepted {
		t.Fatalf("new boot epoch 1 must be accepted: err=%v accepted=%v", err, newBoot.Accepted)
	}
	// First successful authentication is recorded on the current generation.
	view, err := service.View(ctx, "lintel")
	if err != nil {
		t.Fatal(err)
	}
	if view.FirstAuthenticatedAt == nil {
		t.Fatal("first authenticated timestamp must be recorded")
	}
}

// TestCatalogDigestAgreement pins the wire-level agreement: the digest Quoin
// expects from Lintel is exactly the digest the Lintel channel embeds
// (DATA-CONFIG-008, RUNTIME-CTRL-010) — one computation, no re-serialization
// on either side.
func TestCatalogDigestAgreement(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	var session [32]byte
	digest := catalog.Digest()
	raw, generation := prepareAndReveal(t, service, "lintel", 1, session)
	longTerm, _, err := service.Register(ctx, "lintel", raw, generation, "boot-l", release, release)
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := service.Adjudicate(ctx, longTerm, "lintel", "boot-c", 1, release, release, digest, catalog.Digest()); err != nil || !decision.Accepted {
		t.Fatalf("lintel-embedded digest must be accepted: err=%v accepted=%v reason=%s", err, decision.Accepted, decision.Reason)
	}
	if decision, _ := service.Adjudicate(ctx, longTerm, "lintel", "boot-c2", 1, release, release, digest, digest+"00"); decision.Accepted {
		t.Fatal("any other digest must be CATALOG_MISMATCH")
	}
}

func TestReplacementRetiresAndFences(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	var session [32]byte
	raw, generation := prepareAndReveal(t, service, "plinth", 1, session)
	longTerm, _, err := service.Register(ctx, "plinth", raw, generation, "boot-1", release, release)
	if err != nil {
		t.Fatal(err)
	}
	// The accepted bearer authenticates before replacement.
	if decision, err := service.Adjudicate(ctx, longTerm, "plinth", "boot-1", 1, release, release, "", ""); err != nil || !decision.Accepted {
		t.Fatalf("pre-replacement handshake must be accepted: err=%v accepted=%v reason=%s", err, decision.Accepted, decision.Reason)
	}
	view, err := service.View(ctx, "plinth")
	if err != nil {
		t.Fatal(err)
	}
	// Replacement prepare: revoke + clear pointers; the schema trigger
	// retires the old generation.
	after, handle, available, err := service.PrepareRegistration(ctx, "plinth", view.RowVersion, session)
	if err != nil || !available {
		t.Fatalf("replacement prepare: %v %v", err, available)
	}
	if after.State != qruntime.StateRevoked || after.CurrentGeneration != 0 {
		t.Fatalf("replacement must revoke and clear current: %+v", after)
	}
	// The revoked slot rejects the old long-term bearer (SLOT_REVOKED), so
	// the replaced runtime can never silently return.
	if decision, _ := service.Adjudicate(ctx, longTerm, "plinth", "boot-1", 2, release, release, "", ""); decision.Accepted || decision.Reason != "SLOT_REVOKED" {
		t.Fatalf("replaced bearer must be rejected with SLOT_REVOKED, got accepted=%v reason=%s", decision.Accepted, decision.Reason)
	}
	// The consumed one-time token can never mint a second credential.
	if _, _, err := service.Register(ctx, "plinth", raw, generation, "boot-1", release, release); err == nil {
		t.Fatal("consumed token must not register twice")
	}
	// A fresh generation re-registers cleanly.
	fresh, _, freshGeneration, revealErr := service.RevealToken(handle, session)
	if revealErr != nil {
		t.Fatal(revealErr)
	}
	if freshGeneration != 2 {
		t.Fatalf("replacement generation must advance to 2, got %d", freshGeneration)
	}
	if _, _, err := service.Register(ctx, "plinth", fresh, freshGeneration, "boot-2", release, release); err != nil {
		t.Fatalf("re-register after replacement: %v", err)
	}
}

func TestConcurrentRegisterSingleWinner(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	var session [32]byte
	raw, generation := prepareAndReveal(t, service, "plinth", 1, session)
	const attempts = 8
	results := make(chan error, attempts)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < attempts; i++ {
		go func() {
			start.Wait()
			_, _, err := service.Register(ctx, "plinth", raw, generation, "boot-race", release, release)
			results <- err
		}()
	}
	start.Done()
	winners := 0
	for i := 0; i < attempts; i++ {
		if err := <-results; err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one concurrent register must win, got %d", winners)
	}
}

func TestWithCurrentRejectsSupersededStream(t *testing.T) {
	service := newService(t)
	service.AttachStream("lintel", "boot-a", 1)
	if err := service.WithCurrent("lintel", "boot-a", 1, func() error { return nil }); err != nil {
		t.Fatalf("current stream rejected: %v", err)
	}
	service.AttachStream("lintel", "boot-a", 2)
	called := false
	if err := service.WithCurrent("lintel", "boot-a", 1, func() error { called = true; return nil }); !errors.Is(err, qruntime.ErrNotConnected) || called {
		t.Fatalf("superseded stream retained authority: err=%v called=%v", err, called)
	}
}

func TestWithCurrentClosingFencesSupersededOwner(t *testing.T) {
	service := newService(t)
	oldDone := service.AttachStream("lintel", "boot-a", 1)
	var closing <-chan struct{}
	if err := service.WithCurrentClosing("lintel", "boot-a", 1, func(fence <-chan struct{}) error {
		closing = fence
		return nil
	}); err != nil {
		t.Fatalf("capture current owner fence: %v", err)
	}
	service.AttachStream("lintel", "boot-a", 2)
	select {
	case <-oldDone:
	case <-time.After(time.Second):
		t.Fatal("superseding control stream did not close old owner fence")
	}
	select {
	case <-closing:
	case <-time.After(time.Second):
		t.Fatal("captured data-plane owner fence was not closed")
	}
}

func TestDetachedStreamClosesDataPlaneOwnerFence(t *testing.T) {
	service := newService(t)
	service.AttachStream("lintel", "boot-a", 1)
	var closing <-chan struct{}
	if err := service.WithCurrentClosing("lintel", "boot-a", 1, func(fence <-chan struct{}) error {
		closing = fence
		return nil
	}); err != nil {
		t.Fatalf("capture current owner fence: %v", err)
	}
	service.DetachStream("lintel", "boot-a", 1)
	select {
	case <-closing:
	case <-time.After(time.Second):
		t.Fatal("detaching control stream did not close data-plane owner fence")
	}
}

func TestSupersededStreamCannotDetachSuccessor(t *testing.T) {
	service := newService(t)
	oldDone := service.AttachStreamWithSender("lintel", "boot-a", 1, func(any) error { return nil })
	called := false
	service.AttachStreamWithSender("lintel", "boot-a", 2, func(any) error { called = true; return nil })
	<-oldDone
	service.DetachStream("lintel", "boot-a", 1)
	if err := service.SendTo("lintel", "probe"); err != nil {
		t.Fatalf("superseded stream removed successor: %v", err)
	}
	if !called {
		t.Fatal("successor sender was not retained")
	}
}

func TestTicket21LintelCapacityIsBoundToTheLiveHello(t *testing.T) {
	service := newService(t)
	service.AttachStreamWithSender("lintel", "boot-a", 7, func(any) error { return nil })
	if err := service.SetBrowserCapacity("lintel", "boot-a", 7, 1); err != nil {
		t.Fatalf("bind Hello capacity: %v", err)
	}
	capacity, err := service.BrowserCapacity("lintel", "boot-a", 7)
	if err != nil || capacity != 1 {
		t.Fatalf("unexpected frozen browser capacity: capacity=%d err=%v", capacity, err)
	}
	service.AttachStreamWithSender("lintel", "boot-a", 8, func(any) error { return nil })
	if _, err := service.BrowserCapacity("lintel", "boot-a", 7); !errors.Is(err, qruntime.ErrNotConnected) {
		t.Fatalf("superseded stream retained capacity authority: %v", err)
	}
}

func TestTicket21SendToFencedRejectsSuccessorStream(t *testing.T) {
	service := newService(t)
	oldSent, successorSent := false, false
	service.AttachStreamWithSender("lintel", "boot-a", 7, func(any) error {
		oldSent = true
		return nil
	})
	service.AttachStreamWithSender("lintel", "boot-b", 1, func(any) error {
		successorSent = true
		return nil
	})
	if err := service.SendToFenced("lintel", "boot-a", 7, func(_ uint64, sender qruntime.StreamSender) error {
		return sender("stale-browser-start")
	}); !errors.Is(err, qruntime.ErrNotConnected) {
		t.Fatalf("stale boot must not be forwarded through successor: %v", err)
	}
	if oldSent || successorSent {
		t.Fatalf("stale envelope reached a sender: old=%v successor=%v", oldSent, successorSent)
	}
	var messageID uint64
	if err := service.SendToFenced("lintel", "boot-b", 1, func(id uint64, sender qruntime.StreamSender) error {
		messageID = id
		return sender("current-browser-start")
	}); err != nil {
		t.Fatalf("current stream fenced send: %v", err)
	}
	if !successorSent || messageID != 1 {
		t.Fatalf("current stream did not receive its first fenced message: sent=%v id=%d", successorSent, messageID)
	}
}
