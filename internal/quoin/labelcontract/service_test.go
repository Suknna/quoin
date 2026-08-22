package labelcontract

// SQLite harness tests over the frozen schema: immutable drafts, the
// zero-enabled-system activation through the real trigger, replay
// idempotency and the deterministic conflict/rollback behavior
// (DATA-CONFIG-002/003/005/006).

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/config"
	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/test.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	// The singleton pointer row is part of bootstrap initialization.
	if _, err := db.Exec(`INSERT INTO label_contract_state(id,row_version,updated_at) VALUES(1,1,?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,enabled,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,'x',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	return db
}

const validContractYAML = "label_contract:\n  business_system_label: business_system\n"

func mustCreateDraft(t *testing.T, service *Service, body, commandID string) LabelContractDetail {
	t.Helper()
	detail, err := service.CreateDraft(context.Background(), 1, commandID, []byte(body), config.Limits{})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	return detail
}

func TestCreateDraftPersistsImmutableProjection(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	detail := mustCreateDraft(t, service, validContractYAML, "cmd-create-0001")
	if detail.Version != 1 || detail.State != "draft" || detail.RowVersion != 1 || detail.ActivatedAt != nil {
		t.Fatalf("draft projection wrong: %#v", detail)
	}
	if detail.ParserVersion != config.ParserVersion || detail.SchemaVersion == "" || len(detail.Digest) != 64 {
		t.Fatalf("provenance missing: %#v", detail)
	}
	if detail.ContractJSON["label_contract"].(map[string]any)["business_system_label"] != "business_system" {
		t.Fatalf("typed projection wrong: %#v", detail.ContractJSON)
	}
	var stored string
	if err := db.QueryRow(`SELECT yaml_body FROM label_contracts WHERE version=1`).Scan(&stored); err != nil || stored != validContractYAML {
		t.Fatalf("yaml body must be stored verbatim: %q %v", stored, err)
	}
	// Content columns are immutable by trigger (reaching the content guard
	// requires the row_version bump the generic trigger demands first).
	if _, err := db.Exec(`UPDATE label_contracts SET yaml_body='tampered', row_version=row_version+1 WHERE version=1`); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("content update must abort: %v", err)
	}
}

func TestCreateDraftRejectsStrictViolations(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	cases := map[string]string{
		"alias":          "label_contract: &base\n  business_system_label: x\nuse: *base\n",
		"unknown field":  "label_contract:\n  business_system_label: x\n  extra: 1\n",
		"second doc":     validContractYAML + "---\nlabel_contract:\n  business_system_label: y\n",
		"invalid label":  "label_contract:\n  business_system_label: \"1bad\"\n",
		"non-string key": "1: x\n",
	}
	for name, body := range cases {
		_, err := service.CreateDraft(context.Background(), 1, "cmd-reject-"+name, []byte(body), config.Limits{})
		var validation *config.ValidationError
		if !errors.As(err, &validation) || len(validation.Errors) == 0 {
			t.Fatalf("%s must reject with field errors, got %v", name, err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM label_contracts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected drafts must not persist: count=%d err=%v", count, err)
	}
}

func TestCreateDraftCommandReplay(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	first := mustCreateDraft(t, service, validContractYAML, "cmd-replay-0001")
	replayed, err := service.CreateDraft(context.Background(), 1, "cmd-replay-0001", []byte(validContractYAML), config.Limits{})
	if err != nil {
		t.Fatalf("replay must succeed: %v", err)
	}
	if replayed.ID != first.ID || replayed.Version != first.Version {
		t.Fatalf("replay must return the original result: %#v vs %#v", replayed, first)
	}
	var count int
	_ = db.QueryRow(`SELECT COUNT(*) FROM label_contracts`).Scan(&count)
	if count != 1 {
		t.Fatalf("replay must not create a second row: %d", count)
	}
	modified := "label_contract:\n  business_system_label: biz_label\n"
	if _, err := service.CreateDraft(context.Background(), 1, "cmd-replay-0001", []byte(modified), config.Limits{}); !errors.Is(err, ErrCommandReused) {
		t.Fatalf("same id with different digest must conflict, got %v", err)
	}
}

func TestActivateZeroSystemsAtomicallySwitchesPointer(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	mustCreateDraft(t, service, validContractYAML, "cmd-create-0002")
	detail, err := service.Activate(context.Background(), 1, "cmd-activate-0002", ActivateInput{
		ContractVersion:           1,
		ExpectedStateRowVersion:   1,
		ExpectedCurrentContractID: nil,
		ExpectedTargetRowVersion:  1,
	})
	if err != nil {
		t.Fatalf("zero-system activation: %v", err)
	}
	if detail.State != "active" || detail.ActivatedAt == nil {
		t.Fatalf("contract must be active: %#v", detail)
	}
	var currentID int64
	var stateRow int64
	if err := db.QueryRow(`SELECT current_contract_id,row_version FROM label_contract_state WHERE id=1`).Scan(&currentID, &stateRow); err != nil {
		t.Fatal(err)
	}
	if currentID != 1 || stateRow != 2 {
		t.Fatalf("pointer must switch exactly once: contract=%d stateRow=%d", currentID, stateRow)
	}
	// The applied activation is sealed: it can never drive the pointer again.
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM label_contract_activations WHERE applied_at IS NOT NULL`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("activation must be sealed: %d %v", applied, err)
	}
	// Replay is idempotent.
	replayed, err := service.Activate(context.Background(), 1, "cmd-activate-0002", ActivateInput{
		ContractVersion: 1, ExpectedStateRowVersion: 1, ExpectedTargetRowVersion: 1,
	})
	if err != nil || replayed.ID != detail.ID {
		t.Fatalf("activation replay must be idempotent: %v", err)
	}
}

func TestActivateRejectsStalePreconditionsAtomically(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	mustCreateDraft(t, service, validContractYAML, "cmd-create-0003")
	_, err := service.Activate(context.Background(), 1, "cmd-activate-0003", ActivateInput{
		ContractVersion: 1, ExpectedStateRowVersion: 1, ExpectedTargetRowVersion: 1,
	})
	if err != nil {
		t.Fatalf("first activation: %v", err)
	}
	// A second contract draft; activating it with the STALE state row version
	// must abort entirely (no pointer move, no partial retirement).
	draft2 := mustCreateDraft(t, service, "label_contract:\n  business_system_label: other_label\n", "cmd-create-0004")
	_, err = service.Activate(context.Background(), 1, "cmd-activate-0004", ActivateInput{
		ContractVersion: 2, ExpectedStateRowVersion: 1, ExpectedTargetRowVersion: draft2.RowVersion,
	})
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Code != "current_pointer_conflict" {
		t.Fatalf("stale state precondition must conflict, got %v", err)
	}
	var current int64
	_ = db.QueryRow(`SELECT current_contract_id FROM label_contract_state WHERE id=1`).Scan(&current)
	if current != 1 {
		t.Fatalf("aborted activation must not move the pointer: %d", current)
	}
	var activeCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM label_contracts WHERE state='active'`).Scan(&activeCount)
	if activeCount != 1 {
		t.Fatalf("aborted activation must not flip states: %d", activeCount)
	}
	// Re-activating the already-active contract is an invalid state.
	if _, err := service.Activate(context.Background(), 1, "cmd-activate-0005", ActivateInput{
		ContractVersion: 1, ExpectedStateRowVersion: 2, ExpectedTargetRowVersion: 2,
	}); err == nil {
		t.Fatal("activating an already-activated contract must fail")
	}
}

func TestActivateCoversEveryEnabledSystem(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	mustCreateDraft(t, service, validContractYAML, "cmd-create-0006")
	if _, err := db.Exec(`INSERT INTO business_systems(id,key,display_name,enabled,row_version,created_at) VALUES(1,'payments','Payments',0,1,?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	// Systems are born Disabled (upload path); flip the harness row to
	// enabled the way a published configuration would project it.
	if _, err := db.Exec(`UPDATE business_systems SET enabled=1, row_version=row_version+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	// One enabled system exists; an empty item set must abort with the
	// coverage conflict and leave the pointer untouched.
	_, err := service.Activate(context.Background(), 1, "cmd-activate-0006", ActivateInput{
		ContractVersion: 1, ExpectedStateRowVersion: 1, ExpectedTargetRowVersion: 1,
	})
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("enabled-system coverage must conflict, got %v", err)
	}
	var current sql.NullInt64
	_ = db.QueryRow(`SELECT current_contract_id FROM label_contract_state WHERE id=1`).Scan(&current)
	if current.Valid {
		t.Fatal("pointer must stay null when activation aborts")
	}
	// A request naming an unknown system key is a deterministic 400.
	_, err = service.Activate(context.Background(), 1, "cmd-activate-0007", ActivateInput{
		ContractVersion: 1, ExpectedStateRowVersion: 1, ExpectedTargetRowVersion: 1,
		Items: []ActivationItem{{BusinessSystemKey: "nope", ExpectedBusinessSystemRowVersion: 1}},
	})
	var badRequest *BadRequestError
	if !errors.As(err, &badRequest) {
		t.Fatalf("unknown system key must be a bad request, got %v", err)
	}
}

func TestDirectPointerUpdateRejected(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	mustCreateDraft(t, service, validContractYAML, "cmd-create-0008")
	if _, err := db.Exec(`UPDATE label_contract_state SET current_contract_id=1 WHERE id=1`); err == nil {
		t.Fatal("external pointer update must be rejected by the trigger")
	}
	if _, err := db.Exec(`DELETE FROM label_contract_state WHERE id=1`); err == nil {
		t.Fatal("pointer row must not be deletable")
	}
}

func TestListAndGet(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	mustCreateDraft(t, service, validContractYAML, "cmd-create-0009")
	mustCreateDraft(t, service, "label_contract:\n  business_system_label: b2\n", "cmd-create-0010")
	items, more, err := service.List(context.Background(), "", 50)
	if err != nil || more || len(items) != 2 || items[0].Version != 2 {
		t.Fatalf("list wrong: %v %v %#v", err, more, items)
	}
	detail, err := service.GetByVersion(context.Background(), 2)
	if err != nil || detail.YAMLBody == "" || detail.ContractJSON == nil {
		t.Fatalf("get wrong: %v %#v", err, detail)
	}
	if _, err := service.GetByVersion(context.Background(), 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version must be NotFound, got %v", err)
	}
}
