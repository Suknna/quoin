package alerts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// Sources owns the admin alert-source and credential lifecycle commands
// behind the frozen schema triggers (DATA-ALERT-009/010, SEC-REVEAL-*,
// HTTP-COMMAND-*). Every command executes through the shared execution runner
// as a durable, replayable client command: one runner-owned IMMEDIATE
// transaction holding the administrator session re-check (auth.
// VerifyExecutionSession), the idempotent replay lookup, the domain writes,
// the command ledger row and the automatic audit event. No hand-written audit
// calls and no transaction control remain; deterministic conflicts ride as
// execution.Rejection, are persisted as rejected_known ledger rows plus
// rejected audit facts, and replay without re-execution. The ledger payloads
// and request digests carry only non-secret semantic fields — the raw bearer
// never reaches this package, only its 32-byte digest.
//
// Actor, source and correlation come from the execution metadata (the HTTP
// admission middleware provides them); the caller supplies only the client
// command id and the domain inputs. The same registry also declares the
// package's non-ledger mutations (delivery.go, queries.go,
// platform_faults.go), which run through execution.Execute with automatic
// audit but no client-command row.

// Stable operation identities (they double as the durable command types and
// the automatic audit actions) and the audit domain object types.
const (
	opCreateSource     = "alert_source.create"
	opRotateCredential = "alert_source.rotate"
	opRetireCredential = "alert_source.credential_retire"
	opSetSourceEnabled = "alert_source.set_enabled"
	opRevealCredential = "alert_source.credential_reveal"

	opDelivery             = "alert.delivery"
	opAcknowledgeIntake    = "alert_intake_issue.acknowledge"
	opCredentialDenied     = "alert_intake_issue.credential_denied"
	opFaultRuntime         = "platform_fault.observe_runtime_connection"
	opFaultExecution       = "platform_fault.observe_execution_outcome"
	reasonCredentialDenied = "credential or source is not currently accepted"

	objectSource        = "alert_source"
	objectCredential    = "alert_source_credential"
	objectDelivery      = "alert_delivery"
	objectIntakeIssue   = "alert_intake_issue"
	objectPlatformFault = "platform_fault"
)

// Rejection codes surfaced by the source commands (stable machine codes the
// HTTP problem mapping keys on).
const (
	CodeValidationFailed   = "validation_failed"
	CodeNotFound           = "not_found"
	CodeRowVersionConflict = "row_version_conflict"
	CodeNoActiveCredential = "no_active_credential"
)

// alertOperations holds the canonical registered declarations of this
// package's write operations.
type alertOperations struct {
	create     *execution.Operation
	rotate     *execution.Operation
	retire     *execution.Operation
	enabled    *execution.Operation
	reveal     *execution.Operation
	delivery   *execution.Operation
	ackIntake  *execution.Operation
	credDenied *execution.Operation
	faultLive  *execution.Operation
	faultExec  *execution.Operation
}

// registerOperations declares this package's operations on the runner's
// registry; registering the same name twice fails, so two modules can never
// silently claim one operation identity.
func registerOperations(runner *execution.Runner) (alertOperations, error) {
	ops := alertOperations{}
	declare := func(name string, objectType string, authorize func(context.Context, *execution.Tx) error, target **execution.Operation) error {
		op, err := runner.Register(execution.Operation{
			Name:       name,
			Class:      execution.ClassWrite,
			ObjectType: objectType,
			Authorize:  authorize,
		})
		if err != nil {
			return fmt.Errorf("alerts: register %s: %w", name, err)
		}
		*target = op
		return nil
	}
	for _, spec := range []struct {
		name      string
		object    string
		authorize func(context.Context, *execution.Tx) error
		target    **execution.Operation
	}{
		{opCreateSource, objectSource, authorizeSourceAdmin, &ops.create},
		{opRotateCredential, objectCredential, authorizeSourceAdmin, &ops.rotate},
		{opRetireCredential, objectCredential, authorizeSourceAdmin, &ops.retire},
		{opSetSourceEnabled, objectSource, authorizeSourceAdmin, &ops.enabled},
		{opRevealCredential, objectCredential, authorizeSourceAdmin, &ops.reveal},
		{opDelivery, objectDelivery, authorizeMachineEntry, &ops.delivery},
		{opAcknowledgeIntake, objectIntakeIssue, authorizeSourceAdmin, &ops.ackIntake},
		{opCredentialDenied, objectIntakeIssue, authorizeMachineEntry, &ops.credDenied},
		{opFaultRuntime, objectPlatformFault, authorizeMachineEntry, &ops.faultLive},
		{opFaultExecution, objectPlatformFault, authorizeMachineEntry, &ops.faultExec},
	} {
		if err := declare(spec.name, spec.object, spec.authorize, spec.target); err != nil {
			return ops, err
		}
	}
	return ops, nil
}

// authorizeMachineEntry is the in-transaction authorization for the
// deployment-internal machine entries (Stele relay delivery, platform-fault
// projection): the acting principal must be the system principal and the
// entry source must not be HTTP. The upstream admission (service-token or
// fenced slot-view verification) is where the machine identity is
// established; this check keeps an HTTP or user scope from masquerading as
// the machine entry, and a plain error rolls back with no durable trace.
func authorizeMachineEntry(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return errors.New("alerts: machine projection requires the system principal")
	}
	if meta.Source.Kind == execution.SourceHTTP {
		return errors.New("alerts: machine projection cannot arrive from the http channel")
	}
	return nil
}

// CreateSourceResult carries the non-secret outcome of creating a source with
// its first credential generation. It is persisted verbatim as the command
// ledger replay payload, so it must never carry secrets — the raw bearer
// exists only in the caller's reveal path.
type CreateSourceResult struct {
	SourceID     int64  `json:"sourceId"`
	CredentialID int64  `json:"credentialId"`
	SourceKey    string `json:"sourceKey"`
}

// commandActor derives the durable ledger principal from the verified
// execution metadata: the runner re-checks that the command principal matches
// the context actor, so a caller can never execute under someone else's key.
func commandActor(ctx context.Context) (execution.Command, error) {
	meta, err := execution.Require(ctx)
	if err != nil {
		return execution.Command{}, err
	}
	return execution.Command{
		PrincipalType: string(meta.Actor.Kind),
		PrincipalID:   meta.Actor.ID,
	}, nil
}

// CreateSource creates the logical source and its first Active credential in
// one runner transaction, persisting only the bearer digest. The automatic
// audit records the acting administrator. A duplicate source_key surfaces as
// the driver's UNIQUE violation after a full rollback — no ledger row and no
// audit trace — exactly like the legacy contract.
func (service *Service) CreateSource(ctx context.Context, clientCommandID, sourceKey, protocol string, bearerDigest []byte) (CreateSourceResult, bool, error) {
	command, err := commandActor(ctx)
	if err != nil {
		return CreateSourceResult{}, false, err
	}
	command.ClientCommandID = clientCommandID
	command.Digest = auth.DigestCommand(opCreateSource, map[string]any{"key": sourceKey, "protocol": protocol})
	outcome, err := execution.Run(ctx, service.runner, service.ops.create, command,
		func(tx *execution.Tx) (CreateSourceResult, execution.Change, error) {
			if err := validateSourceInput(sourceKey, protocol); err != nil {
				return CreateSourceResult{}, execution.Unchanged, err
			}
			now := service.clockText()
			sourceRow, err := tx.ExecContext(ctx, `INSERT INTO alert_sources(source_key, protocol, enabled, created_at) VALUES(?,?,1,?)`, sourceKey, protocol, now)
			if err != nil {
				return CreateSourceResult{}, execution.Changed, err
			}
			sourceID, err := sourceRow.LastInsertId()
			if err != nil {
				return CreateSourceResult{}, execution.Changed, err
			}
			credentialRow, err := tx.ExecContext(ctx, `INSERT INTO alert_source_credentials(source_id, digest, state, created_at) VALUES(?,?,'Active',?)`, sourceID, bearerDigest, now)
			if err != nil {
				return CreateSourceResult{}, execution.Changed, err
			}
			credentialID, err := credentialRow.LastInsertId()
			if err != nil {
				return CreateSourceResult{}, execution.Changed, err
			}
			return CreateSourceResult{SourceID: sourceID, CredentialID: credentialID, SourceKey: sourceKey}, execution.Changed, nil
		},
		func(result CreateSourceResult) int64 { return result.SourceID })
	if err != nil {
		return CreateSourceResult{}, false, err
	}
	return outcome.Result, outcome.Replayed, nil
}

// validateSourceInput rejects malformed payloads deterministically; the
// reserved-key and trim rules stay at the HTTP entry boundary.
func validateSourceInput(sourceKey, protocol string) error {
	if sourceKey == "" || len(sourceKey) > 200 {
		return &execution.Rejection{Code: CodeValidationFailed, Detail: "告警源 key 必须为 1 到 200 个字符"}
	}
	if protocol != "alertmanager" {
		return &execution.Rejection{Code: CodeValidationFailed, Detail: "protocol 仅支持 alertmanager"}
	}
	return nil
}

// RotateCredential creates a new Active generation superseding the current
// Active one (max two accepted generations per source, schema enforced) in
// one runner transaction with the automatic audit event.
func (service *Service) RotateCredential(ctx context.Context, clientCommandID, sourceKey string, newDigest []byte) (CreateSourceResult, bool, error) {
	command, err := commandActor(ctx)
	if err != nil {
		return CreateSourceResult{}, false, err
	}
	command.ClientCommandID = clientCommandID
	command.Digest = auth.DigestCommand(opRotateCredential, map[string]any{"sourceKey": sourceKey})
	outcome, err := execution.Run(ctx, service.runner, service.ops.rotate, command,
		func(tx *execution.Tx) (CreateSourceResult, execution.Change, error) {
			var sourceID int64
			if err := tx.QueryRowContext(ctx, `SELECT id FROM alert_sources WHERE source_key=?`, sourceKey).Scan(&sourceID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return CreateSourceResult{}, execution.Unchanged, &execution.Rejection{Code: CodeNotFound, Detail: "告警源不存在"}
				}
				return CreateSourceResult{}, execution.Changed, err
			}
			var currentID int64
			if err := tx.QueryRowContext(ctx, `SELECT id FROM alert_source_credentials WHERE source_id=? AND state='Active' ORDER BY id DESC LIMIT 1`, sourceID).Scan(&currentID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return CreateSourceResult{}, execution.Unchanged, &execution.Rejection{Code: CodeNoActiveCredential, Detail: "没有可轮换的 Active 凭据"}
				}
				return CreateSourceResult{}, execution.Changed, err
			}
			credentialRow, err := tx.ExecContext(ctx, `INSERT INTO alert_source_credentials(source_id, digest, state, supersedes_credential_id, created_at) VALUES(?,?,'Active',?,?)`,
				sourceID, newDigest, currentID, service.clockText())
			if err != nil {
				return CreateSourceResult{}, execution.Changed, err
			}
			credentialID, err := credentialRow.LastInsertId()
			if err != nil {
				return CreateSourceResult{}, execution.Changed, err
			}
			return CreateSourceResult{SourceID: sourceID, CredentialID: credentialID, SourceKey: sourceKey}, execution.Changed, nil
		},
		func(result CreateSourceResult) int64 { return result.CredentialID })
	if err != nil {
		return CreateSourceResult{}, false, err
	}
	return outcome.Result, outcome.Replayed, nil
}

// RetireCredential performs the explicit Active|PendingRetirement -> Retired
// transition (DATA-ALERT-009) with row-version fencing, returning the retired
// credential's summary so the durable replay returns the identical projection.
// A stale row version, an unknown source or credential is a recorded
// deterministic rejection.
func (service *Service) RetireCredential(ctx context.Context, clientCommandID, sourceKey string, credentialID, expectedRowVersion int64) (CredentialSummary, bool, error) {
	command, err := commandActor(ctx)
	if err != nil {
		return CredentialSummary{}, false, err
	}
	command.ClientCommandID = clientCommandID
	command.Digest = auth.DigestCommand(opRetireCredential, map[string]any{"sourceKey": sourceKey, "credentialId": credentialID, "expectedRowVersion": expectedRowVersion})
	outcome, err := execution.Run(ctx, service.runner, service.ops.retire, command,
		func(tx *execution.Tx) (CredentialSummary, execution.Change, error) {
			result, err := tx.ExecContext(ctx, `UPDATE alert_source_credentials SET state='Retired', retired_at=?, row_version=row_version+1 WHERE id=? AND source_id=(SELECT id FROM alert_sources WHERE source_key=?) AND row_version=? AND state IN ('Active','PendingRetirement')`,
				service.clockText(), credentialID, sourceKey, expectedRowVersion)
			if err != nil {
				return CredentialSummary{}, execution.Changed, err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return CredentialSummary{}, execution.Changed, err
			}
			if affected == 0 {
				return CredentialSummary{}, execution.Unchanged, &execution.Rejection{Code: CodeRowVersionConflict, Detail: "告警源凭据已变化或已退休", ObjectID: credentialID}
			}
			summary, err := credentialSummaryOn(ctx, tx, credentialID)
			if err != nil {
				return CredentialSummary{}, execution.Changed, err
			}
			return summary, execution.Changed, nil
		},
		func(summary CredentialSummary) int64 { return credentialID })
	if err != nil {
		return CredentialSummary{}, false, err
	}
	return outcome.Result, outcome.Replayed, nil
}

// SetSourceEnabled toggles the logical source with row-version fencing and
// returns the fresh source detail inside the same transaction, so the durable
// replay returns the identical projection. An unknown source is a recorded
// not_found rejection — the HTTP layer maps it to 404 exactly like the legacy
// sentinel.
func (service *Service) SetSourceEnabled(ctx context.Context, clientCommandID, sourceKey string, enabled bool, expectedRowVersion int64) (SourceDetail, bool, error) {
	command, err := commandActor(ctx)
	if err != nil {
		return SourceDetail{}, false, err
	}
	command.ClientCommandID = clientCommandID
	command.Digest = auth.DigestCommand(opSetSourceEnabled, map[string]any{"sourceKey": sourceKey, "enabled": enabled, "expectedRowVersion": expectedRowVersion})
	outcome, err := execution.Run(ctx, service.runner, service.ops.enabled, command,
		func(tx *execution.Tx) (SourceDetail, execution.Change, error) {
			var sourceID int64
			if err := tx.QueryRowContext(ctx, `SELECT id FROM alert_sources WHERE source_key=?`, sourceKey).Scan(&sourceID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return SourceDetail{}, execution.Unchanged, &execution.Rejection{Code: CodeNotFound, Detail: "告警源不存在"}
				}
				return SourceDetail{}, execution.Changed, err
			}
			value := 0
			if enabled {
				value = 1
			}
			var disabledAt any
			if !enabled {
				disabledAt = service.clockText()
			}
			result, err := tx.ExecContext(ctx, `UPDATE alert_sources SET enabled=?, disabled_at=?, row_version=row_version+1 WHERE source_key=? AND row_version=?`,
				value, disabledAt, sourceKey, expectedRowVersion)
			if err != nil {
				return SourceDetail{}, execution.Changed, err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return SourceDetail{}, execution.Changed, err
			}
			if affected == 0 {
				return SourceDetail{}, execution.Unchanged, &execution.Rejection{Code: CodeRowVersionConflict, Detail: "告警源已变化", ObjectID: sourceID}
			}
			detail, err := sourceDetailOn(ctx, tx, sourceKey)
			if err != nil {
				return SourceDetail{}, execution.Changed, err
			}
			return detail, execution.Changed, nil
		},
		func(detail SourceDetail) int64 { return detail.IDAsInt64() })
	if err != nil {
		return SourceDetail{}, false, err
	}
	return outcome.Result, outcome.Replayed, nil
}

// RecordRevealAudit persists the authorized-access audit fact for a
// credential reveal BEFORE the caller releases the raw bearer (HTTP-COMMAND-
// 008, SEC-REVEAL-*): the automatic audit row commits inside the runner
// transaction, so an audit failure withholds the secret. It is an access
// fact, not a replayable command — one-time reveal handles carry the
// idempotency — so it runs as a non-ledger execution. No handle and no bearer
// value is ever recorded; the target is the credential row id.
func (service *Service) RecordRevealAudit(ctx context.Context, credentialID int64) error {
	_, err := execution.Execute(ctx, service.runner, service.ops.reveal,
		func(tx *execution.Tx) (bool, error) {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM alert_source_credentials WHERE id=?`, credentialID).Scan(&exists); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return false, &execution.Rejection{Code: CodeNotFound, Detail: "告警源凭据不存在", ObjectID: credentialID}
				}
				return false, err
			}
			return true, nil
		},
		func(bool) int64 { return credentialID })
	return err
}

// sourceQuerier is the single-row read surface shared by the narrowed reader
// and the runner's guarded *execution.Tx, so projections compose unchanged
// inside a runner transaction.
type sourceQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// credentialSummaryOn reads one credential's non-secret history projection on
// the given reader.
func credentialSummaryOn(ctx context.Context, reader sourceQuerier, credentialID int64) (CredentialSummary, error) {
	var summary CredentialSummary
	var id int64
	var supersedes, firstUsed, pendingRetirement, retired sql.NullString
	err := reader.QueryRowContext(ctx, `SELECT c.id, c.state, c.row_version, c.created_at, c.supersedes_credential_id, c.first_used_at, c.pending_retirement_at, c.retired_at FROM alert_source_credentials c WHERE c.id=?`, credentialID).
		Scan(&id, &summary.State, &summary.RowVersion, &summary.CreatedAt, &supersedes, &firstUsed, &pendingRetirement, &retired)
	if err != nil {
		return CredentialSummary{}, err
	}
	summary.ID = strconv.FormatInt(id, 10)
	if supersedes.Valid {
		summary.SupersedesCredentialID = &supersedes.String
	}
	if firstUsed.Valid {
		summary.FirstUsedAt = &firstUsed.String
	}
	if pendingRetirement.Valid {
		summary.PendingRetirementAt = &pendingRetirement.String
	}
	if retired.Valid {
		summary.RetiredAt = &retired.String
	}
	return summary, nil
}

// ListCredentials returns the credential history (no digests). Read-only
// projection through the runner's trusted read-only surface.
func (service *Service) ListCredentials(ctx context.Context, sourceKey string) ([]CredentialSummary, error) {
	rows, err := service.runner.Reader().QueryContext(ctx, `SELECT c.id, c.state, c.row_version, c.created_at, c.supersedes_credential_id, c.first_used_at, c.pending_retirement_at, c.retired_at FROM alert_source_credentials c JOIN alert_sources s ON s.id=c.source_id WHERE s.source_key=? ORDER BY c.id DESC`, sourceKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	credentials := []CredentialSummary{}
	for rows.Next() {
		var summary CredentialSummary
		var id int64
		var supersedes, firstUsed, pendingRetirement, retired sql.NullString
		if err := rows.Scan(&id, &summary.State, &summary.RowVersion, &summary.CreatedAt, &supersedes, &firstUsed, &pendingRetirement, &retired); err != nil {
			return nil, err
		}
		summary.ID = strconv.FormatInt(id, 10)
		if supersedes.Valid {
			summary.SupersedesCredentialID = &supersedes.String
		}
		if firstUsed.Valid {
			summary.FirstUsedAt = &firstUsed.String
		}
		if pendingRetirement.Valid {
			summary.PendingRetirementAt = &pendingRetirement.String
		}
		if retired.Valid {
			summary.RetiredAt = &retired.String
		}
		credentials = append(credentials, summary)
	}
	return credentials, rows.Err()
}

type CredentialSummary struct {
	ID                     string  `json:"id"`
	State                  string  `json:"state"`
	RowVersion             int64   `json:"rowVersion"`
	CreatedAt              string  `json:"createdAt"`
	SupersedesCredentialID *string `json:"supersedesCredentialId,omitempty"`
	FirstUsedAt            *string `json:"firstUsedAt,omitempty"`
	PendingRetirementAt    *string `json:"pendingRetirementAt,omitempty"`
	RetiredAt              *string `json:"retiredAt,omitempty"`
}
