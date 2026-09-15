package observation

import (
	"context"
	"testing"
)

func TestCancellationScanOnlyAuditsActualWork(t *testing.T) {
	h := newHarness(t, "prometheus")
	count := func() int {
		t.Helper()
		var n int
		if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action=?`, operationCancel).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	for range 3 {
		cancelled, err := h.service.CancelUnobservable(context.Background())
		if err != nil || len(cancelled) != 0 {
			t.Fatalf("idle scan: %v, %v", cancelled, err)
		}
	}
	if got := count(); got != before {
		t.Fatalf("idle scheduler generated %d audit events", got-before)
	}
	h.startRun(t, "cmd-obs-idle-audit-0001", "enablement", nil)
	if _, err := h.service.CancelUnobservable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != before {
		t.Fatalf("healthy run generated cancellation audits: %d", got-before)
	}
	var version int64
	if err := h.db.QueryRow(`SELECT row_version FROM connections WHERE name='main-prometheus'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := h.conns.Disable(h.adminContext(t), "main-prometheus", version); err != nil {
		t.Fatal(err)
	}
	cancelled, err := h.service.CancelUnobservable(context.Background())
	if err != nil || len(cancelled) == 0 {
		t.Fatalf("cancel disabled run: %v, %v", cancelled, err)
	}
	if got := count(); got != before+1 {
		t.Fatalf("actual cancellation audit count=%d, want %d", got, before+1)
	}
	if _, err := h.service.CancelUnobservable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != before+1 {
		t.Fatalf("finished run generated repeated audits: %d", got-before)
	}
}
