// Package backup owns the durable backup state machine and immutable backup
// set publishing. SQLite holds lifecycle authority; files are published only
// after a durable snapshot and manifest checksum are complete.
package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

var (
	ErrActive             = errors.New("a backup is already queued or running")
	ErrNotFound           = errors.New("backup not found")
	ErrCommandReused      = errors.New("client command id was reused with a different request")
	ErrActorUnauthorized  = errors.New("command actor is not an enabled administrator")
	ErrRowVersionConflict = errors.New("row version conflict")
	ErrNoSettingsChange   = errors.New("backup settings request makes no change")
	ErrInvalidCommandID   = errors.New("client command ID must be 8-128 ASCII letters, digits, underscores, or hyphens")
	ErrInvalidSettings    = errors.New("invalid backup settings")
	ErrMaintenanceActive  = errors.New("backup commands are unavailable during maintenance")
)

// ActiveError identifies the already-durable active run without exposing any
// filesystem implementation detail to HTTP callers.
type ActiveError struct{ ID string }

func (err *ActiveError) Error() string { return ErrActive.Error() }
func (err *ActiveError) Unwrap() error { return ErrActive }

type Config struct {
	DataDirectory, BackupDirectory, ArtifactDirectory string
	ArtifactStore                                     *artifact.Store
	// Now is injected at the process boundary for deterministic scheduling and
	// lifecycle timestamps. Nil selects the production UTC wall clock.
	Now func() time.Time
	// ScheduleAdmission is the process-owned normal-mode/storage readiness
	// fence. Manual and offline paths have distinct contracts.
	ScheduleAdmission func() bool
	// AuthorizeActor is invoked inside the runner-owned transaction after the
	// mandatory session re-verification for additional process policy (the
	// maintenance admission fence). It receives the guarded execution
	// transaction surface rather than a raw connection so it can never outlive
	// or commit the command it authorizes.
	AuthorizeActor func(context.Context, execution.Executor, int64) error
}
type Service struct {
	// db is the write authority: the runner's transactions and the explicitly
	// necessary low-level SQLite snapshot mechanisms use it; no public query
	// path touches it.
	db *sql.DB
	// reader is the read-only query surface (SQLite mode=ro) for public
	// queries, download metadata reads and lifecycle entry decisions. SQLite
	// itself rejects any write attempted through it. Ownership of the pool
	// follows selfReader: the service closes only the pool it opened itself
	// and never a reader attached via SetReader or the shared write pool.
	// readerMu guards the swap so shutdown (Close) cannot race startup
	// wiring (SetReader); the long-running Run/GC paths keep their own mu.
	reader            execution.Reader
	selfReader        bool
	readerMu          sync.Mutex
	config            Config
	mu                sync.Mutex
	now               func() time.Time
	metrics           *sharedops.BackupMetrics
	artifactStore     *artifact.Store
	capacity          capacityFunc
	probeDirectory    func(string) error
	removeAll         func(string) error
	afterPublish      func()
	scheduleAdmission func() bool
	authorizeActor    func(context.Context, execution.Executor, int64) error
	commands          *commandRunner
}
type Summary struct {
	ID, Status, Stage, TriggerKind, ExecutionMode string
	ScheduledFor, StartedAt, CompletedAt          *string
	RowVersion                                    int64
	CreatedAt, UpdatedAt                          string
	DBSHA256, ManifestSHA256                      *string
	ArtifactCount                                 *int
	// SizeBytes is the persisted sum of manifest.json, quoin.db, and every
	// manifest-listed artifact payload; it excludes tar framing and is zero
	// until a backup publishes successfully.
	SizeBytes   int64
	ErrorCode   *string
	Retryable   *bool
	ErrorDetail *string
}
type Settings struct {
	Enabled                    bool
	ScheduleCron               *string
	Timezone                   string
	BackupTarget               string
	RetentionCount, RowVersion int64
}
type (
	ArtifactRetention struct{ GeneratedRetentionDays, RowVersion int64 }
	RetentionHealth   struct {
		LastAttemptAt *string
		LastFailureAt *string
		ErrorDetail   *string
	}
)

func NewService(db *sql.DB, config Config) (*Service, error) {
	if db == nil || config.DataDirectory == "" || config.BackupDirectory == "" || config.ArtifactDirectory == "" {
		return nil, errors.New("backup requires database, data, backup, and artifact directories")
	}
	if filepath.Clean(config.DataDirectory) == filepath.Clean(config.BackupDirectory) {
		return nil, errors.New("backup directory must differ from data directory")
	}
	if err := os.MkdirAll(config.BackupDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create backup directory: %w", err)
	}
	store := config.ArtifactStore
	if store == nil {
		var err error
		store, err = artifact.NewStore(db, config.ArtifactDirectory)
		if err != nil {
			return nil, fmt.Errorf("open artifact store: %w", err)
		}
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	admission := config.ScheduleAdmission
	if admission == nil {
		admission = func() bool { return true }
	}
	// The read split is structural, not a wrapper: public queries and download
	// metadata reads run on a real mode=ro pool over the same database file,
	// so SQLite itself rejects any write a query path could attempt. The
	// snapshot mechanisms (VACUUM INTO / artifact snapshot) keep the write
	// pool explicitly.
	reader, err := execution.OpenReadOnly(filepath.Join(config.DataDirectory, "quoin.db"))
	if err != nil {
		return nil, fmt.Errorf("open backup read-only reader: %w", err)
	}
	if config.ArtifactStore == nil {
		if err := store.SetReader(reader); err != nil {
			return nil, errors.Join(err, reader.Close())
		}
	}
	clock := func() time.Time { return now().UTC() }
	service := &Service{
		db: db, reader: reader, selfReader: true,
		config: config, artifactStore: store,
		now:      clock,
		capacity: filesystemCapacity, probeDirectory: durableDirectoryProbe,
		removeAll: os.RemoveAll, scheduleAdmission: admission,
		authorizeActor: config.AuthorizeActor,
	}
	// The runner and audit writers read the service clock indirectly so a
	// test or process that rebinds service.now after construction keeps the
	// ledger and audit timestamps on the same clock.
	commands, err := newCommandRunner(db, config.AuthorizeActor, func() time.Time { return service.now() })
	if err != nil {
		// The service never escapes on this path, so its self-opened reader
		// pool must not leak past the constructor.
		return nil, errors.Join(fmt.Errorf("register backup operations: %w", err), reader.Close())
	}
	service.commands = commands
	return service, nil
}

// SetReader attaches the process's shared read-only query pool, replacing the
// mode=ro pool the service opened for itself at construction (which is then
// closed). Wiring is a startup-time operation and must happen before the
// service serves traffic; without it the self-opened reader already provides
// the full read-only split, so standalone processes (the offline CLI) need no
// wiring. The write pool stays private to the family runner and the explicit
// snapshot mechanisms either way. A failed close of the replaced pool leaves
// the previous wiring intact and is reported instead of silently leaking it.
func (s *Service) SetReader(reader execution.Reader) error {
	if err := reader.PingContext(context.Background()); err != nil {
		return err
	}
	s.readerMu.Lock()
	defer s.readerMu.Unlock()
	if s.reader == reader {
		return nil
	}
	if s.config.ArtifactStore == nil {
		if err := s.artifactStore.SetReader(reader); err != nil {
			return err
		}
	}
	if s.selfReader {
		if err := s.reader.Close(); err != nil {
			return fmt.Errorf("backup: close replaced read-only reader: %w", err)
		}
		s.selfReader = false
	}
	s.reader = reader
	return nil
}

// Close releases exactly the reader pool the service owns: the mode=ro pool
// opened at construction. A reader attached through SetReader and the write
// pool stay with their owners and are never closed here. Close is idempotent;
// callers that own the database must Close the service first so the last
// SQLite connection can checkpoint and drop the WAL sidecar on shutdown.
func (s *Service) Close() error {
	s.readerMu.Lock()
	defer s.readerMu.Unlock()
	if !s.selfReader {
		return nil
	}
	reader := s.reader
	s.reader, s.selfReader = execution.Reader{}, false
	if err := reader.Close(); err != nil {
		return fmt.Errorf("backup: close read-only reader: %w", err)
	}
	return nil
}

func (s *Service) SetMetrics(metrics *sharedops.BackupMetrics) {
	s.metrics = metrics
	// Startup projects actual durable probe results rather than trusting the
	// catalog's initial value. Capacity is operation-specific and is checked at
	// Run time against the exact backup set.
	_ = s.checkTarget("data", s.config.DataDirectory, 0)
	_ = s.checkTarget("backup", s.config.BackupDirectory, 0)
	s.refreshMetrics(context.Background())
}
func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func pointer(value string) *string { return &value }
func ValidCommandID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if !((character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Service) Settings(ctx context.Context) (Settings, error) {
	var value Settings
	var enabled int
	var cron sql.NullString
	err := s.reader.QueryRowContext(ctx, `SELECT enabled,schedule_cron,timezone,retention_count,row_version FROM backup_settings WHERE id=1`).Scan(&enabled, &cron, &value.Timezone, &value.RetentionCount, &value.RowVersion)
	value.Enabled = enabled == 1
	value.BackupTarget = s.config.BackupDirectory
	if cron.Valid {
		value.ScheduleCron = pointer(cron.String)
	}
	return value, err
}

func (s *Service) RetentionHealth(ctx context.Context) (RetentionHealth, error) {
	var value RetentionHealth
	var attempt, failure, detail sql.NullString
	err := s.reader.QueryRowContext(ctx, `SELECT last_attempt_at,last_failure_at,error_detail FROM backup_retention_health WHERE id=1`).Scan(&attempt, &failure, &detail)
	if attempt.Valid {
		value.LastAttemptAt = pointer(attempt.String)
	}
	if failure.Valid {
		value.LastFailureAt = pointer(failure.String)
	}
	if detail.Valid {
		value.ErrorDetail = pointer(detail.String)
	}
	return value, err
}

func (s *Service) ArtifactRetention(ctx context.Context) (ArtifactRetention, error) {
	var value ArtifactRetention
	err := s.reader.QueryRowContext(ctx, `SELECT generated_retention_days,row_version FROM artifact_retention_settings WHERE id=1`).Scan(&value.GeneratedRetentionDays, &value.RowVersion)
	return value, err
}

// QueueManual atomically creates or durably replays an authenticated command.
// The execution context (correlation metadata) must be attached to ctx by the
// public entry: the shared runner fails closed without it.
func (s *Service) QueueManual(ctx context.Context, actor int64, clientCommandID string) (Summary, error) {
	value, err := s.queueManualCommand(ctx, actor, clientCommandID)
	if err == nil {
		s.refreshMetrics(ctx)
	}
	return value, err
}

// QueueScheduled queues one explicit scheduled boundary. It is a scheduler
// entry point: it establishes the system/scheduler execution scope itself,
// and the insert is audited through the shared runner without a client
// command key.
func (s *Service) QueueScheduled(ctx context.Context, due time.Time) (Summary, error) {
	commandCtx, err := s.executionContext(ctx, execution.SourceScheduler)
	if err != nil {
		return Summary{}, err
	}
	value := timestamp(due)
	queued, err := s.queueRun(commandCtx, s.commands.schedule, "scheduled", "online", &value)
	if err == nil {
		s.refreshMetrics(ctx)
	}
	return queued, err
}

// RunOffline queues and executes the offline backup. The CLI is a trusted
// entry point (ADR-0006), so it establishes the system/CLI execution scope
// for the whole queue-plus-run operation.
func (s *Service) RunOffline(ctx context.Context) (Summary, error) {
	commandCtx, err := s.executionContext(ctx, execution.SourceCLI)
	if err != nil {
		return Summary{}, err
	}
	queued, err := s.queueRun(commandCtx, s.commands.offline, "manual", "offline", nil)
	if err != nil {
		return Summary{}, err
	}
	return s.Run(commandCtx, queued.ID)
}

func (s *Service) Get(ctx context.Context, id int64) (Summary, error) {
	return scanSummary(ctx, s.reader, id)
}

func (s *Service) List(ctx context.Context, limit int) ([]Summary, error) {
	items, _, err := s.ListPage(ctx, 0, limit)
	return items, err
}

// ListPageWithLatest reads the page and latest successful Run in one SQLite
// read transaction so the admin summary cannot contradict its list snapshot.
func (s *Service) ListPageWithLatest(ctx context.Context, beforeID int64, limit int) ([]Summary, *int64, *Summary, error) {
	transaction, err := s.reader.BeginSnapshot(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	defer transaction.Rollback()
	items, next, err := s.listPageOn(ctx, transaction, beforeID, limit)
	if err != nil {
		return nil, nil, nil, err
	}
	var latestID int64
	err = transaction.QueryRowContext(ctx, `SELECT id FROM backups WHERE status='succeeded' ORDER BY completed_at DESC, id DESC LIMIT 1`).Scan(&latestID)
	var latest *Summary
	if err == nil {
		value, readErr := scanSummary(ctx, transaction, latestID)
		if readErr != nil {
			return nil, nil, nil, readErr
		}
		latest = &value
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil, err
	}
	if err = transaction.Commit(); err != nil {
		return nil, nil, nil, err
	}
	return items, next, latest, nil
}

// ListPage implements keyset pagination in immutable Backup Run ID order.
func (s *Service) ListPage(ctx context.Context, beforeID int64, limit int) ([]Summary, *int64, error) {
	return s.listPageOn(ctx, s.reader, beforeID, limit)
}

type backupPageReader interface {
	summaryReader
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Service) listPageOn(ctx context.Context, reader backupPageReader, beforeID int64, limit int) ([]Summary, *int64, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		return nil, nil, fmt.Errorf("backup page limit exceeds 200")
	}
	query := `SELECT id FROM backups WHERE (? = 0 OR id < ?) ORDER BY id DESC LIMIT ?`
	rows, err := reader.QueryContext(ctx, query, beforeID, beforeID, limit+1)
	if err != nil {
		return nil, nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	var next *int64
	if len(ids) > limit {
		cursor := ids[limit-1]
		next = &cursor
		ids = ids[:limit]
	}
	// The production DB deliberately permits one connection: release this cursor
	// before querying each summary or List can deadlock against itself.
	out := make([]Summary, 0, len(ids))
	for _, id := range ids {
		value, err := scanSummary(ctx, reader, id)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, value)
	}
	return out, next, nil
}

func isActiveConstraint(err error) bool {
	return strings.Contains(err.Error(), "ux_backups_active") || strings.Contains(err.Error(), "UNIQUE constraint failed")
}
