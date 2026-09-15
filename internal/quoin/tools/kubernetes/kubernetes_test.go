package kubernetes

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/execution"
	_ "modernc.org/sqlite"
)

// fixtureAuditTables are the two audit tables the runner's automatic audit
// write needs. They mirror the generated authority's relevant columns; the
// historical sealed-helper fixtures otherwise stay deliberately minimal.
var fixtureAuditTables = []string{
	`CREATE TABLE audit_events (id INTEGER PRIMARY KEY AUTOINCREMENT, actor_type TEXT NOT NULL, actor_id INTEGER NOT NULL, action TEXT NOT NULL, correlation_id TEXT, request_id TEXT, phase TEXT NOT NULL DEFAULT 'execute', initiator_type TEXT, initiator_id INTEGER, client_command_id TEXT, outcome TEXT NOT NULL, domain_ref_type TEXT, domain_ref_id INTEGER, created_at TEXT NOT NULL)`,
	`CREATE TABLE audit_event_targets (id INTEGER PRIMARY KEY AUTOINCREMENT, audit_event_id INTEGER NOT NULL, target_type TEXT NOT NULL, target_id INTEGER NOT NULL, target_version INTEGER)`,
}

// runSealedHelper drives one historical sealed kubernetes helper exactly as
// the retired attempt machine and grant fulfillment did: composed on a
// runner-owned guarded Tx inside a dedicated registered fixture operation
// carrying trusted system metadata — never on a raw pool connection. Each
// call owns a fresh runner registry, so repeated fixture runs stay
// independent. Errors surface verbatim after the runner's rollback, keeping
// the helpers' failure semantics with no extra business leftovers.
func runSealedHelper[T any](t *testing.T, db *sql.DB, name string, objectID int64, run func(ctx context.Context, tx *execution.Tx) (T, error)) (T, error) {
	t.Helper()
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	op, err := runner.Register(execution.Operation{
		Name:       "test.kubernetes." + name,
		Class:      execution.ClassWrite,
		ObjectType: "attempt_connection_grant",
		Authorize:  func(context.Context, *execution.Tx) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "test-kubernetes-" + name,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceTask},
	})
	if err != nil {
		t.Fatal(err)
	}
	return execution.Execute(ctx, runner, op, func(tx *execution.Tx) (T, error) {
		return run(ctx, tx)
	}, func(T) int64 { return objectID })
}

// resolveSealedRead routes one kubernetes_read Tool Call through the runner
// transaction, mirroring the production grant-resolution composition.
func resolveSealedRead(t *testing.T, db *sql.DB, attemptID, toolCallID int64) (attempt.ToolResolution, error) {
	t.Helper()
	return runSealedHelper(t, db, "resolve_read", attemptID, func(ctx context.Context, tx *execution.Tx) (attempt.ToolResolution, error) {
		return ResolveRead(ctx, tx, attemptID, toolCallID)
	})
}

// validateSealedGrant fences one grant for fulfillment through the runner
// transaction, mirroring the production FulfillGrant composition.
func validateSealedGrant(t *testing.T, db *sql.DB, attemptID, grantID int64) error {
	t.Helper()
	_, err := runSealedHelper(t, db, "validate_grant", attemptID, func(ctx context.Context, tx *execution.Tx) (struct{}, error) {
		return struct{}{}, ValidateGrantForFulfillment(ctx, tx, attemptID, grantID)
	})
	return err
}

// TestResolveReadPreflightNeverCreatesGrants uses the resolver's actual SQL
// seam. Routing failures are accepted Tool Call outcomes, not credential or
// network actions; the returned resolution must consequently contain no grant.
func TestResolveReadPreflightNeverCreatesGrants(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// One pooled connection keeps the in-memory database (and the serialized
	// writer discipline the runner transaction relies on) deterministic.
	db.SetMaxOpenConns(1)
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
	for _, statement := range fixtureAuditTables {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
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
			resolution, err := resolveSealedRead(t, db, 77, int64(index+1))
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
			if err := validateSealedGrant(t, db, 77, 1); err == nil {
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
	// Retirement of grant 1's mapping cannot be masked by still-active grant 2.
	if _, err := db.Exec(`UPDATE business_system_kubernetes_connections SET state='Retired' WHERE connection_id=5`); err != nil {
		t.Fatal(err)
	}
	if err := validateSealedGrant(t, db, 77, 1); err == nil {
		t.Fatal("retired requested grant was accepted because a sibling remained active")
	}
	if err := validateSealedGrant(t, db, 77, 2); err != nil {
		t.Fatalf("active sibling grant rejected after another mapping retired: %v", err)
	}
	// Conversely, invalid grant 2 must not deny grant 1.
	if _, err := db.Exec(`UPDATE business_system_kubernetes_connections SET state='Active' WHERE connection_id=5`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE connections SET enabled=0 WHERE id=6`); err != nil {
		t.Fatal(err)
	}
	if err := validateSealedGrant(t, db, 77, 1); err != nil {
		t.Fatalf("valid requested grant rejected because sibling was invalid: %v", err)
	}
	if err := validateSealedGrant(t, db, 77, 2); err == nil {
		t.Fatal("disabled requested grant was accepted")
	}
}

func newGrantValidationDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// One pooled connection keeps the in-memory database (and the serialized
	// writer discipline the runner transaction relies on) deterministic.
	db.SetMaxOpenConns(1)
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
	for _, statement := range fixtureAuditTables {
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
	// One pooled connection keeps the in-memory database (and the serialized
	// writer discipline the runner transaction relies on) deterministic.
	db.SetMaxOpenConns(1)
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
	for _, statement := range fixtureAuditTables {
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
	var firstGrant, firstRevision, firstGeneration int64
	for _, callID := range []int64{1, 2} {
		if callID == 2 {
			if _, err := db.Exec(`INSERT INTO credential_generations VALUES(14,2)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE connections SET current_revision_id=12,current_credential_generation_id=14 WHERE id=5`); err != nil {
				t.Fatal(err)
			}
		}
		resolution, err := resolveSealedRead(t, db, 77, callID)
		if err != nil {
			t.Fatalf("call %d: %v", callID, err)
		}
		if len(resolution.Grants) != 1 {
			t.Fatalf("call %d grants=%+v", callID, resolution.Grants)
		}
		if callID == 1 {
			firstGrant, firstRevision, firstGeneration = resolution.Grants[0].GrantID, resolution.Grants[0].ConnectionRevisionID, resolution.Grants[0].CredentialGenerationID
		} else if grant := resolution.Grants[0]; grant.GrantID != firstGrant || grant.ConnectionRevisionID != firstRevision || grant.CredentialGenerationID != firstGeneration {
			t.Fatalf("second call changed frozen grant: first=%d/%d/%d second=%+v", firstGrant, firstRevision, firstGeneration, grant)
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

func TestEvidenceForArtifactBackedPartialFailure(t *testing.T) {
	payload := []byte(`{"success":false,"operation":"discovery","observedAt":"2026-08-24T00:00:00Z","errorCode":"partial_failure","errorDetail":"one mapping failed","results":[{"success":false,"errorCode":"grant_missing","errorDetail":"denied"},{"success":true,"output":"{}","truncated":false}]}`)
	projection, err := EvidenceFor([]byte(`{"businessSystem":"payments","operation":"discovery"}`), payload, 99)
	if err != nil {
		t.Fatal(err)
	}
	if projection.ArtifactID != 99 || projection.Integrity != "incomplete" || len(projection.ErrorsJSON) == 0 || len(projection.ResultJSON) != 0 {
		t.Fatalf("projection=%+v", projection)
	}
	allFailure := []byte(`{"success":false,"operation":"discovery","observedAt":"2026-08-24T00:00:00Z","errorCode":"partial_failure","errorDetail":"all failed","results":[{"success":false,"errorCode":"grant_missing","errorDetail":"denied"}]}`)
	if _, err := EvidenceFor([]byte(`{}`), allFailure, 99); err == nil {
		t.Fatal("all-failure result unexpectedly produced Evidence")
	}
}
