// Package plugins owns the shared plugin contract: plugin descriptors, the
// model-tool contract, the optional execution interfaces and the explicit
// registry (ADR-0004).
//
// The contract is free of Quoin business dependencies so Plinth, Quoin and
// future hosts can link it without pulling in the control plane.
// It splits cleanly along the control-plane / runtime boundary:
//
//   - Descriptors are pure data (identity, capabilities, tools, templates,
//     config shape). Any host may load them; Quoin owns the authoritative
//     catalog. Descriptors never carry secrets and never require an
//     executor, so a describing host is never forced into a fake one.
//   - ExecutionBundle is the real, outbound implementation one host binds
//     for a plugin at one execution location. Only hosts that truly execute
//     register bindings; a capability without a bound implementation is
//     simply not executable in that process.
//   - Cross-runtime agreement is verified by contract digest
//     (ToolCatalogDigest), not by sharing objects.
//
// Plugins are registered explicitly at compile time — no dynamic .so
// loading, no third-party online install. The registry enforces each side of
// the description/execution boundary at its own door: RegisterDescriptor
// accepts a pure description (any host may hold one), RegisterBundle rejects
// an execution whose descriptor or declared capability is missing, and the
// cross-component coverage — every executable capability a deployment
// advertises has a bound bundle somewhere — remains a wiring and deployment
// acceptance responsibility; this registry validates only local bindings.
package plugins

import (
	"context"
	"encoding/json"
)

// Capability names one capability a plugin offers. The declared set on a
// descriptor must exactly match the descriptor's own data (tools,
// templates); the executable subset must be covered by execution bindings.
type Capability string

const (
	// CapabilityProbe: real instance probing (Prober).
	CapabilityProbe Capability = "probe"
	// CapabilityDiscover: bounded object discovery (Discoverer).
	CapabilityDiscover Capability = "discover"
	// CapabilityTools: model tool catalog present in Descriptor.Tools.
	CapabilityTools Capability = "tools"
	// CapabilityExecuteTool: model tool execution (ToolExecutor).
	CapabilityExecuteTool Capability = "execute_tool"
	// CapabilityInspectionTemplates: template catalog present in
	// Descriptor.InspectionTemplates.
	CapabilityInspectionTemplates Capability = "inspection_templates"
	// CapabilityCollect: deterministic evidence collection (Collector).
	CapabilityCollect Capability = "collect"
)

// executionCapabilities is the subset of capabilities an ExecutionBundle can
// provide. "tools" and "inspection_templates" are description capabilities
// carried by descriptor data, not executable interfaces.
var executionCapabilities = map[Capability]bool{
	CapabilityProbe:       true,
	CapabilityDiscover:    true,
	CapabilityExecuteTool: true,
	CapabilityCollect:     true,
}

// ExecutionLocation names the process that actually executes a tool or
// capability call. The vocabulary is aligned with the existing attempt
// catalog: worker_local tools run inside the disposable Plinth worker
// sandbox, plinth_supervisor tools run in the Plinth supervisor process,
// and quoin tools run in the Quoin control plane.
type ExecutionLocation string

const (
	// LocationWorkerLocal: disposable Plinth worker sandbox.
	LocationWorkerLocal ExecutionLocation = "worker_local"
	// LocationPlinthSupervisor: Plinth supervisor process (typed outbound
	// calls; the only place Call secrets are resolvable).
	LocationPlinthSupervisor ExecutionLocation = "plinth_supervisor"
	// LocationQuoin: Quoin control plane process.
	LocationQuoin ExecutionLocation = "quoin"
)

// locationSet is the closed execution-location vocabulary.
var locationSet = map[ExecutionLocation]bool{
	LocationWorkerLocal:      true,
	LocationPlinthSupervisor: true,
	LocationQuoin:            true,
}

// Well-known plugin IDs. They are naming authorities only: this package
// registers no descriptors and no bindings, and an ID becomes usable only
// when a host process actually registers the plugin.
const (
	PrometheusID   = "prometheus"
	ThanosID       = "thanos"
	AlertmanagerID = "alertmanager"
)

// Descriptor is the static, declarative description of one plugin: stable
// identity, capability surface, platform connection kind, configuration
// shape and the model-facing tool/template catalogs. It is pure data —
// serializable, digestable and safe for any host to hold.
type Descriptor struct {
	// ID is the stable plugin identity (lowercase, [a-z][a-z0-9_-]*).
	// Enablement config, tool authorization provenance and audit records
	// reference it forever; it is never reused for another plugin.
	ID string
	// Version is the plugin contract generation (non-empty free-form, e.g.
	// "1"). Tool and template versions advance independently.
	Version string
	// DisplayName and Description are human-facing, non-secret text.
	DisplayName string
	Description string
	// Capabilities lists the capabilities the plugin offers. Registration
	// enforces exact agreement with the descriptor's own data:
	// CapabilityTools iff Tools is non-empty, CapabilityInspectionTemplates
	// iff InspectionTemplates is non-empty, CapabilityExecuteTool only with
	// tools, CapabilityCollect only with templates.
	Capabilities []Capability
	// DefaultEnabled is the enablement default when deployment config is
	// silent; plugins backing the default mainline set true.
	DefaultEnabled bool
	// ConfigSchema, when non-nil, is the closed JSON Schema (draft
	// 2020-12, top-level object with additionalProperties disabled) every
	// instance settings document must satisfy. A nil schema is legal only
	// for a plugin that truly takes no configuration: settings must then be
	// the empty object. Secret slots are backend-managed: users enter plain
	// connection material through the platform's typed forms, and the
	// backend stores it as named secret references generated by Quoin — a
	// settings document never carries cleartext, only such references,
	// resolved at call time (see Call).
	ConfigSchema map[string]any
	// ConnectionKind names the external platform kind every instance binds
	// to (e.g. "prometheus", "alertmanager"). Empty means the plugin needs
	// no platform connection.
	ConnectionKind string
	// Tools is the versioned model tool catalog (description only; tools
	// execute wherever their ExecutionLocation says, via a bound
	// ExecutionBundle in that host).
	Tools []Tool
	// DiscoverObjects is the versioned bounded-discovery catalog: one entry
	// per object type the plugin's Discoverer can observe. The registry
	// enforces CapabilityDiscover iff the catalog is non-empty, and the
	// control plane freezes these declarations into each observation attempt
	// so a Run can never observe a scope the descriptor does not own.
	DiscoverObjects []DiscoverObject
	// InspectionTemplates is the versioned deterministic collection
	// template catalog (description only; collection executes via a bound
	// Collector).
	InspectionTemplates []InspectionTemplate
}

// SecretResolver returns the cleartext for one named secret reference. It is
// constructed by the executing host per call; it never reaches descriptors,
// worker sandboxes, tool catalogs, evidence or logs.
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) ([]byte, error)
}

// Call carries one runtime invocation's frozen instance configuration and
// secret resolution. Only the executing host constructs Calls (today the
// Plinth supervisor, where connection secrets are resolvable); description
// code and worker sandboxes never see one.
type Call struct {
	// PluginID is the plugin whose instance is being invoked.
	PluginID string
	// Settings is the frozen non-secret settings document already validated
	// against the descriptor's ConfigSchema.
	Settings json.RawMessage
	// SecretRefs maps the plugin's named secret slots to opaque
	// control-plane credential references. No cleartext ever appears here.
	SecretRefs map[string]string
	// Secrets resolves references for this call only. Nil is legal only for
	// plugins whose SecretRefs is empty.
	Secrets SecretResolver
}

// Prober performs a real probe against one configured instance and reports
// observed capability (the plugin-side counterpart of the connection-probes
// contract).
type Prober interface {
	Probe(ctx context.Context, call *Call, request ProbeRequest) (*ProbeResult, error)
}

// Discoverer enumerates objects inside an explicitly bounded scope and
// budget. Discovery reports what was observed at a point in time including
// incompleteness; it never fabricates absence.
type Discoverer interface {
	Discover(ctx context.Context, call *Call, request DiscoverRequest) (*DiscoverResult, error)
}

// ToolExecutor executes one authorized, typed tool call whose descriptor
// contract lives in Descriptor.Tools. It is bound only in the host named by
// the tool's ExecutionLocation.
type ToolExecutor interface {
	ExecuteTool(ctx context.Context, call *Call, request ToolRequest) (*ToolResult, error)
}

// Collector performs deterministic evidence collection for one frozen
// template version and target, reusing the plugin's tool operations
// underneath. It is not a second execution engine.
type Collector interface {
	Collect(ctx context.Context, call *Call, request CollectRequest) (*CollectResult, error)
}

// ExecutionBundle is the real implementation one host binds for one plugin
// at one execution location. Every listed capability must be backed by the
// matching non-nil interface and must be declared by the descriptor, so a
// bundle can never advertise an execution it cannot perform.
type ExecutionBundle struct {
	// PluginID is the descriptor this bundle executes for.
	PluginID string
	// Location is the process where this bundle's implementations actually
	// run. It must match the ExecutionLocation of every tool the bundle
	// serves.
	Location ExecutionLocation
	// Capabilities must exactly equal the capabilities backed by non-nil
	// interfaces below, and must be a subset of the descriptor's declared
	// capabilities.
	Capabilities []Capability
	Prober       Prober
	Discoverer   Discoverer
	ToolExecutor ToolExecutor
	Collector    Collector
}

// ConfigValidator is an optional, purely static configuration checker for
// plugin-specific rules beyond the declarative ConfigSchema (cross-field
// constraints, value normalization checks). It performs no I/O, so any host
// — including the control plane — may hold one.
type ConfigValidator interface {
	ValidateConfig(settings json.RawMessage) error
}
