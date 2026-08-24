package browser

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	_ "modernc.org/sqlite"
)

func newBrowserTestService(t *testing.T) (*sql.DB, *Service) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/browser.db?_pragma=foreign_keys(1)")
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
		`INSERT INTO business_systems(id,key,display_name,enabled,created_at) VALUES(1,'payments','Payments',0,'` + now + `')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,zeroblob(32),1,'test','` + now + `','` + now + `','2030-01-01T00:00:00Z','2030-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return db, NewService(db)
}

func TestConfigureUsesSharedCatalogBytes(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity, op, err := service.Configure(context.Background(), 1, ConfigureInput{
		SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login",
		Probe:           ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)},
		ClientCommandID: "configure-identity-1",
	})
	if err != nil || op != nil {
		t.Fatalf("configure: identity=%#v operation=%#v err=%v", identity, op, err)
	}
	if identity.State != "AuthenticationRequired" || identity.Revision.CatalogDigest != catalog.Digest() || identity.Revision.CatalogVersion != catalog.Version {
		t.Fatalf("incorrect catalog provenance: %#v", identity)
	}
}

func TestConfigureRejectsUnknownFrozenJourneyVersion(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	_, _, err := service.Configure(context.Background(), 1, ConfigureInput{SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 2, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)}, ClientCommandID: "configure-identity-1"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown catalog journey version accepted: %v", err)
	}
}

func TestStartManualLoginMakesIdentityExclusivelyOccupied(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity, _, err := service.Configure(context.Background(), 1, ConfigureInput{
		SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login",
		Probe:           ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)},
		ClientCommandID: "configure-identity-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartManualLogin(context.Background(), "payments", 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "Queued" || op.ActorUserID == nil || *op.ActorUserID != 1 || op.ActorSessionID == nil || *op.ActorSessionID != 1 {
		t.Fatalf("manual login must retain only actor user metadata: %#v", op)
	}
	_, err = service.StartManualLogin(context.Background(), "payments", 1, 1, identity.RowVersion+1, "start-login-0002")
	if !errors.Is(err, ErrRowVersion) && !errors.Is(err, ErrConflict) {
		t.Fatalf("second start must be fenced, got %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM browser_operations WHERE kind='manual_login'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("manual login lock was not unique: count=%d err=%v", count, err)
	}
}

func TestPrepareDispatchClaimsFifoAndStartAckRuns(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity, _, err := service.Configure(context.Background(), 1, ConfigureInput{
		SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login",
		Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)}, ClientCommandID: "configure-identity-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartManualLogin(context.Background(), "payments", 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := service.PrepareDispatch(context.Background(), op.ID, "lintel-boot", 7)
	if err != nil {
		t.Fatalf("prepare dispatch: %v", err)
	}
	if dispatch.Kind != "manual_login" || dispatch.ActorUserID == nil || *dispatch.ActorUserID != 1 || dispatch.ActorSessionID == nil || *dispatch.ActorSessionID != 1 || dispatch.BootID != "lintel-boot" || dispatch.Epoch != 7 || len(dispatch.CanonicalJSON) == 0 {
		t.Fatalf("bad dispatch: %#v", dispatch)
	}
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := service.HandleStartAck(context.Background(), op.ID, "lintel-boot", 7, true, "", started); err != nil {
		t.Fatalf("start ack: %v", err)
	}
	updated, err := service.GetOperation(context.Background(), "payments", op.ID)
	if err != nil || updated.State != "Running" || updated.StartedAt == nil {
		t.Fatalf("ack did not run operation: %#v err=%v", updated, err)
	}
}

func TestRevokeSessionCancelsManualLogin(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity, _, err := service.Configure(context.Background(), 1, ConfigureInput{SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)}, ClientCommandID: "configure-identity-1"})
	if err != nil {
		t.Fatal(err)
	}
	op, err := service.StartManualLogin(context.Background(), "payments", 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := service.RevokeSession(context.Background(), 1)
	if err != nil || len(ids) != 1 || ids[0] != op.ID {
		t.Fatalf("revoke result ids=%v err=%v", ids, err)
	}
	current, err := service.GetOperation(context.Background(), "payments", op.ID)
	if err != nil || current.State != "Cancelled" || current.TerminalReason == nil || *current.TerminalReason != "session_revoked" {
		t.Fatalf("operation was not terminalized: %#v err=%v", current, err)
	}
}

func TestPrepareDispatchRejectsNotFifoHead(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity, _, err := service.Configure(context.Background(), 1, ConfigureInput{SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)}, ClientCommandID: "configure-identity-1"})
	if err != nil {
		t.Fatal(err)
	}
	service.Dispatch = func(context.Context, int64) error { return nil }
	first, err := service.StartManualLogin(context.Background(), "payments", 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	// A second live row for the same identity is forbidden; create a different
	// identity directly so the test isolates the global FIFO fence.
	if _, err := db.Exec(`INSERT INTO business_systems(id,key,display_name,enabled,created_at) VALUES(2,'orders','Orders',0,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	secondIdentity, _, err := service.Configure(context.Background(), 1, ConfigureInput{SystemKey: "orders", Name: "Orders", StartURL: "https://orders.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://orders.example.test/app"}`)}, ClientCommandID: "configure-identity-2"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.StartManualLogin(context.Background(), "orders", 1, 1, secondIdentity.RowVersion, "start-login-0002")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.PrepareDispatch(context.Background(), second.ID, "lintel-boot", 7); !errors.Is(err, ErrConflict) {
		t.Fatalf("later operation dispatched ahead of FIFO head: %v", err)
	}
	if _, err = service.PrepareDispatch(context.Background(), first.ID, "lintel-boot", 7); err != nil {
		t.Fatal(err)
	}
}

func TestPublishResultAtomicallyMakesIdentityReady(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity, _, err := service.Configure(context.Background(), 1, ConfigureInput{SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)}, ClientCommandID: "configure-identity-1"})
	if err != nil {
		t.Fatal(err)
	}
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartManualLogin(context.Background(), "payments", 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.PrepareDispatch(context.Background(), op.ID, "lintel-boot", 7); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err = service.HandleStartAck(context.Background(), op.ID, "lintel-boot", 7, true, "", started); err != nil {
		t.Fatal(err)
	}
	op, err = service.GetOperation(context.Background(), "payments", op.ID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := service.PreparePublish(context.Background(), "payments", op.ID, 1, op.RowVersion, "publish-login-0001")
	if err != nil {
		t.Fatalf("prepare publish: %v", err)
	}
	digest := make([]byte, 32)
	digest[0] = 1
	if err = service.HandlePublishResult(context.Background(), PublishResult{OperationID: op.ID, CommandID: request.CommandID, Generation: request.NewGeneration, ChromiumRevision: "Chromium 140", ManifestDigest: digest, Accepted: true, BootID: "lintel-boot", Epoch: 7, Probe: ProbeResult{Phase: "publish", Result: "Authenticated", JourneyID: "authentication.url-prefix.v1", JourneyVersion: 1, CatalogDigest: identity.Revision.CatalogDigest, CatalogVersion: identity.Revision.CatalogVersion, ObservedAt: started.Format(time.RFC3339Nano)}}); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	ready, err := service.GetIdentity(context.Background(), "payments")
	if err != nil || ready.State != "Ready" || ready.Profile == nil {
		t.Fatalf("profile publication did not atomically make identity ready: %#v err=%v", ready, err)
	}
	final, err := service.GetOperation(context.Background(), "payments", op.ID)
	if err != nil || final.State != "Succeeded" {
		t.Fatalf("operation was not succeeded: %#v err=%v", final, err)
	}
}

func TestPublishResultCompletesAnAwaitingReconnectManualLogin(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity, _, err := service.Configure(context.Background(), 1, ConfigureInput{SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)}, ClientCommandID: "configure-identity-1"})
	if err != nil {
		t.Fatal(err)
	}
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartManualLogin(context.Background(), "payments", 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.PrepareDispatch(context.Background(), op.ID, "lintel-boot", 7); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err = service.HandleStartAck(context.Background(), op.ID, "lintel-boot", 7, true, "", started); err != nil {
		t.Fatal(err)
	}
	op, err = service.GetOperation(context.Background(), "payments", op.ID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := service.PreparePublish(context.Background(), "payments", op.ID, 1, op.RowVersion, "publish-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.AwaitReconnect(context.Background(), op.ID); err != nil {
		t.Fatal(err)
	}
	digest := make([]byte, 32)
	digest[0] = 1
	if err = service.HandlePublishResult(context.Background(), PublishResult{OperationID: op.ID, CommandID: request.CommandID, Generation: request.NewGeneration, ChromiumRevision: "Chromium 140", ManifestDigest: digest, Accepted: true, BootID: "lintel-boot", Epoch: 7, Probe: ProbeResult{Phase: "publish", Result: "Authenticated", JourneyID: "authentication.url-prefix.v1", JourneyVersion: 1, CatalogDigest: identity.Revision.CatalogDigest, CatalogVersion: identity.Revision.CatalogVersion, ObservedAt: started.Format(time.RFC3339Nano)}}); err != nil {
		t.Fatalf("publish result after websocket loss: %v", err)
	}
	final, err := service.GetOperation(context.Background(), "payments", op.ID)
	if err != nil || final.State != "Succeeded" || final.ReconnectDeadline != nil {
		t.Fatalf("awaiting reconnect publish did not complete: %#v err=%v", final, err)
	}
}

func TestInterruptOldBootRunningRetainsCleanupFence(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity, _, err := service.Configure(context.Background(), 1, ConfigureInput{SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)}, ClientCommandID: "configure-identity-1"})
	if err != nil {
		t.Fatal(err)
	}
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartManualLogin(context.Background(), "payments", 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareDispatch(context.Background(), op.ID, "old-boot", 1); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleStartAck(context.Background(), op.ID, "old-boot", 1, true, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	ids, err := service.InterruptOldBootOperations(context.Background(), "new-boot", 1)
	if err != nil || len(ids) != 1 || ids[0] != op.ID {
		t.Fatalf("interrupt running: ids=%v err=%v", ids, err)
	}
	interrupted, err := service.GetOperation(context.Background(), "payments", op.ID)
	if err != nil || interrupted.State != "Interrupted" || interrupted.TerminalReason == nil || *interrupted.TerminalReason != "new_boot" || interrupted.StopConfirmedAt != nil {
		t.Fatalf("running operation must be interrupted but remain fenced: %#v err=%v", interrupted, err)
	}
	if _, err := service.StartManualLogin(context.Background(), "payments", 1, 1, identity.RowVersion, "start-login-0002"); !errors.Is(err, ErrConflict) {
		t.Fatalf("new login escaped physical cleanup fence: %v", err)
	}
	stop, err := service.PrepareStopForBoot(context.Background(), op.ID, "new-boot", 1)
	if err != nil || stop.BootID != "new-boot" || stop.Epoch != 1 {
		t.Fatalf("cleanup must bind successor boot: %#v err=%v", stop, err)
	}
	if err := service.HandleStopAck(context.Background(), op.ID, "new-boot", 1, true, time.Now(), make([]byte, 32)); err != nil {
		t.Fatalf("successor cleanup ack: %v", err)
	}
	cleaned, err := service.GetOperation(context.Background(), "payments", op.ID)
	if err != nil || cleaned.StopConfirmedAt == nil || cleaned.StopConfirmationBasis == nil || *cleaned.StopConfirmationBasis != "new_boot_cleanup_confirmed" {
		t.Fatalf("successor cleanup proof was not persisted: %#v err=%v", cleaned, err)
	}
}

func TestInterruptOldBootStartingRetainsCleanupFence(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity, _, err := service.Configure(context.Background(), 1, ConfigureInput{SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)}, ClientCommandID: "configure-identity-1"})
	if err != nil {
		t.Fatal(err)
	}
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartManualLogin(context.Background(), "payments", 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareDispatch(context.Background(), op.ID, "old-boot", 4); err != nil {
		t.Fatal(err)
	}
	ids, err := service.InterruptOldBootOperations(context.Background(), "new-boot", 1)
	if err != nil || len(ids) != 1 || ids[0] != op.ID {
		t.Fatalf("interrupt starting: ids=%v err=%v", ids, err)
	}
	interrupted, err := service.GetOperation(context.Background(), "payments", op.ID)
	if err != nil || interrupted.State != "Interrupted" || interrupted.TerminalReason == nil || *interrupted.TerminalReason != "new_boot" || interrupted.StopConfirmedAt != nil {
		t.Fatalf("starting operation must remain cleanup-fenced: %#v err=%v", interrupted, err)
	}
	stop, err := service.PrepareStopForBoot(context.Background(), op.ID, "new-boot", 1)
	if err != nil || stop.BootID != "new-boot" || stop.Epoch != 1 {
		t.Fatalf("cleanup must bind successor boot: %#v err=%v", stop, err)
	}
}

func TestManualLoginReconnectGraceTransitionsAndExpires(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	identity, _, err := service.Configure(context.Background(), 1, ConfigureInput{SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)}, ClientCommandID: "configure-identity-1"})
	if err != nil {
		t.Fatal(err)
	}
	op, err := service.StartManualLogin(context.Background(), "payments", 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareDispatch(context.Background(), op.ID, "boot", 1); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleStartAck(context.Background(), op.ID, "boot", 1, true, "", now); err != nil {
		t.Fatal(err)
	}
	deadline, err := service.AwaitReconnect(context.Background(), op.ID)
	if err != nil || !deadline.Equal(now.Add(ReconnectGrace)) {
		t.Fatalf("await reconnect deadline=%s err=%v", deadline, err)
	}
	awaiting, err := service.GetOperation(context.Background(), "payments", op.ID)
	if err != nil || awaiting.State != "AwaitingReconnect" || awaiting.ReconnectDeadline == nil {
		t.Fatalf("missing reconnect projection: %#v err=%v", awaiting, err)
	}
	if err := service.ResumeReconnect(context.Background(), op.ID); err != nil {
		t.Fatalf("same boot reattach: %v", err)
	}
	if expired, err := service.ExpireReconnect(context.Background(), op.ID); err != nil || expired {
		t.Fatalf("reattached operation must not expire: expired=%v err=%v", expired, err)
	}
	if _, err := service.AwaitReconnect(context.Background(), op.ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(ReconnectGrace)
	expired, err := service.ExpireReconnect(context.Background(), op.ID)
	if err != nil || !expired {
		t.Fatalf("grace must expire: expired=%v err=%v", expired, err)
	}
	terminal, err := service.GetOperation(context.Background(), "payments", op.ID)
	if err != nil || terminal.State != "Cancelled" || terminal.TerminalReason == nil || *terminal.TerminalReason != "grace_expired" {
		t.Fatalf("grace expiry did not terminalize operation: %#v err=%v", terminal, err)
	}
}

func TestTicket21PrepareDispatchWaitsAtGlobalCapacity(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	firstIdentity, _, err := service.Configure(context.Background(), 1, ConfigureInput{SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)}, ClientCommandID: "configure-identity-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO business_systems(id,key,display_name,enabled,created_at) VALUES(2,'orders','Orders',0,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	secondIdentity, _, err := service.Configure(context.Background(), 1, ConfigureInput{SystemKey: "orders", Name: "Orders", StartURL: "https://orders.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://orders.example.test/app"}`)}, ClientCommandID: "configure-identity-2"})
	if err != nil {
		t.Fatal(err)
	}
	service.Dispatch = func(context.Context, int64) error { return nil }
	first, err := service.StartManualLogin(context.Background(), "payments", 1, 1, firstIdentity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.StartManualLogin(context.Background(), "orders", 1, 1, secondIdentity.RowVersion, "start-login-0002")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareDispatchWithCapacity(context.Background(), first.ID, "lintel-boot", 7, 1); err != nil {
		t.Fatalf("dispatch first operation: %v", err)
	}
	if err := service.HandleStartAck(context.Background(), first.ID, "lintel-boot", 7, true, "", time.Now()); err != nil {
		t.Fatalf("ack first operation: %v", err)
	}
	if _, err := service.PrepareDispatchWithCapacity(context.Background(), second.ID, "lintel-boot", 7, 1); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("second operation must wait for Quoin's global capacity decision, got %v", err)
	}
	waiting, err := service.GetOperation(context.Background(), "orders", second.ID)
	if err != nil || waiting.State != "WaitingForCapacity" || waiting.StartDispatchedAt != nil {
		t.Fatalf("capacity wait must be durable without a Start side effect: %#v err=%v", waiting, err)
	}
}

func TestTicket21StartAckReplayUsesOriginalSameBootFence(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity, _, err := service.Configure(context.Background(), 1, ConfigureInput{SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)}, ClientCommandID: "configure-identity-1"})
	if err != nil {
		t.Fatal(err)
	}
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartManualLogin(context.Background(), "payments", 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.PrepareDispatchWithCapacity(context.Background(), op.ID, "lintel-boot", 7, 1)
	if err != nil || first.OperationID != op.ID {
		t.Fatalf("first dispatch: input=%#v err=%v", first, err)
	}
	replayed, err := service.PrepareDispatchWithCapacity(context.Background(), op.ID, "lintel-boot", 8, 1)
	if err != nil || replayed.BootID != "lintel-boot" || replayed.Epoch != 8 {
		t.Fatalf("same-boot Start replay must target the new stream: %#v err=%v", replayed, err)
	}
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := service.HandleStartAck(context.Background(), op.ID, "lintel-boot", 8, true, "", started); err != nil {
		t.Fatalf("replayed ack: %v", err)
	}
	if err := service.HandleStartAck(context.Background(), op.ID, "lintel-boot", 8, true, "", started); err != nil {
		t.Fatalf("duplicate ack must be idempotent: %v", err)
	}
	if err := service.HandleStartAck(context.Background(), op.ID, "other-boot", 8, true, "", started); !errors.Is(err, ErrConflict) {
		t.Fatalf("new boot ack bypassed original fence: %v", err)
	}
	if err := service.HandleStartAck(context.Background(), op.ID, "lintel-boot", 6, true, "", started); !errors.Is(err, ErrConflict) {
		t.Fatalf("older epoch ack bypassed original fence: %v", err)
	}
}

func TestTicket21CapacityRetryRebindsAfterNewBoot(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity, _, err := service.Configure(context.Background(), 1, ConfigureInput{SystemKey: "payments", Name: "Payments", StartURL: "https://payments.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)}, ClientCommandID: "configure-identity-1"})
	if err != nil {
		t.Fatal(err)
	}
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartManualLogin(context.Background(), "payments", 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareDispatchWithCapacity(context.Background(), op.ID, "boot-a", 7, 1); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if err := service.HandleStartAck(context.Background(), op.ID, "boot-a", 7, false, "no_capacity", time.Time{}); err != nil {
		t.Fatalf("no-capacity acknowledgement: %v", err)
	}
	waiting, err := service.GetOperation(context.Background(), "payments", op.ID)
	if err != nil || waiting.State != "WaitingForCapacity" {
		t.Fatalf("no-capacity acknowledgement must durably wait: %#v err=%v", waiting, err)
	}
	retry, err := service.PrepareDispatchWithCapacity(context.Background(), op.ID, "boot-b", 1, 1)
	if err != nil || retry.BootID != "boot-b" || retry.Epoch != 1 {
		t.Fatalf("new boot must rebind an unstarted capacity retry: %#v err=%v", retry, err)
	}
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := service.HandleStartAck(context.Background(), op.ID, "boot-b", 1, true, "", started); err != nil {
		t.Fatalf("new boot acknowledgement must pass its replacement fence: %v", err)
	}
	got, err := service.GetOperation(context.Background(), "payments", op.ID)
	if err != nil || got.State != "Running" {
		t.Fatalf("new boot capacity retry must reach Running: %#v err=%v", got, err)
	}
}
