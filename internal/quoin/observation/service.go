// Package observation owns the source-level observation authority introduced
// by ADR-0004 ("接入即有界观测"): once an administrator verifies and enables a
// connection whose plugin supports bounded discovery, Quoin admits complete
// observation runs on its own authority — no business declaration is required.
//
// Ownership split (ADR-0004):
//   - Quoin owns configuration, authorization, scheduling and the
//     authoritative result: this package freezes each Run's object types,
//     query, identity labels and connection grant before dispatch, adjudicates
//     the supervisor's result proposal in one transaction, and projects the
//     observed_source_objects identity table.
//   - The observable platform surface itself is plugin-owned descriptor
//     metadata (DiscoverObjects on the shared registry): which object types
//     exist, their identity labels, the discovery query and the per-pass
//     budget. The core never hardcodes a platform vocabulary — a new plugin
//     extends observation by declaring DiscoverObjects and binding a
//     Discoverer, without touching this package.
//   - The actual discovery executes in the Plinth supervisor through the
//     plugin Discoverer execution binding (internal/plugins), never by a
//     Quoin-side platform query shortcut.
//
// Invariants enforced here:
//   - Activity identity = (source connection, object type, canonical source
//     identity). Same-named objects from different connections never merge.
//   - Failure, partial results and truncation never clear resources and never
//     infer physical deletion. A complete successful pass over the frozen
//     scope marks objects it no longer saw current=0 ("not_observed"); the
//     stale flag is never derived by a pass and stays an explicit fact.
//   - At most one active Run per connection (ux_observation_run_active); a
//     scheduled tick dedupes on (connection, scheduled_for); a manual or
//     enablement admission returns the already-active Run instead of forking.
package observation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// Frozen wire identities of the source observation execution protocol. The
// input schema kind is pinned by the attempt_input_snapshots closure trigger;
// the result schema kind is the only payload this package accepts back. The
// discovery vocabulary itself lives on the plugin descriptors, not here.
const (
	// ExecutionSchemaKind freezes the dispatched input shape.
	ExecutionSchemaKind = "source_observation_execution_v1"
	// ResultSchemaKind freezes the accepted result proposal shape.
	ResultSchemaKind = "source_observation_result_v1"
	// executionRendererVersion is the input renderer generation.
	executionRendererVersion = "v1"

	// DefaultIntervalSeconds is the durable schedule cadence for automatic
	// observation (minimum boundary owned here, not by the coordinator).
	DefaultIntervalSeconds = 300

	// operationStart is the audited admission mutation (manual administrator
	// refresh, scheduler tick or enablement kick). The command ledger owns
	// replay for it.
	operationStart = "observation.source.start"
	// operationResult commits one supervisor result proposal; the Run
	// convergence it performs inside the same transaction may also emit the
	// run's terminal audit record (actionRunComplete / actionRunFail).
	operationResult = "observation.source.result"
	// operationConverge terminates one permanently unexecutable child.
	operationConverge = "observation.source.converge"
	// operationCancel reconciles runs whose connection lost observability.
	operationCancel = "observation.source.cancel"
	// actionRunComplete / actionRunFail are the run terminal audit actions
	// written once per actual state transition — the central high-volume
	// exception of this domain: per-resource telemetry upserts are never
	// audited row by row, the run lifecycle carries the audit trail.
	actionRunComplete = "observation.run.complete"
	actionRunFail     = "observation.run.fail"

	objectTypeObservationRun = "observation_run"
)

// ErrNotFound reports a missing connection, run or observed resource.
var ErrNotFound = errors.New("connection, observation run or observed resource not found")

// ErrCommandReused reports a client command id replayed with a different
// request digest (HTTP-COMMAND-003).
var ErrCommandReused = errors.New("client command id reused with a different request")

// ErrNotObservable reports an admission attempt against a connection that is
// missing, disabled, or has no enabled discover-capable plugin.
var ErrNotObservable = errors.New("connection is not enabled or has no enabled discover-capable plugin")

// ErrMaintenanceActive reports an admission attempt while a maintenance
// revision is active. It is retryable after maintenance exits.
var ErrMaintenanceActive = errors.New("source observation admission is blocked by maintenance")

// Service owns the source observation SQLite transactions. The registry and
// the deployment-resolved enablement set are mandatory wiring: without an
// explicit enabled discovery catalog nothing is admitted, so a deployment can
// never silently observe more than its YAML selected.
//
// Every lifecycle mutation runs through the shared execution runner: the
// manual refresh is an administrator session mutation
// (auth.VerifyExecutionSession), scheduled ticks and runtime result commits
// carry explicit system metadata, and each accepted/completed/failed run
// transition is audited automatically in the same transaction. Per-resource
// telemetry upserts are deliberately not audited row by row — the run
// lifecycle record is the domain's central audit exception.
type Service struct {
	db  *sql.DB
	now func() time.Time
	// registry is the authoritative plugin descriptor catalog (descriptions
	// only; Quoin never binds execution bundles).
	registry *plugins.Registry
	// enabled is the deployment-resolved enablement set (registry
	// ResolveEnabled output), frozen at process construction.
	enabled []string
	// runner owns every write transaction; business code never commits. Pure
	// reads go through runner.Reader(): fail-closed until the composition
	// layer wires a real read-only pool — never a fallback to the writer.
	runner *execution.Runner
	audit  *audit.Writer
	// startOp/resultOp/convergeOp/cancelOp are the registered write
	// operations; an undeclared mutation cannot execute.
	startOp    *execution.Operation
	resultOp   *execution.Operation
	convergeOp *execution.Operation
	cancelOp   *execution.Operation
}

// NewService constructs the production service. registry and enabled must be
// non-nil/non-empty from the boot wiring; admission fails closed without them.
func NewService(db *sql.DB, registry *plugins.Registry, enabled []string) *Service {
	service := &Service{
		db: db, now: time.Now, registry: registry, enabled: enabled,
		audit: audit.NewWriter(),
	}
	service.runner = execution.NewRunner(db, nil, service.audit)
	service.registerOperations()
	return service
}

// registerOperations declares every lifecycle mutation. Registration failures
// are declaration conflicts — programming errors that surface at startup.
func (service *Service) registerOperations() {
	register := func(op execution.Operation) *execution.Operation {
		declared, err := service.runner.Register(op)
		if err != nil {
			panic("observation: register " + op.Name + ": " + err.Error())
		}
		return declared
	}
	service.startOp = register(execution.Operation{Name: operationStart, Class: execution.ClassWrite, ObjectType: objectTypeObservationRun, Authorize: authorizeObservationStart})
	service.resultOp = register(execution.Operation{Name: operationResult, Class: execution.ClassWrite, ObjectType: objectTypeObservationRun, Authorize: requireObservationSystem})
	service.convergeOp = register(execution.Operation{Name: operationConverge, Class: execution.ClassWrite, ObjectType: objectTypeObservationRun, Authorize: requireObservationSystem})
	service.cancelOp = register(execution.Operation{Name: operationCancel, Class: execution.ClassWrite, ObjectType: objectTypeObservationRun, Authorize: requireObservationSystem})
}

// SetReader installs the composition layer's real read-only query surface
// (execution.OpenReadOnly / Database.Reader). Validation and ownership live
// in the runner: it probes PRAGMA query_only and refuses the writable pool,
// so a wiring gap fails closed instead of silently reading (and contending)
// on the writer.
func (service *Service) SetReader(reader audit.Reader) error {
	if err := service.runner.SetReader(reader); err != nil {
		return fmt.Errorf("observation: install read-only reader: %w", err)
	}
	return nil
}

// authorizeObservationStart admits the two legitimate origins of a run: the
// administrator's verified session (manual refresh) and the system principal
// (scheduler tick, enablement kick). Anything else — in particular a user
// session without the admin role or an unverified context — fails closed;
// there is no fallback that could fake either identity.
func authorizeObservationStart(ctx context.Context, tx *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	switch meta.Actor.Kind {
	case execution.PrincipalUser:
		return auth.VerifyExecutionSession(ctx, tx, "admin")
	case execution.PrincipalSystem:
		return verifySystemOrigin(meta)
	default:
		return errors.New("observation: run admission requires an administrator session or the system scheduler")
	}
}

// requireObservationSystem confines result adjudication and reconciliation to
// the system principal arriving through a trusted background source.
func requireObservationSystem(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem {
		return errors.New("observation: lifecycle operation requires the system principal")
	}
	return verifySystemOrigin(meta)
}

func verifySystemOrigin(meta execution.Metadata) error {
	if meta.Actor.ID != 0 {
		return errors.New("observation: the system principal uses id 0")
	}
	if meta.Source.Kind == execution.SourceHTTP {
		return errors.New("observation: lifecycle operations cannot arrive from the http channel")
	}
	return nil
}

// ensureSystemScope attaches the explicit system metadata for a scheduled or
// runtime-originated mutation when the caller did not bring one. An existing
// metadata scope is reused as-is: a scheduler pass shares one correlation
// across its ticks, and a caller-provided scope is never rewritten.
func ensureSystemScope(ctx context.Context, source execution.SourceKind) (context.Context, error) {
	if _, ok := execution.FromContext(ctx); ok {
		return ctx, nil
	}
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return nil, fmt.Errorf("observation: create correlation id: %w", err)
	}
	system := execution.Principal{Kind: execution.PrincipalSystem, ID: 0}
	scoped, err := execution.ReplaceMetadata(ctx, execution.Metadata{
		CorrelationID: correlation,
		Actor:         system,
		Initiator:     system,
		Source:        execution.Source{Kind: source},
	})
	if err != nil {
		return nil, fmt.Errorf("observation: restore system metadata: %w", err)
	}
	return scoped, nil
}

// UseClock pins the clock for deterministic tests.
func (service *Service) UseClock(now func() time.Time) { service.now = now }

// DB exposes the shared database for the app runtime slice wiring.
// Reader serves the app layer's read-only routing queries through the
// injected bootstrap read-only pool; unwired it fails closed.
func (service *Service) Reader() audit.Reader { return service.runner.Reader() }

func (service *Service) nowText() string { return service.now().UTC().Format(time.RFC3339Nano) }

// descriptorEnabled reports whether one plugin id is in the deployment
// enablement set.
func (service *Service) descriptorEnabled(id string) bool {
	return plugins.IsEnabled(service.enabled, id)
}

// discoverableDescriptor resolves the single enabled discover-capable plugin
// bound to one platform connection kind. A missing match is ErrNotObservable;
// two enabled plugins claiming the same connection kind is an ambiguous
// deployment that must fail closed instead of picking a winner.
func (service *Service) discoverableDescriptor(connectionKind string) (plugins.Descriptor, error) {
	var found []plugins.Descriptor
	for _, descriptor := range service.registry.Descriptors() {
		if descriptor.ConnectionKind != connectionKind || !service.descriptorEnabled(descriptor.ID) {
			continue
		}
		for _, capability := range descriptor.Capabilities {
			if capability == plugins.CapabilityDiscover {
				found = append(found, descriptor)
				break
			}
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return plugins.Descriptor{}, fmt.Errorf("%w: no enabled discover-capable plugin for connection kind %q", ErrNotObservable, connectionKind)
	default:
		ids := make([]string, 0, len(found))
		for _, descriptor := range found {
			ids = append(ids, descriptor.ID)
		}
		sort.Strings(ids)
		return plugins.Descriptor{}, fmt.Errorf("ambiguous deployment: %v all claim connection kind %q", ids, connectionKind)
	}
}

// discoverObject returns one declared object type of an enabled descriptor.
func discoverObject(descriptor plugins.Descriptor, objectType string) (plugins.DiscoverObject, error) {
	for _, object := range descriptor.DiscoverObjects {
		if object.ObjectType == objectType {
			return object, nil
		}
	}
	return plugins.DiscoverObject{}, fmt.Errorf("plugin %s does not declare discovery object %q", descriptor.ID, objectType)
}

// enabledDiscoverKinds lists the platform connection kinds this deployment
// currently observes (used by the scheduler candidate query and cancellation
// reconciliation). The list is derived per pass, never cached across plugin
// or deployment changes.
func (service *Service) enabledDiscoverKinds() []string {
	kinds := map[string]bool{}
	for _, descriptor := range service.registry.Descriptors() {
		if !service.descriptorEnabled(descriptor.ID) || descriptor.ConnectionKind == "" {
			continue
		}
		for _, capability := range descriptor.Capabilities {
			if capability == plugins.CapabilityDiscover {
				kinds[descriptor.ConnectionKind] = true
				break
			}
		}
	}
	ordered := make([]string, 0, len(kinds))
	for kind := range kinds {
		ordered = append(ordered, kind)
	}
	sort.Strings(ordered)
	return ordered
}

// RunObject is one frozen per-object-type discovery child of a Run.
type SourceObservationRunObject struct {
	ObjectType string  `json:"objectType"`
	Status     string  `json:"status"`
	GapReason  *string `json:"gapReason,omitempty"`
	AttemptID  *string `json:"attemptId,omitempty"`
	EvidenceID *string `json:"evidenceId,omitempty"`
}

// RunDetail is the SourceObservationRun DTO (docs/specs OpenAPI). It carries
// immutable run facts; objects are included for detail reads.
type SourceObservationRun struct {
	ID             string                       `json:"id"`
	ConnectionName string                       `json:"connectionName,omitempty"`
	TriggerKind    string                       `json:"triggerKind,omitempty"`
	State          string                       `json:"state"`
	RowVersion     int64                        `json:"rowVersion"`
	EvidenceAt     *string                      `json:"evidenceAt,omitempty"`
	ResultDetail   *string                      `json:"resultDetail,omitempty"`
	CreatedAt      string                       `json:"createdAt,omitempty"`
	Objects        []SourceObservationRunObject `json:"objects,omitempty"`
}

// ResourceSummary is the SourceObservedResource DTO. IdentityLabels are
// decoded from the canonical identity_key encoding (the equality authority);
// they are never stored separately so the encoding cannot drift.
type ResourceSummary struct {
	ID                      string            `json:"id"`
	ConnectionName          string            `json:"connectionName,omitempty"`
	ObjectType              string            `json:"objectType"`
	IdentityKey             string            `json:"identityKey"`
	DisplayName             *string           `json:"displayName,omitempty"`
	Labels                  map[string]string `json:"labels"`
	IdentityLabels          map[string]string `json:"identityLabels"`
	State                   string            `json:"state"`
	LastObservedAt          *string           `json:"lastObservedAt,omitempty"`
	LastSuccessfulRefreshAt *string           `json:"lastSuccessfulRefreshAt,omitempty"`
}

// ResourceState maps the persisted flags plus the refresh cadence onto the
// closed DTO vocabulary. "Stale" is derived, never persisted: an identity the
// latest complete pass no longer saw (current=0) decays from "not_observed"
// to "stale" once its last successful refresh ages past two schedule
// intervals, so a paused or removed source goes stale automatically instead
// of staying frozen. The persisted stale column remains an explicit override.
func ResourceState(current, stale int, lastSuccessfulRefreshAt string, now time.Time) string {
	switch {
	case stale == 1:
		return "stale"
	case current == 1:
		return "observed"
	case lastSuccessfulRefreshAt != "" && isStaleAt(lastSuccessfulRefreshAt, now):
		return "stale"
	default:
		return "not_observed"
	}
}

// StaleAfter is the TTL a not-currently-observed identity gets before it is
// projected as stale: two schedule intervals without any successful refresh.
const StaleAfter = 2 * DefaultIntervalSeconds

func isStaleAt(lastSuccessfulRefreshAt string, now time.Time) bool {
	last, err := time.Parse(time.RFC3339Nano, lastSuccessfulRefreshAt)
	if err != nil {
		return false
	}
	return now.Sub(last) > time.Duration(StaleAfter)*time.Second
}

// IdentityKeyEncoding is the canonical identity_key encoding: label pairs
// sorted by label name, joined with the unit separator. Equality of two
// identity keys means equality of source identity.
func IdentityKeyEncoding(identity map[string]string) string {
	names := make([]string, 0, len(identity))
	for name := range identity {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+identity[name])
	}
	return strings.Join(parts, "\x1f")
}

// DecodeIdentityKey inverts IdentityKeyEncoding for DTO projection. It never
// fails on well-formed keys produced by this package; a malformed key yields
// the partial map rather than an error so reads stay available.
func DecodeIdentityKey(identityKey string) map[string]string {
	identity := map[string]string{}
	if identityKey == "" {
		return identity
	}
	for _, part := range strings.Split(identityKey, "\x1f") {
		name, value, found := strings.Cut(part, "=")
		if found {
			identity[name] = value
		}
	}
	return identity
}

// admissionFence blocks admission inside any maintenance revision. An absent
// singleton row means normal operation (older databases materialize their row
// only when upgrade work begins).
func admissionFence(ctx context.Context, conn audit.Reader) error {
	var maintenanceActive int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE((SELECT active FROM maintenance_state WHERE id=1),0)`).Scan(&maintenanceActive); err != nil {
		return fmt.Errorf("read observation maintenance fence: %w", err)
	}
	if maintenanceActive != 0 {
		return ErrMaintenanceActive
	}
	return nil
}

// commandDigest binds one admission command to its semantic request.
func commandDigest(connectionName, triggerKind string, scheduledFor *string) string {
	return auth.DigestCommand("observation.source.start", map[string]any{"connectionName": connectionName, "triggerKind": triggerKind, "scheduledFor": scheduledFor})
}
