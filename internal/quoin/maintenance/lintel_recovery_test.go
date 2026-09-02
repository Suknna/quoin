package maintenance_test

// T35 offline finalizer coverage: the first-auth fence, the retired domain
// closure (stop confirmations, identity demotion, never reclassifying old
// results), receipt idempotence and the rejection matrix.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/maintenance"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

const t35Release = "v0.1.0-dev"

func t35Digest(parts ...string) string {
	sum := sha256.Sum256([]byte(t35Join(parts)))
	return hex.EncodeToString(sum[:])
}

func t35Join(parts []string) string {
	out := ""
	for _, part := range parts {
		out += part
	}
	return out
}

// t35RecoveryFixture bootstraps a real database, drives the real runtime
// service through the recovery begin + registration, optionally performs the
// replacement's first Hello, seeds browser operations, and releases the
// directory lock so the offline finalizer can take exclusive ownership.
type t35Fixture struct {
	database        *bootstrap.Database
	service         *qruntime.Service
	request         maintenance.LintelRecoveryFinalizeRequest
	replacementLong string
}

func t35Begin(t *testing.T, fixture *t35Fixture) {
	t.Helper()
	fence := qruntime.LintelRecoveryFence{
		Backend: "compose", Disposition: fixture.request.Disposition,
		DispositionDigest: fixture.request.DispositionDigest,
		FenceReportDigest: fixture.request.FenceReportDigest,
	}
	begin, err := fixture.service.BeginLintelRecoveryRegistration(context.Background(), fence)
	if err != nil {
		t.Fatal(err)
	}
	longTerm, _, err := fixture.service.Register(context.Background(), "lintel", begin.RegistrationToken, begin.ReplacementGeneration, "boot-new", t35Release, t35Release)
	if err != nil {
		t.Fatal(err)
	}
	fixture.replacementLong = longTerm
}

func t35FirstHello(t *testing.T, fixture *t35Fixture) {
	t.Helper()
	decision, err := fixture.service.Adjudicate(context.Background(), fixture.replacementLong, "lintel", "boot-new", 1, t35Release, t35Release, catalog.Digest(), catalog.Digest())
	if err != nil || !decision.Accepted || !decision.MarkedFirstAuthenticated {
		t.Fatalf("replacement hello: decision=%+v err=%v", decision, err)
	}
}

func newT35Fixture(t *testing.T, disposition string) *t35Fixture {
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
	fixture := &t35Fixture{
		database: database,
		service:  qruntime.NewService(database.SQL),
		request: maintenance.LintelRecoveryFinalizeRequest{
			DataDirectory: config.DataDirectory, RootKeyFile: config.RootKeyFile, Disposition: disposition,
			DispositionDigest:    t35Digest("disposition", disposition),
			FenceReportDigest:    t35Digest("fence"),
			RecoveryReportDigest: t35Digest("recovery"),
			PostVerifyDigest:     t35Digest("post-verify"),
		},
	}
	// Old lintel credential: real registration + first Hello.
	ctx := context.Background()
	var session [32]byte
	_, handle, _, err := fixture.service.PrepareRegistration(ctx, "lintel", 1, session)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, generation, err := fixture.service.RevealToken(handle, session)
	if err != nil {
		t.Fatal(err)
	}
	oldLong, _, err := fixture.service.Register(ctx, "lintel", raw, generation, "boot-old", t35Release, t35Release)
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := fixture.service.Adjudicate(ctx, oldLong, "lintel", "boot-old", 1, t35Release, t35Release, catalog.Digest(), catalog.Digest()); err != nil || !decision.Accepted {
		t.Fatalf("old hello: %+v %v", decision, err)
	}
	return fixture
}

func (fixture *t35Fixture) close(t *testing.T) {
	t.Helper()
	if err := fixture.database.Close(); err != nil {
		t.Fatal(err)
	}
}

// seedBrowserOperations leaves one dispatched Running operation, one
// undispatched Queued operation, one terminal Failed operation without stop
// confirmation, and one already stop-confirmed Succeeded operation whose
// historic basis must never be reclassified.
func (fixture *t35Fixture) seedBrowserOperations(t *testing.T) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	d64 := t35Digest("catalog")
	db := fixture.database.SQL
	exec := func(query string, arguments ...any) {
		t.Helper()
		if _, err := db.Exec(query, arguments...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO users(id,username,display_name,role,enabled,password_phc,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,'x',?,?)`, now, now)
	exec(`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,?,1,'t35',?,?,?,?)`, make([]byte, 32), now, now, now, now)
	for id := 1; id <= 5; id++ {
		exec(`INSERT INTO business_systems(id,key,display_name,enabled,created_at) VALUES(?,?,'Payments',0,?)`, id, fmt.Sprintf("payments-%d", id), now)
		exec(`INSERT INTO browser_identity_revisions(id,business_system_id,revision,name,start_url,probe_journey_id,probe_journey_version,probe_params_json,journey_catalog_digest,journey_catalog_version,created_at) VALUES(?,?,?,'readonly','https://payments.example','authentication.url-prefix.v1',1,'{}',?,'v1',?)`, id, id, id, d64, now)
		exec(`INSERT INTO browser_identities(id,business_system_id,current_revision_id,state,created_at) VALUES(?,?,?,'AuthenticationRequired',?)`, id, id, id, now)
		exec(`INSERT INTO browser_operations(id,identity_id,identity_revision_id,kind,actor_user_id,actor_session_id,state,journey_catalog_digest,journey_catalog_version,requested_at) VALUES(?,?,?,'manual_login',1,1,'Queued',?,'v1',?)`, id, id, id, d64, now)
	}
	// A single FIFO-consistent history: op1 publishes identity 1 (Ready);
	// op2 is a terminal historic operation; op3 is terminal with an
	// unconfirmed stop; op4 is the stranded Running operation; op5 never
	// dispatched.
	dispatch := func(id int) {
		exec(`UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id='lintel-old',lintel_connection_epoch=3,row_version=row_version+1 WHERE id=?`, now, id)
		exec(`UPDATE browser_operations SET state='Running',started_at=?,row_version=row_version+1 WHERE id=?`, now, id)
	}
	dispatch(1)
	exec(`INSERT INTO browser_probe_results(operation_id,probe_seq,phase,identity_revision_id,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,result,observed_at) VALUES(1,1,'publish',1,'authentication.url-prefix.v1',1,?,'v1','Authenticated',?)`, d64, now)
	exec(`INSERT INTO browser_profile_generations(id,identity_id,identity_revision_id,generation,chromium_revision,profile_manifest_digest,probe_journey_id,probe_journey_version,probe_catalog_digest,probe_catalog_version,published_operation_id,published_by,published_at) VALUES(1,1,1,1,'chrome',?,'authentication.url-prefix.v1',1,?,'v1',1,1,?)`, d64, d64, now)
	exec(`UPDATE browser_operations SET stop_confirmed_at=?,stop_confirmation_basis='stop_ack',row_version=row_version+1 WHERE id=1`, now)
	dispatch(2)
	exec(`UPDATE browser_operations SET state='Cancelled',ended_at=?,terminal_reason='cancelled',row_version=row_version+1 WHERE id=2`, now)
	exec(`UPDATE browser_operations SET stop_confirmed_at=?,stop_confirmation_basis='stop_ack',row_version=row_version+1 WHERE id=2`, now)
	dispatch(3)
	exec(`UPDATE browser_operations SET state='Failed',ended_at=?,terminal_reason='browser_crashed',row_version=row_version+1 WHERE id=3`, now)
	dispatch(4)
}

func TestTicket35FinalizeRejectsBeforeReplacementFirstHello(t *testing.T) {
	fixture := newT35Fixture(t, "exclusively_reattached")
	t35Begin(t, fixture)
	fixture.close(t)
	_, err := maintenance.FinalizeLintelRecovery(context.Background(), fixture.request)
	if !errors.Is(err, maintenance.ErrLintelRecoveryNotReady) {
		t.Fatalf("error=%v, want not-ready", err)
	}
}

func TestTicket35FinalizeSequentialRecoveriesOnSameDatabase(t *testing.T) {
	// The second recovery must re-enter LintelRecovery on the SAME database
	// after the first finalizer exited maintenance: the enter transition has
	// to clear the previous exit's fields in the same UPDATE.
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
	open := func() (*bootstrap.Database, *qruntime.Service) {
		database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
		if err != nil {
			t.Fatal(err)
		}
		return database, qruntime.NewService(database.SQL)
	}
	// Seed the first credential once.
	{
		database, service := open()
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
		first, _, err := service.Register(ctx, "lintel", raw, generation, "boot-seed", t35Release, t35Release)
		if err != nil {
			t.Fatal(err)
		}
		if decision, err := service.Adjudicate(ctx, first, "lintel", "boot-seed", 1, t35Release, t35Release, catalog.Digest(), catalog.Digest()); err != nil || !decision.Accepted {
			t.Fatalf("seed hello: %+v %v", decision, err)
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for cycle, disposition := range []string{"exclusively_reattached", "retired"} {
		suffix := fmt.Sprintf("-%s-%d", disposition, cycle)
		fence := qruntime.LintelRecoveryFence{
			Backend: "compose", Disposition: disposition,
			DispositionDigest: t35Digest("disposition", suffix),
			FenceReportDigest: t35Digest("fence", suffix),
		}
		database, service := open()
		begin, err := service.BeginLintelRecoveryRegistration(context.Background(), fence)
		if err != nil {
			t.Fatalf("cycle %d begin: %v", cycle+1, err)
		}
		if !begin.NeedsRegistration {
			t.Fatalf("cycle %d expected a fresh registration", cycle+1)
		}
		longTerm, _, err := service.Register(context.Background(), "lintel", begin.RegistrationToken, begin.ReplacementGeneration, fmt.Sprintf("boot-%d", cycle), t35Release, t35Release)
		if err != nil {
			t.Fatalf("cycle %d register: %v", cycle+1, err)
		}
		if decision, err := service.Adjudicate(context.Background(), longTerm, "lintel", fmt.Sprintf("boot-%d", cycle), 1, t35Release, t35Release, catalog.Digest(), catalog.Digest()); err != nil || !decision.Accepted || !decision.MarkedFirstAuthenticated {
			t.Fatalf("cycle %d hello: %+v %v", cycle+1, decision, err)
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		result, err := maintenance.FinalizeLintelRecovery(context.Background(), maintenance.LintelRecoveryFinalizeRequest{
			DataDirectory: config.DataDirectory, RootKeyFile: config.RootKeyFile, Disposition: disposition,
			DispositionDigest:    t35Digest("disposition", suffix),
			FenceReportDigest:    t35Digest("fence", suffix),
			RecoveryReportDigest: t35Digest("recovery", suffix),
			PostVerifyDigest:     t35Digest("post-verify", suffix),
		})
		if err != nil {
			t.Fatalf("cycle %d finalize: %v", cycle+1, err)
		}
		if result.AlreadyFinalized || result.OldGeneration != int64(cycle+1) || result.ReplacementGeneration != int64(cycle+2) {
			t.Fatalf("cycle %d result=%+v", cycle+1, result)
		}
	}
}

func TestTicket35FinalizeExclusiveReattachAndReplay(t *testing.T) {
	fixture := newT35Fixture(t, "exclusively_reattached")
	t35Begin(t, fixture)
	t35FirstHello(t, fixture)
	fixture.close(t)
	result, err := maintenance.FinalizeLintelRecovery(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if result.AlreadyFinalized || result.OldGeneration != 1 || result.ReplacementGeneration != 2 || result.ClosedOperations != 0 {
		t.Fatalf("result=%+v", result)
	}
	// Maintenance exited; replay is idempotent.
	replay, err := maintenance.FinalizeLintelRecovery(context.Background(), fixture.request)
	if err != nil || !replay.AlreadyFinalized {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	// Same idempotence key with different evidence conflicts.
	conflicting := fixture.request
	conflicting.PostVerifyDigest = t35Digest("other-post-verify")
	if _, err := maintenance.FinalizeLintelRecovery(context.Background(), conflicting); !errors.Is(err, maintenance.ErrLintelRecoveryReceiptConflict) {
		t.Fatalf("conflict error=%v", err)
	}
}

func TestTicket35FinalizeRetiredClosesDomain(t *testing.T) {
	fixture := newT35Fixture(t, "retired")
	fixture.seedBrowserOperations(t)
	t35Begin(t, fixture)
	t35FirstHello(t, fixture)
	fixture.close(t)
	result, err := maintenance.FinalizeLintelRecovery(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if result.ClosedOperations != 3 || result.DemotedIdentities != 1 {
		t.Fatalf("result=%+v", result)
	}
	db := reopenReadonly(t, fixture)
	defer db.Close()
	rows := []struct {
		id     int
		state  string
		basis  sql.NullString
		reason sql.NullString
	}{}
	query, err := db.Query(`SELECT id,state,stop_confirmation_basis,terminal_reason FROM browser_operations ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for query.Next() {
		var row struct {
			id     int
			state  string
			basis  sql.NullString
			reason sql.NullString
		}
		if err := query.Scan(&row.id, &row.state, &row.basis, &row.reason); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	if err := query.Err(); err != nil {
		t.Fatal(err)
	}
	expected := map[int]struct {
		state  string
		basis  string
		reason string
	}{
		1: {"Succeeded", "stop_ack", ""},
		2: {"Cancelled", "stop_ack", "cancelled"},
		3: {"Failed", "externally_fenced_storage_retired", "browser_crashed"},
		4: {"Interrupted", "externally_fenced_storage_retired", "shutdown"},
		5: {"Interrupted", "not_dispatched", "shutdown"},
	}
	for _, row := range rows {
		want := expected[row.id]
		if row.state != want.state || row.basis.String != want.basis || row.reason.String != want.reason {
			t.Fatalf("operation %d=(%s,%s,%s), want (%s,%s,%s)", row.id, row.state, row.basis.String, row.reason.String, want.state, want.basis, want.reason)
		}
	}
	var identityState string
	if err := db.QueryRow(`SELECT state FROM browser_identities WHERE id=1`).Scan(&identityState); err != nil {
		t.Fatal(err)
	}
	if identityState != "AuthenticationRequired" {
		t.Fatalf("identity state=%s", identityState)
	}
	assertFinalizedAuthority(t, db, "retired", fixture.request)
}

func assertFinalizedAuthority(t *testing.T, db *sql.DB, disposition string, request maintenance.LintelRecoveryFinalizeRequest) {
	t.Helper()
	var active int
	var reason sql.NullString
	if err := db.QueryRow(`SELECT active,reason FROM maintenance_state WHERE id=1`).Scan(&active, &reason); err != nil {
		t.Fatal(err)
	}
	if active != 0 || reason.Valid {
		t.Fatalf("maintenance still active=%d reason=%v", active, reason)
	}
	var gotDisposition string
	var digests [4]string
	var oldRetired sql.NullString
	if err := db.QueryRow(`SELECT storage_disposition,disposition_digest,fence_report_digest,recovery_report_digest,post_verify_digest,(SELECT retired_at FROM runtime_credentials WHERE slot='lintel' AND generation=1) FROM lintel_recovery_receipts WHERE old_slot_id='lintel'`).Scan(&gotDisposition, &digests[0], &digests[1], &digests[2], &digests[3], &oldRetired); err != nil {
		t.Fatal(err)
	}
	if gotDisposition != disposition || digests[0] != request.DispositionDigest || digests[1] != request.FenceReportDigest || digests[2] != request.RecoveryReportDigest || digests[3] != request.PostVerifyDigest || !oldRetired.Valid {
		t.Fatalf("receipt=(%s,%v) oldRetired=%v", gotDisposition, digests, oldRetired.Valid)
	}
	var blocking int
	if err := db.QueryRow(`SELECT COUNT(*) FROM maintenance_items WHERE safe_state='Blocking'`).Scan(&blocking); err != nil {
		t.Fatal(err)
	}
	if blocking != 0 {
		t.Fatalf("blocking items=%d", blocking)
	}
}

func reopenReadonly(t *testing.T, fixture *t35Fixture) *sql.DB {
	t.Helper()
	database, err := bootstrap.OpenDatabase(context.Background(), fixture.request.DataDirectory, fixture.request.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	return database.SQL
}
