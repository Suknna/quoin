package plugins

// Pure-data payload types of the plugin contract (ADR-0004, reworked by
// ADR-0011). They describe what a plugin offers (discovery catalogs,
// inspection templates) and the shared payload shapes its internal tools
// exchange with Quoin's schedulers. They never carry secrets and never carry
// host handles, so they stay serializable and safe for any process to hold.

import "encoding/json"

// InspectionTemplate is the declarative description of one versioned
// deterministic collection template. Templates describe what a Run collects;
// execution happens through the plugin's internal tools, never by
// re-interpreting the description.
type InspectionTemplate struct {
	// ID is stable within the plugin ([a-z][a-z0-9_-]*); (ID, Version) is
	// the frozen template identity a Run binds.
	ID string
	// Version is the template generation; editing a template ships a new
	// version instead of mutating history.
	Version string
	// Title and Description are human-facing, non-secret text.
	Title       string
	Description string
}

// DiscoverObject declares one bounded object type a plugin can observe. The
// plugin owns this metadata: the control plane freezes it into each
// observation run and the plugin's internal discovery tool validates the
// frozen copy against it, so a declaration can never silently diverge from
// execution.
type DiscoverObject struct {
	// ObjectType is the plugin's discovered object vocabulary (e.g. "target").
	ObjectType string
	// IdentityLabels names the source identity label set; the discovery tool
	// derives each object's canonical identity from exactly these labels and
	// errors when a series lacks one of them.
	IdentityLabels []string
	// Query is the plugin-owned canonical discovery selection (for metrics
	// plugins the PromQL vector expression bounding the pass).
	Query string
	// Limit is the hard per-pass result budget; a truncated pass reports
	// incompleteness instead of fabricating absence.
	Limit int
}

// DiscoveredObject is one object observed at a point in time. CanonicalIdentity
// is the plugin's stable source identity; observation identity is the triple
// (source connection, ObjectType, CanonicalIdentity) — same-named objects
// from different sources are never merged.
type DiscoveredObject struct {
	ObjectType        string
	CanonicalIdentity string
	DisplayName       string
}

// DiscoverResult reports what was observed including incompleteness. A
// partial or truncated pass sets Incomplete: absence is only meaningful
// from a complete observation, never fabricated by discovery.
type DiscoverResult struct {
	Objects    []DiscoveredObject
	Incomplete bool
}

// CollectScopeKind is the closed evidence-scope vocabulary of one collection
// pass. The zero value is not a member — collection must reject it (fail
// closed) so a legacy input can never silently run unscoped.
type CollectScopeKind string

const (
	// ScopeIntegration: the whole source connection is the scope; the frozen
	// user-typed expression runs as-is (explicitly unscoped).
	ScopeIntegration CollectScopeKind = "integration"
	// ScopeBusinessView: evidence is bounded by the frozen business-view
	// label conditions carried on the target.
	ScopeBusinessView CollectScopeKind = "businessView"
	// ScopeObjects: evidence is bounded by the frozen object label facts
	// carried on each target.
	ScopeObjects CollectScopeKind = "objects"
)

// CollectScope is the frozen scope kind of one collection pass. Conditions
// themselves live on the targets (single home, mirroring the frozen dispatch
// input); the kind only decides whether they are mandatory.
type CollectScope struct {
	Kind CollectScopeKind
}

// CollectTarget is one frozen inspection target.
type CollectTarget struct {
	ObjectType        string
	CanonicalIdentity string
	// LabelConditions is the frozen exact label=value map this target
	// contributes to evidence scoping (businessView: the view conditions;
	// objects: the object's identity label facts extracted by the control
	// plane). Mandatory for businessView/objects kinds — collection must
	// fail closed when missing, never fall back to parsing identities.
	LabelConditions map[string]string
}

// CollectRequest binds one deterministic collection pass to a frozen
// template version, frozen template parameters, the frozen observation
// instant and a frozen target set. Every field is part of the Run's frozen
// execution input; the plugin's internal collection tool must not read any
// other configuration.
type CollectRequest struct {
	TemplateID      string
	TemplateVersion string
	// Params is the frozen typed template parameter object (for the metrics
	// plugins: the PromQL expression and, for range templates, the window
	// seconds).
	Params json.RawMessage
	// EvidenceAt is the RFC3339 observation instant the template executes
	// against; range templates end their window here.
	EvidenceAt string
	// Scope is the frozen evidence-scope kind; businessView/objects make the
	// targets' LabelConditions mandatory narrowing facts.
	Scope   CollectScope
	Targets []CollectTarget
}

// CheckObservation is the deterministic per-check outcome. EvidenceJSON is
// the persisted fact; model report analysis consumes it but never rewrites
// it. A missing metric is an observation, never an implicit pass.
type CheckObservation struct {
	CheckID      string
	Succeeded    bool
	Detail       string
	EvidenceJSON []byte
}

// CollectResult reports the collected facts. Incomplete marks a pass that
// could not finish every check: consumers must treat the run as partial
// instead of assuming health.
type CollectResult struct {
	Checks     []CheckObservation
	Incomplete bool
}
