package attempt

// Ledger replay (T12, RUNTIME-AGENT-005): a control-stream drop can lose a
// BeginModelCallAck or CompleteModelCallAck after the row committed. The
// reconnecting supervisor resends the identical physical call; these paths
// rebuild the original durable answer from the ledger instead of rejecting,
// and conflict on any divergence (never a silent re-execution).

import (
	"context"
	"database/sql"
	"fmt"
)

// replayCompleteModelCall rebuilds the original Ack payload of one sealed
// physical call: a succeeded replay must match the stored response digest
// and reconstruct the durable tool authorizations from the ledger; a failed
// replay must match its termination reason. Any divergence is a conflict,
// never a silent re-execution (RUNTIME-AGENT-005).
func (service *Service) replayCompleteModelCall(ctx context.Context, conn *sql.Conn, completion CompleteCall, status string) ([]ToolAuthorization, error) {
	if status == "cancelled" {
		return nil, fmt.Errorf("%w: call %d was cancelled", ErrLedgerDenied, completion.CallID)
	}
	if status == "succeeded" {
		var storedDigest string
		if err := conn.QueryRowContext(ctx, `SELECT response_digest FROM model_call_outputs WHERE model_call_id=? AND complete=1`, completion.CallID).Scan(&storedDigest); err != nil {
			return nil, fmt.Errorf("%w: call %d replay has no sealed output: %v", ErrLedgerDenied, completion.CallID, err)
		}
		_, wantDigest, err := CanonicalChatResponseJSON(completion.AssistantText, completion.ProposedTools)
		if err != nil {
			return nil, err
		}
		if storedDigest != wantDigest {
			return nil, fmt.Errorf("%w: replay of call %d carries a divergent response", ErrLedgerDenied, completion.CallID)
		}
		grantRows, err := conn.QueryContext(ctx, `
			SELECT t.tool_index, g.id, g.connection_revision_id, g.credential_generation_id, g.purpose
			FROM tool_calls t
			JOIN tool_call_connection_grants l ON l.tool_call_id = t.id
			JOIN attempt_connection_grants g ON g.id = l.connection_grant_id
			WHERE t.model_call_id = ? ORDER BY t.tool_index, l.ordinal`, completion.CallID)
		if err != nil {
			return nil, err
		}
		grantsByIndex := map[int64][]ToolGrant{}
		for grantRows.Next() {
			var index int64
			var grant ToolGrant
			if err := grantRows.Scan(&index, &grant.GrantID, &grant.ConnectionRevisionID, &grant.CredentialGenerationID, &grant.Purpose); err != nil {
				grantRows.Close()
				return nil, err
			}
			grantsByIndex[index] = append(grantsByIndex[index], grant)
		}
		grantRows.Close()
		toolRows, err := conn.QueryContext(ctx, `
			SELECT tool_index, id, provider_tool_call_id, failure_mode FROM tool_calls WHERE model_call_id=? ORDER BY tool_index`, completion.CallID)
		if err != nil {
			return nil, err
		}
		var authorizations []ToolAuthorization
		for toolRows.Next() {
			var index int64
			authorization := ToolAuthorization{}
			if err := toolRows.Scan(&index, &authorization.ToolCallID, &authorization.ProviderToolCallID, &authorization.FailureMode); err != nil {
				toolRows.Close()
				return nil, err
			}
			authorization.ProviderIndex = uint32(index)
			for _, grant := range grantsByIndex[index] {
				authorization.Grants = append(authorization.Grants, grant)
			}
			authorizations = append(authorizations, authorization)
		}
		toolRows.Close()
		if err := toolRows.Err(); err != nil {
			return nil, err
		}
		return authorizations, nil
	}
	// status == failed
	var storedReason string
	if err := conn.QueryRowContext(ctx, `SELECT termination_reason FROM model_calls WHERE id=?`, completion.CallID).Scan(&storedReason); err != nil {
		return nil, err
	}
	if storedReason != completion.FailureReason {
		return nil, fmt.Errorf("%w: replay of failed call %d carries reason %q, sealed %q", ErrLedgerDenied, completion.CallID, completion.FailureReason, storedReason)
	}
	return nil, nil
}
