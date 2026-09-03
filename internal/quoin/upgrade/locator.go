package upgrade

import (
	"context"
	"database/sql"
	"fmt"
)

// Directive endpoint keys name the frozen upgrade-drain cancel operations
// (openapi x-quoin-maintenance-access: upgrade-drain). The maintenance UI is
// the only consumer; maintenance itself never calls these routes.
const (
	endpointAnalysis            = "analysis"
	endpointInvestigation       = "investigation"
	endpointInspectionRun       = "inspection_run"
	endpointKnowledgeBatch      = "knowledge_batch"
	endpointConnectionProbe     = "connection_probe"
	endpointConfigVerification  = "config_verification"
	endpointBrowserOperation    = "browser_operation"
	directiveConverge           = "converge"
)

// attemptDirective resolves the deterministic drain directive for one active
// attempt. All reads run on the caller's open projection transaction so the
// item's row version is snapshot-consistent with the checklist itself.
func attemptDirective(ctx context.Context, conn *sql.Conn, attemptID int64, scopeType string, scopeID int64, state string, parent sql.NullInt64) (string, error) {
	switch scopeType {
	case "analysis":
		var occurrenceID, rowVersion int64
		if err := conn.QueryRowContext(ctx, `SELECT occurrence_id,row_version FROM initial_analyses WHERE id=?`, scopeID).Scan(&occurrenceID, &rowVersion); err != nil {
			return "", err
		}
		return cancelDirective(endpointAnalysis, fmt.Sprintf("%d/%d", occurrenceID, scopeID), rowVersion), nil
	case "investigation":
		// The stop fence's expected row version is the attempt's own
		// (HTTP-COMMAND-005: a still-active attempt requires the current
		// row version; investigations carry no aggregate row version).
		var rowVersion int64
		if err := conn.QueryRowContext(ctx, `SELECT row_version FROM execution_attempts WHERE id=?`, attemptID).Scan(&rowVersion); err != nil {
			return "", err
		}
		return cancelDirective(endpointInvestigation, fmt.Sprintf("%d/%d", scopeID, attemptID), rowVersion), nil
	case "run", "run_check":
		var state string
		var rowVersion int64
		if err := conn.QueryRowContext(ctx, `SELECT state,row_version FROM inspection_runs WHERE id=?`, scopeID).Scan(&state, &rowVersion); err != nil {
			return "", err
		}
		if state != "Queued" && state != "Running" {
			// The parent already closed; the queued child is a durable
			// missed-schedule record, not drainable work.
			return directiveConverge, nil
		}
		return cancelDirective(endpointInspectionRun, fmt.Sprintf("%d", scopeID), rowVersion), nil
	case "knowledge_import_batch":
		var state string
		var rowVersion int64
		if err := conn.QueryRowContext(ctx, `SELECT state,row_version FROM knowledge_import_batches WHERE id=?`, scopeID).Scan(&state, &rowVersion); err != nil {
			return "", err
		}
		if state != "Processing" {
			return directiveConverge, nil
		}
		return cancelDirective(endpointKnowledgeBatch, fmt.Sprintf("%d", scopeID), rowVersion), nil
	case "connection":
		var name string
		if err := conn.QueryRowContext(ctx, `SELECT name FROM connections WHERE id=?`, scopeID).Scan(&name); err != nil {
			return "", err
		}
		// The probe cancellation fence requires a Running attempt and
		// validates the ATTEMPT row version; a Queued probe has no user cancel
		// path and converges through dispatch or reconnect only.
		if state != "Running" {
			return directiveConverge, nil
		}
		var rowVersion int64
		if err := conn.QueryRowContext(ctx, `SELECT row_version FROM execution_attempts WHERE id=?`, attemptID).Scan(&rowVersion); err != nil {
			return "", err
		}
		return cancelDirective(endpointConnectionProbe, fmt.Sprintf("%s/%d", name, attemptID), rowVersion), nil
	case "config_verification_run":
		var systemKey string
		var versionID, rowVersion int64
		if err := conn.QueryRowContext(ctx, `SELECT b.key,v.config_version_id,v.row_version FROM config_verification_runs v JOIN business_systems b ON b.id=v.business_system_id WHERE v.id=?`, scopeID).Scan(&systemKey, &versionID, &rowVersion); err != nil {
			return "", err
		}
		return cancelDirective(endpointConfigVerification, fmt.Sprintf("%s/%d/%d", systemKey, versionID, scopeID), rowVersion), nil
	case "browser_exploration":
		if !parent.Valid {
			return directiveConverge, nil
		}
		var parentScope string
		var parentScopeID int64
		var parentState string
		var grandparent sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT a.scope_type,a.scope_id,a.state,a.requested_by_tool_call_id FROM execution_attempts a JOIN tool_calls t ON t.attempt_id=a.id WHERE t.id=?`, parent.Int64).Scan(&parentScope, &parentScopeID, &parentState, &grandparent); err != nil {
			return "", err
		}
		return attemptDirective(ctx, conn, attemptID, parentScope, parentScopeID, parentState, grandparent)
	default:
		// embedding_generation and resource_refresh_run have no user cancel
		// command; their queued rows converge through the in-process sweeps
		// or a Runtime reconnect and must not fabricate a drain button.
		return directiveConverge, nil
	}
}

// operationDirective resolves the drain directive for one active browser
// operation. Manual-login-family operations cancel through their own
// endpoint; owned operations (journey, exploration, deployment verification)
// cancel through the owning domain command.
func operationDirective(ctx context.Context, conn *sql.Conn, operationID int64, kind, state string, owner sql.NullInt64) (string, error) {
	switch kind {
	case "manual_login", "authentication_probe":
		var systemKey string
		var rowVersion int64
		if err := conn.QueryRowContext(ctx, `SELECT b.key,o.row_version FROM browser_operations o JOIN browser_identities i ON i.id=o.identity_id JOIN business_systems b ON b.id=i.business_system_id WHERE o.id=?`, operationID).Scan(&systemKey, &rowVersion); err != nil {
			return "", err
		}
		return cancelDirective(endpointBrowserOperation, fmt.Sprintf("%s/%d", systemKey, operationID), rowVersion), nil
	default:
		if !owner.Valid {
			return directiveConverge, nil
		}
		var scopeType string
		var scopeID int64
		var ownerState string
		var parent sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT a.scope_type,a.scope_id,a.state,a.requested_by_tool_call_id FROM execution_attempts a WHERE a.id=?`, owner.Int64).Scan(&scopeType, &scopeID, &ownerState, &parent); err != nil {
			return "", err
		}
		return attemptDirective(ctx, conn, owner.Int64, scopeType, scopeID, ownerState, parent)
	}
}

func cancelDirective(endpoint, path string, rowVersion int64) string {
	return fmt.Sprintf("cancel:%s:%s:%d", endpoint, path, rowVersion)
}
