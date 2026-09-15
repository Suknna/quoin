package embedding

import (
	"context"
	"testing"
)

func TestIdleSweepDoesNotCreateAuditOperations(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	service := NewService(db)
	for range 3 {
		if err := service.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action=?`, opSweep).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("provider-free idle scans generated %d audit events", count)
	}
}

func TestSettledGenerationSweepDoesNotRepeatAudit(t *testing.T) {
	h := newHarness(t)
	defer h.db.Close()
	h.sweepOK(t)
	var before int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action=?`, opSweep).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 1 {
		t.Fatalf("first real generation transition generated %d audits", before)
	}
	for range 3 {
		h.sweepOK(t)
	}
	var after int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action=?`, opSweep).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("settled generation scans generated %d extra audits", after-before)
	}
}
