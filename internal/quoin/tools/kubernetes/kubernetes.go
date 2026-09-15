// Package kubernetes owns deterministic Business System routing for the fixed
// kubernetes_read observation tool. Connection identity never appears in its
// model-facing arguments: Quoin resolves the domain target and freezes every
// active mapping in the Tool Call transaction.
package kubernetes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

const (
	ReadToolName = "kubernetes_read"
	ReadPurpose  = "kubernetes_read"
)

// ResolveRead routes one proposed kubernetes_read call. Two disjoint modes:
//
//   - Business view (businessSystem argument): resolves the system and
//     freezes one grant per active mapping — the declaration view narrows.
//   - Source level (ADR-0004, blank businessSystem): the frozen
//     kubernetes_source items are the authority; sourceRef names the
//     connection explicitly and ambiguity stays a recoverable preflight
//     result instead of a first-pick or a fan-out.
func ResolveRead(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64) (attempt.ToolResolution, error) {
	var arguments struct {
		BusinessSystem string `json:"businessSystem"`
		SourceRef      string `json:"sourceRef"`
	}
	var raw string
	if err := conn.QueryRowContext(ctx, `SELECT arguments_json FROM tool_calls WHERE id=? AND attempt_id=?`, toolCallID, attemptID).Scan(&raw); err != nil {
		return attempt.ToolResolution{}, err
	}
	if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
		return attempt.ToolResolution{}, fmt.Errorf("read kubernetes tool arguments: %w", err)
	}
	if strings.TrimSpace(arguments.BusinessSystem) == "" {
		return resolveSourceRead(ctx, conn, attemptID, toolCallID, arguments.SourceRef)
	}
	// A stable key is the unambiguous authority. Only fall back to a display
	// name when no key exists; another system's display name must never make a
	// valid key unusable.
	var systemID int64
	err := conn.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, arguments.BusinessSystem).Scan(&systemID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return attempt.ToolResolution{}, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		rows, queryErr := conn.QueryContext(ctx, `SELECT id FROM business_systems WHERE display_name=? ORDER BY id`, arguments.BusinessSystem)
		if queryErr != nil {
			return attempt.ToolResolution{}, queryErr
		}
		defer rows.Close()
		var systems []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return attempt.ToolResolution{}, err
			}
			systems = append(systems, id)
		}
		if err := rows.Err(); err != nil {
			return attempt.ToolResolution{}, err
		}
		switch len(systems) {
		case 0:
			return attempt.ToolResolution{PreflightCode: "target_not_found", PreflightDetail: "未找到该业务系统，请提供业务系统 key 或准确名称。"}, nil
		case 1:
			systemID = systems[0]
		default:
			return attempt.ToolResolution{PreflightCode: "target_ambiguous", PreflightDetail: "该业务系统名称对应多个对象，请提供业务系统 key。"}, nil
		}
	}
	mappingRows, err := conn.QueryContext(ctx, `
		SELECT c.id,c.current_revision_id,c.current_credential_generation_id,g.key_binding_revision,s.binding_revision
		FROM business_system_kubernetes_connections m
		JOIN connections c ON c.id=m.connection_id
		JOIN credential_generations g ON g.id=c.current_credential_generation_id
		CROSS JOIN root_key_state s
		WHERE m.business_system_id=? AND m.state='Active'
		ORDER BY c.id`, systemID)
	if err != nil {
		return attempt.ToolResolution{}, err
	}
	defer mappingRows.Close()
	var grants []attempt.ToolGrant
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for mappingRows.Next() {
		var connectionID, revisionID, generationID, bindingRevision, rootBinding int64
		if err := mappingRows.Scan(&connectionID, &revisionID, &generationID, &bindingRevision, &rootBinding); err != nil {
			return attempt.ToolResolution{}, err
		}
		// Availability is deliberately not a routing prerequisite. Every active
		// mapping gets its own frozen grant, then FulfillGrant fences that grant
		// independently so one stale sibling becomes a model-visible partial
		// failure rather than preventing healthy mappings from executing.
		_ = bindingRevision
		_ = rootBinding
		// attempt_connection_grants deliberately has one frozen binding per
		// (attempt, purpose, connection). A later Tool Call reuses that exact
		// snapshot and records its own association; it must never overwrite or
		// silently upgrade the earlier security decision.
		var grantID, frozenRevisionID, frozenGenerationID int64
		err = conn.QueryRowContext(ctx, `
			SELECT id,connection_revision_id,credential_generation_id FROM attempt_connection_grants
			WHERE attempt_id=? AND purpose=? AND business_system_id=? AND connection_id=?`,
			attemptID, ReadPurpose, systemID, connectionID).Scan(&grantID, &frozenRevisionID, &frozenGenerationID)
		if errors.Is(err, sql.ErrNoRows) {
			insert, insertErr := conn.ExecContext(ctx, `
				INSERT INTO attempt_connection_grants(attempt_id,purpose,business_system_id,connection_id,connection_revision_id,credential_generation_id,created_by_tool_call_id,created_at)
				VALUES(?,?,?,?,?,?,?,?)`, attemptID, ReadPurpose, systemID, connectionID, revisionID, generationID, toolCallID, now)
			if insertErr != nil {
				return attempt.ToolResolution{}, insertErr
			}
			grantID, insertErr = insert.LastInsertId()
			if insertErr != nil {
				return attempt.ToolResolution{}, insertErr
			}
		} else if err != nil {
			return attempt.ToolResolution{}, err
		} else {
			revisionID, generationID = frozenRevisionID, frozenGenerationID
		}
		ordinal := len(grants)
		if _, err := conn.ExecContext(ctx, `INSERT INTO tool_call_connection_grants(tool_call_id,connection_grant_id,ordinal) VALUES(?,?,?)`, toolCallID, grantID, ordinal); err != nil {
			return attempt.ToolResolution{}, err
		}
		grants = append(grants, attempt.ToolGrant{GrantID: grantID, ConnectionRevisionID: revisionID, CredentialGenerationID: generationID, Purpose: ReadPurpose})
	}
	if err := mappingRows.Err(); err != nil {
		return attempt.ToolResolution{}, err
	}
	if len(grants) == 0 {
		return attempt.ToolResolution{PreflightCode: "no_mapping", PreflightDetail: "该业务系统尚未绑定可用的 Kubernetes 连接。"}, nil
	}
	return attempt.ToolResolution{Grants: grants}, nil
}

// frozenSourceItem is one source binding frozen as a kubernetes_source
// input item at attempt creation: the exact connection revision that was
// current and enabled at freeze time.
type frozenSourceItem struct {
	ConnectionID int64
	RevisionID   int64
	Name         string
}

// resolveSourceRead authorizes the source-level path: the attempt's frozen
// kubernetes_source items define the whole read-only scope, and an omitted
// sourceRef resolves only when exactly one candidate exists (ADR-0004:
// ambiguity is never silently resolved).
func resolveSourceRead(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64, sourceRef string) (attempt.ToolResolution, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT c.id, item.connection_revision_id, c.name
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items item ON item.snapshot_id=snapshot.id
			AND item.item_role='kubernetes_source' AND item.connection_revision_id IS NOT NULL
		JOIN connection_revisions r ON r.id=item.connection_revision_id
		JOIN connections c ON c.id=r.connection_id
		WHERE snapshot.attempt_id=?
		ORDER BY c.name`, attemptID)
	if err != nil {
		return attempt.ToolResolution{}, err
	}
	defer rows.Close()
	var sources []frozenSourceItem
	for rows.Next() {
		var source frozenSourceItem
		if err := rows.Scan(&source.ConnectionID, &source.RevisionID, &source.Name); err != nil {
			return attempt.ToolResolution{}, err
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return attempt.ToolResolution{}, err
	}
	names := make([]string, 0, len(sources))
	for _, source := range sources {
		names = append(names, source.Name)
	}
	if sourceRef != "" {
		var matched []frozenSourceItem
		for _, source := range sources {
			if source.Name == sourceRef {
				matched = append(matched, source)
			}
		}
		if len(matched) == 0 {
			if len(sources) == 0 {
				return attempt.ToolResolution{PreflightCode: "no_mapping", PreflightDetail: "本次对话没有已授权的 Kubernetes 来源；请管理员先启用 Kubernetes 接入。"}, nil
			}
			return attempt.ToolResolution{PreflightCode: "target_not_found", PreflightDetail: "未找到该 Kubernetes 来源，可用来源：" + strings.Join(names, "、") + "。"}, nil
		}
		sources = matched
	} else {
		switch len(sources) {
		case 0:
			return attempt.ToolResolution{PreflightCode: "no_mapping", PreflightDetail: "本次对话没有已授权的 Kubernetes 来源；请管理员先启用 Kubernetes 接入。"}, nil
		case 1:
		default:
			return attempt.ToolResolution{PreflightCode: "target_ambiguous", PreflightDetail: "存在多个已授权的 Kubernetes 来源，请用 sourceRef 明确指定：" + strings.Join(names, "、") + "。"}, nil
		}
	}
	selected := sources[0]
	// The grant must close onto the exact frozen revision while that revision
	// is still the enabled current pair (the SQL source trigger enforces the
	// same closure); drift is a recoverable routing miss, not a failure.
	var currentRevision, currentGeneration int64
	var enabled, revalidation int
	var bindingRevision, rootBinding int64
	err = conn.QueryRowContext(ctx, `
		SELECT c.current_revision_id, c.current_credential_generation_id, c.enabled, c.revalidation_required,
		       g.key_binding_revision, s.binding_revision
		FROM connections c
		JOIN credential_generations g ON g.id=c.current_credential_generation_id
		CROSS JOIN root_key_state s
		WHERE c.id=?`, selected.ConnectionID).
		Scan(&currentRevision, &currentGeneration, &enabled, &revalidation, &bindingRevision, &rootBinding)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt.ToolResolution{}, fmt.Errorf("kubernetes source connection disappeared")
	}
	if err != nil {
		return attempt.ToolResolution{}, err
	}
	if enabled != 1 || revalidation != 0 || currentRevision != selected.RevisionID {
		return attempt.ToolResolution{PreflightCode: "no_mapping", PreflightDetail: fmt.Sprintf("Kubernetes 来源 %q 已停用或已轮换；请管理员重新启用后发起新的对话。", selected.Name)}, nil
	}
	if bindingRevision != rootBinding {
		return attempt.ToolResolution{}, fmt.Errorf("kubernetes credential root binding %d does not match %d", bindingRevision, rootBinding)
	}
	// attempt_connection_grants deliberately has one frozen binding per
	// (attempt, purpose, connection). A later Tool Call reuses that exact
	// snapshot and records its own association; it must never overwrite or
	// silently upgrade the earlier security decision.
	var grantID, frozenRevisionID, frozenGenerationID int64
	err = conn.QueryRowContext(ctx, `
		SELECT id,connection_revision_id,credential_generation_id FROM attempt_connection_grants
		WHERE attempt_id=? AND purpose=? AND business_system_id IS NULL AND connection_id=?`,
		attemptID, ReadPurpose, selected.ConnectionID).Scan(&grantID, &frozenRevisionID, &frozenGenerationID)
	if errors.Is(err, sql.ErrNoRows) {
		insert, insertErr := conn.ExecContext(ctx, `
			INSERT INTO attempt_connection_grants(attempt_id,purpose,business_system_id,connection_id,connection_revision_id,credential_generation_id,created_by_tool_call_id,created_at)
			VALUES(?,?,NULL,?,?,?,?,?)`, attemptID, ReadPurpose, selected.ConnectionID, selected.RevisionID, currentGeneration, toolCallID, time.Now().UTC().Format(time.RFC3339Nano))
		if insertErr != nil {
			return attempt.ToolResolution{}, insertErr
		}
		grantID, insertErr = insert.LastInsertId()
		if insertErr != nil {
			return attempt.ToolResolution{}, insertErr
		}
	} else if err != nil {
		return attempt.ToolResolution{}, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO tool_call_connection_grants(tool_call_id,connection_grant_id,ordinal) VALUES(?,?,0)`, toolCallID, grantID); err != nil {
		return attempt.ToolResolution{}, err
	}
	return attempt.ToolResolution{Grants: []attempt.ToolGrant{{GrantID: grantID, ConnectionRevisionID: selected.RevisionID, CredentialGenerationID: currentGeneration, Purpose: ReadPurpose}}}, nil
}

// ValidateGrantForFulfillment closes the final credential TOCTOU window. It
// is called by connections.FulfillGrant inside its IMMEDIATE transaction and
// validates only the requested grant. Business-view grants additionally
// require their mapping to stay Active; source-level grants (no business
// system) fence purely on the connection pair and root binding.
func ValidateGrantForFulfillment(ctx context.Context, conn execution.Executor, attemptID, grantID int64) error {
	var (
		connectionID, revisionID, generationID int64
		businessSystemID                       sql.NullInt64
		enabled, revalidation                  int64
		currentRevision, currentGeneration     sql.NullInt64
		bindingRevision, rootBinding           int64
	)
	err := conn.QueryRowContext(ctx, `
		SELECT ag.connection_id,ag.connection_revision_id,ag.credential_generation_id,ag.business_system_id,
			c.enabled,c.revalidation_required,c.current_revision_id,c.current_credential_generation_id,
			g.key_binding_revision,s.binding_revision
		FROM attempt_connection_grants ag
		JOIN tool_call_connection_grants tcg ON tcg.connection_grant_id=ag.id
		JOIN connections c ON c.id=ag.connection_id
		JOIN credential_generations g ON g.id=ag.credential_generation_id
		CROSS JOIN root_key_state s
		WHERE ag.id=? AND ag.attempt_id=? AND ag.purpose=?`,
		grantID, attemptID, ReadPurpose).
		Scan(&connectionID, &revisionID, &generationID, &businessSystemID,
			&enabled, &revalidation, &currentRevision, &currentGeneration, &bindingRevision, &rootBinding)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("kubernetes grant binding missing or inactive")
	}
	if err != nil {
		return err
	}
	// The business view only narrows: a declared grant dies with its mapping.
	if businessSystemID.Valid {
		var mappings int
		if err := conn.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM business_system_kubernetes_connections
			WHERE business_system_id=? AND connection_id=? AND state='Active'`, businessSystemID.Int64, connectionID).Scan(&mappings); err != nil {
			return err
		}
		if mappings == 0 {
			return fmt.Errorf("kubernetes grant %d mapping is no longer active", connectionID)
		}
	}
	if enabled != 1 || revalidation != 0 || !currentRevision.Valid || !currentGeneration.Valid || currentRevision.Int64 != revisionID || currentGeneration.Int64 != generationID || bindingRevision != rootBinding {
		return fmt.Errorf("kubernetes grant %d is no longer current", connectionID)
	}
	return nil
}
