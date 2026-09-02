package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
)

func TestVerifyRejectsExtraArchiveMember(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	value, err := service.RunOffline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(service.config.BackupDirectory, value.ID, "unexpected.txt"), []byte("unexpected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = Verify(filepath.Join(service.config.BackupDirectory, value.ID)); err == nil {
		t.Fatal("archive with an extra member verified")
	}
}

func TestVerifyReleaseRejectsACompleteForeignRelease(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	value, err := service.RunOffline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(service.config.BackupDirectory, value.ID)
	if err := VerifyRelease(archive, buildinfo.Release); err != nil {
		t.Fatalf("matching release rejected: %v", err)
	}
	if err := VerifyRelease(archive, "v999.0.0"); err == nil {
		t.Fatal("foreign release accepted")
	}
}

func TestPrepareArchiveRejectsExtraMemberBeforeStreaming(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	value, err := service.RunOffline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(service.config.BackupDirectory, value.ID, "unexpected.txt"), []byte("unexpected"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, cleanup, err := service.PrepareArchive(context.Background(), mustID(t, value.ID))
	if err == nil {
		cleanup()
		file.Close()
		t.Fatal("corrupt archive was prepared")
	}
}

func TestCommandAuditUsesServiceClock(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	fixed := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	service.now = func() time.Time { return fixed }
	if _, err := service.QueueManual(context.Background(), 1, "clocked-command"); err != nil {
		t.Fatal(err)
	}
	var created string
	if err := db.QueryRow(`SELECT created_at FROM audit_events WHERE action='backup.trigger'`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created != timestamp(fixed) {
		t.Fatalf("audit created_at=%s, want %s", created, timestamp(fixed))
	}
}

func TestReconcileRemovesPublishedDirectoryForInterruptedRun(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	queued, err := service.QueueManual(context.Background(), 1, "interrupted-final")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(service.config.BackupDirectory, queued.ID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan root still present: %v", err)
	}
}

func TestDownloadCompletionRecordsTransportFailureWithoutFalseSuccess(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	value, err := service.RunOffline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id := mustID(t, value.ID)
	if err := service.RecordDownloadStart(context.Background(), 1, id); err != nil {
		t.Fatal(err)
	}
	if err := service.RecordDownloadCompletion(context.Background(), 1, id, errors.New("client disconnected")); err != nil {
		t.Fatal(err)
	}
	var completed, failed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE domain_ref_id=? AND action='backup.download_completed'`, id).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE domain_ref_id=? AND action='backup.download_failed' AND outcome='rejected'`, id).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if completed != 0 || failed != 1 {
		t.Fatalf("completed=%d failed=%d", completed, failed)
	}
}
