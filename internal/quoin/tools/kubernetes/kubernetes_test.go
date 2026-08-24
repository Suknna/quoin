package kubernetes

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// TestResolveReadPreflightNeverCreatesGrants uses the resolver's actual SQL
// seam. Routing failures are accepted Tool Call outcomes, not credential or
// network actions; the returned resolution must consequently contain no grant.
func TestResolveReadPreflightNeverCreatesGrants(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE tool_calls (id INTEGER PRIMARY KEY, attempt_id INTEGER NOT NULL, arguments_json TEXT NOT NULL)`,
		`CREATE TABLE business_systems (id INTEGER PRIMARY KEY, key TEXT NOT NULL, display_name TEXT NOT NULL)`,
		`CREATE TABLE business_system_kubernetes_connections (business_system_id INTEGER, connection_id INTEGER, state TEXT)`,
		`CREATE TABLE connections (id INTEGER PRIMARY KEY, current_revision_id INTEGER, current_credential_generation_id INTEGER, enabled INTEGER, revalidation_required INTEGER)`,
		`CREATE TABLE credential_generations (id INTEGER PRIMARY KEY, key_binding_revision INTEGER)`,
		`CREATE TABLE root_key_state (binding_revision INTEGER)`,
		`CREATE TABLE attempt_connection_grants (id INTEGER PRIMARY KEY, attempt_id INTEGER, purpose TEXT, business_system_id INTEGER, connection_id INTEGER, connection_revision_id INTEGER, credential_generation_id INTEGER, created_by_tool_call_id INTEGER, created_at TEXT)`,
		`CREATE TABLE tool_call_connection_grants (tool_call_id INTEGER, connection_grant_id INTEGER, ordinal INTEGER)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	cases := []struct {
		name       string
		argument   string
		seed       []string
		wantCode   string
		wantGrants int
	}{
		{name: "unknown target", argument: "payments-missing", wantCode: "target_not_found"},
		{name: "ambiguous display name", argument: "支付", seed: []string{`INSERT INTO business_systems(id,key,display_name) VALUES(1,'payments-a','支付')`, `INSERT INTO business_systems(id,key,display_name) VALUES(2,'payments-b','支付')`}, wantCode: "target_ambiguous"},
		{name: "unique target with no mapping", argument: "payments", seed: []string{`INSERT INTO business_systems(id,key,display_name) VALUES(1,'payments','支付')`}, wantCode: "no_mapping"},
		// The stable key wins even if a different business system happens to
		// use identical display text.
		{name: "key wins over another display name", argument: "payments", seed: []string{
			`INSERT INTO business_systems(id,key,display_name) VALUES(1,'payments','支付')`,
			`INSERT INTO business_systems(id,key,display_name) VALUES(2,'orders','payments')`,
			`INSERT INTO connections(id,current_revision_id,current_credential_generation_id,enabled,revalidation_required) VALUES(5,11,13,1,0)`,
			`INSERT INTO credential_generations(id,key_binding_revision) VALUES(13,1)`,
			`INSERT INTO root_key_state(binding_revision) VALUES(1)`,
			`INSERT INTO business_system_kubernetes_connections(business_system_id,connection_id,state) VALUES(1,5,'Active')`,
		}, wantGrants: 1},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			for _, table := range []string{"tool_call_connection_grants", "attempt_connection_grants", "business_system_kubernetes_connections", "connections", "credential_generations", "business_systems", "tool_calls"} {
				if _, err := db.Exec("DELETE FROM " + table); err != nil {
					t.Fatal(err)
				}
			}
			for _, seed := range test.seed {
				if _, err := db.Exec(seed); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`INSERT INTO tool_calls(id,attempt_id,arguments_json) VALUES(?,?,?)`, index+1, 77, `{"businessSystem":"`+test.argument+`"}`); err != nil {
				t.Fatal(err)
			}
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			resolution, err := ResolveRead(ctx, conn, 77, int64(index+1))
			conn.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resolution.PreflightCode != test.wantCode || len(resolution.Grants) != test.wantGrants {
				t.Fatalf("resolution=%+v, want code %q and %d grants", resolution, test.wantCode, test.wantGrants)
			}
			var grants int
			if err := db.QueryRow(`SELECT COUNT(*) FROM attempt_connection_grants`).Scan(&grants); err != nil || grants != test.wantGrants {
				t.Fatalf("persisted grants=%d err=%v, want %d", grants, err, test.wantGrants)
			}
		})
	}
}

func TestValidateGrantForFulfillmentRejectsCommittedInvalidators(t *testing.T) {
	invalidators := []struct {
		name  string
		apply func(*sql.DB) error
	}{
		{"mapping retirement", func(db *sql.DB) error {
			_, err := db.Exec(`UPDATE business_system_kubernetes_connections SET state='Retired'`)
			return err
		}},
		{"connection disabled", func(db *sql.DB) error { _, err := db.Exec(`UPDATE connections SET enabled=0`); return err }},
		{"credential rotation", func(db *sql.DB) error {
			_, err := db.Exec(`UPDATE connections SET current_credential_generation_id=2`)
			return err
		}},
		{"revalidation required", func(db *sql.DB) error {
			_, err := db.Exec(`UPDATE connections SET revalidation_required=1`)
			return err
		}},
		{"root key rebind", func(db *sql.DB) error { _, err := db.Exec(`UPDATE root_key_state SET binding_revision=2`); return err }},
	}
	for _, test := range invalidators {
		t.Run(test.name, func(t *testing.T) {
			db := newGrantValidationDB(t)
			defer db.Close()
			if err := test.apply(db); err != nil {
				t.Fatal(err)
			}
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			err = ValidateGrantForFulfillment(context.Background(), conn, 77, 1)
			conn.Close()
			if err == nil {
				t.Fatal("invalidated Kubernetes grant was accepted")
			}
		})
	}
}

func TestValidateGrantForFulfillmentIsPerGrant(t *testing.T) {
	db := newGrantValidationDB(t)
	defer db.Close()
	for _, statement := range []string{
		`INSERT INTO attempt_connection_grants VALUES(2,77,'kubernetes_read',9,6,12,14,42)`,
		`INSERT INTO tool_call_connection_grants VALUES(42,2,1)`,
		`INSERT INTO connections VALUES(6,1,0,12,14)`,
		`INSERT INTO business_system_kubernetes_connections VALUES(9,6,'Active')`,
		`INSERT INTO credential_generations VALUES(14,1)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	// Retirement of grant 1's mapping cannot be masked by still-active grant 2.
	if _, err := db.Exec(`UPDATE business_system_kubernetes_connections SET state='Retired' WHERE connection_id=5`); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateGrantForFulfillment(ctx, conn, 77, 1); err == nil {
		conn.Close()
		t.Fatal("retired requested grant was accepted because a sibling remained active")
	}
	if err := ValidateGrantForFulfillment(ctx, conn, 77, 2); err != nil {
		conn.Close()
		t.Fatalf("active sibling grant rejected after another mapping retired: %v", err)
	}
	conn.Close()
	// Conversely, invalid grant 2 must not deny grant 1.
	if _, err := db.Exec(`UPDATE business_system_kubernetes_connections SET state='Active' WHERE connection_id=5`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE connections SET enabled=0 WHERE id=6`); err != nil {
		t.Fatal(err)
	}
	conn, err = db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := ValidateGrantForFulfillment(ctx, conn, 77, 1); err != nil {
		t.Fatalf("valid requested grant rejected because sibling was invalid: %v", err)
	}
	if err := ValidateGrantForFulfillment(ctx, conn, 77, 2); err == nil {
		t.Fatal("disabled requested grant was accepted")
	}
}

func newGrantValidationDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE attempt_connection_grants (id INTEGER PRIMARY KEY, attempt_id INTEGER, purpose TEXT, business_system_id INTEGER, connection_id INTEGER, connection_revision_id INTEGER, credential_generation_id INTEGER, created_by_tool_call_id INTEGER)`,
		`CREATE TABLE tool_call_connection_grants (tool_call_id INTEGER, connection_grant_id INTEGER, ordinal INTEGER)`,
		`CREATE TABLE connections (id INTEGER PRIMARY KEY, enabled INTEGER, revalidation_required INTEGER, current_revision_id INTEGER, current_credential_generation_id INTEGER)`,
		`CREATE TABLE business_system_kubernetes_connections (business_system_id INTEGER, connection_id INTEGER, state TEXT)`,
		`CREATE TABLE credential_generations (id INTEGER PRIMARY KEY, key_binding_revision INTEGER)`,
		`CREATE TABLE root_key_state (binding_revision INTEGER)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO attempt_connection_grants VALUES(1,77,'kubernetes_read',9,5,11,13,42)`,
		`INSERT INTO tool_call_connection_grants VALUES(42,1,0)`,
		`INSERT INTO connections VALUES(5,1,0,11,13)`,
		`INSERT INTO business_system_kubernetes_connections VALUES(9,5,'Active')`,
		`INSERT INTO credential_generations VALUES(13,1)`,
		`INSERT INTO root_key_state VALUES(1)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	return db
}

func TestResolveReadReusesCompatibleAttemptGrant(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE tool_calls (id INTEGER PRIMARY KEY, attempt_id INTEGER NOT NULL, arguments_json TEXT NOT NULL)`,
		`CREATE TABLE business_systems (id INTEGER PRIMARY KEY, key TEXT NOT NULL, display_name TEXT NOT NULL)`,
		`CREATE TABLE business_system_kubernetes_connections (business_system_id INTEGER, connection_id INTEGER, state TEXT)`,
		`CREATE TABLE connections (id INTEGER PRIMARY KEY, current_revision_id INTEGER, current_credential_generation_id INTEGER, enabled INTEGER, revalidation_required INTEGER)`,
		`CREATE TABLE credential_generations (id INTEGER PRIMARY KEY, key_binding_revision INTEGER)`, `CREATE TABLE root_key_state (binding_revision INTEGER)`,
		`CREATE TABLE attempt_connection_grants (id INTEGER PRIMARY KEY AUTOINCREMENT, attempt_id INTEGER, purpose TEXT, business_system_id INTEGER, connection_id INTEGER, connection_revision_id INTEGER, credential_generation_id INTEGER, created_by_tool_call_id INTEGER, created_at TEXT, UNIQUE(attempt_id,purpose,business_system_id,connection_id,connection_revision_id,credential_generation_id))`,
		`CREATE TABLE tool_call_connection_grants (tool_call_id INTEGER, connection_grant_id INTEGER, ordinal INTEGER)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO business_systems VALUES(1,'payments','Payments')`, `INSERT INTO business_system_kubernetes_connections VALUES(1,5,'Active')`,
		`INSERT INTO connections VALUES(5,11,13,1,0)`, `INSERT INTO credential_generations VALUES(13,1)`, `INSERT INTO root_key_state VALUES(1)`,
		`INSERT INTO tool_calls VALUES(1,77,'{"businessSystem":"payments"}')`, `INSERT INTO tool_calls VALUES(2,77,'{"businessSystem":"payments"}')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	for _, callID := range []int64{1, 2} {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ResolveRead(ctx, conn, 77, callID)
		conn.Close()
		if err != nil {
			t.Fatalf("call %d: %v", callID, err)
		}
	}
	var grants, joins int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempt_connection_grants`).Scan(&grants); err != nil || grants != 1 {
		t.Fatalf("grants=%d err=%v", grants, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM tool_call_connection_grants`).Scan(&joins); err != nil || joins != 2 {
		t.Fatalf("joins=%d err=%v", joins, err)
	}
}
