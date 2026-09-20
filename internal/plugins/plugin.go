// Package plugins owns the shared plugin contract (ADR-0004, reworked by
// ADR-0011): one Plugin value carries a plugin's identity, its declarative
// catalogs and its optional capability implementations. Plugins register
// themselves from init() — the classic interfaces + registry + blank-import
// pattern — and every host process opt in by blank-importing the plugin
// package; assembly is compile-time, there is no dynamic loading and no
// Go standard library `plugin` (dlopen) anywhere.
//
// The contract is free of Quoin business dependencies so Quoin and Stele can
// link it without pulling in each other's domains. Capabilities split along
// the gateway boundary (ADR-0011):
//
//   - EventSource (inbound): Stele's webhook layer finds the source by URL
//     segment, hands it the authenticated raw request, and receives
//     normalized Events for the local queue. It performs protocol-level
//     parsing only — business interpretation stays with Quoin.
//   - ToolProvider (outbound): tools are type-safe generic implementations
//     whose model-visible manifest (description + parameter schema) derives
//     from the typed definition. Handlers run in Quoin (permission, audit,
//     dispatch) and reach external platforms only through the injected
//     PlatformCaller, which executes at the Stele gateway with credential
//     injection and rate limiting. Handlers never see credentials.
//
// Registration is fail-fast: duplicate plugin IDs, duplicate divergent tool
// manifests, or invalid derivations stop the process at boot.
package plugins

import (
	"encoding/json"
)

// Well-known plugin IDs. They are naming authorities only: an ID becomes
// usable when a host process actually registers the plugin.
const (
	PrometheusID   = "prometheus"
	ThanosID       = "thanos"
	AlertmanagerID = "alertmanager"
)

// Plugin is one compiled plugin's complete self-description plus its optional
// capability implementations. A plugin package constructs one Plugin value and
// passes it to Register from init(); hosts never assemble plugins by hand.
type Plugin struct {
	// ID is the stable plugin identity (lowercase, [a-z][a-z0-9_-]*).
	// Enablement config, tool provenance and audit records reference it
	// forever; it is never reused for another plugin.
	ID string
	// Version is the plugin contract generation (non-empty free-form).
	Version string
	// DisplayName and Description are human-facing, non-secret text.
	DisplayName string
	Description string
	// ConnectionKind names the external platform kind every instance binds
	// to (e.g. "prometheus"). Empty means the plugin needs no platform
	// connection.
	ConnectionKind string
	// DefaultEnabled is the enablement default when deployment config is
	// silent; plugins backing the default mainline set true.
	DefaultEnabled bool
	// ConfigSchema, when non-nil, is the closed JSON Schema (draft 2020-12,
	// top-level object with additionalProperties disabled) every instance
	// settings document must satisfy. A nil schema is legal only for a plugin
	// that truly takes no configuration.
	ConfigSchema map[string]any
	// EventSource is the optional inbound capability: Stele's webhook layer
	// dispatches POST /webhook/{source} requests here.
	EventSource EventSource
	// Tools is the optional outbound capability: Quoin aggregates every
	// provider's entries into the tool catalog and the dispatch table.
	Tools ToolProvider
	// Validator is an optional purely static configuration checker beyond
	// ConfigSchema (cross-field constraints). It performs no I/O.
	Validator ConfigValidator
	// DiscoverObjects is the versioned bounded-discovery catalog: one entry
	// per object type the plugin can observe (consumed by the observation
	// scheduler; execution happens through the plugin's internal tools).
	DiscoverObjects []DiscoverObject
	// InspectionTemplates is the versioned deterministic collection template
	// catalog (consumed by inspection planning; execution happens through the
	// plugin's internal tools).
	InspectionTemplates []InspectionTemplate
}

// ConfigValidator is an optional, purely static configuration checker for
// plugin-specific rules beyond the declarative ConfigSchema. It performs no
// I/O, so any host — including the control plane — may run it.
type ConfigValidator interface {
	ValidateConfig(settings json.RawMessage) error
}

// ToolProvider is the outbound tool capability: the entries a plugin
// contributes. Entries carry both the derived manifest and the type-erased
// invocation, so declaration and implementation are one authority by
// construction.
type ToolProvider interface {
	Tools() []ToolEntry
}

// Connection is the frozen, non-secret connection context of one outbound
// tool call. Settings is the connection revision's validated non-secret
// settings document; credential material never appears here — Stele resolves
// and injects it at the gateway (ADR-0011: Quoin writes credentials, never
// reads them for use).
type Connection struct {
	ID         int64
	RevisionID int64
	Type       string          // connections.type ("prometheus" | "thanos" | ...)
	Settings   json.RawMessage // non-secret settings document
}
