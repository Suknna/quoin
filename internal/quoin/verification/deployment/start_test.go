package deployment_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/verification/deployment"
)

// An empty deployment still owns the full site denominator: the two
// deployment-target helper scenarios and the sixteen release-frozen
// Chromium UI observation cells. Objects add expansion items on top.
func TestStartFreezesImmutableInvocation(t *testing.T) {
	h := newHarness(t)
	invocation := h.start()

	manifest := h.queryInt(`SELECT COUNT(*) FROM verification_invocation_manifests WHERE id=?`, invocation)
	if manifest != 1 {
		t.Fatalf("manifest rows = %d", manifest)
	}
	var startedAt, deadlineAt string
	var itemCount int
	if err := h.db.QueryRow(`SELECT started_at,deadline_at,item_count FROM verification_invocation_manifests WHERE id=?`, invocation).Scan(&startedAt, &deadlineAt, &itemCount); err != nil {
		t.Fatal(err)
	}
	started, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		t.Fatal(err)
	}
	deadline, err := time.Parse(time.RFC3339Nano, deadlineAt)
	if err != nil {
		t.Fatal(err)
	}
	if deadline.Sub(started) != 8*time.Hour {
		t.Fatalf("deadline window = %s, want exactly 8h", deadline.Sub(started))
	}
	if itemCount != 18 {
		t.Fatalf("item count = %d, want 18 (2 deployment helper items + 16 release-frozen ui cells)", itemCount)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_invocation_items WHERE invocation_id=?`, invocation); got != 18 {
		t.Fatalf("frozen items = %d", got)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_deployment_item_locators l JOIN verification_invocation_items i ON i.id=l.item_id WHERE i.invocation_id=?`, invocation); got != 2 {
		t.Fatalf("deployment locators = %d", got)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_ui_observation_item_locators l JOIN verification_invocation_items i ON i.id=l.item_id WHERE i.invocation_id=?`, invocation); got != 16 {
		t.Fatalf("ui locators = %d", got)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_ui_observation_item_locators l JOIN verification_invocation_items i ON i.id=l.item_id WHERE i.invocation_id=? AND l.browser_artifact='branded_chrome'`, invocation); got != 0 {
		t.Fatalf("branded cells must not enter the denominator without a frozen build: %d", got)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_invocation_items WHERE invocation_id=? AND object_kind='browser_identity'`, invocation); got != 0 {
		t.Fatalf("healthy site fabricates no recovery item: %d", got)
	}

	// The same client command replays to the same invocation.
	replayed, err := h.service.Start(t.Context(), h.sessionID, h.userID, "cmd-start")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed != invocation {
		t.Fatalf("replay returned %d, want %d", replayed, invocation)
	}
	if got := h.queryInt(`SELECT COUNT(*) FROM verification_invocation_manifests`); got != 1 {
		t.Fatalf("replay created a second manifest: %d", got)
	}

	// The frozen rows are immutable by trigger.
	if _, err := h.db.Exec(`UPDATE verification_invocation_manifests SET item_count=1 WHERE id=?`, invocation); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("manifest update must abort, got %v", err)
	}
	if _, err := h.db.Exec(`DELETE FROM verification_invocation_items WHERE invocation_id=?`, invocation); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("item delete must abort, got %v", err)
	}
}

func TestStartRequiresFrozenDeploymentBinding(t *testing.T) {
	h := newBoundHarness(t, nil)
	if _, err := h.service.Start(t.Context(), h.sessionID, h.userID, "cmd-start"); err == nil {
		t.Fatal("start on a deployment without a frozen release subject must be refused")
	}
}

func TestStartRequiresActiveAdminSession(t *testing.T) {
	h := newHarness(t)
	if _, err := h.service.Start(t.Context(), h.sessionID+999, h.userID, "cmd-a"); err == nil {
		t.Fatal("unknown session must be refused")
	}
	if _, err := h.service.Start(t.Context(), h.sessionID, h.userID+999, "cmd-b"); err == nil {
		t.Fatal("session bound to another principal must be refused")
	}
}

// Client command collision: the same command id with a different semantic
// request is a conflict, not a silent second invocation.
func TestStartCommandCollisionIsConflict(t *testing.T) {
	h := newHarness(t)
	h.start()
	if _, err := h.service.Start(t.Context(), h.sessionID+1, h.userID, "cmd-start"); err == nil {
		t.Fatal("expected conflict for reused command key under another session")
	} else if serviceErr, ok := err.(*deployment.Error); !ok || string(serviceErr.Family) != "conflict" {
		t.Fatalf("expected conflict family, got %v", err)
	}
}
