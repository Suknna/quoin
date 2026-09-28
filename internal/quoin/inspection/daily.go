package inspection

// 跨来源日报（ADR-0014 后端路径）：日报是核心的跨来源聚合，不是新的巡检
// 计划类型。管理员为一份日报配置时区、每天一次的本地触发时间与参与计划；
// 调度器在触发边界（复用现有分钟调度的边界触发与不隐式补跑原则）冻结
// 「最近一个已完整结束的本地自然日」的 UTC 窗口（本地 [00:00, 次日 00:00)
// 的 UTC 表示，DST 正确）与参与计划/来源版本快照；触发后两小时为采证截止，
// 封存时从不可变的 Run/Check 事实聚合收敛为一份不可变日报。缺失、禁用、
// 失败与超时均显式记录 gap——没有数据绝不推断健康。人工补跑/重分析只追加
// 新版本，窗口仍是原日期，绝不偷换为当前日期；幂等权威是
// UNIQUE(config_key, local_date)。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/agentcontext"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// Command identities: the durable client_commands.command_type values and the
// automatic audit actions.
const (
	CommandCreateDailyConfig   = "inspection_daily_report_config.create"
	CommandUpdateDailyConfig   = "inspection_daily_report_config.update"
	CommandScheduleDailyReport = "inspection_daily_report.schedule"
	CommandSealDailyReport     = "inspection_daily_report.seal"
	CommandManualDailyReport   = "inspection_daily_report.create"
	CommandRerunDailyReport    = "inspection_daily_report.rerun"
)

// Domain object type for the command ledger and the automatic audit.
const ObjectInspectionDailyReport = "inspection_daily_report"

// dailyReportContentKind freezes the sealed content contract; later content
// evolutions add a new kind instead of reinterpreting stored reports.
const dailyReportContentKind = "inspection_daily_report_v1"

// dailyCollectionCutoff is the fixed bounded completion window after the
// trigger (ADR-0014: 触发后两小时为采证截止). Collections that have not
// settled by then are explicit cutoff gaps, never silently dropped and never
// waited on unboundedly.
const dailyCollectionCutoff = 2 * time.Hour

// maxDailyExpectedOutputLength 有界化管理员撰写的期望输出文本（纯文本，
// 不含任何工具/授权语义；渲染时 XML 转义并冻结到具体报告版本）。
const maxDailyExpectedOutputLength = 4000

// triggerTimePattern is the closed local wall-clock vocabulary for the daily
// trigger: 24h 'HH:MM'.
var triggerTimePattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// localDatePattern is the strict 'YYYY-MM-DD' local calendar day identity.
var localDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Daily report content gap vocabulary: source-level explicit facts. These are
// content facts, not inspection_check_results.gap_reason values; a sealed
// report lists them verbatim so missing data can never read as health.
const (
	dailyGapPlanDisabled   = "plan_disabled"
	dailyGapSourceDisabled = "source_disabled"
	dailyGapPlanMissing    = "plan_missing"
	dailyGapNoCollection   = "no_collection"
	dailyGapCutoffExceeded = "cutoff_exceeded"
	dailyGapRunFailed      = "run_failed"
	dailyGapRunCancelled   = "run_cancelled"
	dailyGapRunInterrupted = "run_interrupted"
)

// errDailyReportExists marks an already-committed report for the same
// (config, local date) — a replayed or repeated trigger boundary. Like the
// scheduled plan replay, the first creation's audit stands and the caller
// observes success; it never becomes a durable trace.
var errDailyReportExists = errors.New("inspection daily report already committed for this local date")

// DailyReportConfig is the readable projection of one daily report config.
type DailyReportConfig struct {
	ConfigKey   string   `json:"configKey"`
	DisplayName string   `json:"displayName"`
	Enabled     bool     `json:"enabled"`
	Timezone    string   `json:"timezone"`
	TriggerTime string   `json:"triggerTime"`
	PlanKeys    []string `json:"planKeys"`
	// ReportInstructions 是可选的人类期望输出说明（有界纯文本）；nil 表示
	// 使用渲染器内置的安全默认（逐来源状态/事实变化/缺口/风险与下一步/引用
	// Run 与 Evidence id）。
	ReportInstructions *string `json:"reportInstructions,omitempty"`
	RowVersion         int64   `json:"rowVersion"`
	CreatedAt          string  `json:"createdAt"`
	UpdatedAt          string  `json:"updatedAt"`
	configID           int64
}

// DailyReportConfigInput is the create/update command payload.
type DailyReportConfigInput struct {
	ConfigKey   string
	DisplayName string
	Enabled     bool
	Timezone    string
	TriggerTime string
	PlanKeys    []string
	// ReportInstructions 可选；指向 nil = 使用内置默认；空串 = 显式清除
	// （存 NULL，同样回落默认）；非空 = 仅使用该文本（有界校验）。
	ReportInstructions *string
}

// dailyContribution is the trigger-time frozen identity of one participating
// plan and its source. Later plan/connection changes never re-enter an
// existing report; the frozen snapshot defines the contributing set.
// The shape is shared with the model-visible projection vocabulary
// (internal/agentcontext): the sealed document and analysis snapshots must
// stay byte-compatible with what the daily prompt renderer reads.
type dailyContribution = agentcontext.DailyContribution

// dailyCheckItem is one per-check fact inside a sealed source report. Gaps
// keep their reason and observation time when the collection recorded one;
// absence of data is listed, never filled in.
type dailyCheckItem = agentcontext.DailyCheckItem

// dailySourceReport is the sealed aggregation for one contributing plan: the
// frozen contribution identity plus the explicit outcome. status is "gap"
// whenever any explicit gap reason exists or any check is not ok; it is "ok"
// only with at least one settled check and none failing.
type dailySourceReport = agentcontext.DailySourceReport

// dailyTotals is the coarse sealed roll-up over sources and checks.
type dailyTotals = agentcontext.DailyTotals

// dailyReportContent is the frozen sealed document. All fields come from
// immutable rows plus the seal moment; re-deriving it later from the same
// facts by the same frozen cutoff yields the same check content; a later
// version may freeze a different human output expectation, but never imports
// a check committed after the original cutoff.
type dailyReportContent struct {
	SchemaKind     string              `json:"schemaKind"`
	ConfigKey      string              `json:"configKey"`
	LocalDate      string              `json:"localDate"`
	Timezone       string              `json:"timezone"`
	WindowStartUTC string              `json:"windowStartUtc"`
	WindowEndUTC   string              `json:"windowEndUtc"`
	SealedAt       string              `json:"sealedAt"`
	Sources        []dailySourceReport `json:"sources"`
	Totals         dailyTotals         `json:"totals"`
}

// DailyReportSummary is the list/projection item of one daily report.
type DailyReportSummary struct {
	ID             string  `json:"id"`
	ConfigKey      string  `json:"configKey"`
	LocalDate      string  `json:"localDate"`
	Timezone       string  `json:"timezone"`
	WindowStartUTC string  `json:"windowStartUtc"`
	WindowEndUTC   string  `json:"windowEndUtc"`
	TriggerKind    string  `json:"triggerKind"`
	State          string  `json:"state"`
	SealedAt       *string `json:"sealedAt,omitempty"`
	LatestVersion  int64   `json:"latestVersion"`
	CreatedAt      string  `json:"createdAt"`
}

// DailyReportVersionSummary is one immutable version entry.
type DailyReportVersionSummary struct {
	Version int64 `json:"version"`
	// ExpectedOutput 是该版本分析所用的人类期望输出（封存/重分析时冻结）。
	ExpectedOutput *string `json:"expectedOutput,omitempty"`
	CreatedAt      string  `json:"createdAt"`
}

// DailyReportDetail is the read model of one daily report: frozen identity,
// contributions and versions, plus the latest sealed content when sealed.
type DailyReportDetail struct {
	DailyReportSummary
	ConfigRowVersion int64                       `json:"configRowVersion"`
	CutoffAt         string                      `json:"cutoffAt"`
	Contributions    []dailyContribution         `json:"contributions"`
	Versions         []DailyReportVersionSummary `json:"versions"`
	Latest           *dailyReportContent         `json:"latest,omitempty"`
}

// CreateDailyReportConfig idempotently creates a daily report config through
// the shared ledger command path: session re-verification, replay, validation,
// command ledger and audit commit in one executor transaction.
func (s *Service) CreateDailyReportConfig(ctx context.Context, principalID int64, clientCommandID string, input DailyReportConfigInput) (DailyReportConfig, error) {
	digest := auth.DigestCommand(CommandCreateDailyConfig, dailyConfigDigestPayload(input, 0))
	outcome, err := execution.Run(ctx, s.runner, s.createDailyConfig, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (DailyReportConfig, execution.Change, error) {
		if err := s.validateDailyConfigInput(ctx, tx, input); err != nil {
			return DailyReportConfig{}, execution.Unchanged, planRejection(err)
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inspection_daily_report_configs WHERE config_key=?`, input.ConfigKey).Scan(&exists); err != nil {
			return DailyReportConfig{}, execution.Unchanged, err
		}
		if exists != 0 {
			return DailyReportConfig{}, execution.Unchanged, &execution.Rejection{Code: "config_exists", Detail: "同名日报配置已存在，key 退役后不可复用"}
		}
		now := s.nowText()
		planKeys, err := json.Marshal(dailyPlanKeys(input.PlanKeys))
		if err != nil {
			return DailyReportConfig{}, execution.Unchanged, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO inspection_daily_report_configs(config_key,display_name,enabled,timezone,trigger_time,plan_keys_json,report_instructions,row_version,created_by,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,1,?,?,?)`,
			input.ConfigKey, input.DisplayName, boolInt(input.Enabled), dailyTimezone(input.Timezone), input.TriggerTime, string(planKeys), dailyExpectedOutputText(input), principalID, now, now); err != nil {
			return DailyReportConfig{}, execution.Unchanged, err
		}
		config, err := s.dailyConfigOn(ctx, tx, input.ConfigKey)
		if err != nil {
			return DailyReportConfig{}, execution.Unchanged, err
		}
		return config, execution.Changed, nil
	}, func(config DailyReportConfig) int64 { return config.configID })
	if err != nil {
		return DailyReportConfig{}, translatePlanError(err)
	}
	return outcome.Result, nil
}

// UpdateDailyReportConfig replaces the config definition under an optimistic
// row-version premise. Existing reports keep their frozen trigger-time
// snapshot; only later triggers consume the new definition.
func (s *Service) UpdateDailyReportConfig(ctx context.Context, principalID int64, clientCommandID string, input DailyReportConfigInput, expectedRowVersion int64) (DailyReportConfig, error) {
	digest := auth.DigestCommand(CommandUpdateDailyConfig, dailyConfigDigestPayload(input, expectedRowVersion))
	outcome, err := execution.Run(ctx, s.runner, s.updateDailyConfig, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (DailyReportConfig, execution.Change, error) {
		if err := s.validateDailyConfigInput(ctx, tx, input); err != nil {
			return DailyReportConfig{}, execution.Unchanged, planRejection(err)
		}
		now := s.nowText()
		planKeys, err := json.Marshal(dailyPlanKeys(input.PlanKeys))
		if err != nil {
			return DailyReportConfig{}, execution.Unchanged, err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE inspection_daily_report_configs SET display_name=?,enabled=?,timezone=?,trigger_time=?,plan_keys_json=?,report_instructions=?,row_version=row_version+1,updated_at=?
			WHERE config_key=? AND row_version=?`,
			input.DisplayName, boolInt(input.Enabled), dailyTimezone(input.Timezone), input.TriggerTime, string(planKeys), dailyExpectedOutputText(input), now,
			input.ConfigKey, expectedRowVersion)
		if err != nil {
			return DailyReportConfig{}, execution.Unchanged, err
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return DailyReportConfig{}, execution.Unchanged, &execution.Rejection{Code: "row_version_conflict", Detail: "日报配置已变化，请刷新后重试"}
		}
		config, err := s.dailyConfigOn(ctx, tx, input.ConfigKey)
		if err != nil {
			return DailyReportConfig{}, execution.Unchanged, err
		}
		return config, execution.Changed, nil
	}, func(config DailyReportConfig) int64 { return config.configID })
	if err != nil {
		return DailyReportConfig{}, translatePlanError(err)
	}
	return outcome.Result, nil
}

// ListDailyReportConfigs returns all configs in stable key order (read-only
// reader).
func (s *Service) ListDailyReportConfigs(ctx context.Context) ([]DailyReportConfig, error) {
	reader, err := s.readReader()
	if err != nil {
		return nil, err
	}
	rows, err := reader.QueryContext(ctx, `SELECT config_key FROM inspection_daily_report_configs ORDER BY config_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	configs := []DailyReportConfig{}
	for _, key := range keys {
		config, err := s.dailyConfigOn(ctx, s.reader, key)
		if err != nil {
			return nil, err
		}
		configs = append(configs, config)
	}
	return configs, nil
}

// GetDailyReportConfig returns one config (read-only reader).
func (s *Service) GetDailyReportConfig(ctx context.Context, configKey string) (DailyReportConfig, error) {
	reader, err := s.readReader()
	if err != nil {
		return DailyReportConfig{}, err
	}
	return s.dailyConfigOn(ctx, reader, configKey)
}

func (s *Service) dailyConfigOn(ctx context.Context, q rowQuerier, configKey string) (DailyReportConfig, error) {
	var config DailyReportConfig
	var enabled int
	var planKeysJSON string
	var reportInstructions sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT id,config_key,display_name,enabled,timezone,trigger_time,plan_keys_json,report_instructions,row_version,created_at,updated_at
		FROM inspection_daily_report_configs WHERE config_key=?`, configKey).
		Scan(&config.configID, &config.ConfigKey, &config.DisplayName, &enabled, &config.Timezone, &config.TriggerTime, &planKeysJSON, &reportInstructions, &config.RowVersion, &config.CreatedAt, &config.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DailyReportConfig{}, &PlanConflictError{Code: "not_found", Detail: "日报配置不存在"}
	}
	if err != nil {
		return DailyReportConfig{}, err
	}
	config.Enabled = enabled != 0
	config.PlanKeys = []string{}
	if err := json.Unmarshal([]byte(planKeysJSON), &config.PlanKeys); err != nil {
		return DailyReportConfig{}, err
	}
	if reportInstructions.Valid {
		config.ReportInstructions = &reportInstructions.String
	}
	return config, nil
}

// validateDailyConfigInput statically validates a config payload: key, name,
// IANA timezone, closed 'HH:MM' trigger time, and a non-empty participating
// plan key set whose members all exist. 日报的参与集合显式为空（无接入）不能
// 触发虚构的健康日报，创建时即拒绝。
func (s *Service) validateDailyConfigInput(ctx context.Context, q execution.Executor, input DailyReportConfigInput) error {
	if !planKeyPattern.MatchString(input.ConfigKey) {
		return &PlanConflictError{Code: "malformed_key", Detail: "配置 key 必须匹配 ^[a-z][a-z0-9-]{0,62}$"}
	}
	if strings.TrimSpace(input.DisplayName) == "" {
		return &PlanConflictError{Code: "malformed_config", Detail: "配置显示名不能为空"}
	}
	if input.Timezone == "" {
		input.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(input.Timezone); err != nil {
		return &PlanConflictError{Code: "malformed_timezone", Detail: "时区必须是合法 IANA 名称"}
	}
	if !triggerTimePattern.MatchString(input.TriggerTime) {
		return &PlanConflictError{Code: "malformed_trigger_time", Detail: "触发时间必须是本地 24 小时制 HH:MM"}
	}
	if len(input.PlanKeys) == 0 {
		return &PlanConflictError{Code: "malformed_plan_keys", Detail: "日报至少要显式参与一个巡检计划"}
	}
	seen := map[string]bool{}
	for _, key := range input.PlanKeys {
		if key == "" || seen[key] {
			return &PlanConflictError{Code: "malformed_plan_keys", Detail: "参与计划 key 不能为空且不得重复"}
		}
		seen[key] = true
		var exists int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM inspection_plans WHERE plan_key=?`, key).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return &PlanConflictError{Code: "unknown_plan", Detail: fmt.Sprintf("参与计划 %s 不存在", key)}
		}
	}
	if input.ReportInstructions != nil {
		length := len([]rune(*input.ReportInstructions))
		if length > maxDailyExpectedOutputLength {
			return &PlanConflictError{Code: "malformed_expected_output", Detail: fmt.Sprintf("期望输出不能超过 %d 字符", maxDailyExpectedOutputLength)}
		}
	}
	return nil
}

// dailyConfigDigestPayload keeps the ledger digest explicit and stable per
// command shape; expectedRowVersion is zero for creates.
func dailyConfigDigestPayload(input DailyReportConfigInput, expectedRowVersion int64) map[string]any {
	payload := map[string]any{
		"configKey": input.ConfigKey, "displayName": input.DisplayName, "enabled": input.Enabled,
		"timezone": dailyTimezone(input.Timezone), "triggerTime": input.TriggerTime,
		"planKeys": dailyPlanKeys(input.PlanKeys), "expectedRowVersion": expectedRowVersion,
	}
	// 期望输出参与命令 digest：nil（继承默认）与空串（显式清除，同存 NULL）
	// 在存储上不可区分，但携带与否必须不可混淆 replay；仅非空文本进 digest。
	if input.ReportInstructions != nil && *input.ReportInstructions != "" {
		payload["reportInstructions"] = *input.ReportInstructions
	}
	return payload
}

// dailyExpectedOutputText 折叠配置的期望输出三态到存储列：nil 与空串都存
// NULL（渲染回落内置默认），非空存原文。
func dailyExpectedOutputText(input DailyReportConfigInput) any {
	if input.ReportInstructions == nil || *input.ReportInstructions == "" {
		return nil
	}
	return *input.ReportInstructions
}

// dailyPlanKeys normalizes the payload plan list: never nil, sorted and
// deduplicated so the digest and the frozen snapshot are order-insensitive.
func dailyPlanKeys(keys []string) []string {
	normalized := []string{}
	seen := map[string]bool{}
	for _, key := range keys {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		normalized = append(normalized, key)
	}
	slices.Sort(normalized)
	return normalized
}

// dailyTimezone normalizes an empty timezone to UTC (same default as plans).
func dailyTimezone(timezone string) string {
	if timezone == "" {
		return "UTC"
	}
	return timezone
}

// parseLocalDate strictly parses the 'YYYY-MM-DD' identity in UTC for
// arithmetic; the calendar fields are re-projected into the report timezone.
func parseLocalDate(localDate string) (time.Time, error) {
	if !localDatePattern.MatchString(localDate) {
		return time.Time{}, &PlanConflictError{Code: "malformed_local_date", Detail: "本地日期必须是 YYYY-MM-DD"}
	}
	parsed, err := time.ParseInLocation("2006-01-02", localDate, time.UTC)
	if err != nil {
		return time.Time{}, &PlanConflictError{Code: "malformed_local_date", Detail: "本地日期不是真实存在的日历日"}
	}
	return parsed, nil
}

// dailyWindowUTC freezes the report window: the local calendar day
// [00:00, next 00:00) projected into UTC. time.Date recomputes both
// boundaries inside the location, so DST shifts land in the frozen UTC values
// (a 23h or 25h UTC span remains exactly that local date).
func dailyWindowUTC(localDate string, location *time.Location) (time.Time, time.Time, error) {
	day, err := parseLocalDate(localDate)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, location)
	end := time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, location)
	return start.UTC(), end.UTC(), nil
}

// previousLocalDate returns the local calendar day before the given one.
func previousLocalDate(localDate string) (string, error) {
	day, err := parseLocalDate(localDate)
	if err != nil {
		return "", err
	}
	return day.AddDate(0, 0, -1).Format("2006-01-02"), nil
}

// dailyTimeText canonicalizes stored timestamps (UTC RFC3339Nano).
func dailyTimeText(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

// dueDailyReportConfigs projects the enabled configs whose local wall clock
// equals the boundary minute in the config timezone. A DST fall-back repeats
// a wall time: both occurrences are due, and the second is absorbed by the
// report's (config, local date) identity.
func (s *Service) dueDailyReportConfigs(ctx context.Context, boundary time.Time) ([]DailyReportConfig, error) {
	configs, err := s.ListDailyReportConfigs(ctx)
	if err != nil {
		return nil, err
	}
	due := []DailyReportConfig{}
	for _, config := range configs {
		if !config.Enabled {
			continue
		}
		location, err := time.LoadLocation(config.Timezone)
		if err != nil {
			return nil, fmt.Errorf("daily report config %s: load timezone %q: %w", config.ConfigKey, config.Timezone, err)
		}
		if boundary.In(location).Format("15:04") == config.TriggerTime {
			due = append(due, config)
		}
	}
	return due, nil
}

// DueDailyReportConfigs is the scheduler-facing due projection at a minute
// boundary. It is a read-only projection over enabled configs: the mutation
// authority is enforced at the scheduled create/seal commands, which rebuild
// the system scheduler scope themselves.
func (s *Service) DueDailyReportConfigs(ctx context.Context, boundary time.Time) ([]DailyReportConfig, error) {
	return s.dueDailyReportConfigs(ctx, boundary)
}

// CreateScheduledDailyReport commits the trigger boundary's report: the
// previous fully completed local calendar day, its frozen UTC window and the
// frozen contribution snapshot, in Collecting state until the cutoff seal.
// A repeated boundary (process restart re-observation, DST repeated hour)
// replays as success against the (config_key, local_date) identity.
func (s *Service) CreateScheduledDailyReport(ctx context.Context, config DailyReportConfig, boundary time.Time) error {
	boundary = boundary.UTC()
	if boundary.Nanosecond() != 0 || boundary.Second() != 0 {
		return fmt.Errorf("daily report boundary must be a minute boundary")
	}
	commandCtx, err := s.schedulerContext(ctx)
	if err != nil {
		return err
	}
	_, err = execution.Execute(commandCtx, s.runner, s.scheduleDailyReport, func(tx *execution.Tx) (int64, error) {
		return s.createScheduledDailyReportOn(commandCtx, tx, config.ConfigKey, boundary)
	}, func(reportID int64) int64 { return reportID })
	if errors.Is(err, errDailyReportExists) {
		return nil
	}
	if errors.Is(err, errScheduledPlanUnavailable) {
		// The config was disabled or removed between the due projection and
		// this boundary's transaction: not a scheduling error, no durable trace.
		return nil
	}
	if err == nil {
		// ADR-0014: the due fact (if any subscriber wants it) committed with
		// the report; wake the dispatcher only after the commit returned.
		s.notifyPostCommit()
	}
	return err
}

// createScheduledDailyReportOn is the in-transaction trigger core. The config
// row is re-read inside the transaction so the frozen snapshot is the
// authoritative trigger-moment definition, not a possibly stale projection.
func (s *Service) createScheduledDailyReportOn(ctx context.Context, tx execution.Executor, configKey string, boundary time.Time) (int64, error) {
	var enabled int
	var timezone string
	var configRowVersion int64
	var expectedOutput sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT enabled,timezone,row_version,report_instructions FROM inspection_daily_report_configs WHERE config_key=?`, configKey).
		Scan(&enabled, &timezone, &configRowVersion, &expectedOutput)
	if errors.Is(err, sql.ErrNoRows) {
		// The config vanished between the due projection and this
		// transaction: not a scheduling error, leave no durable trace.
		return 0, errScheduledPlanUnavailable
	}
	if err != nil {
		return 0, err
	}
	if enabled == 0 {
		return 0, errScheduledPlanUnavailable
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return 0, fmt.Errorf("daily report config %s: load timezone %q: %w", configKey, timezone, err)
	}
	localDate, err := previousLocalDate(boundary.In(location).Format("2006-01-02"))
	if err != nil {
		return 0, err
	}
	var existingID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM inspection_daily_reports WHERE config_key=? AND local_date=?`, configKey, localDate).Scan(&existingID)
	if err == nil {
		return 0, errDailyReportExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var configID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM inspection_daily_report_configs WHERE config_key=?`, configKey).Scan(&configID); err != nil {
		return 0, err
	}
	windowStart, windowEnd, err := dailyWindowUTC(localDate, location)
	if err != nil {
		return 0, err
	}
	contributions, err := s.dailyContributionsOn(ctx, tx, configKey)
	if err != nil {
		return 0, err
	}
	contributionsJSON, err := json.Marshal(contributions)
	if err != nil {
		return 0, err
	}
	cutoff := boundary.Add(dailyCollectionCutoff)
	result, err := tx.ExecContext(ctx, `
		INSERT INTO inspection_daily_reports(config_id,config_key,config_row_version,local_date,timezone,window_start_utc,window_end_utc,
			trigger_kind,scheduled_for,cutoff_at,contributions_json,expected_output,state,created_at)
		VALUES(?,?,?,?,?,?,?, 'schedule',?,?,?,?,'Collecting',?)`,
		configID, configKey, configRowVersion, localDate, timezone, dailyTimeText(windowStart), dailyTimeText(windowEnd),
		dailyTimeText(boundary), dailyTimeText(cutoff), string(contributionsJSON), expectedOutput, s.nowText())
	if err != nil {
		return 0, err
	}
	reportID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	// ADR-0014: the frozen window's due fact joins the same authority
	// transaction as the Collecting report row.
	if err := s.emitDailyWindowDueFact(ctx, tx, configID, configRowVersion, reportID, localDate, dailyTimeText(windowStart), dailyTimeText(windowEnd)); err != nil {
		return 0, err
	}
	return reportID, nil
}

// dailyContributionsOn freezes the trigger-time identity of every
// participating plan: plan row, connection and template version as they are
// at this moment. A vanished plan key stays in the snapshot as an explicit
// missing contribution so the sealed report can name the hole.
func (s *Service) dailyContributionsOn(ctx context.Context, tx execution.Executor, configKey string) ([]dailyContribution, error) {
	var planKeysJSON string
	if err := tx.QueryRowContext(ctx, `SELECT plan_keys_json FROM inspection_daily_report_configs WHERE config_key=?`, configKey).Scan(&planKeysJSON); err != nil {
		return nil, err
	}
	planKeys := []string{}
	if err := json.Unmarshal([]byte(planKeysJSON), &planKeys); err != nil {
		return nil, err
	}
	contributions := []dailyContribution{}
	for _, planKey := range dailyPlanKeys(planKeys) {
		var displayName, connectionName, pluginID, templateID string
		var planEnabled, connectionEnabled int
		var templateVersion sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT p.display_name,p.enabled,p.plugin_id,p.template_id,p.template_version,c.name,c.enabled
			FROM inspection_plans p JOIN connections c ON c.id=p.connection_id
			WHERE p.plan_key=?`, planKey).
			Scan(&displayName, &planEnabled, &pluginID, &templateID, &templateVersion, &connectionName, &connectionEnabled)
		if errors.Is(err, sql.ErrNoRows) {
			contributions = append(contributions, dailyContribution{
				PlanKey: planKey, Enabled: false, SourceEnabled: false, Missing: true,
			})
			continue
		}
		if err != nil {
			return nil, err
		}
		frozenVersion := ""
		if templateVersion.Valid {
			frozenVersion = templateVersion.String
		} else if template, ok := TemplateFor(pluginID, templateID); ok {
			frozenVersion = template.Version
		}
		contributions = append(contributions, dailyContribution{
			PlanKey: planKey, DisplayName: displayName, ConnectionName: connectionName,
			PluginID: pluginID, TemplateID: templateID, TemplateVersion: frozenVersion,
			Enabled: planEnabled != 0, SourceEnabled: connectionEnabled != 0,
		})
	}
	return contributions, nil
}

// SealDueDailyReports seals every report whose collection cutoff has passed
// at the given boundary. Sealing re-derives the content from the immutable
// Run/Check facts inside the frozen window; runs that are still unsettled at
// the cutoff become explicit cutoff_exceeded gaps. A seal failure leaves the
// report Collecting and the next boundary retries it — no fabricated seal.
func (s *Service) SealDueDailyReports(ctx context.Context, boundary time.Time) error {
	commandCtx, err := s.schedulerContext(ctx)
	if err != nil {
		return err
	}
	reportIDs, err := s.dueSealReportIDs(ctx, boundary)
	if err != nil {
		return err
	}
	var sealErrors []error
	for _, reportID := range reportIDs {
		if _, err := execution.Execute(commandCtx, s.runner, s.sealDailyReport, func(tx *execution.Tx) (struct{}, error) {
			return struct{}{}, s.sealDailyReportOn(commandCtx, tx, reportID, boundary)
		}, func(struct{}) int64 { return reportID }); err != nil {
			sealErrors = append(sealErrors, fmt.Errorf("seal daily report %d: %w", reportID, err))
		}
	}
	// Sealed facts (if any subscriber wants them) committed above; wake the
	// dispatcher once per boundary regardless of per-report failures — each
	// successful seal is already durable.
	s.notifyPostCommit()
	return errors.Join(sealErrors...)
}

// dueSealReportIDs lists Collecting reports past their cutoff at the boundary.
func (s *Service) dueSealReportIDs(ctx context.Context, boundary time.Time) ([]int64, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id FROM inspection_daily_reports
		WHERE state='Collecting' AND cutoff_at<=? ORDER BY id`, dailyTimeText(boundary))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// sealDailyReportOn builds and freezes version N of the report inside one
// transaction: the state transition and the version append commit together,
// so a sealed report always has exactly the version the seal produced.
func (s *Service) sealDailyReportOn(ctx context.Context, tx execution.Executor, reportID int64, boundary time.Time) error {
	var configKey, localDate, timezone, windowStartUTC, windowEndUTC, cutoffAt, contributionsJSON string
	var expectedOutput sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT config_key,local_date,timezone,window_start_utc,window_end_utc,cutoff_at,contributions_json,expected_output
		FROM inspection_daily_reports WHERE id=? AND state='Collecting' AND cutoff_at<=?`,
		reportID, dailyTimeText(boundary)).
		Scan(&configKey, &localDate, &timezone, &windowStartUTC, &windowEndUTC, &cutoffAt, &contributionsJSON, &expectedOutput)
	if errors.Is(err, sql.ErrNoRows) {
		// Already sealed (or cutoff not yet reached in this transaction's
		// view): the winner owns the seal, a replay records nothing.
		return nil
	}
	if err != nil {
		return err
	}
	contributions := []dailyContribution{}
	if err := json.Unmarshal([]byte(contributionsJSON), &contributions); err != nil {
		return err
	}
	content, err := s.buildDailyReportContent(ctx, tx, dailyReportContent{
		SchemaKind: dailyReportContentKind, ConfigKey: configKey, LocalDate: localDate, Timezone: timezone,
		WindowStartUTC: windowStartUTC, WindowEndUTC: windowEndUTC,
	}, contributions, windowStartUTC, windowEndUTC, cutoffAt)
	if err != nil {
		return err
	}
	content.SealedAt = dailyTimeText(boundary)
	encoded, err := json.Marshal(content)
	if err != nil {
		return err
	}
	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inspection_daily_report_versions WHERE report_id=?`, reportID).Scan(&version); err != nil {
		return err
	}
	versionInsert, err := tx.ExecContext(ctx, `
		INSERT INTO inspection_daily_report_versions(report_id,version,content,expected_output,created_at) VALUES(?,?,?,?,?)`,
		reportID, version+1, string(encoded), expectedOutput, s.nowText())
	if err != nil {
		return err
	}
	versionRowID, err := versionInsert.LastInsertId()
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE inspection_daily_reports SET state='Sealed',sealed_at=?,row_version=row_version+1
		WHERE id=? AND state='Collecting' AND cutoff_at<=?`,
		content.SealedAt, reportID, dailyTimeText(boundary))
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return fmt.Errorf("daily report %d left Collecting after seal append", reportID)
	}
	// ADR-0014: the sealed fact joins the same authority transaction as the
	// immutable version and the Sealed state transition.
	return s.emitReportSealedFact(ctx, tx, reportID, versionRowID, configKey, localDate, content.SealedAt)
}

// buildDailyReportContent aggregates the frozen window's per-source facts.
// Every query reads committed immutable rows: check results carry their
// frozen meta (observedAt), evidence supplies the historical fallback. A run
// belongs to the day its collection actually started in (evidence_at within
// the frozen UTC window), so the report records when evidence was really
// taken, never when the report happened to be built.
func (s *Service) buildDailyReportContent(ctx context.Context, tx execution.Executor, content dailyReportContent, contributions []dailyContribution, windowStartUTC, windowEndUTC, cutoffAt string) (dailyReportContent, error) {
	content.Sources = []dailySourceReport{}
	for _, contribution := range contributions {
		source, err := s.dailySourceReportOn(ctx, tx, contribution, windowStartUTC, windowEndUTC, cutoffAt)
		if err != nil {
			return content, err
		}
		content.Sources = append(content.Sources, source)
		if source.Status != "ok" {
			content.Totals.SourcesGap++
		}
		for _, check := range source.Checks {
			switch check.Status {
			case "ok":
				content.Totals.ChecksOK++
			case "error":
				content.Totals.ChecksError++
			default:
				content.Totals.ChecksGap++
			}
		}
	}
	return content, nil
}

// dailySourceReportOn aggregates one contribution. Run membership is the
// frozen evidence_at window with second precision (both sides are canonical
// UTC RFC3339 timestamps, so the fixed-width prefix comparison is exact at
// the minute-precision window boundaries).
func (s *Service) dailySourceReportOn(ctx context.Context, tx execution.Executor, contribution dailyContribution, windowStartUTC, windowEndUTC, cutoffAt string) (dailySourceReport, error) {
	source := dailySourceReport{DailyContribution: contribution, Status: "ok", Checks: []dailyCheckItem{}}
	addReason := func(reason string) {
		if !slices.Contains(source.GapReasons, reason) {
			source.GapReasons = append(source.GapReasons, reason)
		}
	}
	if contribution.Missing {
		addReason(dailyGapPlanMissing)
	}
	if !contribution.Enabled {
		addReason(dailyGapPlanDisabled)
	}
	if !contribution.SourceEnabled {
		addReason(dailyGapSourceDisabled)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id,state FROM inspection_runs
		WHERE plan_key=? AND evidence_at IS NOT NULL
		  AND substr(evidence_at,1,19) >= substr(?,1,19)
		  AND substr(evidence_at,1,19) < substr(?,1,19)
		ORDER BY id`, contribution.PlanKey, windowStartUTC, windowEndUTC)
	if err != nil {
		return source, err
	}
	type windowRun struct {
		id    int64
		state string
	}
	runs := []windowRun{}
	for rows.Next() {
		var run windowRun
		if err := rows.Scan(&run.id, &run.state); err != nil {
			rows.Close()
			return source, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return source, err
	}
	rows.Close()
	for _, run := range runs {
		switch run.state {
		case "Queued", "Running":
			// 采证截止时仍未收敛：显式超时 gap，绝不等待无界完成。
			addReason(dailyGapCutoffExceeded)
		case "Failed":
			addReason(dailyGapRunFailed)
		case "Cancelled":
			addReason(dailyGapRunCancelled)
		case "Interrupted":
			addReason(dailyGapRunInterrupted)
		}
		markUnsettled := run.state != "Failed" && run.state != "Cancelled" && run.state != "Interrupted"
		checks, err := s.dailyCheckItemsOn(ctx, tx, run.id, cutoffAt, markUnsettled)
		if err != nil {
			return source, err
		}
		source.Checks = append(source.Checks, checks...)
		for _, check := range checks {
			if check.GapReason != nil && *check.GapReason == dailyGapCutoffExceeded {
				addReason(dailyGapCutoffExceeded)
			}
		}
	}
	if len(runs) == 0 {
		addReason(dailyGapNoCollection)
	}
	// A terminal run with no check result is still a missing collection. A
	// SkippedOverlap run may be excluded entirely (it has no evidence_at), but
	// an inconsistent Completed run must never turn an empty source green.
	if len(source.Checks) == 0 && len(source.GapReasons) == 0 {
		addReason(dailyGapNoCollection)
	}
	for _, check := range source.Checks {
		if check.Status != "ok" {
			source.Status = "gap"
		}
	}
	if len(source.GapReasons) != 0 {
		source.Status = "gap"
	}
	return source, nil
}

// dailyCheckItemsOn lists only facts committed by the frozen cutoff, even if
// the sealing tick runs late. A later result remains in its Run/Evidence but
// never upgrades a daily version that had a gap at cutoff.
func (s *Service) dailyCheckItemsOn(ctx context.Context, tx execution.Executor, runID int64, cutoffAt string, markUnsettled bool) ([]dailyCheckItem, error) {
	cutoff, err := time.Parse(time.RFC3339Nano, cutoffAt)
	if err != nil {
		return nil, fmt.Errorf("daily report cutoff %q is invalid: %w", cutoffAt, err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT x.check_key, x.status, x.gap_reason,
		       COALESCE(json_extract(x.meta_json,'$.observedAt'), e.observed_at),
		       x.evidence_id, e.result_json, x.created_at
		FROM inspection_check_results x
		LEFT JOIN evidence e ON e.id=x.evidence_id
		WHERE x.run_id=? ORDER BY x.check_key`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []dailyCheckItem{}
	settled := map[string]bool{}
	for rows.Next() {
		var item dailyCheckItem
		item.RunID = runID
		var gapReason, observedAt sql.NullString
		var evidenceID sql.NullInt64
		var resultJSON sql.NullString
		var committedAt string
		if err := rows.Scan(&item.CheckKey, &item.Status, &gapReason, &observedAt, &evidenceID, &resultJSON, &committedAt); err != nil {
			return nil, err
		}
		committed, err := time.Parse(time.RFC3339Nano, committedAt)
		if err != nil {
			return nil, fmt.Errorf("run %d check %s has invalid commit time: %w", runID, item.CheckKey, err)
		}
		if committed.After(cutoff) {
			continue
		}
		settled[item.CheckKey] = true
		if gapReason.Valid {
			item.GapReason = &gapReason.String
		}
		if observedAt.Valid {
			item.ObservedAt = &observedAt.String
		}
		if evidenceID.Valid {
			item.EvidenceID = &evidenceID.Int64
		}
		// 限界测量投影：只读已提交 Evidence 的确定性摘要（类型/序列/样本计数
		// 与首末样本值），绝不携带原始载荷或任何秘密；完整结果始终以
		// evidenceId 定位。已提交成功 Evidence 的畸形结果形状是数据完整性
		// 故障，带身份显式报错，绝不静默吞掉或编造数值。
		if evidenceID.Valid && resultJSON.Valid {
			measurement, err := dailyMeasurementFromEvidence(evidenceID.Int64, item.CheckKey, resultJSON.String)
			if err != nil {
				return nil, err
			}
			item.Measurement = measurement
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if !markUnsettled {
		return items, nil // run_failed/cancelled/interrupted already names the gap
	}
	// The run's frozen check directory is authoritative, not its current
	// terminal state. Any check still missing at cutoff is an explicit gap.
	checkRows, err := tx.QueryContext(ctx, `SELECT check_key FROM inspection_run_checks WHERE run_id=? ORDER BY check_key`, runID)
	if err != nil {
		return nil, err
	}
	defer checkRows.Close()
	for checkRows.Next() {
		var key string
		if err := checkRows.Scan(&key); err != nil {
			return nil, err
		}
		if !settled[key] {
			reason := dailyGapCutoffExceeded
			items = append(items, dailyCheckItem{RunID: runID, CheckKey: key, Status: "gap", GapReason: &reason})
		}
	}
	if err := checkRows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(items, func(left, right dailyCheckItem) int { return strings.Compare(left.CheckKey, right.CheckKey) })
	return items, nil
}

// dailyMeasurementFromEvidence derives the bounded measurement summary of one
// committed Evidence result. The committed vocabulary is the shared PromQL
// shape (vector/matrix/scalar/string); an unknown shape is summarised
// honestly as its type alone — no values are invented. Per-series entries
// carry the label set and numeric extremes (min/max with timestamps) so a
// middle-series anomaly stays analyzable; series beyond the entry bound are
// deterministically omitted and marked truncated.
//
// The recover guard is only real because the results are NAMED: a recovered
// panic overwrites both named results, so the caller sees a typed error —
// never the silent (nil, nil) success an unnamed-return local would produce.
func dailyMeasurementFromEvidence(evidenceID int64, checkKey, resultJSON string) (measurement *agentcontext.DailyMeasurement, failure error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			measurement, failure = nil, fmt.Errorf("evidence %d check %s measurement projection failed", evidenceID, checkKey)
		}
	}()
	return dailyMeasurementProject(evidenceID, checkKey, resultJSON)
}

// dailyMeasurementProject is the pure parse-and-fold of one evidence result.
// It is split from the recover boundary so tests can exercise that boundary
// with an injected panic — JSON bytes alone cannot panic the parser, and an
// untestable guard is exactly how the silent (nil, nil) regression slipped
// through. Tests substitute it only around the boundary exercise.
var dailyMeasurementProject = dailyMeasurementProjectOn

func dailyMeasurementProjectOn(evidenceID int64, checkKey, resultJSON string) (*agentcontext.DailyMeasurement, error) {
	var payload struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &payload); err != nil {
		return nil, fmt.Errorf("evidence %d check %s result is malformed: %w", evidenceID, checkKey, err)
	}
	measurement := &agentcontext.DailyMeasurement{ResultType: payload.ResultType}
	fold := func(entry *agentcontext.DailyMeasurementSeriesEntry, timestamp, value json.RawMessage) {
		sample := dailySampleText(value)
		entry.Samples++
		measurement.Samples++
		if measurement.FirstValue == nil {
			first := sample
			measurement.FirstValue = &first
		}
		last := sample
		lastAt := string(timestamp)
		entry.FirstValue = firstOrNil(entry.FirstValue, sample)
		entry.LastValue, entry.LastAt = &last, &lastAt
		measurement.LastValue, measurement.LastAt = &last, &lastAt
		// 数值极值只在有限浮点时跟踪（NaN/+Inf/-Inf 不构成可比较的极值，
		// 仍作为首末值原样可见）；平局保留首次出现，保证确定性。
		if number, err := strconv.ParseFloat(sample, 64); err == nil && !math.IsNaN(number) && !math.IsInf(number, 0) {
			minAt, maxAt := string(timestamp), string(timestamp)
			if entry.MinValue == nil || number < dailyMeasurementNumber(entry.MinValue) {
				entry.MinValue, entry.MinAt = &sample, &minAt
			}
			if entry.MaxValue == nil || number > dailyMeasurementNumber(entry.MaxValue) {
				entry.MaxValue, entry.MaxAt = &sample, &maxAt
			}
		}
	}
	switch payload.ResultType {
	case "vector":
		var series []struct {
			Metric map[string]string  `json:"metric"`
			Value  [2]json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(payload.Result, &series); err != nil {
			return nil, fmt.Errorf("evidence %d check %s vector result is malformed: %w", evidenceID, checkKey, err)
		}
		measurement.Series = len(series)
		for index, item := range series {
			entry := dailySeriesEntry(item.Metric)
			fold(&entry, item.Value[0], item.Value[1])
			if index < agentcontext.DailyMeasurementEntryBound {
				measurement.Entries = append(measurement.Entries, entry)
			}
		}
		measurement.Truncated = len(series) > agentcontext.DailyMeasurementEntryBound
	case "matrix":
		var series []struct {
			Metric map[string]string   `json:"metric"`
			Values [][]json.RawMessage `json:"values"`
		}
		if err := json.Unmarshal(payload.Result, &series); err != nil {
			return nil, fmt.Errorf("evidence %d check %s matrix result is malformed: %w", evidenceID, checkKey, err)
		}
		measurement.Series = len(series)
		for index, item := range series {
			entry := dailySeriesEntry(item.Metric)
			for _, sample := range item.Values {
				if len(sample) != 2 {
					continue
				}
				fold(&entry, sample[0], sample[1])
			}
			if index < agentcontext.DailyMeasurementEntryBound {
				measurement.Entries = append(measurement.Entries, entry)
			}
		}
		measurement.Truncated = len(series) > agentcontext.DailyMeasurementEntryBound
	case "scalar", "string":
		// 标量/字符串结果是单个 [timestamp, value] 样本：同样折叠进一条
		// 无标签条目，值不再丢失。
		var sample [2]json.RawMessage
		if err := json.Unmarshal(payload.Result, &sample); err != nil {
			return nil, fmt.Errorf("evidence %d check %s %s result is malformed: %w", evidenceID, checkKey, payload.ResultType, err)
		}
		measurement.Series = 1
		entry := agentcontext.DailyMeasurementSeriesEntry{}
		fold(&entry, sample[0], sample[1])
		measurement.Entries = append(measurement.Entries, entry)
	}
	return measurement, nil
}

// dailySampleText 提取样本值的纯文本：Prometheus 语义把样本值编码为 JSON
// 字符串（为了携带 NaN/+Inf），投影必须剥掉编码引号——模型看到的是值本身
// （0.4、NaN），不是 JSON 字面量（"0.4"）；非字符串字面量（数值时间戳）
// 原样保留。
func dailySampleText(value json.RawMessage) string {
	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		return text
	}
	return string(value)
}

// dailySeriesEntry bounds one series' label set: keys beyond the label bound
// are dropped in deterministic key order and marked.
func dailySeriesEntry(labels map[string]string) agentcontext.DailyMeasurementSeriesEntry {
	entry := agentcontext.DailyMeasurementSeriesEntry{}
	if len(labels) == 0 {
		return entry
	}
	keys := slices.Sorted(maps.Keys(labels))
	entry.LabelsTruncated = len(keys) > agentcontext.DailyMeasurementLabelBound
	if entry.LabelsTruncated {
		keys = keys[:agentcontext.DailyMeasurementLabelBound]
	}
	entry.Labels = make(map[string]string, len(keys))
	for _, key := range keys {
		entry.Labels[key] = labels[key]
	}
	return entry
}

func firstOrNil(existing *string, value string) *string {
	if existing != nil {
		return existing
	}
	return &value
}

// dailyMeasurementNumber re-parses a previously accepted numeric sample;
// the projection only stores values that parsed once, so this cannot fail.
func dailyMeasurementNumber(value *string) float64 {
	number, err := strconv.ParseFloat(*value, 64)
	if err != nil {
		return math.NaN()
	}
	return number
}

// CreateManualDailyReport is the bounded manual backfill (漏过的整日人工补跑):
// the window stays the requested original local date — never relabelled to
// the current date — and only a fully ended local day may be backfilled.
func (s *Service) CreateManualDailyReport(ctx context.Context, principalID int64, clientCommandID, configKey, localDate string) (DailyReportSummary, error) {
	digest := auth.DigestCommand(CommandManualDailyReport, map[string]any{"configKey": configKey, "localDate": localDate})
	outcome, err := execution.Run(ctx, s.runner, s.manualDailyReport, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (DailyReportSummary, execution.Change, error) {
		summary, rejection, err := s.createManualDailyReportOn(ctx, tx, configKey, localDate)
		if err != nil {
			return DailyReportSummary{}, execution.Unchanged, err
		}
		if rejection != nil {
			return DailyReportSummary{}, execution.Unchanged, rejection
		}
		return summary, execution.Changed, nil
	}, func(summary DailyReportSummary) int64 { return mustLocator(summary.ID) })
	if err != nil {
		return DailyReportSummary{}, translatePlanError(err)
	}
	return outcome.Result, nil
}

// dailyRejection converts a domain PlanConflictError into the runner's
// recorded rejection; the input domain here is always deterministic
// validation failures, and anything else becomes a malformed rejection with
// the original detail rather than a panic.
func dailyRejection(err error) *execution.Rejection {
	var rejection *execution.Rejection
	if errors.As(err, &rejection) {
		return rejection
	}
	var conflict *PlanConflictError
	if errors.As(err, &conflict) {
		return &execution.Rejection{Code: conflict.Code, Detail: conflict.Detail}
	}
	return &execution.Rejection{Code: "malformed_local_date", Detail: err.Error()}
}

func (s *Service) createManualDailyReportOn(ctx context.Context, tx execution.Executor, configKey, localDate string) (DailyReportSummary, *execution.Rejection, error) {
	if _, err := parseLocalDate(localDate); err != nil {
		return DailyReportSummary{}, dailyRejection(err), nil
	}
	var enabled int
	var timezone string
	var configID, configRowVersion int64
	var expectedOutput sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT id,enabled,timezone,row_version,report_instructions FROM inspection_daily_report_configs WHERE config_key=?`, configKey).
		Scan(&configID, &enabled, &timezone, &configRowVersion, &expectedOutput)
	if errors.Is(err, sql.ErrNoRows) {
		return DailyReportSummary{}, &execution.Rejection{Code: "not_found", Detail: "日报配置不存在"}, nil
	}
	if err != nil {
		return DailyReportSummary{}, nil, err
	}
	if enabled == 0 {
		return DailyReportSummary{}, &execution.Rejection{Code: "config_disabled", Detail: "日报配置已停用，不能人工补跑"}, nil
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return DailyReportSummary{}, nil, fmt.Errorf("daily report config %s: load timezone %q: %w", configKey, timezone, err)
	}
	windowStart, windowEnd, err := dailyWindowUTC(localDate, location)
	if err != nil {
		return DailyReportSummary{}, dailyRejection(err), nil
	}
	if windowEnd.After(s.clock()) {
		return DailyReportSummary{}, &execution.Rejection{Code: "local_date_not_complete", Detail: "该本地自然日尚未完整结束，不能提前生成日报"}, nil
	}
	var existingID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM inspection_daily_reports WHERE config_key=? AND local_date=?`, configKey, localDate).Scan(&existingID)
	if err == nil {
		return DailyReportSummary{}, &execution.Rejection{Code: "daily_report_exists", Detail: "该本地日期的日报已存在"}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DailyReportSummary{}, nil, err
	}
	contributions, err := s.dailyContributionsOn(ctx, tx, configKey)
	if err != nil {
		return DailyReportSummary{}, nil, err
	}
	contributionsJSON, err := json.Marshal(contributions)
	if err != nil {
		return DailyReportSummary{}, nil, err
	}
	now := s.nowText()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO inspection_daily_reports(config_id,config_key,config_row_version,local_date,timezone,window_start_utc,window_end_utc,
			trigger_kind,scheduled_for,cutoff_at,contributions_json,expected_output,state,created_at)
		VALUES(?,?,?,?,?,?,?, 'manual',NULL,?,?,?,'Collecting',?)`,
		configID, configKey, configRowVersion, localDate, timezone, dailyTimeText(windowStart), dailyTimeText(windowEnd),
		dailyTimeText(s.clock().Add(dailyCollectionCutoff)), string(contributionsJSON), expectedOutput, now)
	if err != nil {
		return DailyReportSummary{}, nil, err
	}
	reportID, err := result.LastInsertId()
	if err != nil {
		return DailyReportSummary{}, nil, err
	}
	return DailyReportSummary{
		ID: locatorID(reportID), ConfigKey: configKey, LocalDate: localDate, Timezone: timezone,
		WindowStartUTC: dailyTimeText(windowStart), WindowEndUTC: dailyTimeText(windowEnd),
		TriggerKind: "manual", State: "Collecting", LatestVersion: 0, CreatedAt: now,
	}, nil, nil
}

// RerunDailyReport re-derives a sealed report as a new immutable version from
// the same frozen window, cutoff and contributions (人工重分析). Late results
// remain readable in their Run/Evidence but never enter any daily version;
// older versions stay readable and comparable.
func (s *Service) RerunDailyReport(ctx context.Context, principalID int64, clientCommandID, configKey, localDate string) (DailyReportSummary, error) {
	digest := auth.DigestCommand(CommandRerunDailyReport, map[string]any{"configKey": configKey, "localDate": localDate})
	outcome, err := execution.Run(ctx, s.runner, s.rerunDailyReport, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (DailyReportSummary, execution.Change, error) {
		summary, rejection, err := s.rerunDailyReportOn(ctx, tx, configKey, localDate)
		if err != nil {
			return DailyReportSummary{}, execution.Unchanged, err
		}
		if rejection != nil {
			return DailyReportSummary{}, execution.Unchanged, rejection
		}
		return summary, execution.Changed, nil
	}, func(summary DailyReportSummary) int64 { return mustLocator(summary.ID) })
	if err != nil {
		return DailyReportSummary{}, translatePlanError(err)
	}
	return outcome.Result, nil
}

func (s *Service) rerunDailyReportOn(ctx context.Context, tx execution.Executor, configKey, localDate string) (DailyReportSummary, *execution.Rejection, error) {
	var reportID int64
	var state, timezone, windowStartUTC, windowEndUTC, cutoffAt, contributionsJSON string
	err := tx.QueryRowContext(ctx, `
		SELECT id,state,timezone,window_start_utc,window_end_utc,cutoff_at,contributions_json
		FROM inspection_daily_reports WHERE config_key=? AND local_date=?`, configKey, localDate).
		Scan(&reportID, &state, &timezone, &windowStartUTC, &windowEndUTC, &cutoffAt, &contributionsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return DailyReportSummary{}, &execution.Rejection{Code: "not_found", Detail: "该本地日期的日报不存在"}, nil
	}
	if err != nil {
		return DailyReportSummary{}, nil, err
	}
	if state != "Sealed" {
		return DailyReportSummary{}, &execution.Rejection{Code: "daily_report_not_sealed", Detail: "日报尚未封存，不能重分析"}, nil
	}
	contributions := []dailyContribution{}
	if err := json.Unmarshal([]byte(contributionsJSON), &contributions); err != nil {
		return DailyReportSummary{}, nil, err
	}
	seed := dailyReportContent{
		SchemaKind: dailyReportContentKind, ConfigKey: configKey, LocalDate: localDate, Timezone: timezone,
		WindowStartUTC: windowStartUTC, WindowEndUTC: windowEndUTC,
	}
	content, err := s.buildDailyReportContent(ctx, tx, seed, contributions, windowStartUTC, windowEndUTC, cutoffAt)
	if err != nil {
		return DailyReportSummary{}, nil, err
	}
	content.SealedAt = s.nowText()
	encoded, err := json.Marshal(content)
	if err != nil {
		return DailyReportSummary{}, nil, err
	}
	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inspection_daily_report_versions WHERE report_id=?`, reportID).Scan(&version); err != nil {
		return DailyReportSummary{}, nil, err
	}
	// 期望输出按版本冻结：人工重分析采用届时配置值（配置未变则与旧版本一致）。
	var expectedOutput sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT report_instructions FROM inspection_daily_report_configs WHERE config_key=?`, configKey).Scan(&expectedOutput); err != nil {
		return DailyReportSummary{}, nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO inspection_daily_report_versions(report_id,version,content,expected_output,created_at) VALUES(?,?,?,?,?)`,
		reportID, version+1, string(encoded), expectedOutput, content.SealedAt); err != nil {
		return DailyReportSummary{}, nil, err
	}
	summary, err := s.dailyReportSummaryOn(ctx, tx, reportID)
	if err != nil {
		return DailyReportSummary{}, nil, err
	}
	return summary, nil, nil
}

// ListDailyReports returns daily reports newest-first, optionally scoped to
// one config, through the read-only reader.
func (s *Service) ListDailyReports(ctx context.Context, configKey string, limit int) ([]DailyReportSummary, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	reader, err := s.readReader()
	if err != nil {
		return nil, err
	}
	query := `
		SELECT r.id FROM inspection_daily_reports r
		WHERE (?='' OR r.config_key=?)
		ORDER BY r.local_date DESC, r.id DESC LIMIT ?`
	rows, err := reader.QueryContext(ctx, query, configKey, configKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	items := []DailyReportSummary{}
	for _, id := range ids {
		summary, err := s.dailyReportSummaryOn(ctx, reader, id)
		if err != nil {
			return nil, err
		}
		items = append(items, summary)
	}
	return items, nil
}

// GetDailyReport returns one report's read model including versions and the
// latest sealed content (nil while Collecting).
func (s *Service) GetDailyReport(ctx context.Context, configKey, localDate string) (DailyReportDetail, error) {
	var detail DailyReportDetail
	var reportID int64
	reader, err := s.readReader()
	if err != nil {
		return DailyReportDetail{}, err
	}
	err = reader.QueryRowContext(ctx, `SELECT id FROM inspection_daily_reports WHERE config_key=? AND local_date=?`, configKey, localDate).Scan(&reportID)
	if errors.Is(err, sql.ErrNoRows) {
		return DailyReportDetail{}, ErrNotFound
	}
	if err != nil {
		return DailyReportDetail{}, err
	}
	summary, err := s.dailyReportSummaryOn(ctx, reader, reportID)
	if err != nil {
		return DailyReportDetail{}, err
	}
	detail.DailyReportSummary = summary
	var contributionsJSON, cutoffAt string
	if err := reader.QueryRowContext(ctx, `
		SELECT contributions_json,cutoff_at,config_row_version FROM inspection_daily_reports WHERE id=?`, reportID).
		Scan(&contributionsJSON, &cutoffAt, &detail.ConfigRowVersion); err != nil {
		return DailyReportDetail{}, err
	}
	detail.CutoffAt = cutoffAt
	detail.Contributions = []dailyContribution{}
	if err := json.Unmarshal([]byte(contributionsJSON), &detail.Contributions); err != nil {
		return DailyReportDetail{}, err
	}
	versions, err := s.ListDailyReportVersions(ctx, configKey, localDate)
	if err != nil {
		return DailyReportDetail{}, err
	}
	detail.Versions = versions
	if len(versions) > 0 {
		raw, err := s.GetDailyReportVersion(ctx, configKey, localDate, versions[0].Version)
		if err != nil {
			return DailyReportDetail{}, err
		}
		var content dailyReportContent
		if err := json.Unmarshal([]byte(raw), &content); err != nil {
			return DailyReportDetail{}, err
		}
		detail.Latest = &content
	}
	return detail, nil
}

// ListDailyReportVersions returns the immutable version entries, newest first.
func (s *Service) ListDailyReportVersions(ctx context.Context, configKey, localDate string) ([]DailyReportVersionSummary, error) {
	reader, err := s.readReader()
	if err != nil {
		return nil, err
	}
	rows, err := reader.QueryContext(ctx, `
		SELECT v.version,v.expected_output,v.created_at FROM inspection_daily_report_versions v
		JOIN inspection_daily_reports r ON r.id=v.report_id
		WHERE r.config_key=? AND r.local_date=? ORDER BY v.version DESC`, configKey, localDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []DailyReportVersionSummary{}
	for rows.Next() {
		var item DailyReportVersionSummary
		var expectedOutput sql.NullString
		if err := rows.Scan(&item.Version, &expectedOutput, &item.CreatedAt); err != nil {
			return nil, err
		}
		if expectedOutput.Valid {
			item.ExpectedOutput = &expectedOutput.String
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// GetDailyReportVersion returns one immutable version's raw content document.
func (s *Service) GetDailyReportVersion(ctx context.Context, configKey, localDate string, version int64) (string, error) {
	reader, err := s.readReader()
	if err != nil {
		return "", err
	}
	var content string
	err = reader.QueryRowContext(ctx, `
		SELECT v.content FROM inspection_daily_report_versions v
		JOIN inspection_daily_reports r ON r.id=v.report_id
		WHERE r.config_key=? AND r.local_date=? AND v.version=?`, configKey, localDate, version).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return content, nil
}

// dailyReportSummaryOn projects one report row plus its latest version.
func (s *Service) dailyReportSummaryOn(ctx context.Context, q rowQuerier, reportID int64) (DailyReportSummary, error) {
	var summary DailyReportSummary
	var sealedAt sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT id,config_key,local_date,timezone,window_start_utc,window_end_utc,trigger_kind,state,sealed_at,created_at
		FROM inspection_daily_reports WHERE id=?`, reportID).
		Scan(&summary.ID, &summary.ConfigKey, &summary.LocalDate, &summary.Timezone, &summary.WindowStartUTC,
			&summary.WindowEndUTC, &summary.TriggerKind, &summary.State, &sealedAt, &summary.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DailyReportSummary{}, ErrNotFound
	}
	if err != nil {
		return DailyReportSummary{}, err
	}
	if sealedAt.Valid {
		summary.SealedAt = &sealedAt.String
	}
	summary.ID = locatorID(reportID)
	var version sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MAX(version) FROM inspection_daily_report_versions WHERE report_id=?`, reportID).Scan(&version); err != nil {
		return DailyReportSummary{}, err
	}
	summary.LatestVersion = version.Int64
	return summary, nil
}
