package alerts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// Service is the alert-domain facade over the frozen SQLite authority.
//
// Ownership split (ADR-0006): every mutation this package owns — the admin
// alert-source management commands (sources.go), the Stele ingestion relay
// (delivery.go), the intake-issue acknowledgment (queries.go) and the
// platform-fault projection (platform_faults.go) — executes through the
// shared command runner: one runner-owned IMMEDIATE transaction carrying the
// authorization re-check, the domain writes and the automatic audit event.
// No business code audits by hand, commits or opens its own transactions;
// the relay deduplicates through the natural alert_deliveries.relay_id key
// instead of a client-command ledger row. Every pure read routes through
// runner.Reader() — the trusted opaque execution.Reader installed through
// runner.SetReader (only an execution.OpenReadOnly pool passes) — so an
// unwired read-only pool fails closed instead of silently reading and
// contending on the writer.
type Service struct {
	// db is the composition write database the runner owns; it is the
	// constructor input for that runner and the package tests' fixture
	// seeding handle. Pure reads never touch it: they all go through
	// runner.Reader(), never a writer fallback.
	db *sql.DB
	// runner executes this package's commands and projections with
	// automatic audit; its Reader() factory is the only read surface.
	runner *execution.Runner
	ops    alertOperations
	now    func() time.Time
}

// NewService is the pre-composition constructor: the service owns a private
// operation registry and no read-only pool, so every pure read fails closed
// until SetReader wires the real read-only database — there is deliberately
// no writer fallback. The composed application switches to
// NewServiceWithReader, which installs the pool through the same
// runner.SetReader factory at assembly time.
func NewService(db *sql.DB) *Service {
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	ops, err := registerOperations(runner)
	if err != nil {
		panic("alerts: compose default service: " + err.Error())
	}
	return &Service{db: db, runner: runner, ops: ops, now: time.Now}
}

// NewServiceWithReader assembles the service: db is the runner-owned write
// database, reader is the read-path query surface, and runner is the command
// executor whose registry receives this package's declared operations. The
// reader is validated and installed through runner.SetReader — only the
// trusted opaque execution.Reader produced by execution.OpenReadOnly (or
// bootstrap.Database.Reader) with a live pool passes — so the read paths can
// only ever see a capability the runner itself accepted. Registration fails
// only on a declaration conflict — a programming error surfaced at assembly
// time.
func NewServiceWithReader(db *sql.DB, reader audit.Reader, runner *execution.Runner) (*Service, error) {
	if db == nil {
		return nil, errors.New("alerts: database is required")
	}
	if reader == nil {
		return nil, errors.New("alerts: read capability is required")
	}
	if runner == nil {
		return nil, errors.New("alerts: command runner is required")
	}
	if err := runner.SetReader(reader); err != nil {
		return nil, fmt.Errorf("alerts: install read-only reader: %w", err)
	}
	ops, err := registerOperations(runner)
	if err != nil {
		return nil, err
	}
	return &Service{db: db, runner: runner, ops: ops, now: time.Now}, nil
}

// authorizeSourceAdmin re-verifies inside the runner transaction that the
// context carries a live administrator session proof (auth.
// VerifyExecutionSession): a session revoked, expired or drifted from its
// issued auth revision after admission — or a non-admin principal — fails
// closed with a clean rollback and no durable trace. It never degrades to a
// bare users-row check, which would miss the post-admission revocation race.
func authorizeSourceAdmin(ctx context.Context, tx *execution.Tx) error {
	return auth.VerifyExecutionSession(ctx, tx, "admin")
}

// clockText is the server-side UTC timestamp for domain rows; operation
// provenance (actor, source, correlation) comes from the execution metadata.
func (service *Service) clockText() string {
	return service.now().UTC().Format(time.RFC3339Nano)
}

// machineScope establishes the receiver-local execution scope for the
// deployment-internal machine entries this package owns (the Stele ingestion
// relay and the platform-fault projection).
//
// Identity source (ADR-0006 §5 跨进程与信任): these operations arrive over
// deployment-internal channels whose service identity — the Stele service
// token, the runtime control stream's fenced slot facts — is verified
// upstream by the owning admission before this package runs; the identity is
// therefore not re-verified here. That verification yields no row-backed
// principal: the schema has no service-principal table and the audit/execution
// identity rules require positive ids for 'service' actors, so the historical
// hand-written ("service", 0) audit actor was not a safe identity. The local
// root is consequently the sanctioned machine identity, the system principal,
// with an internal entry source and a fresh correlation — never inherited
// from client-supplied input, never an HTTP scope (the operation
// authorizations refuse one). A context that already carries execution
// metadata is kept unchanged: the runner's authorization then judges the
// caller's own scope, so a sub-step can neither forge nor replace it.
func (service *Service) machineScope(ctx context.Context) (context.Context, error) {
	if _, exists := execution.FromContext(ctx); exists {
		return ctx, nil
	}
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	return execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: correlationID,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Initiator:     execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceInternal},
	})
}

// ListSources returns the admin-facing alert source list. latest_valid_event_at
// records the most recent successfully committed Alertmanager delivery. An absent
// value deliberately means the source is waiting for its first event, never faulty.
func (service *Service) ListSources(ctx context.Context) ([]SourceSummary, error) {
	rows, err := service.runner.Reader().QueryContext(ctx, `SELECT s.id, s.source_key, s.protocol, s.enabled, s.row_version, s.created_at, s.disabled_at, (SELECT MAX(o.committed_at) FROM alert_observations o JOIN alert_occurrences occurrence ON occurrence.id=o.occurrence_id WHERE occurrence.source_id=s.id) FROM alert_sources s ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := []SourceSummary{}
	for rows.Next() {
		var summary SourceSummary
		var id int64
		var enabled int
		var disabledAt, latestValidEventAt sql.NullString
		if err := rows.Scan(&id, &summary.Key, &summary.Protocol, &enabled, &summary.RowVersion, &summary.CreatedAt, &disabledAt, &latestValidEventAt); err != nil {
			return nil, err
		}
		summary.ID = strconv.FormatInt(id, 10)
		summary.Enabled = enabled == 1
		if disabledAt.Valid {
			summary.DisabledAt = &disabledAt.String
		}
		if latestValidEventAt.Valid {
			summary.LatestValidEventAt = &latestValidEventAt.String
		}
		sources = append(sources, summary)
	}
	return sources, rows.Err()
}

type SourceSummary struct {
	ID                 string  `json:"id"`
	Key                string  `json:"key"`
	Protocol           string  `json:"protocol"`
	Enabled            bool    `json:"enabled"`
	RowVersion         int64   `json:"rowVersion"`
	CreatedAt          string  `json:"createdAt"`
	DisabledAt         *string `json:"disabledAt"`
	LatestValidEventAt *string `json:"latestValidEventAt,omitempty"`
}

// GetSource returns one alert source with its credential count.
func (service *Service) GetSource(ctx context.Context, sourceKey string) (SourceDetail, error) {
	return sourceDetailOn(ctx, service.runner.Reader(), sourceKey)
}

type SourceDetail struct {
	SourceSummary
	CredentialCount int `json:"credentialCount"`
}

// IDAsInt64 parses the string locator back into the row id for the runner's
// audit domain reference.
func (detail SourceDetail) IDAsInt64() int64 {
	id, _ := strconv.ParseInt(detail.ID, 10, 64)
	return id
}

// sourceDetailOn reads one source's admin projection on the given single-row
// reader — the runner's trusted read-only surface for the query path, the
// guarded runner transaction for command results.
func sourceDetailOn(ctx context.Context, reader sourceQuerier, sourceKey string) (SourceDetail, error) {
	var detail SourceDetail
	var id int64
	var enabled int
	var disabledAt, latestValidEventAt sql.NullString
	err := reader.QueryRowContext(ctx, `SELECT s.id, s.source_key, s.protocol, s.enabled, s.row_version, s.created_at, s.disabled_at, (SELECT COUNT(*) FROM alert_source_credentials c WHERE c.source_id=s.id), (SELECT MAX(o.committed_at) FROM alert_observations o JOIN alert_occurrences occurrence ON occurrence.id=o.occurrence_id WHERE occurrence.source_id=s.id) FROM alert_sources s WHERE s.source_key=?`, sourceKey).
		Scan(&id, &detail.Key, &detail.Protocol, &enabled, &detail.RowVersion, &detail.CreatedAt, &disabledAt, &detail.CredentialCount, &latestValidEventAt)
	if err != nil {
		return SourceDetail{}, err
	}
	detail.ID = strconv.FormatInt(id, 10)
	detail.Enabled = enabled == 1
	if disabledAt.Valid {
		detail.DisabledAt = &disabledAt.String
	}
	if latestValidEventAt.Valid {
		detail.LatestValidEventAt = &latestValidEventAt.String
	}
	return detail, nil
}

// LookupDigestForBearer authenticates a Stele-attached bearer against the
// active credential set and returns the source and credential ids for the
// delivery transaction. This is the Stele path; the HTTP reveal path does not
// call it.
func (service *Service) LookupDigestForBearer(ctx context.Context, bearerDigest []byte) (sourceID, credentialID int64, ok bool, err error) {
	err = service.runner.Reader().QueryRowContext(ctx, `SELECT c.source_id, c.id FROM alert_source_credentials c JOIN alert_sources s ON s.id = c.source_id WHERE c.digest = ? AND s.enabled = 1 AND c.state IN ('Active','PendingRetirement')`, bearerDigest).
		Scan(&sourceID, &credentialID)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return sourceID, credentialID, true, nil
}

// CredentialSnapshot returns the frozen non-secret credential digest snapshot
// Stele caches (RUNTIME-STELE-002): active + pending-retirement generations
// only, retired generations absent.
func (service *Service) CredentialSnapshot(ctx context.Context) (version uint64, sources []SnapshotSource, err error) {
	var maxID int64
	if err := service.runner.Reader().QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM alert_source_credentials`).Scan(&maxID); err != nil {
		return 0, nil, err
	}
	rows, err := service.runner.Reader().QueryContext(ctx, `SELECT s.id, s.source_key, s.protocol, s.enabled, c.id, c.digest FROM alert_sources s JOIN alert_source_credentials c ON c.source_id = s.id WHERE c.state IN ('Active','PendingRetirement') ORDER BY s.id, c.id`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	bySource := map[int64]*SnapshotSource{}
	order := []int64{}
	for rows.Next() {
		var sourceID, credentialID int64
		var sourceKey, protocol string
		var enabled int
		var digest []byte
		if err := rows.Scan(&sourceID, &sourceKey, &protocol, &enabled, &credentialID, &digest); err != nil {
			return 0, nil, err
		}
		entry := bySource[sourceID]
		if entry == nil {
			entry = &SnapshotSource{SourceID: sourceID, SourceKey: sourceKey, Protocol: protocol, Enabled: enabled == 1}
			bySource[sourceID] = entry
			order = append(order, sourceID)
		}
		entry.Credentials = append(entry.Credentials, CredentialDigest{ID: credentialID, Digest: digest})
	}
	sources = make([]SnapshotSource, 0, len(order))
	for _, sourceID := range order {
		sources = append(sources, *bySource[sourceID])
	}
	return uint64(maxID), sources, nil
}

type SnapshotSource struct {
	SourceID    int64              `json:"sourceId"`
	SourceKey   string             `json:"sourceKey"`
	Protocol    string             `json:"protocol"`
	Enabled     bool               `json:"enabled"`
	Credentials []CredentialDigest `json:"credentials"`
}

type CredentialDigest struct {
	ID     int64  `json:"id"`
	Digest []byte `json:"digest"`
}

// CountFiringOccurrences supports the snapshot/list query used by the UI.
func (service *Service) CountFiringOccurrences(ctx context.Context) (int64, error) {
	var count int64
	err := service.runner.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_occurrences WHERE state='Firing'`).Scan(&count)
	return count, err
}
