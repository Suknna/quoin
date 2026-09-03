package deployment_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/verification/deployment"
)

// seedReadyIdentity creates one browser identity with a current revision and
// published profile generation (a placeholder login operation satisfies the
// generation's published_operation_id FK). It returns nothing; the expansion
// queries discover it mechanically.
func (h *harness) seedReadyIdentity() {
	h.t.Helper()
	h.mustExec(`INSERT INTO business_systems(key,display_name,enabled,row_version,created_at) VALUES ('orders','订单系统',0,1,'2026-03-01T08:00:00Z')`)
	systemID := h.queryInt(`SELECT id FROM business_systems WHERE key='orders'`)
	h.mustExec(`INSERT INTO browser_identity_revisions(business_system_id,revision,name,start_url,probe_journey_id,probe_journey_version,probe_params_json,journey_catalog_digest,journey_catalog_version,created_by,created_at)
		VALUES (?,1,'r1','https://app.test/login','authentication.url-prefix.v1',1,'{"authenticatedUrlPrefix":"https://app.test/u"}','`+strings.Repeat("ab", 32)+`','v1',?,'2026-03-01T08:00:00Z')`, systemID, h.userID)
	revisionID := h.queryInt(`SELECT id FROM browser_identity_revisions`)
	h.mustExec(`INSERT INTO browser_identities(business_system_id,current_revision_id,current_profile_generation_id,state,row_version,created_at) VALUES (?,?,NULL,'AuthenticationRequired',1,'2026-03-01T08:00:00Z')`, systemID, revisionID)
	identityID := h.queryInt(`SELECT id FROM browser_identities`)
	// A published generation binds the login operation that published it; the
	// frozen state machine only allows stepwise transitions.
	h.mustExec(`INSERT INTO browser_operations(identity_id,identity_revision_id,profile_generation_id,owner_attempt_id,kind,actor_user_id,actor_session_id,
		verification_manifest_item_id,clone_identity,state,journey_catalog_digest,journey_catalog_version,journey_id,journey_version,probe_phase,requested_at)
		VALUES (?,?,NULL,NULL,'manual_login',?,?,NULL,NULL,'Queued','`+strings.Repeat("ab", 32)+`','v1',NULL,NULL,NULL,'2026-03-01T08:10:00Z')`, identityID, revisionID, h.userID, h.sessionID)
	loginID := h.queryInt(`SELECT id FROM browser_operations WHERE kind='manual_login'`)
	h.mustExec(`UPDATE browser_operations SET state='Starting',start_dispatched_at='2026-03-01T08:11:00Z',lintel_boot_id='boot-1',lintel_connection_epoch=1,row_version=row_version+1 WHERE id=?`, loginID)
	h.mustExec(`UPDATE browser_operations SET state='Running',started_at='2026-03-01T08:12:00Z',row_version=row_version+1 WHERE id=?`, loginID)
	h.mustExec(`INSERT INTO browser_probe_results(operation_id,probe_seq,phase,identity_revision_id,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,result,observed_at)
		VALUES (?,1,'publish',?,'authentication.url-prefix.v1',1,'`+strings.Repeat("ab", 32)+`','v1','Authenticated','2026-03-01T08:19:00Z')`, loginID, revisionID)
	h.mustExec(`INSERT INTO browser_profile_generations(identity_id,identity_revision_id,generation,chromium_revision,profile_manifest_digest,probe_journey_id,probe_journey_version,probe_catalog_digest,probe_catalog_version,published_operation_id,published_by,published_at)
		VALUES (?,?,1,'1200.0.6099.109','`+strings.Repeat("cd", 32)+`','authentication.url-prefix.v1',1,'`+strings.Repeat("ab", 32)+`','v1',?,?, '2026-03-01T08:30:00Z')`, identityID, revisionID, loginID, h.userID)
	// The generation publish atomically closes its manual login operation.
	if state := h.text(`SELECT state FROM browser_operations WHERE id=?`, loginID); state != "Succeeded" {
		h.t.Fatalf("publish did not close the login operation: state=%s", state)
	}
	h.mustExec(`UPDATE browser_operations SET stop_confirmed_at='2026-03-01T08:21:00Z',stop_confirmation_basis='stop_ack',row_version=row_version+1 WHERE id=?`, loginID)
	// The publish trigger switches the identity to Ready with the new
	// generation as current; the manual pointer update is neither needed nor
	// allowed.
	if state := h.text(`SELECT state FROM browser_identities WHERE id=?`, identityID); state != "Ready" {
		h.t.Fatalf("publish did not activate the identity: state=%s", state)
	}
}

func TestEnqueueBrowserOperationsBindsFrozenItems(t *testing.T) {
	h := newHarness(t)
	h.seedReadyIdentity()
	invocation := h.start()
	// The identity item entered the denominator.
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_invocation_items WHERE invocation_id=? AND object_kind='browser_identity'`, invocation); got != 1 {
		t.Fatalf("browser identity items = %d, want 1", got)
	}
	if err := h.service.EnqueueBrowserOperations(context.Background(), invocation); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := h.service.EnqueueBrowserOperations(context.Background(), invocation); err != nil {
		t.Fatalf("re-enqueue must be idempotent: %v", err)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM browser_operations WHERE kind='deployment_verification'`); got != 1 {
		t.Fatalf("deployment operations = %d, want 1", got)
	}
	clone := h.text(`SELECT clone_identity FROM browser_operations WHERE kind='deployment_verification'`)
	if clone != "deployment-verification-"+itoa(invocation)+"-"+itoa(h.queryInt(`SELECT id FROM verification_invocation_items WHERE invocation_id=? AND object_kind='browser_identity'`, invocation)) {
		t.Fatalf("clone identity = %s", clone)
	}
	if state := h.text(`SELECT state FROM browser_operations WHERE kind='deployment_verification'`); state != "Queued" {
		t.Fatalf("state = %s, want Queued", state)
	}
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

// Drift: rotating a connection or re-publishing a config after the freeze
// appends immutable drift markers and bans a passed finalization.
func TestSubjectDriftBlocksPassedFinalization(t *testing.T) {
	h := newHarness(t)
	h.wireArtifacts()
	h.mustExec(`INSERT INTO connections(name,type,enabled,row_version,created_at) VALUES ('thanos-main','thanos',1,1,'2026-03-01T08:00:00Z')`)
	connectionID := h.queryInt(`SELECT id FROM connections WHERE name='thanos-main'`)
	h.mustExec(`INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_by,created_at) VALUES (?,1,'{"address":"https://thanos.test"}',?, '2026-03-01T08:00:00Z')`, connectionID, h.userID)
	revisionID := h.queryInt(`SELECT id FROM connection_revisions`)
	h.mustExec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_by,created_at)
		VALUES (?,1,1,1,X'000000000000000000000001',X'00000000000000000000000000000001',?, '2026-03-01T08:00:00Z')`, connectionID, h.userID)
	h.mustExec(`UPDATE connections SET current_revision_id=?,current_credential_generation_id=?,row_version=row_version+1 WHERE id=?`, revisionID, h.queryInt(`SELECT id FROM credential_generations`), connectionID)

	invocation := h.start()
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_invocation_items WHERE invocation_id=? AND object_kind='connection'`, invocation); got != 1 {
		t.Fatalf("connection items = %d", got)
	}
	// Rotate the credential generation after the freeze.
	h.mustExec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_by,created_at)
		VALUES (?,2,1,1,X'000000000000000000000002',X'00000000000000000000000000000002',?, '2026-03-01T08:40:00Z')`, connectionID, h.userID)
	h.mustExec(`UPDATE connections SET current_credential_generation_id=?,row_version=row_version+1 WHERE id=?`, h.queryInt(`SELECT id FROM credential_generations WHERE generation_seq=2`), connectionID)

	h.fillAllItems(invocation)
	// The helper report and observations alone leave the connection item
	// without a result; synthesize one passed runtime result for the drift
	// interplay, then finalize.
	itemID := h.queryInt(`SELECT id FROM verification_invocation_items WHERE invocation_id=? AND object_kind='connection'`, invocation)
	digest := h.text(`SELECT input_digest FROM verification_invocation_items WHERE id=?`, itemID)
	h.advance(10 * time.Minute)
	if _, _, err := h.service.AppendResult(context.Background(), deployment.ResultInput{
		InvocationID: invocation, ItemID: itemID, InputDigest: digest, ResultDigest: strings.Repeat("ef", 32),
		ProducerType: "runtime", Outcome: "passed", Category: "passed", ObservedAt: "2026-03-01T09:00:00Z",
		EvidenceIndexDigest: strings.Repeat("ef", 32),
	}); err != nil {
		t.Fatalf("append connection result: %v", err)
	}
	detail, err := h.service.Finalize(context.Background(), invocation, &h.sessionID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if detail.VerificationReceipt == nil || detail.VerificationReceipt.OverallOutcome != "warned" {
		t.Fatalf("drifted invocation must finalize WARNED at best, got %+v", detail.VerificationReceipt)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_subject_drifts WHERE invocation_id=? AND drift_field='credential_generation'`, invocation); got != 1 {
		t.Fatalf("credential drift markers = %d, want 1", got)
	}
}

// The fixed deadline synthesizes not_run for undecidable items and writes the
// system receipt; after the deadline no late final result is produced.
func TestDeadlineSweepClosesWithinWindowAndRefusesLate(t *testing.T) {
	h := newHarness(t)
	h.wireArtifacts()
	invocation := h.start()
	h.advance(8*time.Hour - time.Minute)
	if err := h.service.SweepDeadline(context.Background(), invocation); err != nil {
		t.Fatalf("sweep at boundary: %v", err)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_item_results WHERE category='not_run'`); got != 18 {
		t.Fatalf("not_run results = %d, want 18", got)
	}
	detail, err := h.service.Finalize(context.Background(), invocation, nil)
	if err != nil {
		t.Fatalf("system finalize: %v", err)
	}
	if detail.VerificationReceipt == nil || detail.VerificationReceipt.OverallOutcome != "warned" {
		t.Fatalf("deadline receipt must be warned, got %+v", detail.VerificationReceipt)
	}

	// A second invocation that misses its window stays receiptless forever.
	h.advance(2 * time.Hour)
	second := int64(0)
	if id, err := h.service.Start(context.Background(), h.sessionID, h.userID, "cmd-late"); err != nil {
		t.Fatalf("start second: %v", err)
	} else {
		second = id
	}
	h.advance(9 * time.Hour)
	if err := h.service.SweepDeadline(context.Background(), second); err != nil {
		t.Fatalf("late sweep: %v", err)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_finalization_receipts WHERE invocation_id=?`, second); got != 0 {
		t.Fatal("missed deadline must never produce a late receipt")
	}
}
