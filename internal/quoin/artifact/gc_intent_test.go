package artifact

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
)

func TestGCIntentFailurePreventsPhysicalDeletion(t *testing.T) {
	db, store := newTestStore(t)
	store.now = func() time.Time { return time.Now().UTC().Add(-91 * 24 * time.Hour) }
	attemptID, toolCallID := seedToolOwner(t, db)
	_, hash := uploadText(t, store, context.Background(), attemptID, toolCallID, "protected expired body")
	store.now = func() time.Time { return time.Now().UTC() }
	if _, err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_gc_intent BEFORE INSERT ON audit_events WHEN NEW.action='%s' BEGIN SELECT RAISE(ABORT,'intent audit unavailable'); END`, operationGCIntent)); err != nil {
		t.Fatal(err)
	}
	if err := store.RunGarbageCollection(context.Background()); err == nil {
		t.Fatal("unrecorded deletion intent must fail")
	}
	if _, err := os.Stat(store.blobPath(hash)); err != nil {
		t.Fatalf("file must survive rejected deletion intent: %v", err)
	}
	if len(fetchAuditEvents(t, db, operationGCIntent)) != 0 || len(fetchAuditEvents(t, db, operationGCComplete)) != 0 {
		t.Fatal("failed intent must not manufacture a collection result")
	}
	if _, err := db.Exec(`DROP TRIGGER fail_gc_intent`); err != nil {
		t.Fatal(err)
	}
	if err := store.RunGarbageCollection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.blobPath(hash)); !os.IsNotExist(err) {
		t.Fatalf("retry did not collect the remaining file: %v", err)
	}
}

func TestGCConfirmationFailureRetainsDurableIntent(t *testing.T) {
	db, store := newTestStore(t)
	store.now = func() time.Time { return time.Now().UTC().Add(-91 * 24 * time.Hour) }
	attemptID, toolCallID := seedToolOwner(t, db)
	_, hash := uploadText(t, store, context.Background(), attemptID, toolCallID, "unconfirmed collected body")
	store.now = func() time.Time { return time.Now().UTC() }
	if _, err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_gc_confirmation BEFORE INSERT ON audit_events WHEN NEW.action='%s' BEGIN SELECT RAISE(ABORT,'confirmation unavailable'); END`, operationGCComplete)); err != nil {
		t.Fatal(err)
	}
	if err := store.RunGarbageCollection(context.Background()); err == nil {
		t.Fatal("confirmation audit failure must surface")
	}
	if _, err := os.Stat(store.blobPath(hash)); !os.IsNotExist(err) {
		t.Fatalf("fixture must reach physical deletion: %v", err)
	}
	begins := fetchAuditEvents(t, db, operationGCIntent)
	if len(begins) != 1 || begins[0].DomainRefType != "artifact_blob" || begins[0].DomainRefID < 1 {
		t.Fatalf("physical deletion lost its prior durable target: %+v", begins)
	}
	if len(fetchAuditEvents(t, db, operationGCComplete)) != 0 {
		t.Fatal("unconfirmed deletion must not claim success")
	}
	if _, err := db.Exec(`DROP TRIGGER fail_gc_confirmation`); err != nil {
		t.Fatal(err)
	}
	if err := store.RunGarbageCollection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fetchAuditEvents(t, db, operationGCComplete)) != 0 || len(fetchAuditEvents(t, db, operationGCIntent)) != 1 {
		t.Fatal("retry must not infer historical success from an absent file")
	}
}

func TestGCRechecksReferencesAfterIntent(t *testing.T) {
	db, store := newTestStore(t)
	store.now = func() time.Time { return time.Now().UTC().Add(-91 * 24 * time.Hour) }
	attemptID, toolCallID := seedToolOwner(t, db)
	artifactID, hash := uploadText(t, store, context.Background(), attemptID, toolCallID, "reused blob body")
	store.now = func() time.Time { return time.Now().UTC() }
	// This fixture hook installs a new live reference at the exact boundary
	// between committed intent and unlink, without a timing-dependent race.
	query := fmt.Sprintf(`CREATE TRIGGER reference_after_gc_intent AFTER INSERT ON audit_events WHEN NEW.action='%s' BEGIN INSERT INTO artifacts(blob_id,kind,media_type,sensitive,retention_kind,owner_type,owner_id,created_at,expires_at) SELECT blob_id,kind,media_type,sensitive,retention_kind,owner_type,owner_id,created_at,expires_at FROM artifacts WHERE id=%d; END`, operationGCIntent, artifactID)
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
	collectionErr := store.RunGarbageCollection(context.Background())
	if collectionErr == nil {
		t.Fatal("superseded collection must not report deletion")
	}
	if _, err := os.Stat(store.blobPath(hash)); err != nil {
		t.Fatalf("newly referenced blob was removed: %v", err)
	}
	completions := fetchAuditEvents(t, db, operationGCComplete)
	if len(completions) != 1 || completions[0].Outcome != audit.OutcomeFailure {
		t.Fatalf("superseded intent result=%+v err=%v", completions, collectionErr)
	}
}
