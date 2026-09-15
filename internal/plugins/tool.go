package plugins

// The compiled model-tool implementation contract (ADR-0004). ToolDef is the
// one authority for a tool's execution contract: the descriptor declaration
// (Tool) is DERIVED from it, so declaration and implementation cannot drift
// by construction, and the frozen per-attempt catalogs, the implementation
// lookup and the executing hosts' dispatch tables all assemble from the same
// registry-visible set of ToolDefs. The type lives beside the plugin
// contract so a plugin package can own its ToolDefs without importing any
// control-plane package.

import "sort"

// ToolDef is one fixed, compiled model tool implementation. The zero-value
// validators (ValidateArguments/ValidateResult) are optional ingress hooks;
// the registry assembly verifies every descriptor declaration against the
// ToolDef it claims, so a declaration can never advertise a tool nobody
// compiles.
type ToolDef struct {
	Name             string
	Version          string
	ExecutionMode    string // worker_local | supervisor_typed | quoin_browser
	FailureMode      string // return_to_model | fail_attempt
	ResultSchemaKind string // exact ResultPayload.schema_kind accepted at runtime ingress
	Description      string
	// Arguments lists the accepted top-level argument keys with their
	// required kind; "required" keys must be present.
	Arguments map[string]ArgumentKind
	Required  []string
	// ProducesEvidence marks a supervisor_typed observation tool: a
	// succeeded execution commits deterministic Evidence together with
	// the Tool Call terminal state (ARCH-TOOL-005, DATA-EVIDENCE-001).
	ProducesEvidence bool
	// RequiresConnectionGrant marks a tool whose authorization freezes
	// connection grants inside the Tool Call persistence transaction
	// (ARCH-INPUT-003); the model never selects the connection.
	RequiresConnectionGrant bool
	// Parameters, when non-nil, is the complete frozen provider-facing JSON
	// Schema. It is reserved for closed union-shaped tools whose arguments
	// cannot be represented by the simple string/number map above.
	Parameters map[string]any
	// ValidateArguments is the matching Quoin-side ingress validator for
	// Parameters. It must reject unknown fields and unsupported union members.
	ValidateArguments func([]byte) error
	// ValidateResult, when set, is the ingress validator for the tool's
	// sealed result payload (dispatched by ResultSchemaKind).
	ValidateResult func([]byte) error
}

// ArgumentKind is the JSON kind of one argument.
type ArgumentKind string

const (
	KindString ArgumentKind = "string"
	KindNumber ArgumentKind = "number"
)

// executionModeLocation maps the compiled execution modes onto the closed
// descriptor execution-location vocabulary.
var executionModeLocation = map[string]ExecutionLocation{
	"worker_local":     LocationWorkerLocal,
	"supervisor_typed": LocationPlinthSupervisor,
	"quoin_browser":    LocationLintel,
}

// LocationExecutionModes maps the descriptor execution-location vocabulary
// onto the compiled ToolDef execution modes. A location without a mapping
// cannot be served by this generation's compiled tools.
func LocationExecutionModes(location ExecutionLocation) string {
	switch location {
	case LocationWorkerLocal:
		return "worker_local"
	case LocationPlinthSupervisor:
		return "supervisor_typed"
	case LocationLintel:
		return "quoin_browser"
	default:
		return ""
	}
}

// DescriptorTool projects one compiled implementation into its registry
// declaration. The declaration and the implementation stay one authority:
// registration/boot verification rejects any declaration that does not
// agree with the ToolDef it was derived from.
func (def ToolDef) DescriptorTool() (Tool, error) {
	location, ok := executionModeLocation[def.ExecutionMode]
	if !ok {
		return Tool{}, ErrInvalidDescriptor
	}
	return Tool{
		Name: def.Name, Version: def.Version, ExecutionLocation: location,
		FailureMode: def.FailureMode, Description: def.Description,
		Parameters: def.ProviderParameters(),
	}, nil
}

// ProviderParameters renders the complete provider-facing parameter schema
// of one compiled definition (the shared derivation of every catalog
// rendering).
func (def ToolDef) ProviderParameters() map[string]any {
	if def.Parameters != nil {
		return def.Parameters
	}
	properties := map[string]any{}
	for key, kind := range def.Arguments {
		properties[key] = map[string]any{"type": string(kind)}
	}
	required := make([]string, 0, len(def.Required))
	required = append(required, def.Required...)
	sort.Strings(required)
	return map[string]any{"type": "object", "properties": properties, "required": required}
}
