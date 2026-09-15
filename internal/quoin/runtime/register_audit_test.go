package runtime_test

// Automatic audit coverage for the runtime-initiated registration (ADR-0006):
// the one-time token is the registration authority, the consumed token carries
// the issuing operation's correlation and initiator, and the confirmation runs
// through the shared runner so success and deterministic rejection commit their
// audit facts in the same transaction.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/execution"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

// auditedService bootstraps the slot authority exactly like newService and
// additionally returns the raw database so the tests inspect the audit facts
// the runner persisted.
func auditedService(t *testing.T) (*qruntime.Service, *sql.DB, context.Context) {
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
	seedAdminSession(t, database.SQL)
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "runtime-" + t.Name(),
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-" + t.Name()},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := qruntime.NewServiceWithReader(database.SQL, database.Reader, execution.NewRunner(database.SQL, execution.NewRegistry(), nil))
	if err != nil {
		t.Fatal(err)
	}
	return service, database.SQL, ctx
}

// registrationAuditRow is one audited execution as the package contract
// states it.
type registrationAuditRow struct {
	actorType     string
	actorID       int64
	initiatorType string
	initiatorID   int64
	correlation   string
	outcome       string
	domainType    string
	domainID      int64
}

func countAudits(t *testing.T, database *sql.DB, action string) int {
	t.Helper()
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action=?`, action).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// singleAudit returns the only audit row of one action; more than one row or
// none both fail the test with the row count in the message.
func singleAudit(t *testing.T, database *sql.DB, action string) registrationAuditRow {
	t.Helper()
	var row registrationAuditRow
	err := database.QueryRow(`
		SELECT actor_type, actor_id, COALESCE(initiator_type,''), COALESCE(initiator_id,0),
		       COALESCE(correlation_id,''), outcome, COALESCE(domain_ref_type,''), COALESCE(domain_ref_id,0)
		FROM audit_events WHERE action=?`, action).Scan(
		&row.actorType, &row.actorID, &row.initiatorType, &row.initiatorID,
		&row.correlation, &row.outcome, &row.domainType, &row.domainID)
	if err != nil {
		t.Fatalf("audit row for %s (count %d): %v", action, countAudits(t, database, action), err)
	}
	return row
}

// registrationAuditByIdentity selects one audit row of an action by the
// audited slot-lifecycle generation (the domain reference of registration
// events), when several generations are audited in one database.
func registrationAuditByIdentity(t *testing.T, database *sql.DB, action string, generation int64) registrationAuditRow {
	t.Helper()
	var row registrationAuditRow
	err := database.QueryRow(`
		SELECT actor_type, actor_id, COALESCE(initiator_type,''), COALESCE(initiator_id,0),
		       COALESCE(correlation_id,''), outcome, COALESCE(domain_ref_type,''), COALESCE(domain_ref_id,0)
		FROM audit_events WHERE action=? AND domain_ref_id=?`, action, generation).Scan(
		&row.actorType, &row.actorID, &row.initiatorType, &row.initiatorID,
		&row.correlation, &row.outcome, &row.domainType, &row.domainID)
	if err != nil {
		t.Fatalf("audit row for %s/%d: %v", action, generation, err)
	}
	return row
}

// assertNoSecretInAudit sweeps every persisted text field of every audit
// event and its targets: the raw one-time token value must appear nowhere.
func assertNoSecretInAudit(t *testing.T, database *sql.DB, secret string) {
	t.Helper()
	rows, err := database.Query(`
		SELECT actor_type, action, COALESCE(correlation_id,''), COALESCE(request_id,''),
		       COALESCE(initiator_type,''), COALESCE(client_command_id,''), outcome,
		       COALESCE(domain_ref_type,''), phase
		FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var actorType, action, correlation, requestID, initiatorType, clientCommandID, outcome, domainType, phase string
		if err := rows.Scan(&actorType, &action, &correlation, &requestID, &initiatorType, &clientCommandID, &outcome, &domainType, &phase); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{actorType, action, correlation, requestID, initiatorType, clientCommandID, outcome, domainType, phase} {
			if strings.Contains(field, secret) {
				t.Fatalf("raw registration token leaked into an audit event field")
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestRegisterAuditJoinsPreparationLifecycle proves the verified design: the
// caller needs no execution metadata at all (the one-time token is the
// authority), and the confirmation's audit row still joins the management
// preparation's lifecycle — original admin initiator, runtime system actor,
// slot-local generation — with the raw token value in no audited field.
func TestRegisterAuditJoinsPreparationLifecycle(t *testing.T) {
	service, database, ctx := auditedService(t)
	var session [32]byte
	raw, generation := prepareAndReveal(t, service, ctx, "plinth", 1, session)
	preparation := singleAudit(t, database, "runtime_slot.prepare_registration")

	// A metadata-less context: possession of the raw one-time token is the
	// entire registration proof (RUNTIME-REG-002/003).
	longTerm, registered, err := service.Register(context.Background(), "plinth", raw, generation, "boot-audit", contract.ProtoAuthorityFingerprint, contract.ProtoAuthorityFingerprint)
	if err != nil || longTerm == "" || registered != 1 {
		t.Fatalf("register: token=%q gen=%d err=%v", longTerm, registered, err)
	}
	row := singleAudit(t, database, "runtime_slot.register")
	if row.actorType != "system" || row.actorID != 0 {
		t.Fatalf("register actor must be the runtime system principal, got %s/%d", row.actorType, row.actorID)
	}
	if row.initiatorType != "user" || row.initiatorID != 1 {
		t.Fatalf("register initiator must be the preparing admin, got %s/%d", row.initiatorType, row.initiatorID)
	}
	if row.correlation == "" || row.correlation != preparation.correlation {
		t.Fatalf("register correlation %q must join the preparation's %q", row.correlation, preparation.correlation)
	}
	if row.outcome != "success" || row.domainType != "runtime_slot" || row.domainID != 1 {
		t.Fatalf("register audit outcome=%s domain=%s/%d", row.outcome, row.domainType, row.domainID)
	}
	assertNoSecretInAudit(t, database, raw)
}

// TestFirstAuthenticationIsAuditedMachineWrite proves the Hello handshake's
// first-authentication bookkeeping executes as an audited runner operation:
// exactly one success fact for the runtime system principal with the
// credential row as the domain reference, no duplicate fact when an already
// first-authenticated generation handshakes again, and no fact at all for a
// failed validation.
func TestFirstAuthenticationIsAuditedMachineWrite(t *testing.T) {
	service, database, ctx := auditedService(t)
	var session [32]byte
	raw, generation := prepareAndReveal(t, service, ctx, "plinth", 1, session)
	longTerm, _, err := service.Register(context.Background(), "plinth", raw, generation, "boot-first", contract.ProtoAuthorityFingerprint, contract.ProtoAuthorityFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := service.Adjudicate(context.Background(), longTerm, "plinth", "boot-first", 1, contract.ProtoAuthorityFingerprint, contract.ProtoAuthorityFingerprint, "", "")
	if err != nil || !decision.Accepted || !decision.MarkedFirstAuthenticated {
		t.Fatalf("first hello: decision=%+v err=%v", decision, err)
	}
	var credentialID int64
	if err := database.QueryRow(`SELECT id FROM runtime_credentials WHERE slot='plinth' AND generation=?`, generation).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	row := singleAudit(t, database, "runtime_credential.first_authenticate")
	if row.actorType != "system" || row.actorID != 0 {
		t.Fatalf("first-auth actor=%s/%d, want the runtime system principal", row.actorType, row.actorID)
	}
	if row.initiatorType != "system" || row.initiatorID != 0 {
		t.Fatalf("first-auth initiator=%s/%d", row.initiatorType, row.initiatorID)
	}
	if row.correlation == "" {
		t.Fatal("first-auth must establish its own machine correlation")
	}
	if row.outcome != "success" || row.domainType != "runtime_credential" || row.domainID != credentialID {
		t.Fatalf("first-auth audit outcome=%s domain=%s/%d, want success runtime_credential/%d", row.outcome, row.domainType, row.domainID, credentialID)
	}
	// An already first-authenticated generation handshakes again (new boot
	// restarts epochs): accepted, but no duplicate audit fact and no marking.
	repeat, err := service.Adjudicate(context.Background(), longTerm, "plinth", "boot-second", 1, contract.ProtoAuthorityFingerprint, contract.ProtoAuthorityFingerprint, "", "")
	if err != nil || !repeat.Accepted || repeat.MarkedFirstAuthenticated {
		t.Fatalf("repeat hello: decision=%+v err=%v", repeat, err)
	}
	if countAudits(t, database, "runtime_credential.first_authenticate") != 1 {
		t.Fatal("repeat handshake must not record a second first-auth fact")
	}
	// A validation failure marks nothing and audits nothing.
	if rejected, err := service.Adjudicate(context.Background(), longTerm, "plinth", "boot-third", 1, "not-a-valid-fingerprint", contract.ProtoAuthorityFingerprint, "", ""); err != nil || rejected.Accepted {
		t.Fatalf("invalid contract must reject: decision=%+v err=%v", rejected, err)
	}
	if countAudits(t, database, "runtime_credential.first_authenticate") != 1 {
		t.Fatal("failed handshake must not record a first-auth fact")
	}
	assertNoSecretInAudit(t, database, longTerm)
}

// TestRecoveryLifecycleSharesOneCorrelation proves the deployment-only
// recovery begin is audited under the deployment-helper scope and the
// recovery registration joins the begin's correlation — one provable
// lifecycle from maintenance enter to replacement confirmation.
func TestRecoveryLifecycleSharesOneCorrelation(t *testing.T) {
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
	beginRow := singleAudit(t, database, "maintenance.lintel_recovery_begin")
	if beginRow.actorType != "system" || beginRow.actorID != 0 {
		t.Fatalf("recovery begin actor=%s/%d, want the deployment helper system principal", beginRow.actorType, beginRow.actorID)
	}
	if beginRow.initiatorType != "system" || beginRow.initiatorID != 0 {
		t.Fatalf("recovery begin initiator=%s/%d", beginRow.initiatorType, beginRow.initiatorID)
	}
	if beginRow.outcome != "success" || beginRow.domainType != "maintenance" || beginRow.domainID != begin.MaintenanceRevision {
		t.Fatalf("recovery begin audit outcome=%s domain=%s/%d revision=%d", beginRow.outcome, beginRow.domainType, beginRow.domainID, begin.MaintenanceRevision)
	}
	if beginRow.correlation == "" {
		t.Fatal("recovery begin must establish a correlation for its lifecycle")
	}
	longTerm, generation, err := service.Register(ctx, "lintel", begin.RegistrationToken, begin.ReplacementGeneration, "boot-recovery", contract.ProtoAuthorityFingerprint, contract.ProtoAuthorityFingerprint)
	if err != nil || generation != begin.ReplacementGeneration {
		t.Fatalf("recovery register: gen=%d err=%v", generation, err)
	}
	registerRow := registrationAuditByIdentity(t, database, "runtime_slot.register", begin.ReplacementGeneration)
	if registerRow.correlation != beginRow.correlation {
		t.Fatalf("recovery register correlation %q must join the begin's %q", registerRow.correlation, beginRow.correlation)
	}
	if registerRow.initiatorType != "system" || registerRow.initiatorID != 0 {
		t.Fatalf("recovery register initiator=%s/%d", registerRow.initiatorType, registerRow.initiatorID)
	}
	if registerRow.outcome != "success" || longTerm == "" {
		t.Fatalf("recovery register outcome=%s", registerRow.outcome)
	}
}

// TestRecoveryBeginIsDeploymentOnly proves the recovery entry accepts only
// the deployment-helper scope: a user session context is rejected before any
// state change or audit fact, and the retired Lintel slot gains no general
// management enablement through it.
func TestRecoveryBeginIsDeploymentOnly(t *testing.T) {
	service, database := recoveryService(t)
	fence := qruntime.LintelRecoveryFence{
		Backend: "compose", Disposition: "exclusively_reattached",
		DispositionDigest: digestOf("compose", "exclusively_reattached", "lintel-bind"),
		FenceReportDigest: digestOf("fence-report"),
	}
	adminCtx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "runtime-user-attempt",
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-user-attempt"},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginLintelRecoveryRegistration(adminCtx, fence); !errors.Is(err, qruntime.ErrLintelRecoveryState) {
		t.Fatalf("user context must not begin the deployment-only recovery, got %v", err)
	}
	var active int
	if err := database.QueryRow(`SELECT active FROM maintenance_state WHERE id=1`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("rejected begin must leave no maintenance state, active=%d", active)
	}
	if countAudits(t, database, "maintenance.lintel_recovery_begin") != 0 {
		t.Fatal("rejected begin must leave no audit fact")
	}
	// The deployment scope itself still begins the recovery (state proof).
	if _, err := service.BeginLintelRecoveryRegistration(context.Background(), fence); err != nil {
		t.Fatalf("deployment scope begin: %v", err)
	}
}
