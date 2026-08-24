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
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

const (
	ReadToolName = "kubernetes_read"
	ReadPurpose  = "kubernetes_read"
)

// ResolveRead resolves a business-system key or display name. Routing misses
// are normal model-visible preflight results; only a unique target with active
// mappings creates grants.
func ResolveRead(ctx context.Context, conn *sql.Conn, attemptID, toolCallID int64) (attempt.ToolResolution, error) {
	var arguments struct {
		BusinessSystem string `json:"businessSystem"`
	}
	var raw string
	if err := conn.QueryRowContext(ctx, `SELECT arguments_json FROM tool_calls WHERE id=? AND attempt_id=?`, toolCallID, attemptID).Scan(&raw); err != nil {
		return attempt.ToolResolution{}, err
	}
	if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
		return attempt.ToolResolution{}, fmt.Errorf("read kubernetes tool arguments: %w", err)
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
		if bindingRevision != rootBinding {
			return attempt.ToolResolution{}, fmt.Errorf("kubernetes connection %d root binding is stale", connectionID)
		}
		var enabled, revalidation int
		if err := conn.QueryRowContext(ctx, `SELECT enabled,revalidation_required FROM connections WHERE id=?`, connectionID).Scan(&enabled, &revalidation); err != nil {
			return attempt.ToolResolution{}, err
		}
		if enabled != 1 || revalidation != 0 {
			return attempt.ToolResolution{}, fmt.Errorf("kubernetes connection %d is unavailable", connectionID)
		}
		// attempt_connection_grants deliberately has one frozen binding per
		// (attempt, purpose, connection). A later Tool Call reuses that exact
		// snapshot and records its own association; it must never overwrite or
		// silently upgrade the earlier security decision.
		var grantID int64
		err = conn.QueryRowContext(ctx, `
			SELECT id FROM attempt_connection_grants
			WHERE attempt_id=? AND purpose=? AND business_system_id=? AND connection_id=?
				AND connection_revision_id=? AND credential_generation_id=?`,
			attemptID, ReadPurpose, systemID, connectionID, revisionID, generationID).Scan(&grantID)
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

// ValidateGrantForFulfillment closes the final credential TOCTOU window. It
// is called by connections.FulfillGrant inside its IMMEDIATE transaction and
// validates only the requested grant. Other mappings on the same Tool Call
// must neither mask a retired grant nor make a still-valid mapping unusable.
func ValidateGrantForFulfillment(ctx context.Context, conn *sql.Conn, attemptID, grantID int64) error {
	var (
		connectionID, revisionID, generationID int64
		enabled, revalidation                  int64
		currentRevision, currentGeneration     sql.NullInt64
		bindingRevision, rootBinding           int64
	)
	err := conn.QueryRowContext(ctx, `
		SELECT ag.connection_id,ag.connection_revision_id,ag.credential_generation_id,
			c.enabled,c.revalidation_required,c.current_revision_id,c.current_credential_generation_id,
			g.key_binding_revision,s.binding_revision
		FROM attempt_connection_grants ag
		JOIN tool_call_connection_grants tcg ON tcg.connection_grant_id=ag.id
		JOIN business_system_kubernetes_connections m
			ON m.business_system_id=ag.business_system_id AND m.connection_id=ag.connection_id AND m.state='Active'
		JOIN connections c ON c.id=ag.connection_id
		JOIN credential_generations g ON g.id=ag.credential_generation_id
		CROSS JOIN root_key_state s
		WHERE ag.id=? AND ag.attempt_id=? AND ag.purpose=? AND ag.created_by_tool_call_id=tcg.tool_call_id`,
		grantID, attemptID, ReadPurpose).
		Scan(&connectionID, &revisionID, &generationID, &enabled, &revalidation, &currentRevision, &currentGeneration, &bindingRevision, &rootBinding)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("kubernetes grant binding missing or inactive")
	}
	if err != nil {
		return err
	}
	if enabled != 1 || revalidation != 0 || !currentRevision.Valid || !currentGeneration.Valid || currentRevision.Int64 != revisionID || currentGeneration.Int64 != generationID || bindingRevision != rootBinding {
		return fmt.Errorf("kubernetes grant %d is no longer current", connectionID)
	}
	return nil
}
