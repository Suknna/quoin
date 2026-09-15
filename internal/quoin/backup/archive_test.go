package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// TestCommandAuditUsesServiceClock keeps the deterministic-clock contract:
// with the injectable runner/writer clocks the automatic audit's created_at
// is the service clock, not a second wall-clock source.
func TestCommandAuditUsesServiceClock(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	fixed := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	service.now = func() time.Time { return fixed }
	if _, err := service.QueueManual(newCommandContext(t, 1), 1, "clocked-command"); err != nil {
		t.Fatal(err)
	}
	var created string
	if err := db.QueryRow(`SELECT created_at FROM audit_events WHERE action='backup.trigger'`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	// The audit module persists created_at in its own fixed-width canonical
	// format, on the injected service clock.
	if want := fixed.UTC().Format("2006-01-02T15:04:05.000000000Z"); created != want {
		t.Fatalf("audit created_at=%s, want %s", created, want)
	}
}

// TestRunLifecycleTransitionsAreAudited pins the bounded lifecycle contract:
// every durable backups-row transition of one Run is an automatic audit row
// executed by the system task principal on the run's correlation, and no
// failed fact exists for a succeeded run.
func TestRunLifecycleTransitionsAreAudited(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	value, err := service.RunOffline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if value.Status != "succeeded" {
		t.Fatalf("status=%s", value.Status)
	}
	id := mustID(t, value.ID)
	var starts, stages, succeeds, failures int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='backup.run.start' AND domain_ref_id=?`, id).Scan(&starts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='backup.run.stage' AND domain_ref_id=?`, id).Scan(&stages); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='backup.run.succeed' AND domain_ref_id=?`, id).Scan(&succeeds); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='backup.run.fail' AND domain_ref_id=?`, id).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if starts != 1 || stages < 1 || succeeds != 1 || failures != 0 {
		t.Fatalf("lifecycle audits start=%d stage=%d succeed=%d fail=%d, want one start, staged progress, one success and no failure", starts, stages, succeeds, failures)
	}
	var identities int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT actor_type || '/' || correlation_id) FROM audit_events WHERE action LIKE 'backup.run.%' AND domain_ref_id=?`, id).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if identities != 1 {
		t.Fatalf("lifecycle audit identities=%d, want one system actor on one run correlation", identities)
	}
	var outsideContract int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action LIKE 'backup.run.%' AND (actor_type<>'system' OR phase<>'execute')`).Scan(&outsideContract); err != nil {
		t.Fatal(err)
	}
	if outsideContract != 0 {
		t.Fatalf("lifecycle audit rows outside the system/execute contract=%d", outsideContract)
	}
}

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

// TestCommandExecutionWritesAutomaticAudit pins the automatic-audit contract
// of the shared runner: the business command never calls audit itself, yet the
// event carries the execute phase, the acting principal and the caller's
// correlation metadata.
func TestCommandExecutionWritesAutomaticAudit(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	if _, err := service.QueueManual(newCommandContext(t, 1), 1, "clocked-command"); err != nil {
		t.Fatal(err)
	}
	var phase, correlation, actorType string
	var actorID int64
	if err := db.QueryRow(`SELECT phase,correlation_id,actor_type,actor_id FROM audit_events WHERE action='backup.trigger'`).Scan(&phase, &correlation, &actorType, &actorID); err != nil {
		t.Fatal(err)
	}
	if phase != "execute" || correlation == "" || actorType != "user" || actorID != 1 {
		t.Fatalf("audit phase=%q correlation=%q actor=%s/%d, want execute phase with user actor and correlation", phase, correlation, actorType, actorID)
	}
}

func TestReconcileRemovesPublishedDirectoryForInterruptedRun(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	queued, err := service.QueueManual(newCommandContext(t, 1), 1, "interrupted-final")
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
	// Start and completion are two facts of one transfer: both carry the same
	// execution metadata the HTTP entry will provide.
	transferCtx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "download-transfer-correlation",
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RecordDownloadStart(transferCtx, 1, id); err != nil {
		t.Fatal(err)
	}
	if err := service.RecordDownloadCompletion(transferCtx, 1, id, errors.New("client disconnected")); err != nil {
		t.Fatal(err)
	}
	var completed, failed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE domain_ref_id=? AND action='backup.download_completed'`, id).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE domain_ref_id=? AND action='backup.download_failed' AND outcome='failure' AND phase='execute'`, id).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if completed != 0 || failed != 1 {
		t.Fatalf("completed=%d failed=%d", completed, failed)
	}
}
