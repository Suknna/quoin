package app

// The source observation scheduler inherits the old discovery scheduler's
// maintenance guarantees: an unreadable maintenance fence is a visible
// scheduler error and active maintenance is a normal admission pause. The
// durability moved into the observation authority, so the guarantees are
// exercised through its public AdmitDue surface (ADR-0004: no compatibility
// scheduler, the declaration-driven admission path is gone).

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/observation"
	_ "modernc.org/sqlite"
)

func TestSourceObservationAdmissionReportsUnreadableMaintenanceFence(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/scheduler.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	// An unreadable singleton (dropped table) must surface as a scheduler
	// error, never as a silent skip: the admission gate owns the last word.
	if _, err := db.Exec(`DROP TABLE maintenance_state`); err != nil {
		t.Fatal(err)
	}
	// An isolated minimal catalog keeps this test decoupled from the shared
	// builtin catalog's tool-schema churn; only discover capability matters.
	registry := plugins.NewRegistry()
	if err := registry.RegisterDescriptor(plugins.Descriptor{
		ID: "prometheus", Version: "1", DisplayName: "Prometheus", Description: "metrics source",
		Capabilities:   []plugins.Capability{plugins.CapabilityDiscover},
		ConnectionKind: "prometheus",
		DiscoverObjects: []plugins.DiscoverObject{
			{ObjectType: "target", IdentityLabels: []string{"job", "instance"}, Query: "up", Limit: 500},
		},
	}); err != nil {
		t.Fatal(err)
	}
	enabled, err := registry.ResolveEnabled(nil)
	if err != nil {
		t.Fatal(err)
	}
	service := observation.NewService(db, registry, enabled)
	// The missing singleton makes the fence unreadable: the scheduler error
	// handler must see it instead of silently skipping a pass.
	if err := service.AdmitDue(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "maintenance fence") {
		t.Fatalf("missing maintenance state must be visible to the scheduler error handler: %v", err)
	}
	// Active maintenance is a normal pause, not a scheduler fault.
	if _, err := db.Exec(`CREATE TABLE maintenance_state(id INTEGER PRIMARY KEY CHECK (id = 1),active INTEGER NOT NULL,reason TEXT,row_version INTEGER NOT NULL,entered_at TEXT,entered_by_type TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO maintenance_state(id,active,reason,row_version,entered_at,entered_by_type) VALUES(1,1,'Upgrade',1,'2026-01-01T00:00:00Z','system')`); err != nil {
		t.Fatal(err)
	}
	if err := service.AdmitDue(context.Background(), time.Now()); err != nil {
		t.Fatalf("active maintenance is a normal admission pause: %v", err)
	}
}

func TestSourceObservationSchedulerFailsClosedWithoutWiring(t *testing.T) {
	// A scheduler without its authority or dispatch kick is inert, never
	// nil-panicking: construction wiring mistakes must be loud at the call
	// site, not at the first timer tick.
	scheduler := NewSourceObservationScheduler(nil, nil)
	scheduler.Run(context.Background(), func(error) {})
}
