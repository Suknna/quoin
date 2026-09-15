package browser

// Standalone identity lifecycle coverage (ADR-0004): the plugin-owned
// identity_key vocabulary must run the identical durable operation machinery
// without a single business_systems dependency. The harness deliberately
// seeds no business system at all, so any hidden business join fails loudly.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

func newStandaloneTestService(t *testing.T) (*sql.DB, *Service) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/browser-standalone.db?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`INSERT INTO label_contract_state(id,row_version,updated_at) VALUES(1,1,'2026-01-01T00:00:00Z')`,
		`INSERT INTO users(id,username,display_name,role,enabled,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,'x',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,zeroblob(32),1,'test','` + now + `','` + now + `','2030-01-01T00:00:00Z','2030-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return db, NewService(db)
}

func configureStandaloneIdentity(t *testing.T, service *Service, name string) Identity {
	t.Helper()
	identity, _, err := service.ConfigureStandalone(context.Background(), 1, StandaloneConfigureInput{
		Name: name, StartURL: "https://ops.example.test/login",
		Probe:           ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://ops.example.test/app"}`)},
		ClientCommandID: "configure-standalone-" + name,
	})
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestConfigureStandaloneCreatesGeneratedKeyAndRevisesWithoutBusinessSystems(t *testing.T) {
	db, service := newStandaloneTestService(t)
	defer db.Close()
	identity := configureStandaloneIdentity(t, service, "Ops Console")
	if identity.IdentityKey != "ops-console" || identity.State != "AuthenticationRequired" || identity.Revision.Number != 1 {
		t.Fatalf("creation must generate a stable slug key: %#v", identity)
	}
	var businessRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM business_systems`).Scan(&businessRows); err != nil || businessRows != 0 {
		t.Fatalf("standalone identities must exist without business systems: %d %v", businessRows, err)
	}
	// Edit revision: same identity, new immutable revision, optimistic fence.
	second, _, err := service.ConfigureStandalone(context.Background(), 1, StandaloneConfigureInput{
		IdentityKey: identity.IdentityKey, Name: "Ops Console v2", StartURL: "https://ops.example.test/login",
		Probe:              ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://ops.example.test/app2"}`)},
		ExpectedRowVersion: &identity.RowVersion, ClientCommandID: "configure-standalone-edit-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision.Number != 2 || second.Revision.Name != "Ops Console v2" || second.RowVersion != identity.RowVersion+1 {
		t.Fatalf("edit must bump revision and row version: %#v", second)
	}
	// The historical row remains reachable through the same key.
	fetched, err := service.GetStandaloneIdentity(context.Background(), identity.IdentityKey)
	if err != nil || fetched.Revision.Number != 2 {
		t.Fatalf("identity key must stay stable across revisions: %#v %v", fetched, err)
	}
	stale := identity.RowVersion
	if _, _, err := service.ConfigureStandalone(context.Background(), 1, StandaloneConfigureInput{
		IdentityKey: identity.IdentityKey, Name: "Ops Console v3", StartURL: "https://ops.example.test/login",
		Probe:              ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://ops.example.test/app3"}`)},
		ExpectedRowVersion: &stale, ClientCommandID: "configure-standalone-edit-2",
	}); !errors.Is(err, ErrRowVersion) {
		t.Fatalf("stale row version must be rejected: %v", err)
	}
}

// runStandaloneLogin drives the real closed loop: start, durable Lintel Start
// fence, Running acknowledgement and — optionally — publish.
func runStandaloneLogin(t *testing.T, service *Service, identity Identity, commandID string) Operation {
	t.Helper()
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, commandID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := service.PrepareDispatchWithCapacity(context.Background(), op.ID, "lintel-boot", 3, unboundedCapacity)
	if err != nil {
		t.Fatal(err)
	}
	if input.Kind != "manual_login" || input.IdentityID != identity.ID || input.ActorUserID == nil || *input.ActorUserID != 1 {
		t.Fatalf("dispatch input must carry the standalone identity and actor: %#v", input)
	}
	if err := service.HandleStartAck(context.Background(), op.ID, "lintel-boot", 3, true, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	running, err := service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
	if err != nil || running.State != "Running" || !running.CanPublish || !running.CanAttach {
		t.Fatalf("standalone login must reach Running through the shared dispatch: %#v %v", running, err)
	}
	return running
}

func TestStandaloneLoginCancelAndScopeIsolation(t *testing.T) {
	db, service := newStandaloneTestService(t)
	defer db.Close()
	first := configureStandaloneIdentity(t, service, "Ops Console")
	second := configureStandaloneIdentity(t, service, "Billing Portal")
	if first.IdentityKey == second.IdentityKey {
		t.Fatalf("distinct identities must not share a key: %q", first.IdentityKey)
	}
	service.Dispatch = func(context.Context, int64) error { return nil }
	running := runStandaloneLogin(t, service, first, "standalone-start-0001")

	// The durable identity lock is per identity: a different identity may
	// queue its own login; the global FIFO and Lintel capacity fence the
	// physical dispatch, not the admission.
	secondStart, err := service.StartStandaloneManualLogin(context.Background(), second.IdentityKey, 1, 1, second.RowVersion, "standalone-start-0002")
	if err != nil || secondStart.State != "Queued" || secondStart.ID == running.ID {
		t.Fatalf("a different identity must be able to queue its own login: %#v %v", secondStart, err)
	}
	// Cross-identity reads are invisible: the locator, not the numeric id,
	// decides what a caller can see or command.
	if _, err := service.GetStandaloneOperation(context.Background(), second.IdentityKey, running.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("operation must be invisible from another identity key: %v", err)
	}
	ctx := userCommandContext(t)
	if _, err := service.CancelStandalone(ctx, "no-such-identity", running.ID, 1, running.RowVersion, "standalone-cancel-0001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel through an unknown key must be not found: %v", err)
	}
	cancelled, err := service.CancelStandalone(ctx, first.IdentityKey, running.ID, 1, running.RowVersion, "standalone-cancel-0001")
	if err != nil || cancelled.State != "Cancelled" || cancelled.TerminalReason == nil || *cancelled.TerminalReason != "cancelled" {
		t.Fatalf("standalone cancel must terminate the operation: %#v %v", cancelled, err)
	}
	// A started operation keeps the identity/slot fence until Lintel confirms
	// the physical stop; only pre-dispatch cancels confirm immediately.
	if cancelled.StopConfirmedAt != nil {
		t.Fatalf("physical stop confirmation must stay asynchronous: %#v", cancelled)
	}
	// A replayed cancel command returns the identical terminal operation; a
	// replay with different arguments is rejected by the shared command
	// ledger as a command reuse.
	replayed, err := service.CancelStandalone(ctx, first.IdentityKey, running.ID, 1, running.RowVersion, "standalone-cancel-0001")
	if err != nil || replayed.ID != cancelled.ID || replayed.State != "Cancelled" {
		t.Fatalf("cancel replay must be idempotent: %#v %v", replayed, err)
	}
	if _, err := service.CancelStandalone(ctx, first.IdentityKey, running.ID, 1, running.RowVersion+9, "standalone-cancel-0001"); !errors.Is(err, execution.ErrCommandReused) {
		t.Fatalf("command reuse with a different request must conflict: %v", err)
	}
}

func TestListStandaloneIdentitiesCompletesOnSingleConnection(t *testing.T) {
	db, service := newStandaloneTestService(t)
	defer db.Close()
	first := configureStandaloneIdentity(t, service, "Ops Console")
	second := configureStandaloneIdentity(t, service, "Billing Portal")

	// Regression fence: the list projection once kept the keys cursor open
	// while re-querying the same single-connection pool per identity, which
	// deadlocked production. The bounded context turns any future
	// connection-held-while-projecting regression into a fast failure instead
	// of a hung request.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	items, err := service.ListStandaloneIdentities(ctx)
	if err != nil {
		t.Fatalf("list must complete inside the deadline: %v", err)
	}
	if len(items) != 2 || items[0].IdentityKey != second.IdentityKey || items[1].IdentityKey != first.IdentityKey {
		t.Fatalf("list must project every standalone identity in key order: %#v", items)
	}
}

func TestStandalonePublishClosedLoopAndReplay(t *testing.T) {
	db, service := newStandaloneTestService(t)
	defer db.Close()
	identity := configureStandaloneIdentity(t, service, "Ops Console")
	service.Dispatch = func(context.Context, int64) error { return nil }
	running := runStandaloneLogin(t, service, identity, "standalone-start-0003")

	// Publish before Running or with a stale fence is rejected.
	if _, err := service.PrepareStandalonePublish(context.Background(), identity.IdentityKey, running.ID, 1, running.RowVersion+5, "standalone-publish-0001"); !errors.Is(err, ErrRowVersion) {
		t.Fatalf("stale publish fence must be rejected: %v", err)
	}
	request, err := service.PrepareStandalonePublish(context.Background(), identity.IdentityKey, running.ID, 1, running.RowVersion, "standalone-publish-0001")
	if err != nil || request.AlreadyPublished || request.NewGeneration != 1 {
		t.Fatalf("publish admission must persist the command: %#v %v", request, err)
	}
	// Replay reconstructs the same deterministic generation for a safe resend.
	replayed, err := service.PrepareStandalonePublish(context.Background(), identity.IdentityKey, running.ID, 1, running.RowVersion, "standalone-publish-0001")
	if err != nil || replayed.NewGeneration != 1 || replayed.AlreadyPublished {
		t.Fatalf("publish replay must be deterministic: %#v %v", replayed, err)
	}
	// The Lintel Runtime result commits through the shared authority.
	err = service.HandlePublishResult(context.Background(), PublishResult{
		OperationID: running.ID, CommandID: "standalone-publish-0001", Generation: 1,
		ChromiumRevision: "140", ManifestDigest: make([]byte, 32), Accepted: true,
		Probe:  ProbeResult{Phase: "publish", Result: "Authenticated", JourneyID: "authentication.url-prefix.v1", JourneyVersion: 1, CatalogDigest: running.CatalogDigest, CatalogVersion: running.CatalogVersion, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)},
		BootID: "lintel-boot", Epoch: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	published, err := service.GetStandaloneIdentity(context.Background(), identity.IdentityKey)
	if err != nil || published.State != "Ready" || published.Profile == nil || published.Profile.Generation != 1 {
		t.Fatalf("standalone publish must make the identity Ready: %#v %v", published, err)
	}
	// Post-publication replay reports AlreadyPublished instead of resending.
	final, err := service.PrepareStandalonePublish(context.Background(), identity.IdentityKey, running.ID, 1, running.RowVersion, "standalone-publish-0001")
	if err != nil || !final.AlreadyPublished {
		t.Fatalf("publish replay after the result must be AlreadyPublished: %#v %v", final, err)
	}
}
