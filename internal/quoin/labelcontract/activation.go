package labelcontract

// Activation INSERT plumbing: canonical items_json, digest fingerprints and
// the deterministic mapping of trigger aborts to the frozen conflict codes.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// canonicalJSON marshals with sorted keys (encoding/json sorts map keys), the
// same deterministic form used for stored projections.
func canonicalJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func marshalResult(detail LabelContractDetail) string { return canonicalJSON(detail) }

func decodeStoredResult(payload string, target any) error {
	if payload == "" {
		return errors.New("empty stored result")
	}
	return json.Unmarshal([]byte(payload), target)
}

func nullableSQL(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableID(value *int64) string {
	if value == nil {
		return "null"
	}
	return strconv.FormatInt(*value, 10)
}

// activationItemDigest fingerprints the item set for command replay (the
// resolved business-system keys travel as-is; row ids are resolved inside
// the transaction and never enter the digest).
func activationItemDigest(items []ActivationItem) string {
	entries := make([]any, 0, len(items))
	for _, item := range items {
		entries = append(entries, map[string]any{
			"businessSystemKey": item.BusinessSystemKey,
			"configVersionId":   item.ConfigVersionID,
			"verificationRunId": item.VerificationRunID,
			"expectedCurrent":   nullableID(item.ExpectedCurrentConfigVersionID),
			"expectedRow":       item.ExpectedBusinessSystemRowVersion,
		})
	}
	return canonicalJSON(entries)
}

// buildItemsJSON renders the closed item array carried by the activation
// INSERT. business_system_id values were resolved inside the caller's open
// transaction; an unknown key was already rejected as a 400 there.
func buildItemsJSON(items []ActivationItem) string {
	entries := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entries = append(entries, map[string]any{
			"business_system_id":                   item.businessSystemID,
			"config_version_id":                    item.ConfigVersionID,
			"verification_run_id":                  item.VerificationRunID,
			"expected_current_config_version_id":   item.ExpectedCurrentConfigVersionID,
			"expected_business_system_row_version": item.ExpectedBusinessSystemRowVersion,
		})
	}
	return canonicalJSON(entries)
}

// mapActivationAbort converts the frozen trigger RAISE messages into typed
// service errors; unknown aborts surface as internal errors with the
// original message (never silently swallowed).
func mapActivationAbort(err error, input ActivateInput) error {
	message := err.Error()
	switch {
	case strings.Contains(message, "activation items must be closed objects"):
		return &BadRequestError{Reason: "compatibleVersions 项必须是封闭的 typed 字段且每个业务系统只出现一次"}
	case strings.Contains(message, "activation target must be an unactivated draft"):
		return &ConflictError{Code: "row_version_conflict", Detail: "目标契约不是未激活草稿（可能刚被激活），请刷新后重试", ObjectID: input.ContractVersion}
	case strings.Contains(message, "activation target row_version mismatch"):
		return &ConflictError{Code: "row_version_conflict", Detail: "目标契约行已被其他操作修改，请刷新后重试", ObjectID: input.ContractVersion}
	case strings.Contains(message, "activation state pointer/row_version mismatch"):
		return &ConflictError{Code: "current_pointer_conflict", Detail: "契约当前指针已变化，请刷新就绪视图后重试", CurrentStateRow: input.ExpectedStateRowVersion}
	case strings.Contains(message, "activation items must cover every enabled system"):
		return &ConflictError{Code: "current_pointer_conflict", Detail: "启用系统集合已变化：必须精确覆盖每个启用系统且不含禁用系统，请刷新后重试"}
	case strings.Contains(message, "activation item validation failed"):
		return &ConflictError{Code: "row_version_conflict", Detail: "某个系统的配置版本、验证 Run 或并发前提不再匹配，请刷新就绪视图后重试"}
	default:
		return err
	}
}

// scanSummary reads the summary column set shared by List.
func scanSummary(rows *sql.Rows) (LabelContractDetail, error) {
	var (
		detail    LabelContractDetail
		id        int64
		activated sql.NullString
	)
	if err := rows.Scan(&id, &detail.Version, &detail.State, &detail.RowVersion, &detail.ParserVersion, &detail.SchemaVersion, &detail.CreatedAt, &activated); err != nil {
		return LabelContractDetail{}, err
	}
	detail.ID = strconv.FormatInt(id, 10)
	if activated.Valid {
		value := activated.String
		detail.ActivatedAt = &value
	}
	return detail, nil
}

func recordAudit(ctx context.Context, conn *sql.Conn, principalID int64, action string, contractID int64, timestamp string) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('user',?,?,'success','label_contract',?,?)`,
		principalID, action, contractID, timestamp)
	return err
}
