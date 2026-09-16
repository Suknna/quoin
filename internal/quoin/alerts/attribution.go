// Alert attribution is decided once, while the first immutable delivery item
// creates an occurrence (ADR-0008). The authority is the business view
// aggregate (ADR-0004): a view participates only through its explicit
// alertSourceKeys scope naming the delivering Alertmanager source, and only
// when every exact label condition is present. Empty label conditions never
// become a catch-all match, the Alertmanager source identity is never
// substituted by a Prometheus connection identity, and later view edits must
// never recalculate a frozen decision.
package alerts

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// attributionViewScope freezes the participating scope of one candidate view
// inside the immutable diagnostic, so a unique attribution and every
// ambiguous candidate stay traceable after renames or retirement.
type attributionViewScope struct {
	AlertSourceKeys []string          `json:"alertSourceKeys"`
	LabelConditions map[string]string `json:"labelConditions"`
}

// attributedCandidateView is one matching view's frozen snapshot.
type attributedCandidateView struct {
	ViewID      int64                `json:"viewId"`
	ViewKey     string               `json:"viewKey"`
	DisplayName string               `json:"displayName"`
	Scope       attributionViewScope `json:"scope"`
}

type attributionDecision struct {
	Status           string
	AttributedViewID *int64
	CandidatesJSON   string
	ReasonJSON       string
}

// loadAttribution deliberately keeps the delivery-scoped seam: all first
// observations in one delivery share the same authority transaction, so a
// mid-batch view edit cannot mix authorities inside one delivery.
func loadAttribution(context.Context, execution.Executor) (attributionIndex, error) {
	return attributionIndex{}, nil
}

type attributionIndex struct{}

// attribute evaluates the current business views. Every returned row is a
// view explicitly scoped to the delivering source (alert_source_keys_json
// contains the delivering alert source's source_key); a view is a candidate
// only when it additionally declares at least one label condition and every
// one of them matches the incoming labels exactly.
func (attributionIndex) attribute(ctx context.Context, conn execution.Executor, sourceID int64, labels map[string]string) (attributionDecision, error) {
	canonical, err := CanonicalLabels(labels)
	if err != nil {
		return attributionDecision{}, fmt.Errorf("canonicalize labels for attribution: %w", err)
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT v.id, v.view_key, v.display_name, v.alert_source_keys_json, v.label_conditions_json,
			EXISTS(SELECT 1 FROM json_each(v.label_conditions_json) conditions
				WHERE NOT EXISTS(SELECT 1 FROM json_each(?) incoming
					WHERE incoming.key=conditions.key AND incoming.value=conditions.value)) AS labels_differ
		FROM business_views v
		WHERE EXISTS(SELECT 1 FROM json_each(v.alert_source_keys_json) scoped
			WHERE scoped.value = (SELECT source_key FROM alert_sources WHERE id=?))
		ORDER BY v.id`, canonical, sourceID)
	if err != nil {
		return attributionDecision{}, err
	}
	defer rows.Close()

	candidates := []attributedCandidateView{}
	anyScopedView := false
	for rows.Next() {
		var candidate attributedCandidateView
		var sourceKeysJSON, conditionsJSON string
		var labelsDiffer int
		if err := rows.Scan(&candidate.ViewID, &candidate.ViewKey, &candidate.DisplayName, &sourceKeysJSON, &conditionsJSON, &labelsDiffer); err != nil {
			return attributionDecision{}, err
		}
		anyScopedView = true
		if err := json.Unmarshal([]byte(sourceKeysJSON), &candidate.Scope.AlertSourceKeys); err != nil {
			return attributionDecision{}, err
		}
		if err := json.Unmarshal([]byte(conditionsJSON), &candidate.Scope.LabelConditions); err != nil {
			return attributionDecision{}, err
		}
		// 空标签条件绝不构成吞掉一切的兜底匹配：无条件的视图即使声明了来源
		// 也不是候选。
		if len(candidate.Scope.LabelConditions) == 0 || labelsDiffer != 0 {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return attributionDecision{}, err
	}

	candidatesJSON, err := json.Marshal(candidates)
	if err != nil {
		return attributionDecision{}, err
	}
	decision := attributionDecision{CandidatesJSON: string(candidatesJSON)}
	switch len(candidates) {
	case 0:
		decision.Status = "unattributed"
		reason := "label_mismatch"
		if !anyScopedView {
			reason = "source_mismatch"
		}
		decision.ReasonJSON = `{"code":"` + reason + `"}`
	case 1:
		attributed := candidates[0].ViewID
		decision.Status = "attributed"
		decision.AttributedViewID = &attributed
		decision.ReasonJSON = `{"code":"exactly_one_matching_view"}`
	default:
		decision.Status = "ambiguous"
		decision.ReasonJSON = `{"code":"multiple_matching_views"}`
	}
	return decision, nil
}

// persistAttribution freezes the full decision alongside the occurrence and
// its first delivery snapshot (alert_occurrence_view_attributions). The
// legacy business-system attribution table is history: it is never written
// again and its existing rows are never rewritten.
func persistAttribution(ctx context.Context, conn execution.Executor, occurrenceID, deliveryID, deliveryItemID int64, decision attributionDecision, createdAt string) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO alert_occurrence_view_attributions(occurrence_id,status,attributed_view_id,candidates_json,reason_json,evaluated_from_delivery_id,evaluated_from_delivery_item_id,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		occurrenceID, decision.Status, nullableID(decision.AttributedViewID), decision.CandidatesJSON, decision.ReasonJSON, deliveryID, deliveryItemID, createdAt)
	return err
}
