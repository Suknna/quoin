// Alert attribution is decided once, while the first immutable delivery item
// creates an occurrence. It deliberately has no dependency on the global Label
// Contract: each current business declaration is a complete candidate rule.
// Later configuration publications must never recalculate an occurrence.
package alerts

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// attributionCandidate is retained in the immutable diagnostic so an operator
// can distinguish a conflict from a declaration that matched nothing.
type attributionCandidate struct {
	SystemID        int64
	ConfigVersionID int64
}

type attributionDecision struct {
	BusinessSystemID       *int64
	Status                 string
	CandidateSystemIDsJSON string
	CandidateConfigIDsJSON string
	ReasonJSON             string
}

// loadAttribution deliberately does not load an active global contract. The
// type remains a delivery-scoped seam so all first observations in one delivery
// use the same authority transaction.
func loadAttribution(context.Context, *sql.Conn) (attributionIndex, error) {
	return attributionIndex{}, nil
}

type attributionIndex struct{}

// attribute evaluates all current business declarations. A declaration is a
// candidate only when its explicit source reference contains the delivering
// source and every exact label condition is present with its declared value.
// The compiler supplies derived public-metric matchLabels when YAML omits
// AlertSourceLabels, so source-only declarations never become broad matches.
func (attributionIndex) attribute(ctx context.Context, conn *sql.Conn, sourceID int64, labels map[string]string) (attributionDecision, error) {
	canonical, err := CanonicalLabels(labels)
	if err != nil {
		return attributionDecision{}, fmt.Errorf("canonicalize labels for attribution: %w", err)
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT systems.id, versions.id,
			EXISTS(SELECT 1 FROM config_alert_source_refs refs
				WHERE refs.config_version_id=versions.id AND refs.alert_source_id=?) AS source_matches,
			EXISTS(SELECT 1 FROM config_alert_label_conditions conditions
				WHERE conditions.config_version_id=versions.id) AS has_labels,
			NOT EXISTS(
				SELECT 1 FROM config_alert_label_conditions conditions
				WHERE conditions.config_version_id=versions.id
				  AND NOT EXISTS(SELECT 1 FROM json_each(?) incoming
					WHERE incoming.key=conditions.label_name AND incoming.value=conditions.label_value)
			) AS labels_match
		FROM business_systems systems
		JOIN business_system_config_versions versions ON versions.id=systems.current_config_version_id
		WHERE systems.enabled=1 AND versions.state='published'
		ORDER BY systems.id`, sourceID, canonical)
	if err != nil {
		return attributionDecision{}, err
	}
	defer rows.Close()

	candidates := []attributionCandidate{}
	anyDeclaration := false
	anySourceMatch := false
	anyLabelsMatch := false
	for rows.Next() {
		var candidate attributionCandidate
		var sourceMatches, hasLabels, labelsMatch int
		if err := rows.Scan(&candidate.SystemID, &candidate.ConfigVersionID, &sourceMatches, &hasLabels, &labelsMatch); err != nil {
			return attributionDecision{}, err
		}
		if hasLabels == 0 {
			continue
		}
		anyDeclaration = true
		if sourceMatches == 1 {
			anySourceMatch = true
		}
		if labelsMatch == 1 {
			anyLabelsMatch = true
		}
		if sourceMatches == 1 && labelsMatch == 1 {
			candidates = append(candidates, candidate)
		}
	}
	if err := rows.Err(); err != nil {
		return attributionDecision{}, err
	}
	systemIDs := make([]int64, 0, len(candidates))
	configIDs := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		systemIDs = append(systemIDs, candidate.SystemID)
		configIDs = append(configIDs, candidate.ConfigVersionID)
	}
	systemIDsJSON, err := json.Marshal(systemIDs)
	if err != nil {
		return attributionDecision{}, err
	}
	configIDsJSON, err := json.Marshal(configIDs)
	if err != nil {
		return attributionDecision{}, err
	}
	decision := attributionDecision{CandidateSystemIDsJSON: string(systemIDsJSON), CandidateConfigIDsJSON: string(configIDsJSON)}
	switch len(candidates) {
	case 0:
		decision.Status = "unattributed"
		reason := "no_matching_declaration"
		switch {
		case !anyDeclaration:
			reason = "no_declaration_labels"
		case !anySourceMatch:
			reason = "source_mismatch"
		case !anyLabelsMatch:
			reason = "label_mismatch"
		}
		decision.ReasonJSON = `{"code":"` + strings.TrimSpace(reason) + `"}`
	case 1:
		decision.Status = "attributed"
		decision.BusinessSystemID = &candidates[0].SystemID
		decision.ReasonJSON = `{"code":"exactly_one_matching_declaration"}`
	default:
		decision.Status = "conflict"
		decision.ReasonJSON = `{"code":"multiple_matching_declarations"}`
	}
	return decision, nil
}

// persistAttribution freezes the full decision alongside the occurrence and its
// first delivery snapshot. This makes diagnostics historical facts rather than
// a read-time re-evaluation of subsequently changed declarations.
func persistAttribution(ctx context.Context, conn *sql.Conn, occurrenceID, deliveryID, deliveryItemID int64, decision attributionDecision, createdAt string) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO alert_occurrence_attributions(occurrence_id,status,candidate_system_ids_json,candidate_config_version_ids_json,reason_json,evaluated_from_delivery_id,evaluated_from_delivery_item_id,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		occurrenceID, decision.Status, decision.CandidateSystemIDsJSON, decision.CandidateConfigIDsJSON, decision.ReasonJSON, deliveryID, deliveryItemID, createdAt)
	return err
}
