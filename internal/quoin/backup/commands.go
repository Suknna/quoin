package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

const (
	commandTrigger   = "backup.trigger"
	commandSettings  = "backup.settings.update"
	commandRetention = "artifact_retention.update"
)

// queueManualCommand atomically creates the queued row, audit event, and
// durable replay result. BEGIN IMMEDIATE is important: a same-key request that
// waited for the writer lock must re-read the ledger on this connection.
func (s *Service) queueManualCommand(ctx context.Context, actor int64, commandID string) (Summary, error) {
	if !ValidCommandID(commandID) {
		return Summary{}, ErrInvalidCommandID
	}
	digest := auth.DigestCommand(commandTrigger, map[string]any{"executionMode": "online"})
	return commandObject(ctx, s.db, s.now, s.authorizeActor, actor, commandID, commandTrigger, digest, "backup", func(value Summary) int64 { return mustInt(value.ID) }, func(Summary) bool { return true }, func(conn *sql.Conn) (Summary, error) {
		now := timestamp(s.now())
		result, err := conn.ExecContext(ctx, `INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,row_version,created_at,updated_at,triggered_by) VALUES('queued','queued','manual','online',NULL,1,?,?,?)`, now, now, actor)
		if err != nil {
			if isActiveConstraint(err) {
				var activeID int64
				if lookupErr := conn.QueryRowContext(ctx, `SELECT id FROM backups WHERE status IN ('queued','running') ORDER BY id LIMIT 1`).Scan(&activeID); lookupErr == nil {
					return Summary{}, &ActiveError{ID: fmt.Sprintf("%d", activeID)}
				}
				return Summary{}, ErrActive
			}
			return Summary{}, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return Summary{}, err
		}
		return s.getOn(ctx, conn, id)
	})
}

// UpdateSettingsCommand applies the optimistic settings update with the same
// ledger discipline as every external write.
func (s *Service) UpdateSettingsCommand(ctx context.Context, actor, expected int64, commandID string, enabled *bool, cron *string, timezone *string, retention *int64) (Settings, error) {
	if !ValidCommandID(commandID) {
		return Settings{}, ErrInvalidCommandID
	}
	if enabled == nil && cron == nil && timezone == nil && retention == nil {
		return Settings{}, ErrInvalidSettings
	}
	digest := auth.DigestCommand(commandSettings, map[string]any{"expectedRowVersion": expected, "enabled": enabled, "scheduleCron": cron, "timezone": timezone, "retentionCount": retention})
	value, err := s.settingsCommand(ctx, actor, expected, commandID, commandSettings, digest, func(conn *sql.Conn) (Settings, error) {
		current, err := s.settingsOn(ctx, conn)
		if err != nil {
			return Settings{}, err
		}
		if current.RowVersion != expected {
			return Settings{}, ErrRowVersionConflict
		}
		if retention != nil && *retention < 1 {
			return Settings{}, fmt.Errorf("%w: retention count must be positive", ErrInvalidSettings)
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
			return Settings{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
		}
		// A semantically identical command is durably accepted and replayable,
		// but must not rewrite the settings row or schedule anchor.
		if settingsEqual(original, current) {
			return original, nil
		}
		var cronValue any
		if current.ScheduleCron != nil {
			cronValue = *current.ScheduleCron
		}
		now := timestamp(s.now())
		if _, err = conn.ExecContext(ctx, `UPDATE backup_settings SET enabled=?,schedule_cron=?,timezone=?,retention_count=?,schedule_enabled_at=CASE WHEN ?=0 THEN NULL WHEN ?=1 AND ?=0 THEN ? ELSE schedule_enabled_at END,row_version=row_version+1,updated_by=?,updated_at=? WHERE id=1 AND row_version=?`, boolInt(current.Enabled), cronValue, current.Timezone, current.RetentionCount, boolInt(current.Enabled), boolInt(current.Enabled), boolInt(wasEnabled), now, actor, now, expected); err != nil {
			return Settings{}, err
		}
		return s.settingsOn(ctx, conn)
	})
	if err != nil {
		return Settings{}, err
	}
	value.BackupTarget = s.config.BackupDirectory
	return value, nil
}

func (s *Service) UpdateArtifactRetentionCommand(ctx context.Context, actor, expected, days int64, commandID string) (ArtifactRetention, error) {
	if !ValidCommandID(commandID) {
		return ArtifactRetention{}, ErrInvalidCommandID
	}
	digest := auth.DigestCommand(commandRetention, map[string]any{"expectedRowVersion": expected, "generatedRetentionDays": days})
	return s.retentionCommand(ctx, actor, expected, commandID, commandRetention, digest, func(conn *sql.Conn) (ArtifactRetention, error) {
		if days < 1 {
			return ArtifactRetention{}, fmt.Errorf("%w: retention days must be positive", ErrInvalidSettings)
		}
		current, err := s.retentionOn(ctx, conn)
		if err != nil {
			return ArtifactRetention{}, err
		}
		if current.RowVersion != expected {
			return ArtifactRetention{}, ErrRowVersionConflict
		}
		// A no-op still gets a durable command result but does not create a
		// new settings revision or state-change audit event.
		if current.GeneratedRetentionDays == days {
			return current, nil
		}
		result, err := conn.ExecContext(ctx, `UPDATE artifact_retention_settings SET generated_retention_days=?,row_version=row_version+1,updated_by=?,updated_at=? WHERE id=1 AND row_version=?`, days, actor, timestamp(s.now()), expected)
		if err != nil {
			return ArtifactRetention{}, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return ArtifactRetention{}, err
		}
		if rows != 1 {
			return ArtifactRetention{}, ErrRowVersionConflict
		}
		return s.retentionOn(ctx, conn)
	})
}

func (s *Service) settingsCommand(ctx context.Context, actor, expected int64, commandID, commandType, digest string, run func(*sql.Conn) (Settings, error)) (Settings, error) {
	return commandObject(ctx, s.db, s.now, s.authorizeActor, actor, commandID, commandType, digest, "backup_settings", func(Settings) int64 { return 1 }, func(value Settings) bool { return value.RowVersion > expected }, run)
}
func (s *Service) retentionCommand(ctx context.Context, actor, expected int64, commandID, commandType, digest string, run func(*sql.Conn) (ArtifactRetention, error)) (ArtifactRetention, error) {
	return commandObject(ctx, s.db, s.now, s.authorizeActor, actor, commandID, commandType, digest, "artifact_retention_settings", func(ArtifactRetention) int64 { return 1 }, func(value ArtifactRetention) bool { return value.RowVersion > expected }, run)
}

func commandObject[T any](ctx context.Context, db *sql.DB, now func() time.Time, authorize func(context.Context, *sql.Conn, int64) error, actor int64, commandID, commandType, digest, objectType string, objectID func(T) int64, stateChanged func(T) bool, run func(*sql.Conn) (T, error)) (T, error) {
	var zero T
	conn, err := db.Conn(ctx)
	if err != nil {
		return zero, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return zero, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if authorize != nil {
		if err := authorize(ctx, conn, actor); err != nil {
			return zero, err
		}
	}
	record, found, err := auth.LookupCommandOn(ctx, conn, actor, commandID)
	if err != nil {
		return zero, err
	}
	if found {
		if record.CommandType != commandType || record.RequestDigest != digest {
			return zero, ErrCommandReused
		}
		if record.Outcome == auth.OutcomeRejectedKnown {
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return zero, err
			}
			committed = true
			return zero, replayRejected(record.ResultPayload)
		}
		var replay T
		if err = json.Unmarshal([]byte(record.ResultPayload), &replay); err != nil {
			return zero, err
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return zero, err
		}
		committed = true
		return replay, nil
	}
	value, err := run(conn)
	if err != nil {
		if recordErr := recordKnownRejection(ctx, conn, actor, commandID, commandType, digest, objectType, 0, err, now); recordErr != nil {
			return zero, recordErr
		}
		if _, commitErr := conn.ExecContext(ctx, "COMMIT"); commitErr != nil {
			return zero, commitErr
		}
		committed = true
		return zero, err
	}
	body, err := json.Marshal(value)
	if err != nil {
		return zero, err
	}
	resolvedObjectID := objectID(value)
	if err = auth.RecordCommand(ctx, conn, actor, commandID, commandType, digest, auth.OutcomeCommitted, objectType, resolvedObjectID, string(body)); err != nil {
		return zero, err
	}
	if stateChanged(value) {
		if err = recordAuditAt(ctx, conn, now, actor, commandType, commandID, objectType, resolvedObjectID); err != nil {
			return zero, err
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return zero, err
	}
	committed = true
	return value, nil
}

type rejectionPayload struct {
	Code     string `json:"code"`
	ObjectID int64  `json:"objectId,omitempty"`
	Detail   string `json:"detail"`
}

type replayedRejection struct {
	cause  error
	detail string
}

func (err *replayedRejection) Error() string { return err.detail }
func (err *replayedRejection) Unwrap() error { return err.cause }

func knownRejectionCode(err error) string {
	switch {
	case errors.Is(err, ErrActive):
		return "active_conflict"
	case errors.Is(err, ErrRowVersionConflict):
		return "row_version_conflict"
	case errors.Is(err, ErrNoSettingsChange):
		return "no_change"
	case errors.Is(err, ErrInvalidCommandID):
		return "invalid_client_command_id"
	case errors.Is(err, ErrInvalidSettings):
		return "invalid_settings"
	default:
		return ""
	}
}
func replayRejected(payload string) error {
	var value rejectionPayload
	if json.Unmarshal([]byte(payload), &value) != nil {
		return errors.New("stored rejected command is invalid")
	}
	switch value.Code {
	case "active_conflict":
		return &ActiveError{ID: fmt.Sprintf("%d", value.ObjectID)}
	case "row_version_conflict":
		return &replayedRejection{cause: ErrRowVersionConflict, detail: value.Detail}
	case "no_change":
		return &replayedRejection{cause: ErrNoSettingsChange, detail: value.Detail}
	case "invalid_client_command_id":
		return &replayedRejection{cause: ErrInvalidCommandID, detail: value.Detail}
	case "invalid_settings":
		return &replayedRejection{cause: ErrInvalidSettings, detail: value.Detail}
	default:
		return errors.New(value.Code)
	}
}
func recordKnownRejection(ctx context.Context, conn *sql.Conn, actor int64, commandID, commandType, digest, objectType string, objectID int64, commandErr error, now func() time.Time) error {
	code := knownRejectionCode(commandErr)
	if code == "" {
		return commandErr
	}
	if active := new(ActiveError); errors.As(commandErr, &active) {
		if parsed, parseErr := strconv.ParseInt(active.ID, 10, 64); parseErr == nil && parsed > 0 {
			objectID = parsed
		}
	}
	body, err := json.Marshal(rejectionPayload{Code: code, ObjectID: objectID, Detail: commandErr.Error()})
	if err != nil {
		return err
	}
	if err = auth.RecordCommand(ctx, conn, actor, commandID, commandType, digest, auth.OutcomeRejectedKnown, objectType, objectID, string(body)); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,client_command_id,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('user',?,?,?,?,?,?,?)`, actor, commandType, commandID, "rejected", objectType, objectID, timestamp(now()))
	return err
}

func (s *Service) recordAudit(ctx context.Context, conn *sql.Conn, actor int64, action, commandID, refType string, refID int64) error {
	return recordAuditAt(ctx, conn, s.now, actor, action, commandID, refType, refID)
}
func recordAuditAt(ctx context.Context, conn *sql.Conn, now func() time.Time, actor int64, action, commandID, refType string, refID int64) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,client_command_id,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('user',?,?,?,?,?,?,?)`, actor, action, commandID, "success", refType, refID, timestamp(now()))
	return err
}

func (s *Service) getOn(ctx context.Context, conn *sql.Conn, id int64) (Summary, error) {
	return scanSummary(ctx, conn, id)
}
func (s *Service) settingsOn(ctx context.Context, conn *sql.Conn) (Settings, error) {
	var value Settings
	var enabled int
	var cron sql.NullString
	err := conn.QueryRowContext(ctx, `SELECT enabled,schedule_cron,timezone,retention_count,row_version FROM backup_settings WHERE id=1`).Scan(&enabled, &cron, &value.Timezone, &value.RetentionCount, &value.RowVersion)
	value.Enabled = enabled == 1
	if cron.Valid {
		value.ScheduleCron = pointer(cron.String)
	}
	return value, err
}
func (s *Service) retentionOn(ctx context.Context, conn *sql.Conn) (ArtifactRetention, error) {
	var value ArtifactRetention
	err := conn.QueryRowContext(ctx, `SELECT generated_retention_days,row_version FROM artifact_retention_settings WHERE id=1`).Scan(&value.GeneratedRetentionDays, &value.RowVersion)
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
