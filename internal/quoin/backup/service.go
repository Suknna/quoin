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
)

var ErrActive = errors.New("a backup is already queued or running")
var ErrNotFound = errors.New("backup not found")
var ErrCommandReused = errors.New("client command id was reused with a different request")
var ErrActorUnauthorized = errors.New("command actor is not an enabled administrator")
var ErrRowVersionConflict = errors.New("row version conflict")
var ErrNoSettingsChange = errors.New("backup settings request makes no change")
var ErrInvalidCommandID = errors.New("client command ID must be 8-128 ASCII letters, digits, underscores, or hyphens")
var ErrInvalidSettings = errors.New("invalid backup settings")

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
	// AuthorizeActor is invoked after BEGIN IMMEDIATE for every interactive
	// command, closing the role/disable race between HTTP authentication and
	// the durable state change.
	AuthorizeActor func(context.Context, *sql.Conn, int64) error
}
type Service struct {
	db                *sql.DB
	config            Config
	mu                sync.Mutex
	now               func() time.Time
	metrics           *sharedops.BackupMetrics
	artifactStore     *artifact.Store
	capacity          capacityFunc
	probeDirectory    func(string) error
	scheduleAdmission func() bool
	authorizeActor    func(context.Context, *sql.Conn, int64) error
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
type ArtifactRetention struct{ GeneratedRetentionDays, RowVersion int64 }

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
	return &Service{
		db: db, config: config, artifactStore: store,
		now:      func() time.Time { return now().UTC() },
		capacity: filesystemCapacity, probeDirectory: durableDirectoryProbe,
		scheduleAdmission: admission,
		authorizeActor:    config.AuthorizeActor,
	}, nil
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
	err := s.db.QueryRowContext(ctx, `SELECT enabled,schedule_cron,timezone,retention_count,row_version FROM backup_settings WHERE id=1`).Scan(&enabled, &cron, &value.Timezone, &value.RetentionCount, &value.RowVersion)
	value.Enabled = enabled == 1
	value.BackupTarget = s.config.BackupDirectory
	if cron.Valid {
		value.ScheduleCron = pointer(cron.String)
	}
	return value, err
}
func (s *Service) ArtifactRetention(ctx context.Context) (ArtifactRetention, error) {
	var value ArtifactRetention
	err := s.db.QueryRowContext(ctx, `SELECT generated_retention_days,row_version FROM artifact_retention_settings WHERE id=1`).Scan(&value.GeneratedRetentionDays, &value.RowVersion)
	return value, err
}

// QueueManual atomically creates or durably replays an authenticated command.
func (s *Service) QueueManual(ctx context.Context, actor int64, clientCommandID string) (Summary, error) {
	value, err := s.queueManualCommand(ctx, actor, clientCommandID)
	if err == nil {
		s.refreshMetrics(ctx)
	}
	return value, err
}
func (s *Service) QueueScheduled(ctx context.Context, due time.Time) (Summary, error) {
	value := timestamp(due)
	queued, err := s.queue(ctx, "scheduled", "online", &value, 0)
	if err == nil {
		s.refreshMetrics(ctx)
	}
	return queued, err
}
func (s *Service) RunOffline(ctx context.Context) (Summary, error) {
	value, err := s.queue(ctx, "manual", "offline", nil, 0)
	if err != nil {
		return Summary{}, err
	}
	return s.Run(ctx, value.ID)
}
func (s *Service) queue(ctx context.Context, kind, mode string, scheduled *string, actor int64) (Summary, error) {
	triggered := any(nil)
	if actor > 0 {
		triggered = actor
	}
	now := timestamp(s.now())
	result, err := s.db.ExecContext(ctx, `INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,row_version,created_at,updated_at,triggered_by) VALUES('queued','queued',?,?,?,1,?,?,?)`, kind, mode, scheduled, now, now, triggered)
	if err != nil {
		if isActiveConstraint(err) {
			return Summary{}, ErrActive
		}
		return Summary{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Summary{}, err
	}
	return s.Get(ctx, id)
}
func (s *Service) Get(ctx context.Context, id int64) (Summary, error) {
	return scanSummary(ctx, s.db, id)
}
func (s *Service) List(ctx context.Context, limit int) ([]Summary, error) {
	items, _, err := s.ListPage(ctx, 0, limit)
	return items, err
}

// ListPage implements keyset pagination in immutable Backup Run ID order.
func (s *Service) ListPage(ctx context.Context, beforeID int64, limit int) ([]Summary, *int64, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		return nil, nil, fmt.Errorf("backup page limit exceeds 200")
	}
	query := `SELECT id FROM backups WHERE (? = 0 OR id < ?) ORDER BY id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, beforeID, beforeID, limit+1)
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
		value, err := s.Get(ctx, id)
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
