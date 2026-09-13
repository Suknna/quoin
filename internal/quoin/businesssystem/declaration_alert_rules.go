package businesssystem

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/config"
)

// rejectDuplicatePublishedAlertRule prevents two enabled Business Systems from
// claiming the same explicitly scoped Alertmanager traffic. Source overlap is
// intentional: a shared sender plus an identical complete label condition set
// is deterministic attribution, while different conditions remain available to
// runtime conflict handling. Config versions of the same system are excluded so
// an ordinary new version can preserve its predecessor's rule.
func rejectDuplicatePublishedAlertRule(ctx context.Context, conn *sql.Conn, systemID, versionID int64, targetEnabled bool) error {
	if !targetEnabled {
		return nil
	}

	var sourceCount, labelCount int
	if err := conn.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM config_alert_source_refs WHERE config_version_id=?),
			(SELECT COUNT(*) FROM config_alert_label_conditions WHERE config_version_id=?)`,
		versionID, versionID).Scan(&sourceCount, &labelCount); err != nil {
		return err
	}
	// Declarations without both restrictions delegate attribution and must not
	// acquire an automatic duplicate-rule claim at publish time.
	if sourceCount == 0 || labelCount == 0 {
		return nil
	}

	var conflictingSystemKey string
	err := conn.QueryRowContext(ctx, `
		SELECT published_system.key
		FROM business_systems AS published_system
		JOIN business_system_config_versions AS published_version
			ON published_version.id = published_system.current_config_version_id
		WHERE published_system.id <> ?
			AND published_system.enabled = 1
			AND published_version.state = 'published'
			AND EXISTS (
				SELECT 1
				FROM config_alert_source_refs AS candidate_source
				JOIN config_alert_source_refs AS target_source
					ON target_source.alert_source_id = candidate_source.alert_source_id
				WHERE candidate_source.config_version_id = published_version.id
					AND target_source.config_version_id = ?
			)
			AND NOT EXISTS (
				SELECT 1
				FROM config_alert_label_conditions AS candidate_label
				WHERE candidate_label.config_version_id = published_version.id
					AND NOT EXISTS (
						SELECT 1
						FROM config_alert_label_conditions AS target_label
						WHERE target_label.config_version_id = ?
							AND target_label.label_name = candidate_label.label_name
							AND target_label.label_value = candidate_label.label_value
					)
			)
			AND NOT EXISTS (
				SELECT 1
				FROM config_alert_label_conditions AS target_label
				WHERE target_label.config_version_id = ?
					AND NOT EXISTS (
						SELECT 1
						FROM config_alert_label_conditions AS candidate_label
						WHERE candidate_label.config_version_id = published_version.id
							AND candidate_label.label_name = target_label.label_name
							AND candidate_label.label_value = target_label.label_value
					)
			)
		LIMIT 1`, systemID, versionID, versionID, versionID).Scan(&conflictingSystemKey)
	if err == nil {
		return &config.ValidationError{Errors: []config.FieldError{{
			Path:        "spec.alerts.matchLabels",
			Reason:      fmt.Sprintf("业务冲突：已启用业务系统 %q 使用重叠告警来源和相同标签条件声明了该告警范围", conflictingSystemKey),
			Remediation: "修改告警来源或 matchLabels，使业务归属保持明确",
		}}}
	}
	if err == sql.ErrNoRows {
		return nil
	}
	return err
}
