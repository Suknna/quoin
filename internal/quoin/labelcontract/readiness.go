// Activation readiness projection (T17, DATA-CONFIG-002/007): for each
// enabled business system, every (unpublished draft, Passed prepublish
// verification run) pair the activation command can legally select, plus the
// closed blocker set when no candidate exists. Derived read-only from the
// authority rows; "latest" never overrides an older still-legal candidate.

package labelcontract

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// ReadinessCandidate is one explicit activation-selectable pair.
type ReadinessCandidate struct {
	ConfigVersionID         string `json:"configVersionId"`
	PassedVerificationRunID string `json:"passedVerificationRunId"`
}

// ReadinessSystem is one enabled system's readiness row.
type ReadinessSystem struct {
	BusinessSystemKey        string               `json:"businessSystemKey"`
	CurrentConfigVersionID   *string              `json:"currentConfigVersionId"`
	BusinessSystemRowVersion int64                `json:"businessSystemRowVersion"`
	ActivationCandidates     []ReadinessCandidate `json:"activationCandidates"`
	Blockers                 []string             `json:"blockers"`
}

// Readiness is the LabelContractReadiness projection for one target contract.
type Readiness struct {
	TargetContractVersion    int64             `json:"targetContractVersion"`
	StateRowVersion          int64             `json:"stateRowVersion"`
	TargetRowVersion         int64             `json:"targetRowVersion"`
	CurrentContractVersionID *string           `json:"currentContractVersionId"`
	Systems                  []ReadinessSystem `json:"systems"`
}

// Readiness derives the readiness view for one contract version; unknown
// versions are 404s. Zero enabled systems yields an empty systems array
// (the zero-system activation path).
func (service *Service) Readiness(ctx context.Context, contractVersion int64) (Readiness, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return Readiness{}, err
	}
	defer conn.Close()
	var contractID, targetRowVersion int64
	err = conn.QueryRowContext(ctx, `SELECT id,row_version FROM label_contracts WHERE version=?`, contractVersion).Scan(&contractID, &targetRowVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return Readiness{}, ErrNotFound
	}
	if err != nil {
		return Readiness{}, err
	}
	var current sql.NullInt64
	var stateRow int64
	if err := conn.QueryRowContext(ctx, `SELECT current_contract_id,row_version FROM label_contract_state WHERE id=1`).Scan(&current, &stateRow); err != nil {
		return Readiness{}, err
	}
	readiness := Readiness{
		TargetContractVersion: contractVersion,
		StateRowVersion:       stateRow,
		TargetRowVersion:      targetRowVersion,
		Systems:               []ReadinessSystem{},
	}
	if current.Valid {
		value := strconv.FormatInt(current.Int64, 10)
		readiness.CurrentContractVersionID = &value
	}
	systemRows, err := conn.QueryContext(ctx, `SELECT id,key,current_config_version_id,row_version FROM business_systems WHERE enabled=1 ORDER BY key`)
	if err != nil {
		return Readiness{}, err
	}
	type systemRow struct {
		id         int64
		key        string
		current    sql.NullInt64
		rowVersion int64
	}
	systems := []systemRow{}
	for systemRows.Next() {
		var row systemRow
		if err := systemRows.Scan(&row.id, &row.key, &row.current, &row.rowVersion); err != nil {
			systemRows.Close()
			return Readiness{}, err
		}
		systems = append(systems, row)
	}
	if err := systemRows.Err(); err != nil {
		return Readiness{}, err
	}
	systemRows.Close()
	for _, row := range systems {
		system := ReadinessSystem{
			BusinessSystemKey:        row.key,
			BusinessSystemRowVersion: row.rowVersion,
			ActivationCandidates:     []ReadinessCandidate{},
			Blockers:                 []string{},
		}
		if row.current.Valid {
			value := strconv.FormatInt(row.current.Int64, 10)
			system.CurrentConfigVersionID = &value
		}
		candidates, blockers, err := readinessForSystem(ctx, conn, row.id, contractID)
		if err != nil {
			return Readiness{}, err
		}
		system.ActivationCandidates = candidates
		system.Blockers = blockers
		readiness.Systems = append(readiness.Systems, system)
	}
	return readiness, nil
}

// readinessForSystem computes the candidate set and — only when it is empty —
// the closed blocker list derived from the unpublished drafts targeting the
// contract and their prepublish runs.
func readinessForSystem(ctx context.Context, conn *sql.Conn, systemID, contractID int64) ([]ReadinessCandidate, []string, error) {
	candidateRows, err := conn.QueryContext(ctx, `
		SELECT v.id, t.id
		FROM business_system_config_versions v
		JOIN config_verification_runs t
		  ON t.config_version_id = v.id AND t.business_system_id = v.business_system_id
		 AND t.label_contract_version_id = v.label_contract_version_id
		 AND t.purpose = 'prepublish' AND t.verification_manifest_item_id IS NULL
		 AND t.state = 'Passed'
		WHERE v.business_system_id = ? AND v.label_contract_version_id = ?
		  AND v.published_at IS NULL
		ORDER BY v.version_seq, t.id`, systemID, contractID)
	if err != nil {
		return nil, nil, err
	}
	candidates := []ReadinessCandidate{}
	for candidateRows.Next() {
		var versionID, runID int64
		if err := candidateRows.Scan(&versionID, &runID); err != nil {
			candidateRows.Close()
			return nil, nil, err
		}
		candidates = append(candidates, ReadinessCandidate{
			ConfigVersionID:         strconv.FormatInt(versionID, 10),
			PassedVerificationRunID: strconv.FormatInt(runID, 10),
		})
	}
	if err := candidateRows.Err(); err != nil {
		return nil, nil, err
	}
	candidateRows.Close()
	if len(candidates) > 0 {
		return candidates, []string{}, nil
	}
	drafts, err := countDrafts(ctx, conn, systemID, contractID)
	if err != nil {
		return nil, nil, err
	}
	if drafts == 0 {
		return []ReadinessCandidate{}, []string{"no_compatible_version"}, nil
	}
	runRows, err := conn.QueryContext(ctx, `
		SELECT DISTINCT t.state
		FROM business_system_config_versions v
		JOIN config_verification_runs t
		  ON t.config_version_id = v.id AND t.business_system_id = v.business_system_id
		 AND t.label_contract_version_id = v.label_contract_version_id
		 AND t.purpose = 'prepublish'
		WHERE v.business_system_id = ? AND v.label_contract_version_id = ?
		  AND v.published_at IS NULL`, systemID, contractID)
	if err != nil {
		return nil, nil, err
	}
	states := map[string]bool{}
	for runRows.Next() {
		var state string
		if err := runRows.Scan(&state); err != nil {
			runRows.Close()
			return nil, nil, err
		}
		states[state] = true
	}
	if err := runRows.Err(); err != nil {
		return nil, nil, err
	}
	runRows.Close()
	if len(states) == 0 {
		return []ReadinessCandidate{}, []string{"verification_run_missing"}, nil
	}
	blockers := []string{}
	if states["Queued"] || states["Running"] {
		blockers = append(blockers, "verification_run_pending")
	}
	if states["Failed"] {
		blockers = append(blockers, "verification_run_failed")
	}
	if states["Cancelled"] {
		blockers = append(blockers, "verification_run_cancelled")
	}
	if states["Interrupted"] {
		blockers = append(blockers, "verification_run_interrupted")
	}
	return []ReadinessCandidate{}, blockers, nil
}

func countDrafts(ctx context.Context, conn *sql.Conn, systemID, contractID int64) (int, error) {
	var drafts int
	err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM business_system_config_versions
		WHERE business_system_id = ? AND label_contract_version_id = ? AND published_at IS NULL`,
		systemID, contractID).Scan(&drafts)
	return drafts, err
}
