package app

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestResourceDiscoveryAdmissionReportsUnreadableMaintenanceFence(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/scheduler.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE maintenance_state(id INTEGER PRIMARY KEY,active INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	started := false
	admit := resourceDiscoveryAdmissionWithStart(db, func(context.Context, string, int64, string) error {
		started = true
		return nil
	})
	if err := admit(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "maintenance fence") {
		t.Fatalf("missing maintenance state must be visible to the scheduler error handler: %v", err)
	}
	if started {
		t.Fatal("unreadable maintenance fence admitted discovery work")
	}
	if _, err := db.Exec(`INSERT INTO maintenance_state(id,active) VALUES(1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := admit(context.Background(), time.Now()); err != nil {
		t.Fatalf("active maintenance is a normal admission pause: %v", err)
	}
	if started {
		t.Fatal("active maintenance admitted discovery work")
	}
}
