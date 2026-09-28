// Package agentcontext defines the model-visible facts shared by Quoin's
// frozen input producers and Plinth's prompt renderers. Execution metadata
// (grants, tool catalogs, model budgets and internal locators) stays in each
// attempt's envelope instead of being implicitly exposed to the model.
package agentcontext

type Integration struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type Correlation struct {
	ViewKey     string `json:"viewKey"`
	DisplayName string `json:"displayName"`
}

type RelatedAlert struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	State    string `json:"state"`
	StartsAt string `json:"startsAt"`
}

// Occurrence is the full model-facing first-observation projection. Keep the
// JSON tags stable: Quoin rebuilds digested snapshots from immutable facts.
type Occurrence struct {
	ID              string            `json:"id"`
	State           string            `json:"state"`
	Severity        string            `json:"severity"`
	Title           string            `json:"title"`
	Resource        string            `json:"resource,omitempty"`
	FirstSeenAt     string            `json:"firstSeenAt"`
	LastStateChange string            `json:"lastStateChangeAt"`
	ResolvedAt      *string           `json:"resolvedAt,omitempty"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	Enrichment      map[string]string `json:"enrichment,omitempty"`
	Correlations    []Correlation     `json:"correlations,omitempty"`
	RelatedAlerts   []RelatedAlert    `json:"relatedAlerts,omitempty"`
}

type RecentOccurrence struct {
	ID        string            `json:"id"`
	Severity  string            `json:"severity"`
	Title     string            `json:"title"`
	SourceKey string            `json:"sourceKey"`
	StartsAt  string            `json:"startsAt"`
	Labels    map[string]string `json:"labels"`
}

// InspectionPlan is the non-secret, frozen scope and semantics of one run.
type InspectionPlan struct {
	Key                string         `json:"key"`
	Params             map[string]any `json:"params"`
	Scope              map[string]any `json:"scope"`
	CheckDescription   *string        `json:"checkDescription,omitempty"`
	MetricUnit         *string        `json:"metricUnit,omitempty"`
	ReportInstructions *string        `json:"reportInstructions,omitempty"`
}

type InspectionCheck struct {
	CheckKey            string   `json:"checkKey"`
	DisplayName         string   `json:"displayName"`
	Status              string   `json:"status"`
	EvidenceID          *int64   `json:"evidenceId,omitempty"`
	ArtifactID          *int64   `json:"artifactId,omitempty"`
	Expression          string   `json:"expression,omitempty"`
	RangeSeconds        *int64   `json:"rangeSeconds,omitempty"`
	StepSeconds         *int64   `json:"stepSeconds,omitempty"`
	ObservedAt          string   `json:"observedAt,omitempty"`
	WindowStartAt       string   `json:"windowStartAt,omitempty"`
	WindowEndAt         string   `json:"windowEndAt,omitempty"`
	ExecutedStepSeconds *int64   `json:"executedStepSeconds,omitempty"`
	Warnings            []string `json:"warnings,omitempty"`
	GapReason           *string  `json:"gapReason,omitempty"`
}

// DailyContribution is the trigger-time frozen identity of one daily
// report's participating plan and source. Keep the JSON tags stable: the
// same shape is frozen into inspection_daily_reports.contributions_json and
// re-derived into analysis snapshots.
type DailyContribution struct {
	PlanKey         string `json:"planKey"`
	DisplayName     string `json:"displayName,omitempty"`
	ConnectionName  string `json:"connectionName,omitempty"`
	PluginID        string `json:"pluginId,omitempty"`
	TemplateID      string `json:"templateId,omitempty"`
	TemplateVersion string `json:"templateVersion,omitempty"`
	Enabled         bool   `json:"enabled"`
	SourceEnabled   bool   `json:"sourceEnabled"`
	Missing         bool   `json:"missing,omitempty"`
}

// DailyCheckItem is one per-check fact inside a sealed daily source report.
// Gaps keep their reason and observation time when the collection recorded
// one; absence of data is listed, never filled in. EvidenceID locates the
// immutable Evidence row for exact citation; Measurement is its bounded
// deterministic summary (never the raw payload).
type DailyCheckItem struct {
	RunID       int64             `json:"runId"`
	CheckKey    string            `json:"checkKey"`
	Status      string            `json:"status"`
	GapReason   *string           `json:"gapReason,omitempty"`
	ObservedAt  *string           `json:"observedAt,omitempty"`
	EvidenceID  *int64            `json:"evidenceId,omitempty"`
	Measurement *DailyMeasurement `json:"measurement,omitempty"`
}

// DailyMeasurement is the bounded projection of one check's immutable
// Evidence result: result type, series/sample counts, the first/last sample
// values with the last sample's timestamp, and per-series bounded entries
// (labels + numeric extremes) so a middle-series anomalous value and the
// labels that identify it stay analyzable without the raw payload. It is a
// deterministic summary derived from committed evidence — never a truncated
// raw payload and never a secret carrier; series beyond the entry bound are
// marked truncated and the full result stays locatable by evidenceId.
// Field order is part of the frozen-byte contract: new fields append AFTER
// the established ones so previously sealed documents round-trip identically
// through decode→marshal.
type DailyMeasurement struct {
	ResultType string  `json:"resultType"`
	Series     int     `json:"series"`
	Samples    int     `json:"samples"`
	FirstValue *string `json:"firstValue,omitempty"`
	LastValue  *string `json:"lastValue,omitempty"`
	LastAt     *string `json:"lastAt,omitempty"`
	// Entries 是逐序列的有界摘要（按结果数组的确定性顺序，取前
	// DailyMeasurementEntryBound 个）；Truncated 为真表示其余序列被省略。
	Entries   []DailyMeasurementSeriesEntry `json:"entries,omitempty"`
	Truncated bool                          `json:"truncated,omitempty"`
}

// DailyMeasurementEntryBound 是一个检查项测量摘要携带的逐序列条目上限：
// 超出按确定性顺序省略并以 truncated 标记，摘要保持有界。
const DailyMeasurementEntryBound = 32

// DailyMeasurementLabelBound 是一个序列条目携带的标签数上限：超出按键的
// 确定性字典序省略并以 labelsTruncated 标记。
const DailyMeasurementLabelBound = 16

// DailyMeasurementSeriesEntry is one series' bounded summary: the PromQL
// label set (bounded), its sample count, first/last values and — when the
// values are numeric — the min/max extremes with their timestamps, so an
// intermediate spike or dip is visible without every sample.
type DailyMeasurementSeriesEntry struct {
	Labels          map[string]string `json:"labels,omitempty"`
	LabelsTruncated bool              `json:"labelsTruncated,omitempty"`
	Samples         int               `json:"samples"`
	FirstValue      *string           `json:"firstValue,omitempty"`
	LastValue       *string           `json:"lastValue,omitempty"`
	LastAt          *string           `json:"lastAt,omitempty"`
	MinValue        *string           `json:"minValue,omitempty"`
	MinAt           *string           `json:"minAt,omitempty"`
	MaxValue        *string           `json:"maxValue,omitempty"`
	MaxAt           *string           `json:"maxAt,omitempty"`
}

// DailySourceReport is the sealed aggregation for one contributing plan: the
// frozen contribution identity plus the explicit outcome.
type DailySourceReport struct {
	DailyContribution
	Status     string           `json:"status"`
	GapReasons []string         `json:"gapReasons,omitempty"`
	Checks     []DailyCheckItem `json:"checks,omitempty"`
}

// DailyTotals is the coarse sealed roll-up over sources and checks.
type DailyTotals struct {
	ChecksOK    int `json:"checksOk"`
	ChecksGap   int `json:"checksGap"`
	ChecksError int `json:"checksError"`
	SourcesGap  int `json:"sourcesGap"`
}
