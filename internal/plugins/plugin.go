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

// InboundManifestMetadataKey is sent over the authenticated Stele→Quoin
// channel on inbound-control RPCs. It is a compatibility fingerprint, not an
// authentication token; mTLS remains the service identity authority.
const InboundManifestMetadataKey = "quoin-inbound-manifest"

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
	// connection. A declared kind must pair with the transport and auth-mode
	// declaration below (ADR-0014): it joins the trusted HTTP connection-kind
	// class the Quoin connections domain and the Stele gateway resolve
	// through this registry instead of host-side type switches.
	ConnectionKind string
	// ConnectionTransport declares the connection transport. Only
	// ConnectionTransportHTTP exists; any other value fails registration.
	// Required (exactly "http") whenever ConnectionKind is set.
	ConnectionTransport string
	// ConnectionAuthModes is the bounded auth-mode subset (none/basic/
	// bearer) instances of this connection kind accept. Required and
	// non-empty whenever ConnectionKind is set; credential material never
	// appears here.
	ConnectionAuthModes []string
	// ConnectionProbePath, when set alongside ConnectionKind, declares the
	// kind's bounded read-only HTTP probe contract: one GET request against
	// this absolute path (it may carry a query string) on the connection's
	// configured base URL, expected to answer with HTTP 200. Quoin freezes the
	// contract with every probe attempt and executes it through the generic
	// Stele gateway; a kind without a probe path can never start or close a
	// probe attempt (fail closed), and one that does still enables only
	// through a passed real probe. The path is non-secret routing metadata —
	// credential material never appears here.
	ConnectionProbePath string
	// DefaultEnabled is the enablement default when deployment config is
	// silent; plugins backing the default mainline set true.
	DefaultEnabled bool
	// ConfigSchema, when non-nil, is the closed JSON Schema (draft 2020-12,
	// top-level object with additionalProperties disabled) every instance
	// settings document must satisfy. A nil schema is legal only for a plugin
	// that truly takes no configuration.
	ConfigSchema map[string]any
	// EventSourceConfigSchema, when non-nil, is the closed JSON Schema
	// (draft 2020-12, top-level object with additionalProperties disabled)
	// every EventSource INSTANCE settings document must satisfy (ADR-0014
	// story 2). It is deliberately separate from ConfigSchema: a plugin may
	// carry both an EventSource and an HTTP ConnectionKind whose instance
	// settings follow different schemas, and overloading ConfigSchema (the
	// connection revision contract) would blur those authorities. Nil (or
	// no EventSource capability) means the source takes no configuration —
	// any non-empty settings document is rejected. The schema must not
	// allow secret material: registration rejects any property named
	// password, bearerToken, apiKey, token or secret at any depth.
	EventSourceConfigSchema map[string]any
	// EventSource is the optional inbound capability: Stele's webhook layer
	// dispatches POST /webhook/{source} requests here.
	EventSource EventSource
	// EventContracts is the non-empty list of normalized event type/version
	// pairs this source may enqueue. The gateway rejects undeclared pairs
	// before ACK; the control plane independently verifies the declaration.
	EventContracts []EventContract
	// AlertNormalizer is the optional alert-normalization capability
	// (ADR-0012): it maps this source's EventSource payload onto the unified
	// alert semantics (severity/title/annotations). It requires EventSource
	// (the payload comes from the source); Quoin's intake executes it.
	AlertNormalizer AlertNormalizer
	// AlertIdentity selects the stable identity supplied by an alert batch:
	// labels (Alertmanager fingerprint checked against labels) or external
	// (source-owned externalId, stored alongside its SHA-256). A plugin with an
	// AlertNormalizer must declare one; the choice cannot vary per event.
	AlertIdentity string
	// Tools is the optional outbound capability: Quoin aggregates every
	// provider's entries into the tool catalog and the dispatch table.
	Tools ToolProvider
	// Validator is an optional purely static configuration checker beyond
	// ConfigSchema (cross-field constraints). It performs no I/O.
	Validator ConfigValidator
	// EventSourceValidator is the optional purely static checker for
	// EventSource instance settings beyond EventSourceConfigSchema
	// (cross-field constraints). It performs no I/O and is consulted only
	// when the plugin declares an EventSource.
	EventSourceValidator ConfigValidator
	// DiscoverObjects is the versioned bounded-discovery catalog: one entry
	// per object type the plugin can observe (consumed by the observation
	// scheduler; execution happens through the plugin's internal tools).
	DiscoverObjects []DiscoverObject
	// InspectionTemplates is the versioned deterministic collection template
	// catalog (consumed by inspection planning; execution happens through the
	// plugin's internal tools).
	InspectionTemplates []InspectionTemplate
	// DefaultInspectionPlan, if declared, creates one manual-only starter
	// inspection plan when this plugin's connection is enabled. The plugin
	// owns both the template identity and its valid initial params.
	DefaultInspectionPlan *DefaultInspectionPlan
	// PostCommitSubscriptions declares the finite committed-fact vocabulary
	// entries this plugin consumes (ADR-0014). Valid only together with a
	// non-nil PostCommitHandler; registration rejects unknown fact types and
	// duplicate subscriptions.
	PostCommitSubscriptions []PostCommitSubscription
	// PostCommitHandler is the after-commit subscriber invoked outside every
	// authority transaction, at-least-once per fact, with no database handle
	// and no credentials. A failure never changes the committed inbound
	// adjudication.
	PostCommitHandler PostCommitHandler
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
