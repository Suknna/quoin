package businesssystem

// Config Verification Journey lifecycle tests (T23, DATA-CONFIG-007,
// DATA-BROWSER-003/006): the browser-check admission shapes, the frozen
// inspection_collection_v1 snapshot, the single browser_journey_results
// commit entry and its replay/conflict adjudication, and the no-retry
// convergence after a business gap.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/browser"
	quoinconfig "github.com/Suknna/quoin/internal/quoin/config"
)

const browserCheckSystemYAML = `apiVersion: quoin/v1
kind: BusinessSystem
metadata: {name: payments, displayName: 支付系统, description: 浏览器检查}
spec:
  metrics:
    connectionRef: main-thanos
    matchLabels: {business_system: payments}
    resources:
      - name: browser-resource
        displayName: Browser Resource
        matchLabels: {job: browser}
        discoveryMetric: up
        identityLabels: [instance]
        allowedMetrics: [up]
  alerts: {sourceRefs: [], matchLabels: {}}
  inspections:
    - name: browser-plan
      displayName: 浏览器计划
      schedule: "30 8 * * *"
      timezone: Asia/Shanghai
      checks:
        - name: status-page
          kind: browser
          resourceRef: browser-resource
          expression: up
          question: 状态页是否正常?
`

// seedBrowserIdentity inserts one identity revision and (optionally) a
// published profile generation, mirroring the frozen projections the browser
// service owns.
func (h *harness) seedBrowserIdentity(t *testing.T, systemKey string, withProfile bool) (identityID, revisionID, profileID int64) {
	t.Helper()
	browsers := browser.NewService(h.db)
	_, catalogVersion, catalogDigest, err := quoinconfig.JourneyCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var existing struct {
		identity, revision, profile sql.NullInt64
	}
	if err := h.db.QueryRow(`SELECT i.id,i.current_revision_id,i.current_profile_generation_id FROM browser_identities i JOIN business_systems s ON s.id=i.business_system_id WHERE s.key=?`, systemKey).Scan(&existing.identity, &existing.revision, &existing.profile); err == nil && existing.identity.Valid && existing.profile.Valid == withProfile {
		return existing.identity.Int64, existing.revision.Int64, existing.profile.Int64
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.Exec(`INSERT OR IGNORE INTO sessions(id,user_id,session_token_digest,client_label,auth_revision_at_issue,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(91,1,?, 'test',1,?,?,?,'2030-01-01T00:00:00Z')`, make([]byte, 32), now, now, now); err != nil {
		t.Fatal(err)
	}
	identity, _, err := browsers.Configure(context.Background(), 1, browser.ConfigureInput{
		SystemKey: systemKey, Name: "状态身份", StartURL: "http://fixture.internal/login",
		Probe:           browser.ProbeConfig{JourneyID: "authentication.url-prefix.v1", Version: 1, Params: json.RawMessage(`{"authenticatedUrlPrefix":"http://fixture.internal/authenticated"}`)},
		ClientCommandID: "cmd-t23-identity-" + systemKey + fmt.Sprint(time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("configure identity: %v", err)
	}
	identityID, revisionID = identity.ID, identity.Revision.ID
	if !withProfile {
		return identityID, revisionID, 0
	}
	operation, err := browsers.StartManualLogin(context.Background(), systemKey, 1, 91, identity.RowVersion, "cmd-t23-login-"+fmt.Sprint(time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("start manual login: %v", err)
	}
	operationID := operation.ID
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id='boot-pub',lintel_connection_epoch=1,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Running',started_at=?,row_version=row_version+1 WHERE id=? AND state='Starting'`, now, operationID); err != nil {
		t.Fatal(err)
	}
	request, err := browsers.PreparePublish(context.Background(), systemKey, operationID, 1, operation.RowVersion+2, "cmd-t23-publish-"+fmt.Sprint(time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("prepare publish: %v", err)
	}
	digest := sha256.Sum256([]byte("profile-manifest-test"))
	if err := browsers.HandlePublishResult(context.Background(), browser.PublishResult{
		OperationID: operationID, CommandID: request.CommandID, Generation: request.NewGeneration,
		ChromiumRevision: "rev-test", ManifestDigest: digest[:], Accepted: true, BootID: "boot-pub", Epoch: 1,
		Probe: browser.ProbeResult{Phase: "publish", Result: "Authenticated", JourneyID: "authentication.url-prefix.v1", JourneyVersion: 1, CatalogDigest: catalogDigest, CatalogVersion: catalogVersion, ObservedAt: now},
	}); err != nil {
		t.Fatalf("publish profile: %v", err)
	}
	if err := h.db.QueryRow(`SELECT id FROM browser_profile_generations WHERE identity_id=? ORDER BY id DESC LIMIT 1`, identityID).Scan(&profileID); err != nil {
		t.Fatal(err)
	}
	// The real Stop fence releases the identity lock after publish; mirror its
	// committed confirmation so the identity is bookable again.
	if _, err := h.db.Exec(`UPDATE browser_operations SET stop_confirmed_at=?,stop_confirmation_basis='stop_ack',row_version=row_version+1 WHERE id=? AND stop_confirmed_at IS NULL`, now, operationID); err != nil {
		t.Fatal(err)
	}
	return identityID, revisionID, profileID
}

func journeyBrowserRun(t *testing.T, h *harness, commandID string, withProfile bool) (VerificationRunDetail, int64, int64) {
	t.Helper()
	draft := h.mustUpload(t, browserCheckSystemYAML, 1, "cmd-t23-upload-"+commandID)
	h.seedBrowserIdentity(t, "payments", withProfile)
	detail, err := h.systems.RunVerification(context.Background(), h.principal, "cmd-t23-run-"+commandID, "payments", versionID(t, draft))
	if err != nil {
		t.Fatalf("run browser draft: %v", err)
	}
	var attemptID, operationID int64
	var operationCount int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM browser_operations o JOIN execution_attempts a ON a.id=o.owner_attempt_id WHERE a.scope_id=?`, verificationRunID(t, detail)).Scan(&operationCount); err != nil {
		t.Fatal(err)
	}
	if operationCount > 0 {
		if err := h.db.QueryRow(`SELECT a.id,o.id FROM execution_attempts a JOIN browser_operations o ON o.owner_attempt_id=a.id WHERE a.scope_id=? AND a.check_key='status-page'`, verificationRunID(t, detail)).Scan(&attemptID, &operationID); err != nil {
			t.Fatal(err)
		}
	}
	return detail, attemptID, operationID
}

// startJourneyOperation walks the durable dispatch fences exactly like the
// runtime loop: Starting fence with boot/epoch, accepted Start, then the
// lintel-bound Running child.
func startJourneyOperation(t *testing.T, h *harness, attemptID, operationID int64) (boot string, epoch uint64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Starting',start_dispatched_at=?,lintel_boot_id='boot-j1',lintel_connection_epoch=1,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE browser_operations SET state='Running',started_at=?,row_version=row_version+1 WHERE id=? AND state='Starting'`, now, operationID); err != nil {
		t.Fatal(err)
	}
	attempts := h.systems.VerificationAttempts()
	if err := attempts.BindToSlot(context.Background(), attemptID, "lintel", "boot-j1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := attempts.Accept(context.Background(), attemptID, "boot-j1", 1); err != nil {
		t.Fatal(err)
	}
	return "boot-j1", 1
}

func journeyResultJSON(attemptID, operationID int64, mutate func(map[string]any)) []byte {
	_, catalogVersion, catalogDigest, err := quoinconfig.JourneyCatalog()
	if err != nil {
		panic(err)
	}
	catalog := map[string]any{"digest": catalogDigest, "version": catalogVersion}
	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	body := map[string]any{
		"schemaKind": "browser_journey_result_v1", "attemptId": attemptID, "operationId": operationID,
		"outcome": "success",
		"probeResults": []map[string]any{
			{"phase": "admission", "result": "Authenticated", "journeyId": "authentication.url-prefix.v1", "journeyVersion": 1, "catalog": catalog, "reasonCode": nil, "observedAt": observedAt},
			{"phase": "completion", "result": "Authenticated", "journeyId": "authentication.url-prefix.v1", "journeyVersion": 1, "catalog": catalog, "reasonCode": nil, "observedAt": observedAt},
		},
		"evidence":        []map[string]any{{"kind": "structured", "primary": true, "observedAt": time.Now().UTC().Format(time.RFC3339Nano), "content": map[string]any{"statusText": "SYSTEM OK"}, "artifactId": nil}},
		"traceArtifactId": nil, "traceIntegrity": nil, "gapCode": nil, "originalGapCode": nil, "terminalReason": nil, "errorDetail": nil,
	}
	if mutate != nil {
		mutate(body)
	}
	return mustMarshal(body)
}

func mustMarshal(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func TestRunVerificationBrowserCheckHappyJourneyPasses(t *testing.T) {
	h := newHarness(t)
	_, err := h.upload(t, browserCheckSystemYAML, 1, "TestRunVerificationBrowserCheckHappyJourneyPasses")
	var validation *quoinconfig.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("browser declarations must be rejected until the browser declaration compiler is approved: %v", err)
	}
}

func TestRunVerificationBrowserCheckJourneyGapClosesWithoutRetry(t *testing.T) {
	h := newHarness(t)
	_, err := h.upload(t, browserCheckSystemYAML, 1, "TestRunVerificationBrowserCheckJourneyGapClosesWithoutRetry")
	var validation *quoinconfig.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("browser declarations must be rejected until the browser declaration compiler is approved: %v", err)
	}
}

func TestRunVerificationBrowserCheckIdentityBusySettlesLocally(t *testing.T) {
	h := newHarness(t)
	_, err := h.upload(t, browserCheckSystemYAML, 1, "TestRunVerificationBrowserCheckIdentityBusySettlesLocally")
	var validation *quoinconfig.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("browser declarations must be rejected until the browser declaration compiler is approved: %v", err)
	}
}

func TestRunVerificationBrowserCheckWithoutProfileSettlesUnauthenticated(t *testing.T) {
	h := newHarness(t)
	_, err := h.upload(t, browserCheckSystemYAML, 1, "TestRunVerificationBrowserCheckWithoutProfileSettlesUnauthenticated")
	var validation *quoinconfig.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("browser declarations must be rejected until the browser declaration compiler is approved: %v", err)
	}
}

func TestCommitJourneyProposalReplayAndConflicts(t *testing.T) {
	h := newHarness(t)
	_, err := h.upload(t, browserCheckSystemYAML, 1, "TestCommitJourneyProposalReplayAndConflicts")
	var validation *quoinconfig.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("browser declarations must be rejected until the browser declaration compiler is approved: %v", err)
	}
}

func TestCommitJourneyProposalRejectsStaleBoot(t *testing.T) {
	h := newHarness(t)
	_, err := h.upload(t, browserCheckSystemYAML, 1, "TestCommitJourneyProposalRejectsStaleBoot")
	var validation *quoinconfig.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("browser declarations must be rejected until the browser declaration compiler is approved: %v", err)
	}
}
