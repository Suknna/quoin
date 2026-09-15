package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

const (
	commandTrigger   = "backup.trigger"
	commandSettings  = "backup.settings.update"
	commandRetention = "artifact_retention.update"
	commandSchedule  = "backup.schedule.queue"
	commandOffline   = "backup.queue.offline"
	// Backup Run lifecycle facts: each durable backups-row transition is one
	// audited system operation, so a Run cannot change state without its
	// automatic audit row. Bounded by the state machine itself.
	commandRunStart        = "backup.run.start"
	commandRunStage        = "backup.run.stage"
	commandRunSucceed      = "backup.run.succeed"
	commandRunFail         = "backup.run.fail"
	commandRunInterrupted  = "backup.run.interrupted"
	commandRetentionHealth = "backup.retention.health"

	// Download access facts (sensitive reads): the transfer authorization
	// fact before bytes move and the terminal completion/failure fact.
	commandDownloadStart      = "backup.download_started"
	commandDownloadCompleted  = "backup.download_completed"
	commandDownloadFailed     = "backup.download_failed"
)

// Plain skip sentinels for lifecycle steps whose target state was already
// taken by someone else. They are deliberately not deterministic rejections:
// the losing racer changed nothing, so nothing is recorded — the winner's
// transition is the single audited fact.
var (
	errRunNotQueued          = errors.New("backup run is no longer queued")
	errRunNotRunning         = errors.New("backup run is no longer running")
	errRetentionHealthStable = errors.New("backup retention health state is unchanged")
)

// errScheduleSuperseded marks a scheduler pass whose admission fence was
// superseded between the read-only decision and the writer transaction. It is
// deliberately a plain error, not a deterministic rejection: the pass changed
// nothing and must leave no durable trace, so an overlapping schedule boundary
// cannot flood the audit log while a backup is already active.
var errScheduleSuperseded = errors.New("backup schedule admission was superseded before the writer transaction")

// commandRunner owns this family's shared execution runner (ADR-0006) and the
// backup operation declarations. Every active backup mutation is declared
// here — there is no bypass list: an undeclared operation cannot execute, and
// each write operation carries its authorization callback, re-verified inside
// the runner-owned transaction before replay lookup and business execution.
type commandRunner struct {
	runner          *execution.Runner
	audit           *audit.Writer
	trigger         *execution.Operation
	settings        *execution.Operation
	retention       *execution.Operation
	schedule        *execution.Operation
	offline         *execution.Operation
	runStart        *execution.Operation
	runStage        *execution.Operation
	runSucceed      *execution.Operation
	runFail         *execution.Operation
	runInterrupted  *execution.Operation
	retentionHealth *execution.Operation
	// Download access facts.
	downloadStart     *execution.Operation
	downloadCompleted *execution.Operation
	downloadFailed    *execution.Operation
}

// newCommandRunner registers the backup operations and builds the runner over
// db. authorize is the process-boundary policy hook (maintenance admission,
// wired with the session recheck in production); a nil hook only skips that
// extra policy — the mandatory session and role recheck stays in
// authorizeInteractive either way. now is the service's deterministic clock,
// shared with the ledger and audit writers.
func newCommandRunner(db *sql.DB, authorize func(context.Context, execution.Executor, int64) error, now func() time.Time) (*commandRunner, error) {
	registry := execution.NewRegistry()
	commands := &commandRunner{audit: audit.NewWriterWithClock(now)}
	var err error
	if commands.trigger, err = registry.Register(execution.Operation{
		Name: commandTrigger, Class: execution.ClassWrite, ObjectType: "backup",
		Authorize: authorizeInteractive(authorize),
	}); err != nil {
		return nil, err
	}
	if commands.settings, err = registry.Register(execution.Operation{
		Name: commandSettings, Class: execution.ClassWrite, ObjectType: "backup_settings",
		Authorize: authorizeInteractive(authorize),
	}); err != nil {
		return nil, err
	}
	if commands.retention, err = registry.Register(execution.Operation{
		Name: commandRetention, Class: execution.ClassWrite, ObjectType: "artifact_retention_settings",
		Authorize: authorizeInteractive(authorize),
	}); err != nil {
		return nil, err
	}
	if commands.schedule, err = registry.Register(execution.Operation{
		Name: commandSchedule, Class: execution.ClassWrite, ObjectType: "backup",
		Authorize: requireSystemSource(execution.SourceScheduler),
	}); err != nil {
		return nil, err
	}
	if commands.offline, err = registry.Register(execution.Operation{
		Name: commandOffline, Class: execution.ClassWrite, ObjectType: "backup",
		Authorize: requireSystemSource(execution.SourceCLI),
	}); err != nil {
		return nil, err
	}
	// The Run lifecycle is system work driven by the task executor; it must
	// never arrive through a client channel, and no session callback applies.
	lifecycle := requireSystemLifecycle
	for _, operation := range []struct {
		operation **execution.Operation
		name      string
		object    string
	}{
		{&commands.runStart, commandRunStart, "backup"},
		{&commands.runStage, commandRunStage, "backup"},
		{&commands.runSucceed, commandRunSucceed, "backup"},
		{&commands.runFail, commandRunFail, "backup"},
		{&commands.runInterrupted, commandRunInterrupted, "backup"},
		{&commands.retentionHealth, commandRetentionHealth, "backup_retention_health"},
	} {
		if *operation.operation, err = registry.Register(execution.Operation{
			Name: operation.name, Class: execution.ClassWrite, ObjectType: operation.object,
			Authorize: lifecycle,
		}); err != nil {
			return nil, err
		}
	}
	// Download access facts record a user's sensitive transfer; the session
	// recheck inside the transaction keeps a since-revoked session from
	// minting the fact, exactly like the interactive commands.
	for _, operation := range []struct {
		operation **execution.Operation
		name      string
	}{
		{&commands.downloadStart, commandDownloadStart},
		{&commands.downloadCompleted, commandDownloadCompleted},
		{&commands.downloadFailed, commandDownloadFailed},
	} {
		if *operation.operation, err = registry.Register(execution.Operation{
			Name: operation.name, Class: execution.ClassWrite, ObjectType: "backup",
			Authorize: authorizeInteractive(nil),
		}); err != nil {
			return nil, err
		}
	}
	commands.runner = execution.NewRunnerWithClock(db, registry, commands.audit, now)
	return commands, nil
}

// backupCommandRole is the only role allowed to execute interactive backup
// commands.
const backupCommandRole = "admin"

// authorizeInteractive adapts the process-boundary policy hook to the
// operation contract. The acting principal and its session proof come from
// the execution context, never from client-controlled arguments, and the
// session recheck is mandatory: inside the runner transaction the referenced
// session must still exist, be enabled, initialized, unrevoked and unexpired,
// sit at the current auth revision, and hold the admin role. A nil hook skips
// only the additional process policy (maintenance admission) wired at the
// process boundary — never the session recheck.
func authorizeInteractive(authorize func(context.Context, execution.Executor, int64) error) func(context.Context, *execution.Tx) error {
	return func(ctx context.Context, tx *execution.Tx) error {
		meta, err := execution.Require(ctx)
		if err != nil {
			return err
		}
		if err := auth.VerifyExecutionSession(ctx, tx, backupCommandRole); err != nil {
			return err
		}
		if authorize != nil {
			return authorize(ctx, tx, meta.Actor.ID)
		}
		return nil
	}
}

// requireSystemSource confines a system-initiated operation to the system
// principal arriving through one explicit trusted source. The bypass of the
// user session callback is valid only for those declared system operations —
// there is no fallback that lets the system principal pose as an admin
// session, and a user context can never pass.
func requireSystemSource(source execution.SourceKind) func(context.Context, *execution.Tx) error {
	return func(ctx context.Context, _ *execution.Tx) error {
		meta, err := execution.Require(ctx)
		if err != nil {
			return err
		}
		if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
			return fmt.Errorf("backup: operation requires the system principal, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
		}
		if meta.Source.Kind != source {
			return fmt.Errorf("backup: system operation requires %q source, got %q", source, meta.Source.Kind)
		}
		return nil
	}
}

// requireSystemLifecycle confines Run-lifecycle mutations (start, stage
// advance, terminal result, restart recovery, retention health) to the system
// task executor arriving through any trusted background source. A user or
// service context — in particular anything from a client channel — can never
// drive the lifecycle, and there is no session fallback to fake.
func requireSystemLifecycle(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return fmt.Errorf("backup: lifecycle operation requires the system principal, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
	}
	if meta.Source.Kind == execution.SourceHTTP {
		return fmt.Errorf("backup: lifecycle operation cannot arrive from the %s channel", meta.Source.Kind)
	}
	return nil
}

// runIdentity is the persisted operation correlation of one queued Backup Run:
// the correlation the queueing operation recorded and the original initiator
// (the existing triggered_by column holds the initiator id; NULL maps to the
// system principal id 0). A NULL correlation_id is a historical
// no-correlation fact and is never backfilled with a guess.
type runIdentity struct {
	CorrelationID string
	InitiatorType string
	InitiatorID   int64
}

// restorable reports whether the row carries a complete, valid persisted
// correlation that a restart may reattach.
func (identity runIdentity) restorable() bool {
	if identity.CorrelationID == "" {
		return false
	}
	switch identity.InitiatorType {
	case string(execution.PrincipalSystem):
		return identity.InitiatorID == 0
	case string(execution.PrincipalUser), string(execution.PrincipalService):
		return identity.InitiatorID > 0
	default:
		return false
	}
}

// runIdentityOn reads one run's persisted correlation through the given
// reader — the guarded transaction or the read-only pool for the
// pre-transaction entry decision in Run. The caller's context scopes the
// read; no detached context is substituted.
func (s *Service) runIdentityOn(ctx context.Context, reader summaryReader, id int64) (runIdentity, error) {
	var identity runIdentity
	var correlation, initiatorType sql.NullString
	var triggeredBy sql.NullInt64
	err := reader.QueryRowContext(ctx, `SELECT correlation_id,initiator_type,triggered_by FROM backups WHERE id=?`, id).Scan(&correlation, &initiatorType, &triggeredBy)
	if err != nil {
		return runIdentity{}, err
	}
	if correlation.Valid {
		identity.CorrelationID = correlation.String
	}
	if initiatorType.Valid {
		identity.InitiatorType = initiatorType.String
	}
	if triggeredBy.Valid {
		identity.InitiatorID = triggeredBy.Int64
	}
	return identity, nil
}

// restoreTaskContext re-roots a fresh scope onto a queued run's persisted
// correlation with the system task executor acting on the original
// initiator's behalf. This is the deliberate ADR-0006 re-rooting for restart
// recovery; the finished caller scope's cancellation never travels with it.
func (s *Service) restoreTaskContext(base context.Context, identity runIdentity) (context.Context, error) {
	return execution.ReplaceMetadata(base, execution.Metadata{
		CorrelationID: identity.CorrelationID,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Initiator:     execution.Principal{Kind: execution.PrincipalKind(identity.InitiatorType), ID: identity.InitiatorID},
		Source:        execution.Source{Kind: execution.SourceTask},
	})
}

// executionContext returns ctx unchanged when it already carries execution
// metadata (the caller owns the correlation and this pass acts inside it).
// Otherwise it establishes a fresh system-principal scope: scheduler and CLI
// entries are the trusted origins named by ADR-0006, so only entry points may
// create the correlation here — arbitrary business children never synthesize
// metadata.
func (s *Service) executionContext(ctx context.Context, source execution.SourceKind) (context.Context, error) {
	if _, ok := execution.FromContext(ctx); ok {
		return ctx, nil
	}
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	return execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: source},
	})
}

// queueManualCommand executes the authenticated trigger as a durable,
// replayable command. The public entry requires execution metadata on ctx;
// the runner fails closed without it.
func (s *Service) queueManualCommand(ctx context.Context, actor int64, commandID string) (Summary, error) {
	if !ValidCommandID(commandID) {
		return Summary{}, ErrInvalidCommandID
	}
	digest := auth.DigestCommand(commandTrigger, map[string]any{"executionMode": "online"})
	outcome, err := execution.Run(ctx, s.commands.runner, s.commands.trigger, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     actor,
		ClientCommandID: commandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (Summary, execution.Change, error) {
		now := timestamp(s.now())
		// The queued row carries the command's correlation and original
		// initiator so a later restart can restore the operation context.
		meta, metaErr := execution.Require(ctx)
		if metaErr != nil {
			return Summary{}, execution.Changed, metaErr
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,row_version,created_at,updated_at,triggered_by,correlation_id,initiator_type) VALUES('queued','queued','manual','online',NULL,1,?,?,?,?,?)`, now, now, actor, meta.CorrelationID, string(meta.Initiator.Kind))
		if err != nil {
			if isActiveConstraint(err) {
				// The already-durable active run is the rejection's object, so the
				// deterministic record carries the same reference on replay.
				rejection := &execution.Rejection{Code: "active_conflict", Detail: ErrActive.Error()}
				var activeID int64
				if lookupErr := tx.QueryRowContext(ctx, `SELECT id FROM backups WHERE status IN ('queued','running') ORDER BY id LIMIT 1`).Scan(&activeID); lookupErr == nil {
					rejection.ObjectID = activeID
				}
				return Summary{}, execution.Changed, rejection
			}
			return Summary{}, execution.Changed, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return Summary{}, execution.Changed, err
		}
		value, err := scanSummary(ctx, tx, id)
		return value, execution.Changed, err
	}, func(value Summary) int64 { return mustInt(value.ID) })
	if err != nil {
		return Summary{}, commandError(err)
	}
	return outcome.Result, nil
}

// UpdateSettingsCommand applies the optimistic settings update through the
// shared runner: ledger, audit and the settings row commit atomically, and a
// no-op stays a replayable command classified as unchanged.
func (s *Service) UpdateSettingsCommand(ctx context.Context, actor, expected int64, commandID string, enabled *bool, cron *string, timezone *string, retention *int64) (Settings, error) {
	if !ValidCommandID(commandID) {
		return Settings{}, ErrInvalidCommandID
	}
	if enabled == nil && cron == nil && timezone == nil && retention == nil {
		return Settings{}, ErrInvalidSettings
	}
	digest := auth.DigestCommand(commandSettings, map[string]any{"expectedRowVersion": expected, "enabled": enabled, "scheduleCron": cron, "timezone": timezone, "retentionCount": retention})
	outcome, err := execution.Run(ctx, s.commands.runner, s.commands.settings, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     actor,
		ClientCommandID: commandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (Settings, execution.Change, error) {
		current, err := s.settingsOn(ctx, tx)
		if err != nil {
			return Settings{}, execution.Unchanged, err
		}
		if current.RowVersion != expected {
			return Settings{}, execution.Unchanged, rejection("row_version_conflict", ErrRowVersionConflict.Error(), 0)
		}
		if retention != nil && *retention < 1 {
			return Settings{}, execution.Unchanged, rejection("invalid_settings", fmt.Sprintf("%s: retention count must be positive", ErrInvalidSettings), 0)
		}
		original := current
		wasEnabled := current.Enabled
		if enabled != nil {
			current.Enabled = *enabled
		}
		if cron != nil {
			if *cron == "" {
				current.ScheduleCron = nil
			} else {
				current.ScheduleCron = cron
			}
		}
		if timezone != nil {
			current.Timezone = *timezone
		}
		if retention != nil {
			current.RetentionCount = *retention
		}
		if err := validateScheduleSettings(current); err != nil {
			return Settings{}, execution.Unchanged, rejection("invalid_settings", fmt.Sprintf("%s: %v", ErrInvalidSettings, err), 0)
		}
		// A semantically identical command is durably accepted and replayable,
		// but must not rewrite the settings row or schedule anchor.
		if settingsEqual(original, current) {
			return original, execution.Unchanged, nil
		}
		var cronValue any
		if current.ScheduleCron != nil {
			cronValue = *current.ScheduleCron
		}
		now := timestamp(s.now())
		if _, err = tx.ExecContext(ctx, `UPDATE backup_settings SET enabled=?,schedule_cron=?,timezone=?,retention_count=?,schedule_enabled_at=CASE WHEN ?=0 THEN NULL WHEN ?=1 AND ?=0 THEN ? ELSE schedule_enabled_at END,row_version=row_version+1,updated_by=?,updated_at=? WHERE id=1 AND row_version=?`, boolInt(current.Enabled), cronValue, current.Timezone, current.RetentionCount, boolInt(current.Enabled), boolInt(current.Enabled), boolInt(wasEnabled), now, actor, now, expected); err != nil {
			return Settings{}, execution.Unchanged, err
		}
		value, err := s.settingsOn(ctx, tx)
		return value, execution.Changed, err
	}, func(Settings) int64 { return 1 })
	if err != nil {
		return Settings{}, commandError(err)
	}
	value := outcome.Result
	value.BackupTarget = s.config.BackupDirectory
	return value, nil
}

func (s *Service) UpdateArtifactRetentionCommand(ctx context.Context, actor, expected, days int64, commandID string) (ArtifactRetention, error) {
	if !ValidCommandID(commandID) {
		return ArtifactRetention{}, ErrInvalidCommandID
	}
	digest := auth.DigestCommand(commandRetention, map[string]any{"expectedRowVersion": expected, "generatedRetentionDays": days})
	outcome, err := execution.Run(ctx, s.commands.runner, s.commands.retention, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     actor,
		ClientCommandID: commandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (ArtifactRetention, execution.Change, error) {
		if days < 1 {
			return ArtifactRetention{}, execution.Unchanged, rejection("invalid_settings", fmt.Sprintf("%s: retention days must be positive", ErrInvalidSettings), 0)
		}
		current, err := s.retentionOn(ctx, tx)
		if err != nil {
			return ArtifactRetention{}, execution.Unchanged, err
		}
		if current.RowVersion != expected {
			return ArtifactRetention{}, execution.Unchanged, rejection("row_version_conflict", ErrRowVersionConflict.Error(), 0)
		}
		// A no-op still gets a durable command result but does not create a
		// new settings revision; the runner classifies it as unchanged.
		if current.GeneratedRetentionDays == days {
			return current, execution.Unchanged, nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE artifact_retention_settings SET generated_retention_days=?,row_version=row_version+1,updated_by=?,updated_at=? WHERE id=1 AND row_version=?`, days, actor, timestamp(s.now()), expected)
		if err != nil {
			return ArtifactRetention{}, execution.Unchanged, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return ArtifactRetention{}, execution.Unchanged, err
		}
		if rows != 1 {
			return ArtifactRetention{}, execution.Unchanged, rejection("row_version_conflict", ErrRowVersionConflict.Error(), 0)
		}
		value, err := s.retentionOn(ctx, tx)
		return value, execution.Changed, err
	}, func(ArtifactRetention) int64 { return 1 })
	if err != nil {
		return ArtifactRetention{}, commandError(err)
	}
	return outcome.Result, nil
}

// queueRun inserts one queued Backup Run through the shared runner. These
// system-initiated mutations own their one-time identifiers (schedule
// boundary, offline run), so they use the audited non-ledger path instead of
// a synthetic client command key. The queued row persists this operation's
// correlation and the system initiator for later restart restore.
func (s *Service) queueRun(ctx context.Context, op *execution.Operation, kind, mode string, scheduled *string) (Summary, error) {
	now := timestamp(s.now())
	meta, metaErr := execution.Require(ctx)
	if metaErr != nil {
		return Summary{}, metaErr
	}
	outcome, err := execution.Execute(ctx, s.commands.runner, op, func(tx *execution.Tx) (Summary, error) {
		result, err := tx.ExecContext(ctx, `INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,row_version,created_at,updated_at,triggered_by,correlation_id,initiator_type) VALUES('queued','queued',?,?,?,1,?,?,NULL,?,?)`, kind, mode, scheduled, now, now, meta.CorrelationID, string(meta.Initiator.Kind))
		if err != nil {
			return Summary{}, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return Summary{}, err
		}
		return scanSummary(ctx, tx, id)
	}, func(value Summary) int64 { return mustInt(value.ID) })
	if err != nil {
		return Summary{}, err
	}
	return outcome, nil
}

// commandError translates the shared runner's outcomes back to this family's
// stable exported errors: recorded rejections replay as their original
// sentinel errors, and ledger key conflicts keep the historic identity.
func commandError(err error) error {
	var rejection *execution.Rejection
	if errors.As(err, &rejection) {
		return rejectionFromLedger(rejection.Code, rejection.Detail, rejection.ObjectID)
	}
	if errors.Is(err, execution.ErrCommandReused) {
		return ErrCommandReused
	}
	return err
}

func rejectionFromLedger(code, detail string, objectID int64) error {
	switch code {
	case "active_conflict":
		return &ActiveError{ID: fmt.Sprintf("%d", objectID)}
	case "row_version_conflict":
		return &replayedRejection{cause: ErrRowVersionConflict, detail: detail}
	case "no_change":
		return &replayedRejection{cause: ErrNoSettingsChange, detail: detail}
	case "invalid_client_command_id":
		return &replayedRejection{cause: ErrInvalidCommandID, detail: detail}
	case "invalid_settings":
		return &replayedRejection{cause: ErrInvalidSettings, detail: detail}
	default:
		return errors.New(code)
	}
}

func rejection(code, detail string, objectID int64) *execution.Rejection {
	return &execution.Rejection{Code: code, Detail: detail, ObjectID: objectID}
}

type replayedRejection struct {
	cause  error
	detail string
}

func (err *replayedRejection) Error() string { return err.detail }
func (err *replayedRejection) Unwrap() error { return err.cause }

// settingsOn reads the settings row through a read-only surface: it composes
// both on the guarded runner transaction during a command and on the
// read-only pool for public queries, and can never write.
func (s *Service) settingsOn(ctx context.Context, reader summaryReader) (Settings, error) {
	var value Settings
	var enabled int
	var cron sql.NullString
	err := reader.QueryRowContext(ctx, `SELECT enabled,schedule_cron,timezone,retention_count,row_version FROM backup_settings WHERE id=1`).Scan(&enabled, &cron, &value.Timezone, &value.RetentionCount, &value.RowVersion)
	value.Enabled = enabled == 1
	if cron.Valid {
		value.ScheduleCron = pointer(cron.String)
	}
	return value, err
}

// retentionOn reads the retention row through a read-only surface; see
// settingsOn.
func (s *Service) retentionOn(ctx context.Context, reader summaryReader) (ArtifactRetention, error) {
	var value ArtifactRetention
	err := reader.QueryRowContext(ctx, `SELECT generated_retention_days,row_version FROM artifact_retention_settings WHERE id=1`).Scan(&value.GeneratedRetentionDays, &value.RowVersion)
	return value, err
}
func settingsEqual(first, second Settings) bool {
	if first.Enabled != second.Enabled || first.Timezone != second.Timezone || first.RetentionCount != second.RetentionCount {
		return false
	}
	if first.ScheduleCron == nil || second.ScheduleCron == nil {
		return first.ScheduleCron == nil && second.ScheduleCron == nil
	}
	return *first.ScheduleCron == *second.ScheduleCron
}
func mustInt(value string) int64 { var id int64; _, _ = fmt.Sscan(value, &id); return id }

type summaryReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func scanSummary(ctx context.Context, reader summaryReader, id int64) (Summary, error) {
	var value Summary
	var scheduled, started, completed, dbsum, manifestSum, errorCode, errorDetail sql.NullString
	var artifacts sql.NullInt64
	var retry sql.NullInt64
	err := reader.QueryRowContext(ctx, `SELECT id,status,stage,trigger_kind,execution_mode,scheduled_for,row_version,created_at,updated_at,started_at,completed_at,db_sha256,manifest_sha256,artifact_count,size_bytes,error_code,retryable,error_detail FROM backups WHERE id=?`, id).Scan(&value.ID, &value.Status, &value.Stage, &value.TriggerKind, &value.ExecutionMode, &scheduled, &value.RowVersion, &value.CreatedAt, &value.UpdatedAt, &started, &completed, &dbsum, &manifestSum, &artifacts, &value.SizeBytes, &errorCode, &retry, &errorDetail)
	if errors.Is(err, sql.ErrNoRows) {
		return Summary{}, ErrNotFound
	}
	if err != nil {
		return Summary{}, err
	}
	if scheduled.Valid {
		value.ScheduledFor = pointer(scheduled.String)
	}
	if started.Valid {
		value.StartedAt = pointer(started.String)
	}
	if completed.Valid {
		value.CompletedAt = pointer(completed.String)
	}
	if dbsum.Valid {
		value.DBSHA256 = pointer(dbsum.String)
	}
	if manifestSum.Valid {
		value.ManifestSHA256 = pointer(manifestSum.String)
	}
	if artifacts.Valid {
		count := int(artifacts.Int64)
		value.ArtifactCount = &count
	}
	if errorCode.Valid {
		value.ErrorCode = pointer(errorCode.String)
	}
	if retry.Valid {
		retryable := retry.Int64 == 1
		value.Retryable = &retryable
	}
	if errorDetail.Valid {
		value.ErrorDetail = pointer(errorDetail.String)
	}
	return value, nil
}
