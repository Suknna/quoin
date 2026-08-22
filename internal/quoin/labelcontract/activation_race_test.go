package labelcontract

// Enabled-system activation tests (T17): the single INSERT either switches
// every enabled system's config pointer together with the contract pointer
// or leaves no trace at all; concurrent activations of the same draft are
// adjudicated by commit order (DATA-CONFIG-002/005/006).

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
)

func TestActivateEnabledSystemsAllOrNothing(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	mustCreateDraft(t, service, validContractYAML, "cmd-act-0001") // v1
	target := mustCreateDraft(t, service, validContractYAML, "cmd-act-0002")
	// v1 active over zero systems.
	if _, err := service.Activate(context.Background(), 1, "cmd-act-0003", ActivateInput{ContractVersion: 1, ExpectedStateRowVersion: 1, ExpectedTargetRowVersion: 1}); err != nil {
		t.Fatal(err)
	}

	system := seedReadinessSystem(t, db, "alpha", true) // row_version 2
	draft := seedDraft(t, db, system, contractRowOf(t, db, target.Version), 1, "alpha")
	passed := seedRun(t, db, system, draft, contractRowOf(t, db, target.Version), "Passed")

	// A non-Passed run for a second system-less draft must not leak into the
	// item set; the activation carries exactly the one legal pair.
	input := ActivateInput{
		ContractVersion:           2,
		ExpectedStateRowVersion:   2,
		ExpectedCurrentContractID: ptrInt64(1),
		ExpectedTargetRowVersion:  1,
		Items: []ActivationItem{{
			BusinessSystemKey:                "alpha",
			ConfigVersionID:                  draft,
			VerificationRunID:                passed,
			ExpectedCurrentConfigVersionID:   nil,
			ExpectedBusinessSystemRowVersion: 2,
		}},
	}
	detail, err := service.Activate(context.Background(), 1, "cmd-act-0004", input)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if detail.State != "active" {
		t.Fatalf("target must be active, got %s", detail.State)
	}
	var current, enabled int64
	if err := db.QueryRow(`SELECT current_config_version_id, enabled FROM business_systems WHERE id=?`, system).Scan(&current, &enabled); err != nil {
		t.Fatal(err)
	}
	if current != draft || enabled != 0 {
		// enabled comes from the draft's root projection (fixture drafts are
		// enabled=0); the pointer is the proof that matters here.
		t.Fatalf("system pointer not switched: current=%d enabled=%d", current, enabled)
	}
	if current != draft {
		t.Fatalf("system pointer must equal the activated draft %d, got %d", draft, current)
	}
	var versionState string
	if err := db.QueryRow(`SELECT state FROM business_system_config_versions WHERE id=?`, draft).Scan(&versionState); err != nil {
		t.Fatal(err)
	}
	if versionState != "published" {
		t.Fatalf("activated version must derive published, got %s", versionState)
	}
	var statePointer int64
	if err := db.QueryRow(`SELECT current_contract_id FROM label_contract_state WHERE id=1`).Scan(&statePointer); err != nil {
		t.Fatal(err)
	}
	if statePointer != contractRowOf(t, db, target.Version) {
		t.Fatalf("contract pointer must equal the target, got %d", statePointer)
	}
	var retired string
	if err := db.QueryRow(`SELECT state FROM label_contracts WHERE version=1`).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	if retired != "retired" {
		t.Fatalf("previous contract must retire, got %s", retired)
	}
}

func TestActivateAbortsWholeStatementOnBadEvidence(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	mustCreateDraft(t, service, validContractYAML, "cmd-act-0011") // v1
	target := mustCreateDraft(t, service, validContractYAML, "cmd-act-0012")
	if _, err := service.Activate(context.Background(), 1, "cmd-act-0013", ActivateInput{ContractVersion: 1, ExpectedStateRowVersion: 1, ExpectedTargetRowVersion: 1}); err != nil {
		t.Fatal(err)
	}

	system := seedReadinessSystem(t, db, "alpha", true)
	draft := seedDraft(t, db, system, contractRowOf(t, db, target.Version), 1, "alpha")
	// Only a Failed run exists: the item cannot close and the whole INSERT
	// must roll back (no pointer move, no activation row).
	failedRun := seedRun(t, db, system, draft, contractRowOf(t, db, target.Version), "Failed")
	_ = failedRun
	_, err := service.Activate(context.Background(), 1, "cmd-act-0014", ActivateInput{
		ContractVersion:           2,
		ExpectedStateRowVersion:   2,
		ExpectedCurrentContractID: ptrInt64(1),
		ExpectedTargetRowVersion:  1,
		Items: []ActivationItem{{
			BusinessSystemKey: "alpha", ConfigVersionID: draft, VerificationRunID: failedRun,
			ExpectedBusinessSystemRowVersion: 2,
		}},
	})
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("non-Passed evidence must conflict, got %v", err)
	}
	var activations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM label_contract_activations WHERE contract_id=?`, contractRowOf(t, db, target.Version)).Scan(&activations); err != nil {
		t.Fatal(err)
	}
	if activations != 0 {
		t.Fatalf("failed activation must leave no row, got %d", activations)
	}
	var pointer int64
	var stateRow int64
	if err := db.QueryRow(`SELECT current_contract_id,row_version FROM label_contract_state WHERE id=1`).Scan(&pointer, &stateRow); err != nil {
		t.Fatal(err)
	}
	if pointer != 1 || stateRow != 2 {
		t.Fatalf("state pointer must be untouched, got contract=%d row=%d", pointer, stateRow)
	}
	var current any
	if err := db.QueryRow(`SELECT current_config_version_id FROM business_systems WHERE id=?`, system).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != nil {
		t.Fatalf("system pointer must stay null, got %v", current)
	}
}

func TestConcurrentActivationCommitOrder(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	mustCreateDraft(t, service, validContractYAML, "cmd-act-0021")
	target := mustCreateDraft(t, service, validContractYAML, "cmd-act-0022")
	if _, err := service.Activate(context.Background(), 1, "cmd-act-0023", ActivateInput{ContractVersion: 1, ExpectedStateRowVersion: 1, ExpectedTargetRowVersion: 1}); err != nil {
		t.Fatal(err)
	}
	system := seedReadinessSystem(t, db, "alpha", true)
	draft := seedDraft(t, db, system, contractRowOf(t, db, target.Version), 1, "alpha")
	passed := seedRun(t, db, system, draft, contractRowOf(t, db, target.Version), "Passed")

	build := func() ActivateInput {
		return ActivateInput{
			ContractVersion:           2,
			ExpectedStateRowVersion:   2,
			ExpectedCurrentContractID: ptrInt64(1),
			ExpectedTargetRowVersion:  1,
			Items: []ActivationItem{{
				BusinessSystemKey: "alpha", ConfigVersionID: draft, VerificationRunID: passed,
				ExpectedBusinessSystemRowVersion: 2,
			}},
		}
	}
	const racers = 4
	results := make(chan error, racers)
	var start sync.WaitGroup
	start.Add(1)
	for index := 0; index < racers; index++ {
		go func(index int) {
			start.Wait()
			_, err := service.Activate(context.Background(), 1, "cmd-act-race-"+string(rune('a'+index)), build())
			results <- err
		}(index)
	}
	start.Done()
	wins := 0
	for index := 0; index < racers; index++ {
		if err := <-results; err == nil {
			wins++
		} else {
			var conflict *ConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("loser must be a typed conflict, got %v", err)
			}
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one racer must win, got %d", wins)
	}
	var activations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM label_contract_activations WHERE contract_id=?`, contractRowOf(t, db, target.Version)).Scan(&activations); err != nil {
		t.Fatal(err)
	}
	if activations != 1 {
		t.Fatalf("exactly one activation row must exist, got %d", activations)
	}
}

func TestConcurrentSameActivationCommandReplaysCommittedResult(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	mustCreateDraft(t, service, validContractYAML, "cmd-act-replay-1")
	target := mustCreateDraft(t, service, validContractYAML, "cmd-act-replay-2")
	if _, err := service.Activate(context.Background(), 1, "cmd-act-replay-3", ActivateInput{ContractVersion: 1, ExpectedStateRowVersion: 1, ExpectedTargetRowVersion: 1}); err != nil {
		t.Fatal(err)
	}
	system := seedReadinessSystem(t, db, "alpha", true)
	draft := seedDraft(t, db, system, contractRowOf(t, db, target.Version), 1, "alpha")
	passed := seedRun(t, db, system, draft, contractRowOf(t, db, target.Version), "Passed")
	input := ActivateInput{
		ContractVersion:           target.Version,
		ExpectedStateRowVersion:   2,
		ExpectedCurrentContractID: ptrInt64(1),
		ExpectedTargetRowVersion:  1,
		Items: []ActivationItem{{
			BusinessSystemKey: "alpha", ConfigVersionID: draft, VerificationRunID: passed,
			ExpectedBusinessSystemRowVersion: 2,
		}},
	}

	const callers = 8
	type result struct {
		detail LabelContractDetail
		err    error
	}
	results := make(chan result, callers)
	var start sync.WaitGroup
	start.Add(1)
	for index := 0; index < callers; index++ {
		go func() {
			start.Wait()
			detail, err := service.Activate(context.Background(), 1, "cmd-act-same-replay", input)
			results <- result{detail: detail, err: err}
		}()
	}
	start.Done()
	for index := 0; index < callers; index++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("same command must replay instead of conflict: %v", result.err)
		}
		if result.detail.Version != target.Version || result.detail.State != "active" {
			t.Fatalf("replay returned wrong result: %#v", result.detail)
		}
	}
	var activations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM label_contract_activations WHERE contract_id=?`, contractRowOf(t, db, target.Version)).Scan(&activations); err != nil {
		t.Fatal(err)
	}
	if activations != 1 {
		t.Fatalf("same command must create exactly one activation, got %d", activations)
	}
}

func contractRowOf(t *testing.T, db interface {
	QueryRow(query string, args ...any) *sql.Row
}, version int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM label_contracts WHERE version=?`, version).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
