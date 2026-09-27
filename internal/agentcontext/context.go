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
