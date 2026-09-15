package audit

// Query tests execute against the applied contract schema
// (internal/gen/contracts/schema.sql), seeding audit_events and
// audit_event_targets directly to control record order, timestamps and
// mixed-precision RFC3339Nano fractions.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	_ "modernc.org/sqlite"
)

func newQueryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "query.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedQueryEvent(t *testing.T, db *sql.DB, event EventView) int64 {
	t.Helper()
	var (
		correlation, request, initiatorType, commandID, refType any
		initiatorID, refID                                      any
	)
	if event.CorrelationID != "" {
		correlation = event.CorrelationID
	}
	if event.RequestID != "" {
		request = event.RequestID
	}
	if event.InitiatorType != "" {
		initiatorType = event.InitiatorType
		initiatorID = event.InitiatorID
	}
	if event.ClientCommandID != "" {
		commandID = event.ClientCommandID
	}
	if event.DomainRefType != "" {
		refType = event.DomainRefType
		refID = event.DomainRefID
	}
	phase := event.Phase
	if phase == "" {
		phase = "execute"
	}
	res, err := db.Exec(`INSERT INTO audit_events(
			actor_type,actor_id,action,correlation_id,request_id,phase,
			initiator_type,initiator_id,client_command_id,outcome,
			domain_ref_type,domain_ref_id,created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		event.ActorType, event.ActorID, event.Action, correlation, request, phase,
		initiatorType, initiatorID, commandID, event.Outcome,
		refType, refID, event.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range event.Targets {
		var version any
		if target.Version != 0 {
			version = target.Version
		}
		if _, err := db.Exec(`INSERT INTO audit_event_targets(audit_event_id,target_type,target_id,target_version) VALUES(?,?,?,?)`,
			id, target.Type, target.ID, version); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestQueryEventsPagesNewestFirstWithTargets(t *testing.T) {
	db := newQueryDB(t)
	ctx := context.Background()
	seedQueryEvent(t, db, EventView{
		ActorType: ActorUser, ActorID: 7, Action: "item.create", CorrelationID: "corr-1",
		Phase: "execute", Outcome: QueryOutcomeSuccess, CreatedAt: "2026-06-01T12:00:00Z",
		Targets: []TargetView{{Type: "item", ID: 11, Version: 3}},
	})
	seedQueryEvent(t, db, EventView{
		ActorType: ActorSystem, ActorID: 0, Action: "retention.cleanup", Outcome: QueryOutcomeSuccess,
		InitiatorType: ActorUser, InitiatorID: 7, CreatedAt: "2026-06-01T12:00:00.500000000Z",
		Targets: []TargetView{{Type: "item", ID: 12}, {Type: "item", ID: 13}},
	})
	seedQueryEvent(t, db, EventView{
		ActorType: ActorUser, ActorID: 8, Action: "item.update", Outcome: QueryOutcomeRejected,
		ClientCommandID: "cmd-9", CreatedAt: "2026-06-02T09:30:00Z",
	})

	page, err := QueryEvents(ctx, db, Filter{}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 3 || page.NextCursor != "" {
		t.Fatalf("single page events=%d cursor=%q, want 3 without cursor", len(page.Events), page.NextCursor)
	}
	// Record order: newest id first.
	if page.Events[0].Action != "item.update" || page.Events[2].Action != "item.create" {
		t.Fatalf("order actions=%s,%s,%s", page.Events[0].Action, page.Events[1].Action, page.Events[2].Action)
	}
	targets := page.Events[2].Targets
	if len(targets) != 1 || targets[0].Type != "item" || targets[0].ID != 11 || targets[0].Version != 3 {
		t.Fatalf("targets=%+v, want item/11/version 3", targets)
	}
	if len(page.Events[1].Targets) != 2 || page.Events[0].Targets != nil {
		t.Fatalf("target attachment wrong: %+v / %+v", page.Events[1].Targets, page.Events[0].Targets)
	}
	system := page.Events[1]
	if system.ActorType != ActorSystem || system.ActorID != 0 || system.InitiatorType != ActorUser || system.InitiatorID != 7 {
		t.Fatalf("system actor/initiator=%s/%d/%s/%d", system.ActorType, system.ActorID, system.InitiatorType, system.InitiatorID)
	}

	// Keyset pagination walks every event exactly once, oldest page last.
	var (
		seen     []int64
		cursor   string
		rounds   int
		defaults = 2
	)
	for {
		page, err := QueryEvents(ctx, db, Filter{}, cursor, defaults)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) > defaults {
			t.Fatalf("page overrun: %d events", len(page.Events))
		}
		for _, event := range page.Events {
			seen = append(seen, event.ID)
		}
		rounds++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if rounds > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 3 || seen[0] != 3 || seen[2] != 1 {
		t.Fatalf("paged ids=%v, want [3 2 1]", seen)
	}
	if _, err := QueryEvents(ctx, db, Filter{}, "not-a-cursor", 2); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("malformed cursor err=%v, want ErrInvalidCursor", err)
	}
}

func TestQueryEventsAppliesAllFilters(t *testing.T) {
	db := newQueryDB(t)
	ctx := context.Background()
	seedQueryEvent(t, db, EventView{
		ActorType: ActorUser, ActorID: 7, Action: "item.create", CorrelationID: "corr-a",
		Outcome: QueryOutcomeSuccess, DomainRefType: "item", DomainRefID: 11,
		CreatedAt: "2026-06-01T12:00:00Z",
	})
	seedQueryEvent(t, db, EventView{
		ActorType: ActorUser, ActorID: 7, Action: "item.delete", CorrelationID: "corr-a",
		Outcome: QueryOutcomeFailure, DomainRefType: "item", DomainRefID: 12,
		CreatedAt: "2026-06-01T12:00:00.500000000Z",
	})
	seedQueryEvent(t, db, EventView{
		ActorType: ActorService, ActorID: 2, Action: "item.create", CorrelationID: "corr-b",
		Outcome: QueryOutcomeSuccess, DomainRefType: "item", DomainRefID: 11,
		CreatedAt: "2026-06-15T08:00:00Z",
	})

	cases := []struct {
		name    string
		filter  Filter
		wantIDs []int64
	}{
		{"correlation", Filter{CorrelationID: "corr-a"}, []int64{2, 1}},
		{"actor type and id", Filter{ActorType: ActorUser, ActorID: 7}, []int64{2, 1}},
		{"actor id only", Filter{ActorID: 2}, []int64{3}},
		{"action", Filter{Action: "item.delete"}, []int64{2}},
		{"outcome", Filter{Outcome: QueryOutcomeFailure}, []int64{2}},
		{"domain ref", Filter{DomainRefType: "item", DomainRefID: 12}, []int64{2}},
		{"since inclusive", Filter{Since: "2026-06-01T12:00:00Z"}, []int64{3, 2, 1}},
		{"until exclusive", Filter{Until: "2026-06-01T12:00:01Z"}, []int64{2, 1}},
		// Fractional boundary against a stored zero-fraction row: stored
		// RFC3339Nano trims trailing zeros, so lexicographic comparison would
		// be wrong here (":00Z" sorts after ":00.1Z"); julianday decides and
		// must neither falsely exclude the .0 row from a :00Z since nor
		// include it past a :00.1 since.
		{"since at zero-fraction second", Filter{Since: "2026-06-01T12:00:00Z"}, []int64{3, 2, 1}},
		{"since past mid-second fraction", Filter{Since: "2026-06-01T12:00:00.100000000Z"}, []int64{3, 2}},
		{"until before mid-second fraction", Filter{Until: "2026-06-01T12:00:00.100000000Z"}, []int64{1}},
		{"narrow window", Filter{Since: "2026-06-01T12:00:00.250000000Z", Until: "2026-06-15T00:00:00Z"}, []int64{2}},
		{"combined", Filter{CorrelationID: "corr-a", Outcome: QueryOutcomeSuccess, Since: "2026-01-01T00:00:00Z"}, []int64{1}},
		{"no match", Filter{CorrelationID: "corr-z"}, nil},
	}
	for _, testCase := range cases {
		page, err := QueryEvents(ctx, db, testCase.filter, "", 50)
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		var gotIDs []int64
		for _, event := range page.Events {
			gotIDs = append(gotIDs, event.ID)
		}
		if page.NextCursor != "" {
			t.Fatalf("%s: unexpected next cursor", testCase.name)
		}
		if len(gotIDs) != len(testCase.wantIDs) {
			t.Fatalf("%s: ids=%v, want %v", testCase.name, gotIDs, testCase.wantIDs)
		}
		for i := range gotIDs {
			if gotIDs[i] != testCase.wantIDs[i] {
				t.Fatalf("%s: ids=%v, want %v", testCase.name, gotIDs, testCase.wantIDs)
			}
		}
	}

	if _, err := QueryEvents(ctx, db, Filter{Outcome: "ok"}, "", 2); !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("invalid outcome err=%v, want ErrInvalidFilter", err)
	}
	if _, err := QueryEvents(ctx, db, Filter{ActorType: "robot"}, "", 2); !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("invalid actor err=%v, want ErrInvalidFilter", err)
	}
	if _, err := QueryEvents(ctx, db, Filter{Since: "junk"}, "", 2); !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("invalid since err=%v, want ErrInvalidFilter", err)
	}
}

func TestGetEventReturnsTargetsOrNoRows(t *testing.T) {
	db := newQueryDB(t)
	id := seedQueryEvent(t, db, EventView{
		ActorType: ActorUser, ActorID: 7, Action: "item.create", CorrelationID: "corr-a",
		Phase: "access", Outcome: QueryOutcomeSuccess, CreatedAt: "2026-06-01T12:00:00Z",
		Targets: []TargetView{{Type: "item", ID: 11, Version: 2}},
	})
	event, err := GetEvent(context.Background(), db, id)
	if err != nil {
		t.Fatal(err)
	}
	if event.CorrelationID != "corr-a" || event.Phase != "access" || len(event.Targets) != 1 {
		t.Fatalf("event=%+v", event)
	}
	if _, err := GetEvent(context.Background(), db, id+100); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing event err=%v, want sql.ErrNoRows", err)
	}
}
