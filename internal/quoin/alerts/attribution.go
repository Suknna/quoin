// Alert attribution (T17): an occurrence's business_system_id is decided
// once, inside the delivery transaction that creates it, by matching the
// occurrence's immutable labels against the currently active Label
// Contract's business_system_label. A missing/unknown label value routes to
// 未归属 (NULL). The value is never rewritten later: a mid-life attribution
// flip would change list filter membership without any alert_change_log
// change_type the frozen schema admits ('created'|'state_changed' only,
// DATA-ALERT-007), so attribution is a write-once fact of first observation
// and contract changes never rewrite historical attribution (CONTEXT
// 「标签契约」).

package alerts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// attributionIndex is the per-delivery snapshot of the active contract's
// business_system_label; zero allocations when no contract is active.
type attributionIndex struct {
	label string
}

// loadAttribution reads the active contract's business_system_label once per
// delivery transaction; no active contract means every item routes to
// 未归属.
func loadAttribution(ctx context.Context, conn *sql.Conn) (attributionIndex, error) {
	var contractJSON sql.NullString
	err := conn.QueryRowContext(ctx, `
		SELECT c.contract_json FROM label_contract_state s
		JOIN label_contracts c ON c.id = s.current_contract_id
		WHERE s.id = 1`).Scan(&contractJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return attributionIndex{}, nil
	}
	if err != nil {
		return attributionIndex{}, err
	}
	if !contractJSON.Valid {
		return attributionIndex{}, nil
	}
	var projection struct {
		LabelContract struct {
			BusinessSystemLabel string `json:"business_system_label"`
		} `json:"label_contract"`
	}
	if err := json.Unmarshal([]byte(contractJSON.String), &projection); err != nil {
		return attributionIndex{}, fmt.Errorf("active contract projection decode: %w", err)
	}
	return attributionIndex{label: projection.LabelContract.BusinessSystemLabel}, nil
}

// attribute resolves the business-system row id for one label set under the
// active contract; (nil, nil) means 未归属 — the label is absent, the
// contract defines no label, no contract is active, or the value matches no
// business system key.
func (index attributionIndex) attribute(ctx context.Context, conn *sql.Conn, labels map[string]string) (*int64, error) {
	if index.label == "" {
		return nil, nil
	}
	value, ok := labels[index.label]
	if !ok || value == "" {
		return nil, nil
	}
	var id int64
	err := conn.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, value).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}
