package plugins

// Pure-data payload types of the plugin contract (ADR-0004). They describe
// what a plugin offers and what a runtime call carries; they never carry
// secrets (secret material travels only through Call.SecretRefs/Secrets) and
// never carry host handles, so descriptors stay serializable and safe for
// any process to hold.

import (
	"context"
	"encoding/json"
	"time"
)

// Tool is the declarative description of one model tool a plugin offers.
// It is DERIVED from the plugin's compiled ToolDef (DescriptorTool), so
// there is no second tool protocol: the provider-facing schema rendering
// and the digest remain owned by the frozen catalog assembly, and
// registration/boot verification rejects any declaration that does not
// agree with the compiled implementation.
type Tool struct {
	// Name is the globally unique tool name vocabulary ([a-z][a-z0-9_]*).
	// Identical tool contracts may share one name across plugins (shared
	// contract reuse is allowed); only divergent contracts under the same
	// name are a registration conflict.
	Name string
	// Version is the tool contract generation (advances independently of the
	// plugin version).
	Version string
	// ExecutionLocation names the process that actually executes the tool.
	ExecutionLocation ExecutionLocation
	// FailureMode is the closed runtime vocabulary ("return_to_model" keeps
	// the attempt alive on tool failure; "fail_attempt" terminates it).
	FailureMode string
	// Description is the model-facing text rendered into the provider schema.
	Description string
	// Parameters, when non-nil, is the tool's closed provider-facing
	// parameter JSON Schema (top-level object, additionalProperties
	// disabled). Execution-binding hosts verify a registering bundle's
	// schema against the compiled implementation with it.
	Parameters map[string]any
}

// InspectionTemplate is the declarative description of one versioned
// deterministic collection template. Templates describe what a Run collects;
// execution happens through a bound Collector, never by re-interpreting the
// description.
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

// ProbeRequest bounds one real instance probe. The probed instance and its
// frozen settings/secrets travel in Call.
type ProbeRequest struct {
	// Timeout bounds the whole probe; zero means the host default.
	Timeout time.Duration
}

// ProbeResult reports the observed reachability of one configured instance.
// It carries non-secret facts only; failure detail must never include
// credential material.
type ProbeResult struct {
	Reachable bool
	LatencyMS int64
	// Detail is a bounded, non-secret human explanation for failures.
	Detail string
}

// DiscoverObject declares one bounded object type a plugin's Discoverer can
// observe. The plugin owns this metadata: the control plane freezes it into
// each observation attempt and the executing host validates the frozen copy
// against it, so a declaration can never silently diverge from execution.
type DiscoverObject struct {
	// ObjectType is the plugin's discovered object vocabulary (e.g. "target").
	ObjectType string
	// IdentityLabels names the source identity label set; the executing
	// adapter derives each object's canonical identity from exactly these
	// labels and errors when a series lacks one of them.
	IdentityLabels []string
	// Query is the plugin-owned canonical discovery selection (for metrics
	// plugins the PromQL vector expression bounding the pass).
	Query string
	// Limit is the hard per-pass result budget; a truncated pass reports
	// incompleteness instead of fabricating absence.
	Limit int
}

// DiscoverRequest bounds one object-discovery pass to an object type and a
// hard result budget. Discovery never widens its scope beyond the Call's
// connection boundary.
type DiscoverRequest struct {
	// ObjectType selects the discovered object kind (plugin-defined
	// vocabulary, e.g. "metric", "namespace").
	ObjectType string
	// Limit is the maximum number of objects to return.
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

// ToolRequest carries one authorized tool invocation. Name must be one of
// the plugin's declared tools; ArgumentsJSON must satisfy the tool's frozen
// parameter schema.
type ToolRequest struct {
	Name          string
	ArgumentsJSON json.RawMessage
	// Workspace, when non-nil, is the executing host's per-call execution
	// environment for tools that spill long bodies into the host's
	// tool_result Artifact store. Executing hosts always provide it for
	// supervisor-side tool execution; description-only hosts never build
	// requests at all.
	Workspace *ToolWorkspace
}

// ToolWorkspace is the bounded host seam one spill-capable tool execution
// may use. Dir is the attempt's one-shot workspace on the executing host
// (transient spill staging, never a durable locator); UploadFile streams a
// staged file into the host's tool_result Artifact store and returns the
// committed artifact id.
type ToolWorkspace struct {
	Dir        string
	AttemptID  int64
	ToolCallID int64
	UploadFile func(ctx context.Context, path, mediaType string) (int64, error)
}

// ToolResult is the closed, non-secret tool outcome sealed as Evidence by
// the executing host. Payload shape is the tool's result schema kind.
type ToolResult struct {
	Success bool
	Payload json.RawMessage
	// ArtifactID, when non-zero, is the long-body Artifact the executing
	// host committed for this result; the host seals the locator together
	// with the payload.
	ArtifactID int64
	// ErrorCode/ErrorDetail are set when Success is false; Detail is bounded
	// and must not leak credentials or endpoint internals.
	ErrorCode   string
	ErrorDetail string
}

// CollectScopeKind is the closed evidence-scope vocabulary of one collection
// pass. It mirrors the control plane's frozen scope kind; the zero value is
// not a member — collectors must reject it (fail closed) so a legacy input
// can never silently run unscoped.
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
	// plane). Mandatory for businessView/objects kinds — collectors must
	// fail closed when missing, never fall back to parsing identities.
	LabelConditions map[string]string
}

// CollectRequest binds one deterministic collection pass to a frozen
// template version, frozen template parameters, the frozen observation
// instant and a frozen target set. Every field is part of the Run's frozen
// execution input; collectors must not read any other configuration.
type CollectRequest struct {
	TemplateID      string
	TemplateVersion string
	// Params is the frozen typed template parameter object (for the metrics
	// plugins: the PromQL expression and, for range templates, the window
	// seconds). Collectors validate it against the declared template.
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
