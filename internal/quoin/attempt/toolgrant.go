package attempt

// Generic per-tool-call and per-attempt connection-grant authorization
// (ARCH-INPUT-003, DATA-CONN-002, ADR-0014). Every plugin tool carries its
// declarative GrantPlan on its compiled ToolDef; the strategy below is the
// ONE implementation — read the frozen scope items of the declared role,
// resolve the declared disambiguation argument, freeze the declared grant
// purpose inside the Tool Call persistence transaction and re-validate the
// frozen pair before execution. Business flows never add per-tool resolver,
// validator or switch code: a new trusted plugin tool authorizes through the
// same machinery by declaration alone.
//
// Preserved contracts (unchanged semantics from the previous fixed-tool
// implementation): the frozen per-attempt catalog and input items are the
// only authority (historical declarations never widen a scope); sourceRef
// ambiguity stays a recoverable model-visible preflight, never a first-pick;
// the revision/generation/root-binding fence re-closes before every
// execution; grants carry only non-secret connection/revision/generation
// ids — credentials leave storage exclusively through the fenced fulfiller.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// ErrSourceUnavailable reports that no enabled source connection exists when
// an authorization freezes (RUNTIME-AGENT-005: an unresolvable tool route
// fails the whole model call; a collection freeze refuses the run).
var ErrSourceUnavailable = errors.New("no enabled source connection")

// ErrGrantNotCurrent reports a frozen grant whose connection pair or root
// binding lost currency after the grant was created (DATA-CONN-002: the
// execution authorization re-check failed).
var ErrGrantNotCurrent = errors.New("frozen grant is no longer current")

// Recoverable preflight codes (the frozen tool_calls.preflight_error_code
// vocabulary): routing misses return these as model-visible Tool Results
// instead of failing the attempt, so the model can ask the user or retry
// with an explicit sourceRef (ADR-0004: ambiguity is never resolved by
// picking the first source or querying all of them).
const (
	PreflightTargetNotFound  = "target_not_found"
	PreflightTargetAmbiguous = "target_ambiguous"
	PreflightNoMapping       = "no_mapping"
)

// frozenSource is one source binding frozen as an attempt input item when
// the attempt was created: the exact connection revision that was current
// and enabled at freeze time.
type frozenSource struct {
	ConnectionID int64
	RevisionID   int64
	Name         string
}

// frozenScopeSourceItems loads the attempt's frozen source items of one
// declared role, joined to their stable connection names.
func frozenScopeSourceItems(ctx context.Context, conn execution.Executor, attemptID int64, itemRole string) ([]frozenSource, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT c.id, item.connection_revision_id, c.name
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items item ON item.snapshot_id=snapshot.id AND item.connection_revision_id IS NOT NULL
		JOIN connection_revisions r ON r.id=item.connection_revision_id
		JOIN connections c ON c.id=r.connection_id
		WHERE snapshot.attempt_id=? AND item.item_role=?
		ORDER BY c.name`, attemptID, itemRole)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sources []frozenSource
	for rows.Next() {
		var source frozenSource
		if err := rows.Scan(&source.ConnectionID, &source.RevisionID, &source.Name); err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

// sourcePreflight renders a recoverable routing miss.
func sourcePreflight(code string, detail string) ToolResolution {
	return ToolResolution{PreflightCode: code, PreflightDetail: detail}
}

// namesOf renders the bounded candidate list carried by preflight details.
func namesOf(sources []frozenSource) string {
	names := make([]string, 0, len(sources))
	for _, source := range sources {
		names = append(names, source.Name)
	}
	return strings.Join(names, "、")
}

// currentConnectionPair re-reads the connection's current binding and root
// state inside the caller's transaction. enabled=false reports an admin
// disable/revalidation without an error so callers can preflight.
func currentConnectionPair(ctx context.Context, conn execution.Executor, connectionID int64) (revisionID, generationID int64, enabled bool, err error) {
	var (
		revalidation int
		bindingRev   int64
		rootBinding  int64
	)
	err = conn.QueryRowContext(ctx, `
		SELECT c.current_revision_id, c.current_credential_generation_id, c.enabled, c.revalidation_required,
		       g.key_binding_revision, s.binding_revision
		FROM connections c
		JOIN credential_generations g ON g.id=c.current_credential_generation_id
		CROSS JOIN root_key_state s
		WHERE c.id=?`, connectionID).
		Scan(&revisionID, &generationID, &enabled, &revalidation, &bindingRev, &rootBinding)
	if err != nil {
		return 0, 0, false, err
	}
	if !enabled || revalidation != 0 {
		return 0, 0, false, nil
	}
	if bindingRev != rootBinding {
		return 0, 0, false, fmt.Errorf("%w: credential root binding %d does not match %d", ErrGrantNotCurrent, bindingRev, rootBinding)
	}
	return revisionID, generationID, true, nil
}

// ResolveConnectionGrant authorizes one proposed connection-grant tool call
// inside the Tool Call persistence transaction, exactly as the tool's
// declared GrantPlan prescribes. Authority derives solely from the attempt's
// frozen source items of the declared role: one per admin-enabled source
// connection at creation. The model names the source explicitly through the
// declared disambiguation argument whenever the frozen list is ambiguous;
// zero or several candidates without a name is a recoverable preflight
// result, never a silent first-pick.
func ResolveConnectionGrant(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64, tool ToolDef) (ToolResolution, error) {
	plan := tool.Grant
	if plan == nil {
		return ToolResolution{}, fmt.Errorf("tool %s declares no authorization plan", tool.Name)
	}
	var argumentsJSON string
	if err := conn.QueryRowContext(ctx, `SELECT arguments_json FROM tool_calls WHERE id=? AND attempt_id=?`, toolCallID, attemptID).Scan(&argumentsJSON); err != nil {
		return ToolResolution{}, err
	}
	proposed := []byte(argumentsJSON)
	// Defense in depth: even a raw low-level proposal (frozen catalog
	// drift, hand-inserted rows) must meet the same argument contract the
	// offered catalog enforces at ingress; an out-of-contract proposal is
	// a recoverable no_mapping, never an authorized grant — removed
	// vocabularies can never route again.
	if err := ValidateToolArguments(tool, proposed); err != nil {
		return sourcePreflight(PreflightNoMapping, err.Error()), nil
	}
	var arguments map[string]any
	if err := json.Unmarshal(proposed, &arguments); err != nil {
		return ToolResolution{}, fmt.Errorf("tool %s proposal is not a JSON object: %w", tool.Name, err)
	}
	sourceRef := ""
	if plan.SourceRefArgument != "" {
		if value, ok := arguments[plan.SourceRefArgument].(string); ok {
			sourceRef = value
		}
	}
	sources, err := frozenScopeSourceItems(ctx, conn, attemptID, plan.SourceItemRole)
	if err != nil {
		return ToolResolution{}, err
	}
	if sourceRef != "" {
		var matched []frozenSource
		for _, source := range sources {
			if source.Name == sourceRef {
				matched = append(matched, source)
			}
		}
		if len(matched) == 0 {
			if len(sources) == 0 {
				return sourcePreflight(PreflightNoMapping, "本次分析没有已授权的来源接入；请管理员先启用相应来源接入。"), nil
			}
			return sourcePreflight(PreflightTargetNotFound, "未找到该来源接入，可用来源："+namesOf(sources)+"。"), nil
		}
		sources = matched
	} else {
		switch len(sources) {
		case 0:
			return sourcePreflight(PreflightNoMapping, "本次分析没有已授权的来源接入；请管理员先启用相应来源接入。"), nil
		case 1:
		default:
			refArgument := "sourceRef"
			if plan.SourceRefArgument != "" {
				refArgument = plan.SourceRefArgument
			}
			return sourcePreflight(PreflightTargetAmbiguous, "存在多个已授权的来源接入，请用 "+refArgument+" 明确指定："+namesOf(sources)+"。"), nil
		}
	}
	selected := sources[0]
	// The grant must close onto the exact frozen revision while that
	// revision is still the enabled current pair; anything else is a
	// recoverable routing miss (a fresh analysis re-freezes sources).
	revisionID, _, enabled, err := currentConnectionPair(ctx, conn, selected.ConnectionID)
	if errors.Is(err, sql.ErrNoRows) {
		return ToolResolution{}, fmt.Errorf("%w: source connection disappeared", ErrSourceUnavailable)
	}
	if err != nil {
		return ToolResolution{}, err
	}
	if !enabled || revisionID != selected.RevisionID {
		return sourcePreflight(PreflightNoMapping, fmt.Sprintf("来源接入 %q 已停用或已轮换；请管理员重新启用后发起新的分析。", selected.Name)), nil
	}
	// The frozen execution request stamps the resolved source into the
	// declared disambiguation argument; only a real grant resolution
	// freezes it (a preflight carries none by design).
	if plan.FreezeExecutionArguments {
		execution := make(map[string]any, len(arguments)+1)
		for key, value := range arguments {
			execution[key] = value
		}
		if plan.SourceRefArgument != "" {
			execution[plan.SourceRefArgument] = selected.Name
		}
		canonical, err := json.Marshal(execution)
		if err != nil {
			return ToolResolution{}, err
		}
		if err := freezeToolCallExecution(ctx, conn, toolCallID, canonical); err != nil {
			return ToolResolution{}, err
		}
	}
	grant, err := freezeAttemptConnectionGrant(ctx, conn, attemptID, selected.ConnectionID, plan.Purpose, &toolCallID)
	if err != nil {
		return ToolResolution{}, err
	}
	if err := bindToolCallGrant(ctx, conn, toolCallID, grant.GrantID); err != nil {
		return ToolResolution{}, err
	}
	return ToolResolution{Grants: []ToolGrant{grant}}, nil
}

// ValidateConnectionGrantForExecution re-checks the frozen binding before a
// pending connection-grant tool call may begin executing (DATA-CONN-002:
// the execution authorization transaction re-reads the connection state; a
// disable, rotation or root rebind committed first refuses execution).
func ValidateConnectionGrantForExecution(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64, tool ToolDef) error {
	plan := tool.Grant
	if plan == nil {
		return fmt.Errorf("tool %s declares no authorization plan", tool.Name)
	}
	var (
		grantRevisionID, grantGenerationID int64
		enabled, revalidation              int
		currentRevisionID, currentGenID    sql.NullInt64
		bindingRevision, rootBinding       int64
	)
	err := conn.QueryRowContext(ctx, `
		SELECT ag.connection_revision_id, ag.credential_generation_id,
		       c.enabled, c.revalidation_required, c.current_revision_id, c.current_credential_generation_id,
		       g.key_binding_revision, s.binding_revision
		FROM tool_call_connection_grants tcg
		JOIN attempt_connection_grants ag ON ag.id = tcg.connection_grant_id
		JOIN connections c ON c.id = ag.connection_id
		JOIN credential_generations g ON g.id = ag.credential_generation_id
		CROSS JOIN root_key_state s
		WHERE tcg.tool_call_id = ? AND ag.attempt_id = ? AND ag.purpose = ?`,
		toolCallID, attemptID, plan.Purpose,
	).Scan(&grantRevisionID, &grantGenerationID, &enabled, &revalidation,
		&currentRevisionID, &currentGenID, &bindingRevision, &rootBinding)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: grant binding missing", ErrGrantNotCurrent)
	}
	if err != nil {
		return err
	}
	if enabled != 1 || revalidation != 0 {
		return fmt.Errorf("%w: connection disabled or pending revalidation", ErrGrantNotCurrent)
	}
	if !currentRevisionID.Valid || !currentGenID.Valid ||
		currentRevisionID.Int64 != grantRevisionID || currentGenID.Int64 != grantGenerationID {
		return fmt.Errorf("%w: connection pair rotated since the grant", ErrGrantNotCurrent)
	}
	if bindingRevision != rootBinding {
		return fmt.Errorf("%w: credential root binding %d does not match %d", ErrGrantNotCurrent, bindingRevision, rootBinding)
	}
	return nil
}

// freezeToolCallExecution persists the canonical execution request of one
// resolved tool call. The dispatch carries exactly these bytes.
func freezeToolCallExecution(ctx context.Context, conn execution.Executor, toolCallID int64, executionJSON []byte) error {
	sum := sha256Sum(executionJSON)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO tool_call_execution_inputs(tool_call_id,arguments_json,arguments_digest,created_at)
		VALUES(?,?,?,?)`, toolCallID, string(executionJSON), hex.EncodeToString(sum[:]), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// freezeAttemptConnectionGrant reuses the attempt's identical grant row or
// inserts one, freezing the exact (connection, revision, generation) triple
// under the declared purpose. One binding per attempt and connection
// authorizes every identical call, while each Tool Call keeps its own
// auditable association row（ADR-0004 来源级授权）. toolCallID, when
// non-nil, stamps the originating Tool Call on the inserted grant.
func freezeAttemptConnectionGrant(ctx context.Context, conn execution.Executor, attemptID, connectionID int64, purpose string, toolCallID *int64) (ToolGrant, error) {
	revisionID, generationID, enabled, err := currentConnectionPair(ctx, conn, connectionID)
	if errors.Is(err, sql.ErrNoRows) {
		return ToolGrant{}, fmt.Errorf("%w: create or enable a source connection first", ErrSourceUnavailable)
	}
	if err != nil {
		return ToolGrant{}, err
	}
	if !enabled {
		return ToolGrant{}, fmt.Errorf("%w: connection disabled or pending revalidation", ErrGrantNotCurrent)
	}
	var grantID int64
	err = conn.QueryRowContext(ctx, `
		SELECT id FROM attempt_connection_grants
		WHERE attempt_id=? AND purpose=? AND connection_id=? AND connection_revision_id=? AND credential_generation_id=?`,
		attemptID, purpose, connectionID, revisionID, generationID).Scan(&grantID)
	if errors.Is(err, sql.ErrNoRows) {
		insert, insertErr := conn.ExecContext(ctx, `
			INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,
				credential_generation_id,created_by_tool_call_id,created_at)
			VALUES(?,?,?,?,?,?,?)`,
			attemptID, purpose, connectionID, revisionID, generationID, toolCallID, time.Now().UTC().Format(time.RFC3339Nano))
		if insertErr != nil {
			return ToolGrant{}, insertErr
		}
		grantID, insertErr = insert.LastInsertId()
		if insertErr != nil {
			return ToolGrant{}, insertErr
		}
	} else if err != nil {
		return ToolGrant{}, err
	}
	return ToolGrant{GrantID: grantID, ConnectionRevisionID: revisionID, CredentialGenerationID: generationID, Purpose: purpose}, nil
}

// bindToolCallGrant associates one Tool Call with its frozen grant.
func bindToolCallGrant(ctx context.Context, conn execution.Executor, toolCallID, grantID int64) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO tool_call_connection_grants(tool_call_id,connection_grant_id,ordinal) VALUES(?,?,0)`, toolCallID, grantID)
	return err
}

// FreezeConnectionGrant freezes one exact enabled source connection for a
// deterministic collection attempt (inspection run_check children, bounded
// observation discovery). The declaration locator is mandatory: historical
// attempts retain their already-created immutable grants rather than
// re-resolving a global default. conn is the execution.Executor surface, so
// grant freezing composes both inside plain caller connections and inside
// the shared execution runner's guarded transaction (ADR-0006).
func FreezeConnectionGrant(ctx context.Context, conn execution.Executor, attemptID, connectionID int64, purpose string) (ToolGrant, error) {
	return freezeAttemptConnectionGrant(ctx, conn, attemptID, connectionID, purpose, nil)
}

// ValidateConnectionGrantRowCurrent re-checks one frozen grant row just
// before its credential fulfillment: the connection must still be enabled,
// the frozen revision/generation pair must still be the current pointer and
// the credential root binding must not have drifted. A connection disable,
// rotation or root-key rebind committed first wins the race.
func ValidateConnectionGrantRowCurrent(ctx context.Context, conn execution.Executor, grantID int64) error {
	return validateGrantBinding(ctx, conn, `ag.id=?`, grantID)
}

// ValidateAttemptConnectionGrantsCurrent re-checks every grant of one
// attempt (dispatch guard for collection children: a queued collection
// whose connection was disabled or rotated after freezing must not acquire
// credentials or start platform calls).
func ValidateAttemptConnectionGrantsCurrent(ctx context.Context, conn execution.Executor, attemptID int64) error {
	rows, err := conn.QueryContext(ctx, `SELECT id FROM attempt_connection_grants WHERE attempt_id=? ORDER BY id`, attemptID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := validateGrantBinding(ctx, conn, `ag.id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

// validateGrantBinding runs the frozen-binding re-check for grants selected
// by the given predicate (one row expected).
func validateGrantBinding(ctx context.Context, conn execution.Executor, predicate string, arg int64) error {
	var (
		grantRevisionID, grantGenerationID int64
		enabled, revalidation              int
		currentRevisionID, currentGenID    sql.NullInt64
		bindingRevision, rootBinding       int64
	)
	err := conn.QueryRowContext(ctx, `
		SELECT ag.connection_revision_id, ag.credential_generation_id,
		       c.enabled, c.revalidation_required, c.current_revision_id, c.current_credential_generation_id,
		       g.key_binding_revision, s.binding_revision
		FROM attempt_connection_grants ag
		JOIN connections c ON c.id = ag.connection_id
		JOIN credential_generations g ON g.id = ag.credential_generation_id
		CROSS JOIN root_key_state s
		WHERE `+predicate, arg).
		Scan(&grantRevisionID, &grantGenerationID, &enabled, &revalidation,
			&currentRevisionID, &currentGenID, &bindingRevision, &rootBinding)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: grant binding missing", ErrGrantNotCurrent)
	}
	if err != nil {
		return err
	}
	switch {
	case enabled != 1 || revalidation != 0:
		return fmt.Errorf("%w: connection disabled or pending revalidation", ErrGrantNotCurrent)
	case !currentRevisionID.Valid || !currentGenID.Valid ||
		currentRevisionID.Int64 != grantRevisionID || currentGenID.Int64 != grantGenerationID:
		return fmt.Errorf("%w: frozen revision/generation pair no longer current", ErrGrantNotCurrent)
	case bindingRevision != rootBinding:
		return fmt.Errorf("%w: credential root binding drifted", ErrGrantNotCurrent)
	}
	return nil
}

// SourceScope is one frozen integration scope of the agent input: the
// attempt input item role whose connection set an authorization resolves
// against, and the connection kinds (registry-declared plugin connection
// kinds) rendered and frozen under that role.
type SourceScope struct {
	Role            string
	ConnectionTypes []string
}

// IntegrationKind is the model-visible integration kind rendered for the
// scope (metrics_source → metrics). The suffix convention is the declared
// role's; no host-side type map exists.
func (scope SourceScope) IntegrationKind() string {
	return strings.TrimSuffix(scope.Role, "_source")
}

// SourceScopeTable is the deterministic lookup of a derived source scope
// plan: the union connection-type list for one bounded query plus the
// per-connection-type role map and per-role rendered integration kind.
type SourceScopeTable struct {
	types      []string
	roleByType map[string]string
	kindByRole map[string]string
}

// NewSourceScopeTable indexes one derived scope plan. Types are sorted so
// the IN-list and the ORDER BY stay deterministic.
func NewSourceScopeTable(scopes []SourceScope) SourceScopeTable {
	table := SourceScopeTable{roleByType: map[string]string{}, kindByRole: map[string]string{}}
	for _, scope := range scopes {
		table.kindByRole[scope.Role] = scope.IntegrationKind()
		for _, kind := range scope.ConnectionTypes {
			if _, exists := table.roleByType[kind]; !exists {
				table.types = append(table.types, kind)
			}
			table.roleByType[kind] = scope.Role
		}
	}
	sort.Strings(table.types)
	return table
}

// Types returns the union connection-type list (sorted).
func (table SourceScopeTable) Types() []string { return table.types }

// RoleOf resolves the frozen source item role of one connection type; an
// empty result means the type is outside every declared scope.
func (table SourceScopeTable) RoleOf(connectionType string) string {
	return table.roleByType[connectionType]
}

// KindOf resolves the rendered integration kind of one frozen item role.
func (table SourceScopeTable) KindOf(role string) string { return table.kindByRole[role] }

// Placeholders renders the SQLite IN-list placeholders for Types().
func (table SourceScopeTable) Placeholders() string {
	if len(table.types) == 0 {
		return "NULL"
	}
	return strings.TrimRight(strings.Repeat("?,", len(table.types)), ",")
}

// SourceScopes derives the grant-scoped integration freeze plan from a
// plugin registry under a deployment enablement set: for every enabled
// plugin contributing connection-grant tools, the plugin's declared
// connection kind joins the declared source item role. A new trusted plugin
// therefore extends the frozen source authority by declaration alone; hosts
// only assemble the registry and push the derived plan.
func SourceScopes(registry *plugins.Registry, enabled []string) []SourceScope {
	if registry == nil {
		registry = plugins.Default()
	}
	byRole := map[string]map[string]bool{}
	var roles []string
	for _, plugin := range registry.Plugins() {
		if plugin.ConnectionKind == "" || plugin.Tools == nil || !plugins.IsEnabled(enabled, plugin.ID) {
			continue
		}
		for _, entry := range plugin.Tools.Tools() {
			if entry.Definition.Grant == nil {
				continue
			}
			role := entry.Definition.Grant.SourceItemRole
			if byRole[role] == nil {
				byRole[role] = map[string]bool{}
				roles = append(roles, role)
			}
			byRole[role][plugin.ConnectionKind] = true
		}
	}
	sort.Strings(roles)
	scopes := make([]SourceScope, 0, len(roles))
	for _, role := range roles {
		types := make([]string, 0, len(byRole[role]))
		for kind := range byRole[role] {
			types = append(types, kind)
		}
		sort.Strings(types)
		scopes = append(scopes, SourceScope{Role: role, ConnectionTypes: types})
	}
	return scopes
}

// sha256Sum is the shared digest helper of the grant machinery.
func sha256Sum(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}
