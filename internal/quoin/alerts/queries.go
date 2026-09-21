package alerts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// Queries owns the unified alert read projections. Upstream occurrences and
// platform faults retain distinct storage and lifecycles but share this safe,
// non-secret representation for the existing alert list and detail surfaces.

// AlertCorrelation 是一条冻结的视图关联证据（ADR-0012 Correlate 段）：首观测
// 时命中的业务视图身份快照，视图改名/退役后不漂移。
type AlertCorrelation struct {
	ViewKey     string `json:"viewKey"`
	DisplayName string `json:"displayName"`
}

// AlertEnrichment 是首观测冻结的富化投影（ADR-0012 Enrich 段）：fields 是
// 按规则 priority 升序叠加、后命中不覆盖的终值；ruleKeys 是命中规则的溯源
// key 列表（求值序），仅详情面返回，列表只投影 fields。
type AlertEnrichment struct {
	Fields   map[string]string `json:"fields"`
	RuleKeys []string          `json:"ruleKeys,omitempty"`
}

type OccurrenceSummary struct {
	// ID is an occurrence locator or platform:<fault-id>; Source is mandatory so
	// consumers cannot mistake an internal fault for an Alertmanager occurrence.
	ID              string             `json:"id"`
	Source          string             `json:"source"`
	State           string             `json:"state"`
	RowVersion      int64              `json:"rowVersion"`
	Severity        string             `json:"severity"`
	Title           string             `json:"title"`
	Resource        string             `json:"resource,omitempty"`
	Component       string             `json:"component,omitempty"`
	Reason          string             `json:"reason,omitempty"`
	FirstSeenAt     string             `json:"firstSeenAt"`
	LastStateChange string             `json:"lastStateChangeAt"`
	ResolvedAt      *string            `json:"resolvedAt,omitempty"`
	Labels          map[string]string  `json:"labels"`
	Annotations     map[string]string  `json:"annotations,omitempty"`
	Correlations    []AlertCorrelation `json:"correlations"`
	Enrichment      *AlertEnrichment   `json:"enrichment,omitempty"`
}

type AlertSnapshot struct {
	SnapshotSeq int64               `json:"snapshotSeq"`
	Items       []OccurrenceSummary `json:"items"`
	NextCursor  string              `json:"nextCursor,omitempty"`
}

// occurrenceSummaryRow 是 occurrence 列表/详情共享的行扫描形状：统一语义列
// (severity/title/resource) 与 labels/annotations 快照均来自 alert_occurrences
// 自身，不再从交付 body 现算 annotations，也不再投影任何业务系统归属。
func occurrenceSummaryRow(id int64, state string, version int64, severity, title, resource, first, changed string, resolved sql.NullString, labelsJSON, annotationsJSON string) (OccurrenceSummary, error) {
	summary := OccurrenceSummary{
		ID: strconv.FormatInt(id, 10), Source: "alertmanager", State: state, RowVersion: version,
		Severity: severity, Title: title, Resource: resource,
		FirstSeenAt: first, LastStateChange: changed, Correlations: []AlertCorrelation{},
	}
	if resolved.Valid {
		value := resolved.String
		summary.ResolvedAt = &value
	}
	if err := json.Unmarshal([]byte(labelsJSON), &summary.Labels); err != nil {
		return OccurrenceSummary{}, err
	}
	if annotationsJSON != "" && annotationsJSON != "{}" {
		if err := json.Unmarshal([]byte(annotationsJSON), &summary.Annotations); err != nil {
			return OccurrenceSummary{}, err
		}
	}
	return summary, nil
}

// platformFaultSummary 从 platform_faults 自身的语义列投影统一摘要
// (ADR-0012：入库侧已冻结 severity/title/annotations，读侧不再伪造)。
func platformFaultSummary(id int64, component, reason, state string, version int64, severity, title, first, last string, resolved sql.NullString, annotationsJSON string) OccurrenceSummary {
	// Repeat disconnects only advance last_seen_at and emit no change-log event.
	// Sort unified rows by a state transition time so those diagnostic repeats
	// cannot reorder a client view without a corresponding SSE notification.
	lifecycleAt := first
	if state == "Resolved" && resolved.Valid {
		lifecycleAt = resolved.String
	}
	summary := OccurrenceSummary{
		ID: "platform:" + strconv.FormatInt(id, 10), Source: "platform", State: state, RowVersion: version,
		Severity: severity, Title: title, Component: component, Reason: reason,
		FirstSeenAt: first, LastStateChange: lifecycleAt,
		Labels: map[string]string{}, Correlations: []AlertCorrelation{},
	}
	if annotationsJSON != "" && annotationsJSON != "{}" {
		// 语义列由 platformFaultSemantics 以确定形状写入；解析失败保持缺省。
		_ = json.Unmarshal([]byte(annotationsJSON), &summary.Annotations)
	}
	if resolved.Valid {
		value := resolved.String
		summary.ResolvedAt = &value
	}
	return summary
}

// occurrenceColumns 是两处 occurrence 查询共享的投影列表（不含 o.id：列表
// 查询自行前置，详情查询以路径 locator 定位）。
const occurrenceColumns = `o.state,o.row_version,o.severity,o.title,o.resource,o.first_seen_at,o.last_state_change_at,o.resolved_at,o.labels_canonical,o.annotations_canonical`

// loadCorrelations 按 matched_at 稳定序读取一批 occurrence 的关联证据；
// occurrenceIDs 为空时直接返回空索引。
func loadCorrelations(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, occurrenceIDs []int64) (map[int64][]AlertCorrelation, error) {
	index := map[int64][]AlertCorrelation{}
	if len(occurrenceIDs) == 0 {
		return index, nil
	}
	placeholders := strings.Repeat("?,", len(occurrenceIDs))
	args := make([]any, 0, len(occurrenceIDs))
	for _, id := range occurrenceIDs {
		args = append(args, id)
	}
	rows, err := q.QueryContext(ctx, `SELECT occurrence_id, view_key, display_name FROM alert_occurrence_correlations WHERE occurrence_id IN (`+placeholders[:len(placeholders)-1]+`) ORDER BY occurrence_id, matched_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var occurrenceID int64
		var correlation AlertCorrelation
		if err := rows.Scan(&occurrenceID, &correlation.ViewKey, &correlation.DisplayName); err != nil {
			return nil, err
		}
		index[occurrenceID] = append(index[occurrenceID], correlation)
	}
	return index, rows.Err()
}

// enrichmentDocumentRow 是 alert_enrichments 冻结文档的读取形状；withRuleKeys
// 控制是否投影规则溯源（详情面专用，列表只给 fields）。
func enrichmentProjection(documentJSON string, withRuleKeys bool) *AlertEnrichment {
	var document struct {
		Fields map[string]string `json:"fields"`
		Rules  []struct {
			Key string `json:"key"`
		} `json:"rules"`
	}
	if err := json.Unmarshal([]byte(documentJSON), &document); err != nil {
		// 冻结文档由本包写入且经 CHECK 约束；解析失败按空富化降级而非断读。
		return &AlertEnrichment{Fields: map[string]string{}}
	}
	if document.Fields == nil {
		document.Fields = map[string]string{}
	}
	projection := &AlertEnrichment{Fields: document.Fields}
	if withRuleKeys {
		projection.RuleKeys = []string{}
		for _, rule := range document.Rules {
			projection.RuleKeys = append(projection.RuleKeys, rule.Key)
		}
	}
	return projection
}

// loadEnrichments 读取一批 occurrence 的富化 fields（列表投影，不带溯源）。
func loadEnrichments(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, occurrenceIDs []int64) (map[int64]*AlertEnrichment, error) {
	index := map[int64]*AlertEnrichment{}
	if len(occurrenceIDs) == 0 {
		return index, nil
	}
	placeholders := strings.Repeat("?,", len(occurrenceIDs))
	args := make([]any, 0, len(occurrenceIDs))
	for _, id := range occurrenceIDs {
		args = append(args, id)
	}
	rows, err := q.QueryContext(ctx, `SELECT occurrence_id, enrichment_json FROM alert_enrichments WHERE occurrence_id IN (`+placeholders[:len(placeholders)-1]+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var occurrenceID int64
		var documentJSON string
		if err := rows.Scan(&occurrenceID, &documentJSON); err != nil {
			return nil, err
		}
		index[occurrenceID] = enrichmentProjection(documentJSON, false)
	}
	return index, rows.Err()
}

// AlertSnapshot returns occurrences and independently durable platform faults.
// Platform rows intentionally ignore viewKey: no business view was matched and
// filtering them beside a view-scoped request would imply fabricated
// correlation. viewKey filters by the frozen occurrence correlations
// (ADR-0012): an occurrence is in scope when any frozen correlation row names
// that view key.
//
// The watermark query and both unions run inside ONE read-only snapshot
// transaction opened on the trusted execution.Reader (BeginSnapshot), so the
// returned SnapshotSeq and items always describe the same committed state and
// every statement degrades together: any failure rolls the whole snapshot
// back, and only a clean read reaches the single Commit.
func (service *Service) AlertSnapshot(ctx context.Context, state string, viewKey string) (AlertSnapshot, error) {
	if state != "Firing" && state != "Resolved" {
		state = "Firing"
	}
	snapshot, err := service.runner.Reader().BeginSnapshot(ctx)
	if err != nil {
		return AlertSnapshot{}, err
	}
	defer snapshot.Rollback()
	var seq int64
	if err := snapshot.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM alert_change_log`).Scan(&seq); err != nil {
		return AlertSnapshot{}, err
	}
	conditions, args := `o.state=?`, []any{state}
	if viewKey != "" {
		conditions += ` AND EXISTS(SELECT 1 FROM alert_occurrence_correlations c WHERE c.occurrence_id=o.id AND c.view_key=?)`
		args = append(args, viewKey)
	}
	rows, err := snapshot.QueryContext(ctx, `SELECT o.id,`+occurrenceColumns+`
			FROM alert_occurrences o
			WHERE `+conditions, args...)
	if err != nil {
		return AlertSnapshot{}, err
	}
	items := []OccurrenceSummary{}
	occurrenceIDs := []int64{}
	for rows.Next() {
		var id, version int64
		var summaryState, severity, title, resource, first, changed, labels, annotations string
		var resolved sql.NullString
		if err := rows.Scan(&id, &summaryState, &version, &severity, &title, &resource, &first, &changed, &resolved, &labels, &annotations); err != nil {
			rows.Close()
			return AlertSnapshot{}, err
		}
		summary, err := occurrenceSummaryRow(id, summaryState, version, severity, title, resource, first, changed, resolved, labels, annotations)
		if err != nil {
			rows.Close()
			return AlertSnapshot{}, err
		}
		items = append(items, summary)
		occurrenceIDs = append(occurrenceIDs, id)
	}
	if err := rows.Close(); err != nil {
		return AlertSnapshot{}, err
	}
	// 关联与富化在同一只读快照内补齐，保证与列表行同刻一致。
	correlations, err := loadCorrelations(ctx, snapshot, occurrenceIDs)
	if err != nil {
		return AlertSnapshot{}, err
	}
	enrichments, err := loadEnrichments(ctx, snapshot, occurrenceIDs)
	if err != nil {
		return AlertSnapshot{}, err
	}
	for index := range items {
		id, _ := strconv.ParseInt(items[index].ID, 10, 64)
		items[index].Correlations = correlations[id]
		if items[index].Correlations == nil {
			items[index].Correlations = []AlertCorrelation{}
		}
		if enrichment, ok := enrichments[id]; ok {
			items[index].Enrichment = enrichment
		}
	}
	// Platform faults join the unified list only on unfiltered reads: the view
	// correlation filter scopes to Alertmanager occurrences, and listing them
	// beside a view-scoped request would imply fabricated correlation.
	if viewKey == "" {
		faultRows, err := snapshot.QueryContext(ctx, `SELECT id,component,reason,state,row_version,severity,title,first_seen_at,last_seen_at,resolved_at,annotations_canonical FROM platform_faults WHERE state=?`, state)
		if err != nil {
			return AlertSnapshot{}, err
		}
		for faultRows.Next() {
			var id, version int64
			var component, reason, faultState, severity, title, first, last string
			var resolved sql.NullString
			var annotations string
			if err := faultRows.Scan(&id, &component, &reason, &faultState, &version, &severity, &title, &first, &last, &resolved, &annotations); err != nil {
				faultRows.Close()
				return AlertSnapshot{}, err
			}
			items = append(items, platformFaultSummary(id, component, reason, faultState, version, severity, title, first, last, resolved, annotations))
		}
		if err := faultRows.Close(); err != nil {
			return AlertSnapshot{}, err
		}
	}
	// Lexical RFC3339Nano order is chronological UTC order; this keeps the union
	// deterministic without collapsing its source identities.
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].LastStateChange == items[j].LastStateChange {
			return items[i].ID > items[j].ID
		}
		return items[i].LastStateChange > items[j].LastStateChange
	})
	if err := snapshot.Commit(); err != nil {
		return AlertSnapshot{}, err
	}
	return AlertSnapshot{SnapshotSeq: seq, Items: items}, nil
}

// GetAlert resolves either member of the explicit unified alert union. 详情
// 面的富化投影附带 ruleKeys 溯源（求值序）；关联按 matched_at 稳定序返回。
func (service *Service) GetAlert(ctx context.Context, alertID string) (OccurrenceSummary, error) {
	if strings.HasPrefix(alertID, "platform:") {
		id, err := strconv.ParseInt(strings.TrimPrefix(alertID, "platform:"), 10, 64)
		if err != nil || id <= 0 {
			return OccurrenceSummary{}, sql.ErrNoRows
		}
		var component, reason, state, severity, title, first, last, annotations string
		var version int64
		var resolved sql.NullString
		err = service.runner.Reader().QueryRowContext(ctx, `SELECT component,reason,state,row_version,severity,title,first_seen_at,last_seen_at,resolved_at,annotations_canonical FROM platform_faults WHERE id=?`, id).
			Scan(&component, &reason, &state, &version, &severity, &title, &first, &last, &resolved, &annotations)
		if err != nil {
			return OccurrenceSummary{}, err
		}
		return platformFaultSummary(id, component, reason, state, version, severity, title, first, last, resolved, annotations), nil
	}
	id, err := strconv.ParseInt(alertID, 10, 64)
	if err != nil || id <= 0 {
		return OccurrenceSummary{}, sql.ErrNoRows
	}
	var summaryState, severity, title, resource, first, changed, labels, annotations string
	var version int64
	var resolved sql.NullString
	err = service.runner.Reader().QueryRowContext(ctx, `SELECT `+occurrenceColumns+`
		FROM alert_occurrences o
		WHERE o.id=?`, id).Scan(&summaryState, &version, &severity, &title, &resource, &first, &changed, &resolved, &labels, &annotations)
	if err != nil {
		return OccurrenceSummary{}, err
	}
	summary, err := occurrenceSummaryRow(id, summaryState, version, severity, title, resource, first, changed, resolved, labels, annotations)
	if err != nil {
		return OccurrenceSummary{}, err
	}
	correlationRows, err := service.runner.Reader().QueryContext(ctx, `SELECT view_key, display_name FROM alert_occurrence_correlations WHERE occurrence_id=? ORDER BY matched_at, id`, id)
	if err != nil {
		return OccurrenceSummary{}, err
	}
	defer correlationRows.Close()
	for correlationRows.Next() {
		var correlation AlertCorrelation
		if err := correlationRows.Scan(&correlation.ViewKey, &correlation.DisplayName); err != nil {
			return OccurrenceSummary{}, err
		}
		summary.Correlations = append(summary.Correlations, correlation)
	}
	if err := correlationRows.Err(); err != nil {
		return OccurrenceSummary{}, err
	}
	var documentJSON string
	if err := service.runner.Reader().QueryRowContext(ctx, `SELECT enrichment_json FROM alert_enrichments WHERE occurrence_id=?`, id).Scan(&documentJSON); err == nil {
		summary.Enrichment = enrichmentProjection(documentJSON, true)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return OccurrenceSummary{}, err
	}
	return summary, nil
}

// GetOccurrence preserves the upstream-only public domain seam for callers
// which genuinely require an Alertmanager occurrence.
func (service *Service) GetOccurrence(ctx context.Context, occurrenceID int64) (OccurrenceSummary, error) {
	return service.GetAlert(ctx, strconv.FormatInt(occurrenceID, 10))
}

type Observation struct {
	ID            string  `json:"id"`
	ObservedState string  `json:"observedState"`
	StartsAt      string  `json:"startsAt"`
	EndsAt        *string `json:"endsAt,omitempty"`
	ReceivedAt    string  `json:"receivedAt"`
	CommittedAt   string  `json:"committedAt"`
	Effect        string  `json:"effect"`
}

// ListObservations remains upstream-only because platform fault history is its
// first/last/recovery lifecycle, not invented Alertmanager observations.
func (service *Service) ListObservations(ctx context.Context, alertID string) ([]Observation, error) {
	if strings.HasPrefix(alertID, "platform:") {
		return []Observation{}, nil
	}
	occurrenceID, err := strconv.ParseInt(alertID, 10, 64)
	if err != nil || occurrenceID <= 0 {
		return nil, sql.ErrNoRows
	}
	rows, err := service.runner.Reader().QueryContext(ctx, `SELECT id, observed_state, starts_at_source, ends_at_source, received_at, committed_at, effect FROM alert_observations WHERE occurrence_id=? ORDER BY committed_at DESC,id DESC`, occurrenceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	observations := []Observation{}
	for rows.Next() {
		var item Observation
		var id int64
		var ends sql.NullString
		if err := rows.Scan(&id, &item.ObservedState, &item.StartsAt, &ends, &item.ReceivedAt, &item.CommittedAt, &item.Effect); err != nil {
			return nil, err
		}
		item.ID = strconv.FormatInt(id, 10)
		if ends.Valid {
			value := ends.String
			item.EndsAt = &value
		}
		observations = append(observations, item)
	}
	return observations, rows.Err()
}

type IntakeIssue struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	IssueKey        string `json:"issueKey"`
	DetailJSON      string `json:"detailJson"`
	FirstSeenAt     string `json:"firstSeenAt"`
	LastSeenAt      string `json:"lastSeenAt"`
	OccurrenceCount int    `json:"occurrenceCount"`
	RowVersion      int64  `json:"rowVersion"`
}

func (service *Service) ListIntakeIssues(ctx context.Context, acknowledged bool) ([]IntakeIssue, error) {
	filter := "acknowledged_at IS NULL"
	if acknowledged {
		filter = "acknowledged_at IS NOT NULL"
	}
	rows, err := service.runner.Reader().QueryContext(ctx, `SELECT id,kind,issue_key,detail_json,first_seen_at,last_seen_at,occurrence_count,row_version FROM alert_intake_issues WHERE `+filter+` ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []IntakeIssue{}
	for rows.Next() {
		var item IntakeIssue
		var id int64
		if err := rows.Scan(&id, &item.Kind, &item.IssueKey, &item.DetailJSON, &item.FirstSeenAt, &item.LastSeenAt, &item.OccurrenceCount, &item.RowVersion); err != nil {
			return nil, err
		}
		item.ID = strconv.FormatInt(id, 10)
		items = append(items, item)
	}
	return items, rows.Err()
}

// AcknowledgeIntakeIssue is the Admin-only, one-way sticky acknowledgment of
// one intake issue, executed through the shared runner: one runner-owned
// IMMEDIATE transaction carrying the administrator session re-check, the
// fenced UPDATE and the automatic audit event (ADR-0006). The acknowledged
// actor is the verified execution-metadata principal — the caller-supplied
// actorID must match it exactly, so a caller can never acknowledge under
// someone else's identity. A stale expectedRowVersion or an already
// acknowledged issue is a recorded deterministic rejection that changes
// nothing; the caller-visible outcome stays (false, nil), the historical
// conflict shape.
func (service *Service) AcknowledgeIntakeIssue(ctx context.Context, issueID int64, actorID int64, expectedRowVersion int64, timestamp string) (bool, error) {
	meta, err := execution.Require(ctx)
	if err != nil {
		return false, err
	}
	if meta.Actor.Kind != execution.PrincipalUser || meta.Actor.ID != actorID {
		return false, fmt.Errorf("alerts: acknowledgment actor %d does not match the execution metadata principal", actorID)
	}
	_, err = execution.Execute(ctx, service.runner, service.ops.ackIntake,
		func(tx *execution.Tx) (bool, error) {
			result, err := tx.ExecContext(ctx, `UPDATE alert_intake_issues SET acknowledged_at=?,acknowledged_by=?,row_version=row_version+1 WHERE id=? AND row_version=? AND acknowledged_at IS NULL`, timestamp, meta.Actor.ID, issueID, expectedRowVersion)
			if err != nil {
				return false, err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return false, err
			}
			if affected == 0 {
				return false, &execution.Rejection{Code: CodeRowVersionConflict, Detail: "接入问题版本已变化或已确认", ObjectID: issueID}
			}
			return true, nil
		},
		func(bool) int64 { return issueID })
	if err != nil {
		var rejection *execution.Rejection
		if errors.As(err, &rejection) && rejection.Code == CodeRowVersionConflict {
			// The rejection was recorded; the caller sees the same
			// not-applied outcome it always has.
			return false, nil
		}
		return false, err
	}
	return true, nil
}
