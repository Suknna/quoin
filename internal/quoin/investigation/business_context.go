package investigation

// Direct-chat business context is stored using the existing immutable attempt
// input-item references. This keeps the selected published configuration and
// Label Contract auditable without changing the live Investigation schema.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/config"
)

type frozenBusinessContext struct {
	BusinessSystemID int64
	SystemKey        string
	ConfigVersionID  int64
	Resources        []config.ResourceProjection
}

// resolveBusinessContext validates a user-selected key inside the write
// transaction. A blank key deliberately means general chat: it receives no
// metrics authority. Disabled systems cannot start new direct investigations.
func resolveBusinessContext(ctx context.Context, queries queryer, key string) (*frozenBusinessContext, error) {
	if key == "" {
		return nil, nil
	}
	var result frozenBusinessContext
	var declarationJSON string
	err := queries.QueryRowContext(ctx, `
		SELECT system.id, config.id, config.declaration_json
		FROM business_systems system
		JOIN business_system_config_versions config ON config.id=system.current_config_version_id
		WHERE system.key=? AND system.enabled=1 AND config.published_at IS NOT NULL
		  AND config.declaration_json IS NOT NULL`, key).
		Scan(&result.BusinessSystemID, &result.ConfigVersionID, &declarationJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBusinessSystemInvalid
	}
	if err != nil {
		return nil, err
	}
	var declaration config.BusinessSystemDocument
	if err := json.Unmarshal([]byte(declarationJSON), &declaration); err != nil {
		return nil, err
	}
	if declaration.SystemKey == "" || declaration.MetricsConnectionID <= 0 || len(declaration.Resources) == 0 {
		return nil, ErrBusinessSystemInvalid
	}
	result.SystemKey = declaration.SystemKey
	result.Resources = append([]config.ResourceProjection(nil), declaration.Resources...)
	return &result, nil
}

// businessContextForAttempt recovers exactly the pair frozen for an Attempt,
// rather than consulting mutable current business-system pointers.
func businessContextForAttempt(ctx context.Context, queries queryer, attemptID int64) (*frozenBusinessContext, error) {
	var result frozenBusinessContext
	var declarationJSON string
	err := queries.QueryRowContext(ctx, `
		SELECT config.business_system_id, config.id, config.declaration_json
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items config_item ON config_item.snapshot_id=snapshot.id
			AND config_item.business_system_config_version_id IS NOT NULL
		JOIN business_system_config_versions config ON config.id=config_item.business_system_config_version_id
		WHERE snapshot.attempt_id=? AND config.declaration_json IS NOT NULL`, attemptID).
		Scan(&result.BusinessSystemID, &result.ConfigVersionID, &declarationJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var declaration config.BusinessSystemDocument
	if err := json.Unmarshal([]byte(declarationJSON), &declaration); err != nil {
		return nil, err
	}
	if declaration.SystemKey == "" || len(declaration.Resources) == 0 {
		return nil, ErrBusinessSystemInvalid
	}
	result.SystemKey = declaration.SystemKey
	result.Resources = append([]config.ResourceProjection(nil), declaration.Resources...)
	return &result, nil
}

// businessContextForInvestigation recovers the immutable context from the
// first attempt. Every later send and retry writes that same pair into its own
// snapshot so context is not silently changed by publication races.
func businessContextForInvestigation(ctx context.Context, queries queryer, investigationID int64) (*frozenBusinessContext, error) {
	var attemptID int64
	err := queries.QueryRowContext(ctx, `
		SELECT id FROM execution_attempts
		WHERE scope_type='investigation' AND scope_id=?
		ORDER BY id LIMIT 1`, investigationID).Scan(&attemptID)
	if err != nil {
		return nil, err
	}
	return businessContextForAttempt(ctx, queries, attemptID)
}

func digestText(prefix string, id int64) string {
	sum := sha256.Sum256([]byte(prefix + strconv.FormatInt(id, 10)))
	return hex.EncodeToString(sum[:])
}

func insertBusinessContextLineage(ctx context.Context, conn *sql.Conn, snapshotID int64, after int, business frozenBusinessContext) error {
	configDigest := digestText("business-system-config-version:", business.ConfigVersionID)
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,business_system_config_version_id)
		VALUES(?,?, 'business_config', ?, ?)`, snapshotID, after+1, configDigest, business.ConfigVersionID); err != nil {
		return err
	}
	return nil
}
