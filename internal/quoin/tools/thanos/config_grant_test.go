package thanos

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/execution"
	_ "modernc.org/sqlite"
)

// Exercise the production SQL and predicate inside a real guarded transaction,
// independently of connection creation, probes, and the inspection fixture.
func TestValidateConfigGrantForExecutionDenialReasons(t *testing.T) {
	cases := []struct {
		name   string
		change string
		reason string
	}{
		{name: "current pair"},
		{"disabled", "UPDATE connections SET enabled=0", "connection disabled or pending revalidation"},
		{"pending revalidation", "UPDATE connections SET revalidation_required=1", "connection disabled or pending revalidation"},
		{"revision changed", "UPDATE connections SET current_revision_id=11", "frozen revision/generation pair no longer current"},
		{"generation changed", "UPDATE connections SET current_credential_generation_id=21", "frozen revision/generation pair no longer current"},
		{"revision missing", "UPDATE connections SET current_revision_id=NULL", "frozen revision/generation pair no longer current"},
		{"generation missing", "UPDATE connections SET current_credential_generation_id=NULL", "frozen revision/generation pair no longer current"},
		{"root rebound", "UPDATE root_key_state SET binding_revision=2", "credential root binding drifted"},
		{"grant missing", "DELETE FROM attempt_connection_grants", "config grant binding missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "grant.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			// Only the columns read by the validator are needed. Lifecycle and
			// schema-trigger integration remain covered by connections tests.
			_, err = db.Exec(`
				CREATE TABLE connections(id INTEGER PRIMARY KEY, enabled INTEGER, revalidation_required INTEGER, current_revision_id INTEGER, current_credential_generation_id INTEGER);
				CREATE TABLE credential_generations(id INTEGER PRIMARY KEY, key_binding_revision INTEGER);
				CREATE TABLE root_key_state(binding_revision INTEGER);
				CREATE TABLE attempt_connection_grants(attempt_id INTEGER, purpose TEXT, connection_id INTEGER, connection_revision_id INTEGER, credential_generation_id INTEGER);
				INSERT INTO connections VALUES(1,1,0,10,20);
				INSERT INTO credential_generations VALUES(20,1);
				INSERT INTO root_key_state VALUES(1);
				INSERT INTO attempt_connection_grants VALUES(1,'config_thanos_query',1,10,20);`)
			if err != nil {
				t.Fatal(err)
			}
			if tc.change != "" {
				if _, err := db.Exec(tc.change); err != nil {
					t.Fatal(err)
				}
			}
			registry := execution.NewRegistry()
			op, err := registry.Register(execution.Operation{
				Name: "test.config-grant.validate", Class: execution.ClassWrite, ObjectType: "attempt",
				Authorize: func(context.Context, *execution.Tx) error { return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
				CorrelationID: "config-grant-regression",
				Actor:         execution.Principal{Kind: execution.PrincipalSystem},
				Source:        execution.Source{Kind: execution.SourceTask},
			})
			if err != nil {
				t.Fatal(err)
			}
			// Roll back after observing the validator, including the valid case:
			// this focused read test does not need an audit schema or writes.
			rollback := errors.New("test observation complete")
			var validationErr error
			_, err = execution.Execute(ctx, execution.NewRunner(db, registry, nil), op,
				func(tx *execution.Tx) (int64, error) {
					validationErr = ValidateConfigGrantForExecution(ctx, tx, 1)
					return 0, rollback
				}, func(id int64) int64 { return id })
			if !errors.Is(err, rollback) {
				t.Fatalf("guarded transaction did not reach validator: %v", err)
			}
			if tc.reason == "" {
				if validationErr != nil {
					t.Fatalf("current pair must execute: %v", validationErr)
				}
				return
			}
			if !errors.Is(validationErr, ErrGrantNotCurrent) {
				t.Fatalf("must fail closed with ErrGrantNotCurrent, got %v", validationErr)
			}
			if !strings.Contains(validationErr.Error(), tc.reason) {
				t.Fatalf("denial must identify %q, got %v", tc.reason, validationErr)
			}
		})
	}
}
