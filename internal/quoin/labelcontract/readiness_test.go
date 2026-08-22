package labelcontract

// Readiness projection tests (T17, DATA-CONFIG-002/007): per enabled system
// every legal (unpublished draft, Passed prepublish run) candidate pair plus
// the closed blocker set when no candidate exists; disabled systems never
// appear; "latest" never overrides an older still-legal candidate.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// seedReadinessSystem inserts the Disabled aggregate the frozen insert
// trigger demands, then flips enabled via the version-bumped UPDATE the
// row-version trigger requires; readiness exposes the resulting row_version.
func seedReadinessSystem(t *testing.T, db *sql.DB, key string, enabled bool) int64 {
	t.Helper()
	result, err := db.Exec(`INSERT INTO business_systems(key,display_name,enabled,row_version,created_at) VALUES(?,?,0,1,?)`,
		key, key, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	if enabled {
		if _, err := db.Exec(`UPDATE business_systems SET enabled=1, row_version=2 WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// seedDraft inserts one unpublished config version targeting the contract;
// system_key must equal the owning system's stable key (frozen trigger).
func seedDraft(t *testing.T, db *sql.DB, systemID, contractID int64, seq int64, systemKey string) int64 {
	t.Helper()
	result, err := db.Exec(`INSERT INTO business_system_config_versions(
		business_system_id,version_seq,state,yaml_body,parser_version,schema_version,
		label_contract_version_id,journey_catalog_digest,journey_catalog_version,digest,
		created_by,created_at,system_key,display_name,enabled,timezone,resource_refresh_interval_seconds)
		VALUES(?,?,'draft','fixture','v1','v1',?,'0000000000000000000000000000000000000000000000000000000000000000','v1',?,NULL,?,?,?,0,'Asia/Shanghai',300)`,
		systemID, seq, contractID, testDigest(int(seq)), time.Now().UTC().Format(time.RFC3339Nano), systemKey, systemKey)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	return id
}

func testDigest(seed int) string {
	digest := make([]byte, 32)
	for index := range digest {
		digest[index] = byte(seed + index)
	}
	return fmt.Sprintf("%x", digest)
}

// seedRun inserts one prepublish verification run through the frozen
// lifecycle (INSERT must be Queued, then one UPDATE to the target state).
func seedRun(t *testing.T, db *sql.DB, systemID, versionID, contractID int64, state string) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := db.Exec(`INSERT INTO config_verification_runs(
		purpose,business_system_id,config_version_id,label_contract_version_id,state,row_version,created_by,created_at)
		VALUES('prepublish',?,?,?,'Queued',1,1,?)`,
		systemID, versionID, contractID, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	if state == "Passed" {
		// Queued -> Running -> Passed (the frozen forward machine).
		if _, err := db.Exec(`UPDATE config_verification_runs SET state='Running', evidence_at=?, row_version=2 WHERE id=?`, now, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE config_verification_runs SET state='Passed', row_version=3 WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if state != "Queued" {
		var resultDetail any
		if state == "Failed" || state == "Cancelled" || state == "Interrupted" {
			resultDetail = "fixture"
		}
		if _, err := db.Exec(`UPDATE config_verification_runs SET state=?, result_detail=?, row_version=2 WHERE id=?`, state, resultDetail, id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func readinessBlockers(t *testing.T, view Readiness, systemKey string) []string {
	t.Helper()
	for _, system := range view.Systems {
		if system.BusinessSystemKey == systemKey {
			return system.Blockers
		}
	}
	t.Fatalf("system %s missing from readiness", systemKey)
	return nil
}

func readinessCandidates(t *testing.T, view Readiness, systemKey string) []ReadinessCandidate {
	t.Helper()
	for _, system := range view.Systems {
		if system.BusinessSystemKey == systemKey {
			return system.ActivationCandidates
		}
	}
	t.Fatalf("system %s missing from readiness", systemKey)
	return nil
}

func TestReadinessZeroEnabledSystems(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	mustCreateDraft(t, service, validContractYAML, "cmd-ready-0001")
	view, err := service.Readiness(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Systems) != 0 || view.TargetContractVersion != 1 || view.StateRowVersion != 1 || view.TargetRowVersion != 1 || view.CurrentContractVersionID != nil {
		t.Fatalf("zero-system readiness wrong: %#v", view)
	}
}

func TestReadinessBlockersAndCandidates(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	mustCreateDraft(t, service, validContractYAML, "cmd-ready-0002") // contract 1 (target)

	ready := seedReadinessSystem(t, db, "alpha", true)       // draft + Passed run
	missing := seedReadinessSystem(t, db, "bravo", true)     // draft, no run at all
	failed := seedReadinessSystem(t, db, "charlie", true)    // draft + Failed run only
	queued := seedReadinessSystem(t, db, "delta", true)      // draft + Queued run only
	mixed := seedReadinessSystem(t, db, "echo", true)        // draft + Failed + Cancelled runs
	seedReadinessSystem(t, db, "golf", true)                 // no draft at all
	seedReadinessSystem(t, db, "disabled-sys", false)        // never in readiness

	readyDraft := seedDraft(t, db, ready, 1, 1, "alpha")
	seedRun(t, db, ready, readyDraft, 1, "Passed")
	// An older draft with its own Passed run stays an explicit candidate.
	readyOld := seedDraft(t, db, ready, 1, 2, "alpha")
	seedRun(t, db, ready, readyOld, 1, "Passed")

	seedDraft(t, db, missing, 1, 1, "bravo")

	failedDraft := seedDraft(t, db, failed, 1, 1, "charlie")
	seedRun(t, db, failed, failedDraft, 1, "Failed")

	queuedDraft := seedDraft(t, db, queued, 1, 1, "delta")
	seedRun(t, db, queued, queuedDraft, 1, "Queued")

	mixedDraft := seedDraft(t, db, mixed, 1, 1, "echo")
	seedRun(t, db, mixed, mixedDraft, 1, "Failed")
	seedRun(t, db, mixed, mixedDraft, 1, "Cancelled")

	view, err := service.Readiness(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Systems) != 6 {
		t.Fatalf("readiness must list exactly the six enabled systems, got %d", len(view.Systems))
	}
	candidates := readinessCandidates(t, view, "alpha")
	if len(candidates) != 2 {
		t.Fatalf("alpha must expose both Passed candidate pairs, got %#v", candidates)
	}
	if blockers := readinessBlockers(t, view, "alpha"); len(blockers) != 0 {
		t.Fatalf("alpha must have no blockers, got %v", blockers)
	}
	if blockers := readinessBlockers(t, view, "bravo"); len(blockers) != 1 || blockers[0] != "verification_run_missing" {
		t.Fatalf("bravo blockers wrong: %v", blockers)
	}
	if blockers := readinessBlockers(t, view, "charlie"); len(blockers) != 1 || blockers[0] != "verification_run_failed" {
		t.Fatalf("charlie blockers wrong: %v", blockers)
	}
	if blockers := readinessBlockers(t, view, "delta"); len(blockers) != 1 || blockers[0] != "verification_run_pending" {
		t.Fatalf("delta blockers wrong: %v", blockers)
	}
	if blockers := readinessBlockers(t, view, "echo"); len(blockers) != 2 || blockers[0] != "verification_run_failed" || blockers[1] != "verification_run_cancelled" {
		t.Fatalf("echo must list both terminal blockers in the frozen order: %v", blockers)
	}
	// A Passed run over a published version is no candidate (the published
	// path is proven over the real publish command in the businesssystem and
	// acceptance suites; forging state/published_at is trigger-blocked).
	if blockers := readinessBlockers(t, view, "golf"); len(blockers) != 1 || blockers[0] != "no_compatible_version" {
		t.Fatalf("golf blockers wrong: %v", blockers)
	}
	for _, system := range view.Systems {
		if system.BusinessSystemKey == "disabled-sys" {
			t.Fatal("disabled system must never appear in readiness")
		}
	}
}

func TestReadinessSurfacesConcurrencyPreconditions(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	mustCreateDraft(t, service, validContractYAML, "cmd-ready-0003") // v1
	detail := mustCreateDraft(t, service, validContractYAML, "cmd-ready-0004")
	if _, err := service.Activate(context.Background(), 1, "cmd-ready-0005", ActivateInput{ContractVersion: 1, ExpectedStateRowVersion: 1, ExpectedTargetRowVersion: 1}); err != nil {
		t.Fatal(err)
	}
	view, err := service.Readiness(context.Background(), detail.Version)
	if err != nil {
		t.Fatal(err)
	}
	if view.StateRowVersion != 2 {
		t.Fatalf("state row version must advance to 2, got %d", view.StateRowVersion)
	}
	if view.CurrentContractVersionID == nil || *view.CurrentContractVersionID != "1" {
		t.Fatalf("current pointer must surface contract 1, got %v", view.CurrentContractVersionID)
	}
	if view.TargetRowVersion != detail.RowVersion {
		t.Fatalf("target row version wrong: %d vs %d", view.TargetRowVersion, detail.RowVersion)
	}
}

func TestReadinessUnknownContract(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	if _, err := service.Readiness(context.Background(), 99); err != ErrNotFound {
		t.Fatalf("unknown contract must 404, got %v", err)
	}
}
