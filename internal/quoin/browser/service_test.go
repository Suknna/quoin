package browser

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// unboundedCapacity reproduces the retired unlimited-capacity dispatch seam
// for focused durable-state tests: production dispatch always passes the
// Lintel Hello-frozen slot total.
const unboundedCapacity = ^uint32(0)

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
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,zeroblob(32),1,'test','` + now + `','` + now + `','2030-01-01T00:00:00Z','2030-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return db, NewService(db)
}

// userCommandContext is the trusted execution metadata an HTTP cancel command
// carries: the acting user principal on the http channel under one trusted
// correlation.
func userCommandContext(t *testing.T) context.Context {
	t.Helper()
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlationID,
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "browser-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func configureStandalonePaymentsIdentity(t *testing.T, service *Service, name, startURL, urlPrefix, commandID string) Identity {
	t.Helper()
	identity, _, err := service.ConfigureStandalone(context.Background(), 1, StandaloneConfigureInput{
		Name: name, StartURL: startURL,
		Probe:           ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: []byte(`{"authenticatedUrlPrefix":"` + urlPrefix + `"}`)},
		ClientCommandID: commandID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func configurePayments(t *testing.T, service *Service) Identity {
	t.Helper()
	return configureStandalonePaymentsIdentity(t, service, "Payments", "https://payments.example.test/login", "https://payments.example.test/app", "configure-payments-1")
}

func TestStandaloneConfigureUsesSharedCatalogBytes(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity, op, err := service.ConfigureStandalone(context.Background(), 1, StandaloneConfigureInput{
		Name: "Payments", StartURL: "https://payments.example.test/login",
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

func TestStandaloneConfigureRejectsUnknownFrozenJourneyVersion(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	_, _, err := service.ConfigureStandalone(context.Background(), 1, StandaloneConfigureInput{Name: "Payments", StartURL: "https://payments.example.test/login", Probe: ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 2, Params: []byte(`{"authenticatedUrlPrefix":"https://payments.example.test/app"}`)}, ClientCommandID: "configure-identity-1"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown catalog journey version accepted: %v", err)
	}
}

func TestStandaloneLoginMakesIdentityExclusivelyOccupied(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity := configurePayments(t, service)
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "Queued" || op.ActorUserID == nil || *op.ActorUserID != 1 || op.ActorSessionID == nil || *op.ActorSessionID != 1 {
		t.Fatalf("manual login must retain only actor user metadata: %#v", op)
	}
	_, err = service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion+1, "start-login-0002")
	if !errors.Is(err, ErrRowVersion) && !errors.Is(err, ErrConflict) {
		t.Fatalf("second start must be fenced, got %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM browser_operations WHERE kind='manual_login'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("manual login lock was not unique: count=%d err=%v", count, err)
	}
}

func TestPrepareStopForBootTargetsUnknownStartingOutcome(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity := configurePayments(t, service)
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.PrepareDispatchWithCapacity(context.Background(), op.ID, "lintel-boot", 7, unboundedCapacity); err != nil {
		t.Fatal(err)
	}
	stop, err := service.PrepareStopForBoot(context.Background(), op.ID, "lintel-boot", 7)
	if err != nil || stop.OperationID != op.ID || stop.BootID != "lintel-boot" || stop.Epoch != 7 {
		t.Fatalf("unknown Start outcome did not produce typed Stop: %#v err=%v", stop, err)
	}
}

func TestInitialDownloadStartRejectionIsPersisted(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity := configurePayments(t, service)
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.PrepareDispatchWithCapacity(context.Background(), op.ID, "lintel-boot", 7, unboundedCapacity); err != nil {
		t.Fatal(err)
	}
	if err = service.HandleStartAck(context.Background(), op.ID, "lintel-boot", 7, false, "download_blocked", time.Time{}); err != nil {
		t.Fatalf("persist initial download rejection: %v", err)
	}
	var state, rejected string
	if err := db.QueryRow(`SELECT state,start_reject_reason FROM browser_operations WHERE id=?`, op.ID).Scan(&state, &rejected); err != nil {
		t.Fatal(err)
	}
	if state != "Failed" || rejected != "download_blocked" {
		t.Fatalf("initial download rejection state=%q reason=%q", state, rejected)
	}
}

func TestPrepareDispatchClaimsFifoAndStartAckRuns(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity := configurePayments(t, service)
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := service.PrepareDispatchWithCapacity(context.Background(), op.ID, "lintel-boot", 7, unboundedCapacity)
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
	updated, err := service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
	if err != nil || updated.State != "Running" || updated.StartedAt == nil {
		t.Fatalf("ack did not run operation: %#v err=%v", updated, err)
	}
}

func TestRevokeSessionCancelsManualLogin(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity := configurePayments(t, service)
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := service.RevokeSession(context.Background(), 1)
	if err != nil || len(ids) != 1 || ids[0] != op.ID {
		t.Fatalf("revoke result ids=%v err=%v", ids, err)
	}
	current, err := service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
	if err != nil || current.State != "Cancelled" || current.TerminalReason == nil || *current.TerminalReason != "session_revoked" {
		t.Fatalf("operation was not terminalized: %#v err=%v", current, err)
	}
	// The drain is a declared browser.revoke_session execution: the automatic
	// audit row commits inside the same runner transaction as the cancellation,
	// under the system drain principal with the revoked session as the object.
	var actorType, outcome string
	var actorID int64
	if err := db.QueryRow(`SELECT actor_type,actor_id,outcome FROM audit_events WHERE action=?`, OpRevokeSession).Scan(&actorType, &actorID, &outcome); err != nil {
		t.Fatalf("revocation drain must be automatically audited: %v", err)
	}
	if actorType != "system" || actorID != 0 || outcome != "success" {
		t.Fatalf("unexpected drain audit actor/outcome: %s/%d %s", actorType, actorID, outcome)
	}
	var targetType string
	var targetID int64
	if err := db.QueryRow(`SELECT target_type,target_id FROM audit_event_targets WHERE audit_event_id=(SELECT id FROM audit_events WHERE action=?)`, OpRevokeSession).Scan(&targetType, &targetID); err != nil {
		t.Fatalf("drain audit target: %v", err)
	}
	if targetType != "auth_session" || targetID != 1 {
		t.Fatalf("drain audit must reference the revoked session: %s/%d", targetType, targetID)
	}
}

func TestRevokeSessionWithoutLiveOperationsRecordsUnchangedDrain(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	// A revocation whose session owns no browser operation is still one
	// recorded drain execution; the runner records it honestly.
	if _, err := service.RevokeSession(context.Background(), 1); err != nil {
		t.Fatalf("drain without operations: %v", err)
	}
	var recorded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action=? AND outcome='success'`, OpRevokeSession).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("empty drain must still be recorded once: %d %v", recorded, err)
	}
}

func TestCancelStandaloneRecordsAutomaticAuditAndLedger(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity := configurePayments(t, service)
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	ctx := userCommandContext(t)
	cancelled, err := service.CancelStandalone(ctx, identity.IdentityKey, op.ID, 1, op.RowVersion, "standalone-cancel-audit-1")
	if err != nil || cancelled.State != "Cancelled" {
		t.Fatalf("cancel: %#v %v", cancelled, err)
	}
	// The automatic audit row is the runner's, bound to the acting user and
	// the cancelled operation.
	var actorType, outcome string
	var actorID, objectID int64
	if err := db.QueryRow(`SELECT actor_type,actor_id,outcome,domain_ref_id FROM audit_events WHERE action=?`, OpCancelOperation).Scan(&actorType, &actorID, &outcome, &objectID); err != nil {
		t.Fatalf("cancel must be automatically audited: %v", err)
	}
	if actorType != "user" || actorID != 1 || outcome != "success" || objectID != op.ID {
		t.Fatalf("unexpected cancel audit: %s/%d %s object=%d", actorType, actorID, outcome, objectID)
	}
	// The durable command ledger row is the shared one: replay goes through
	// the runner, not the local helper.
	var ledgerOutcome string
	if err := db.QueryRow(`SELECT outcome FROM client_commands WHERE principal_type='user' AND principal_id=1 AND client_command_id='standalone-cancel-audit-1'`).Scan(&ledgerOutcome); err != nil || ledgerOutcome != "committed" {
		t.Fatalf("cancel ledger row: %v", err)
	}
	// A replay returns the stored outcome without a second audit row.
	if _, err = service.CancelStandalone(ctx, identity.IdentityKey, op.ID, 1, op.RowVersion, "standalone-cancel-audit-1"); err != nil {
		t.Fatalf("cancel replay: %v", err)
	}
	var auditRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action=?`, OpCancelOperation).Scan(&auditRows); err != nil || auditRows != 1 {
		t.Fatalf("replay must not duplicate the audit record: %d %v", auditRows, err)
	}
}

func TestPrepareDispatchRejectsNotFifoHead(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	first := configurePayments(t, service)
	second := configureStandalonePaymentsIdentity(t, service, "Orders", "https://orders.example.test/login", "https://orders.example.test/app", "configure-orders-1")
	service.Dispatch = func(context.Context, int64) error { return nil }
	firstOp, err := service.StartStandaloneManualLogin(context.Background(), first.IdentityKey, 1, 1, first.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	secondOp, err := service.StartStandaloneManualLogin(context.Background(), second.IdentityKey, 1, 1, second.RowVersion, "start-login-0002")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.PrepareDispatchWithCapacity(context.Background(), secondOp.ID, "lintel-boot", 7, unboundedCapacity); !errors.Is(err, ErrConflict) {
		t.Fatalf("later operation dispatched ahead of FIFO head: %v", err)
	}
	if _, err = service.PrepareDispatchWithCapacity(context.Background(), firstOp.ID, "lintel-boot", 7, unboundedCapacity); err != nil {
		t.Fatal(err)
	}
}

func TestPublishResultAtomicallyMakesIdentityReady(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity := configurePayments(t, service)
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.PrepareDispatchWithCapacity(context.Background(), op.ID, "lintel-boot", 7, unboundedCapacity); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err = service.HandleStartAck(context.Background(), op.ID, "lintel-boot", 7, true, "", started); err != nil {
		t.Fatal(err)
	}
	op, err = service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := service.PrepareStandalonePublish(context.Background(), identity.IdentityKey, op.ID, 1, op.RowVersion, "publish-login-0001")
	if err != nil {
		t.Fatalf("prepare publish: %v", err)
	}
	digest := make([]byte, 32)
	digest[0] = 1
	if err = service.HandlePublishResult(context.Background(), PublishResult{OperationID: op.ID, CommandID: request.CommandID, Generation: request.NewGeneration, ChromiumRevision: "Chromium 140", ManifestDigest: digest, Accepted: true, BootID: "lintel-boot", Epoch: 7, Probe: ProbeResult{Phase: "publish", Result: "Authenticated", JourneyID: "authentication.url-prefix.v1", JourneyVersion: 1, CatalogDigest: identity.Revision.CatalogDigest, CatalogVersion: identity.Revision.CatalogVersion, ObservedAt: started.Format(time.RFC3339Nano)}}); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	ready, err := service.GetStandaloneIdentity(context.Background(), identity.IdentityKey)
	if err != nil || ready.State != "Ready" || ready.Profile == nil {
		t.Fatalf("profile publication did not atomically make identity ready: %#v err=%v", ready, err)
	}
	final, err := service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
	if err != nil || final.State != "Succeeded" {
		t.Fatalf("operation was not succeeded: %#v err=%v", final, err)
	}
}

func TestPublishResultCompletesAnAwaitingReconnectManualLogin(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity := configurePayments(t, service)
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.PrepareDispatchWithCapacity(context.Background(), op.ID, "lintel-boot", 7, unboundedCapacity); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err = service.HandleStartAck(context.Background(), op.ID, "lintel-boot", 7, true, "", started); err != nil {
		t.Fatal(err)
	}
	op, err = service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := service.PrepareStandalonePublish(context.Background(), identity.IdentityKey, op.ID, 1, op.RowVersion, "publish-login-0001")
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
	final, err := service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
	if err != nil || final.State != "Succeeded" || final.ReconnectDeadline != nil {
		t.Fatalf("awaiting reconnect publish did not complete: %#v err=%v", final, err)
	}
}

func TestInterruptOldBootRunningRetainsCleanupFence(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity := configurePayments(t, service)
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareDispatchWithCapacity(context.Background(), op.ID, "old-boot", 1, unboundedCapacity); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleStartAck(context.Background(), op.ID, "old-boot", 1, true, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	ids, err := service.InterruptOldBootOperations(context.Background(), "new-boot", 1)
	if err != nil || len(ids) != 1 || ids[0] != op.ID {
		t.Fatalf("interrupt running: ids=%v err=%v", ids, err)
	}
	interrupted, err := service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
	if err != nil || interrupted.State != "Interrupted" || interrupted.TerminalReason == nil || *interrupted.TerminalReason != "new_boot" || interrupted.StopConfirmedAt != nil {
		t.Fatalf("running operation must be interrupted but remain fenced: %#v err=%v", interrupted, err)
	}
	if _, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0002"); !errors.Is(err, ErrConflict) {
		t.Fatalf("new login escaped physical cleanup fence: %v", err)
	}
	stop, err := service.PrepareStopForBoot(context.Background(), op.ID, "new-boot", 1)
	if err != nil || stop.BootID != "new-boot" || stop.Epoch != 1 {
		t.Fatalf("cleanup must bind successor boot: %#v err=%v", stop, err)
	}
	if err := service.HandleStopAck(context.Background(), op.ID, "new-boot", 1, true, time.Now(), make([]byte, 32)); err != nil {
		t.Fatalf("successor cleanup ack: %v", err)
	}
	cleaned, err := service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
	if err != nil || cleaned.StopConfirmedAt == nil || cleaned.StopConfirmationBasis == nil || *cleaned.StopConfirmationBasis != "new_boot_cleanup_confirmed" {
		t.Fatalf("successor cleanup proof was not persisted: %#v err=%v", cleaned, err)
	}
	// The physical StopAck is the release fence: only after its durable
	// stop_confirmed_at may another operation acquire the Browser Identity.
	if _, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-after-cleanup"); err != nil {
		t.Fatalf("StopAck did not release identity: %v", err)
	}
}

func TestInterruptOldBootStartingRetainsCleanupFence(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity := configurePayments(t, service)
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareDispatchWithCapacity(context.Background(), op.ID, "old-boot", 4, unboundedCapacity); err != nil {
		t.Fatal(err)
	}
	ids, err := service.InterruptOldBootOperations(context.Background(), "new-boot", 1)
	if err != nil || len(ids) != 1 || ids[0] != op.ID {
		t.Fatalf("interrupt starting: ids=%v err=%v", ids, err)
	}
	interrupted, err := service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
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
	identity := configurePayments(t, service)
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareDispatchWithCapacity(context.Background(), op.ID, "boot", 1, unboundedCapacity); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleStartAck(context.Background(), op.ID, "boot", 1, true, "", now); err != nil {
		t.Fatal(err)
	}
	deadline, err := service.AwaitReconnect(context.Background(), op.ID)
	if err != nil || !deadline.Equal(now.Add(ReconnectGrace)) {
		t.Fatalf("await reconnect deadline=%s err=%v", deadline, err)
	}
	awaiting, err := service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
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
	terminal, err := service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
	if err != nil || terminal.State != "Cancelled" || terminal.TerminalReason == nil || *terminal.TerminalReason != "grace_expired" {
		t.Fatalf("grace expiry did not terminalize operation: %#v err=%v", terminal, err)
	}
}

func TestTicket21PrepareDispatchWaitsAtGlobalCapacity(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	first := configurePayments(t, service)
	second := configureStandalonePaymentsIdentity(t, service, "Orders", "https://orders.example.test/login", "https://orders.example.test/app", "configure-orders-1")
	service.Dispatch = func(context.Context, int64) error { return nil }
	firstOp, err := service.StartStandaloneManualLogin(context.Background(), first.IdentityKey, 1, 1, first.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	secondOp, err := service.StartStandaloneManualLogin(context.Background(), second.IdentityKey, 1, 1, second.RowVersion, "start-login-0002")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareDispatchWithCapacity(context.Background(), firstOp.ID, "lintel-boot", 7, 1); err != nil {
		t.Fatalf("dispatch first operation: %v", err)
	}
	if err := service.HandleStartAck(context.Background(), firstOp.ID, "lintel-boot", 7, true, "", time.Now()); err != nil {
		t.Fatalf("ack first operation: %v", err)
	}
	if _, err := service.PrepareDispatchWithCapacity(context.Background(), secondOp.ID, "lintel-boot", 7, 1); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("second operation must wait for Quoin's global capacity decision, got %v", err)
	}
	waiting, err := service.GetStandaloneOperation(context.Background(), second.IdentityKey, secondOp.ID)
	if err != nil || waiting.State != "WaitingForCapacity" || waiting.StartDispatchedAt != nil {
		t.Fatalf("capacity wait must be durable without a Start side effect: %#v err=%v", waiting, err)
	}
}

func TestTicket21StartAckReplayUsesOriginalSameBootFence(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity := configurePayments(t, service)
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
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
	identity := configurePayments(t, service)
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareDispatchWithCapacity(context.Background(), op.ID, "boot-a", 7, 1); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if err := service.HandleStartAck(context.Background(), op.ID, "boot-a", 7, false, "no_capacity", time.Time{}); err != nil {
		t.Fatalf("no-capacity acknowledgement: %v", err)
	}
	waiting, err := service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
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
	got, err := service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
	if err != nil || got.State != "Running" {
		t.Fatalf("new boot capacity retry must reach Running: %#v err=%v", got, err)
	}
}

func TestInterruptForQuoinRestartInterruptsSameLintelBoot(t *testing.T) {
	db, service := newBrowserTestService(t)
	defer db.Close()
	identity := configurePayments(t, service)
	service.Dispatch = func(context.Context, int64) error { return nil }
	op, err := service.StartStandaloneManualLogin(context.Background(), identity.IdentityKey, 1, 1, identity.RowVersion, "start-login-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareDispatchWithCapacity(context.Background(), op.ID, "same-lintel-boot", 1, unboundedCapacity); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleStartAck(context.Background(), op.ID, "same-lintel-boot", 1, true, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	ids, err := service.InterruptForQuoinRestart(context.Background(), "same-lintel-boot")
	if err != nil || len(ids) != 1 || ids[0] != op.ID {
		t.Fatalf("interrupt after Quoin restart: ids=%v err=%v", ids, err)
	}
	interrupted, err := service.GetStandaloneOperation(context.Background(), identity.IdentityKey, op.ID)
	if err != nil || interrupted.State != "Interrupted" || interrupted.TerminalReason == nil || *interrupted.TerminalReason != "shutdown" || interrupted.StopConfirmedAt != nil {
		t.Fatalf("same-boot operation must be interrupted and remain cleanup fenced: %#v err=%v", interrupted, err)
	}
}

func TestProfileUnavailableTerminalReasonUsesFrozenOperationGeneration(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/profile-reason.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE browser_operations (id INTEGER PRIMARY KEY, profile_generation_id INTEGER);
		CREATE TABLE browser_profile_reconciliations (
			id INTEGER PRIMARY KEY, boot_id TEXT, connection_epoch INTEGER,
			profile_generation_id INTEGER, result TEXT
		);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO browser_operations(id,profile_generation_id) VALUES(7,101);
		INSERT INTO browser_profile_reconciliations(id,boot_id,connection_epoch,profile_generation_id,result) VALUES
			(1,'lintel',4,100,'compatible'),
			(2,'lintel',4,101,'manifest_invalid'),
			(3,'lintel',5,101,'chromium_revision_mismatch')`); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := profileUnavailableTerminalReason(context.Background(), conn, 7, "lintel", 4); got != "profile_manifest_invalid" {
		t.Fatalf("manifest-invalid operation profile classified as %q", got)
	}
	if got := profileUnavailableTerminalReason(context.Background(), conn, 7, "lintel", 5); got != "chromium_revision_mismatch" {
		t.Fatalf("latest matching inventory reason=%q", got)
	}
	if got := profileUnavailableTerminalReason(context.Background(), conn, 7, "other-boot", 5); got != "profile_missing" {
		t.Fatalf("unobserved boot must remain conservatively missing, got %q", got)
	}
}
