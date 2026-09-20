package plugins

// The plugin registry (ADR-0004, reworked by ADR-0011): plugins self-register
// from init() — interfaces + registry + blank imports, no dynamic loading.
// Registration validates each plugin in isolation; the first read freezes the
// registry and validates the cross-plugin invariants (unique IDs, unique
// event-source kinds, shared-contract tool dedup). Mis-assembly is a
// programming error that stops the process at boot: Register after freeze,
// duplicate IDs, or divergent shared tool manifests all fail fast.
//
// Host wiring:
//
//   - cmd/quoin and cmd/stele blank-import the builtin plugin packages; the
//     process default registry (Default) assembles from those init calls.
//   - cmd/plinth imports nothing from this package beyond the pure contract
//     types — Plinth is plugin-unaware by construction (ADR-0011).

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Registry errors. They surface at boot; a deployed process never carries
// them past startup.
var (
	// ErrDuplicatePlugin: two registrations carry the same plugin ID.
	ErrDuplicatePlugin = fmt.Errorf("duplicate plugin")
	// ErrInvalidPlugin: a registration fails structural validation.
	ErrInvalidPlugin = fmt.Errorf("invalid plugin")
	// ErrDuplicateToolName: two plugins contribute divergent manifests under
	// one tool name (identical shared contracts are allowed).
	ErrDuplicateToolName = fmt.Errorf("duplicate tool name")
	// ErrUnknownPlugin: an operation references an unregistered plugin ID.
	ErrUnknownPlugin = fmt.Errorf("unknown plugin")
	// ErrRegistryFrozen: registration after the first registry read.
	ErrRegistryFrozen = fmt.Errorf("plugin registry frozen")
)

var (
	pluginIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	sourceKindPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	toolNamePattern   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// Registry is one process's frozen plugin assembly.
type Registry struct {
	mu      sync.Mutex
	plugins map[string]Plugin
	order   []string
	entries map[string]ToolEntry
	owners  map[string][]string
	sources map[string]string // event-source kind -> plugin ID
	frozen  bool
}

// NewRegistry creates an empty registry (tests and explicit assemblies).
func NewRegistry() *Registry {
	return &Registry{
		plugins: map[string]Plugin{},
		entries: map[string]ToolEntry{},
		owners:  map[string][]string{},
		sources: map[string]string{},
	}
}

// Register adds one plugin. It validates the plugin in isolation (identity,
// manifest basics, capability shapes) and rejects duplicates; cross-plugin
// invariants are checked at freeze.
func (r *Registry) Register(plugin Plugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return fmt.Errorf("%w: plugin %s rejected", ErrRegistryFrozen, plugin.ID)
	}
	if err := validatePlugin(plugin); err != nil {
		return err
	}
	if _, exists := r.plugins[plugin.ID]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicatePlugin, plugin.ID)
	}
	r.plugins[plugin.ID] = plugin
	r.order = append(r.order, plugin.ID)
	return nil
}

// freeze validates cross-plugin invariants and locks the registry. It is
// idempotent.
func (r *Registry) freeze() error {
	if r.frozen {
		return nil
	}
	ids := append([]string(nil), r.order...)
	sort.Strings(ids)
	for _, id := range ids {
		plugin := r.plugins[id]
		if plugin.EventSource != nil {
			kind := plugin.EventSource.Kind()
			if owner, exists := r.sources[kind]; exists {
				return fmt.Errorf("event source kind %q registered by both %s and %s", kind, owner, id)
			}
			r.sources[kind] = id
		}
		if plugin.Tools == nil {
			continue
		}
		for _, entry := range plugin.Tools.Tools() {
			entry.Owner = id
			if err := validateToolEntry(entry, id); err != nil {
				return err
			}
			existing, exists := r.entries[entry.Definition.Name]
			if !exists {
				r.entries[entry.Definition.Name] = entry
				r.owners[entry.Definition.Name] = []string{id}
				continue
			}
			// Shared-contract tools (e.g. the PromQL query tool of prometheus
			// and thanos): one name may carry exactly one manifest; divergent
			// contracts under one name are registration conflicts.
			if !toolDefinitionsEqual(existing.Definition, entry.Definition) {
				return fmt.Errorf("%w: %s is contributed with divergent manifests by %s and %s",
					ErrDuplicateToolName, entry.Definition.Name, strings.Join(r.owners[entry.Definition.Name], ", "), id)
			}
			r.owners[entry.Definition.Name] = append(r.owners[entry.Definition.Name], id)
		}
	}
	r.frozen = true
	return nil
}

// ensureFrozen locks the registry, panicking on freeze failures: by the time
// a host reads the registry the assembly is a boot-time fact, and a broken
// assembly must stop the process before it serves.
func (r *Registry) ensureFrozen() {
	if err := r.freeze(); err != nil {
		panic(fmt.Sprintf("plugin registry assembly failed: %v", err))
	}
}

// Plugins returns the registered plugins ordered by ID (frozen assembly).
func (r *Registry) Plugins() []Plugin {
	r.mu.Lock()
	r.ensureFrozen()
	ids := append([]string(nil), r.order...)
	sort.Strings(ids)
	plugins := make([]Plugin, 0, len(ids))
	for _, id := range ids {
		plugins = append(plugins, r.plugins[id])
	}
	r.mu.Unlock()
	return plugins
}

// Plugin resolves one registered plugin by ID.
func (r *Registry) Plugin(id string) (Plugin, bool) {
	r.mu.Lock()
	r.ensureFrozen()
	plugin, ok := r.plugins[id]
	r.mu.Unlock()
	return plugin, ok
}

// ToolEntries returns every registered tool entry ordered by tool name
// (model-visible and internal).
func (r *Registry) ToolEntries() []ToolEntry {
	r.mu.Lock()
	r.ensureFrozen()
	names := make([]string, 0, len(r.entries))
	for name := range r.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]ToolEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, r.entries[name])
	}
	r.mu.Unlock()
	return entries
}

// ToolEntryByName resolves one tool entry and its contributing plugin IDs.
func (r *Registry) ToolEntryByName(name string) (ToolEntry, []string, bool) {
	r.mu.Lock()
	r.ensureFrozen()
	entry, ok := r.entries[name]
	owners := append([]string(nil), r.owners[name]...)
	r.mu.Unlock()
	return entry, owners, ok
}

// EventSource resolves the inbound capability for one source kind.
func (r *Registry) EventSource(kind string) (EventSource, string, bool) {
	r.mu.Lock()
	r.ensureFrozen()
	pluginID, ok := r.sources[kind]
	var source EventSource
	if ok {
		source = r.plugins[pluginID].EventSource
	}
	r.mu.Unlock()
	return source, pluginID, ok
}

// EventSourceKinds returns the registered inbound source kinds, sorted.
func (r *Registry) EventSourceKinds() []string {
	r.mu.Lock()
	r.ensureFrozen()
	kinds := make([]string, 0, len(r.sources))
	for kind := range r.sources {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	r.mu.Unlock()
	return kinds
}

// AlertNormalizer resolves the alert-normalization capability for one source
// kind (the plugin's EventSource kind).
func (r *Registry) AlertNormalizer(kind string) (AlertNormalizer, string, bool) {
	r.mu.Lock()
	r.ensureFrozen()
	pluginID, ok := r.sources[kind]
	var normalizer AlertNormalizer
	if ok {
		normalizer = r.plugins[pluginID].AlertNormalizer
	}
	r.mu.Unlock()
	return normalizer, pluginID, ok
}

// validatePlugin checks one registration in isolation.
func validatePlugin(plugin Plugin) error {
	if !pluginIDPattern.MatchString(plugin.ID) {
		return fmt.Errorf("%w: id %q is not [a-z][a-z0-9_-]*", ErrInvalidPlugin, plugin.ID)
	}
	if plugin.Version == "" {
		return fmt.Errorf("%w: %s carries no version", ErrInvalidPlugin, plugin.ID)
	}
	if plugin.ConfigSchema != nil {
		if _, err := json.Marshal(plugin.ConfigSchema); err != nil {
			return fmt.Errorf("%w: %s config schema is not JSON: %v", ErrInvalidPlugin, plugin.ID, err)
		}
	}
	if plugin.EventSource != nil && !sourceKindPattern.MatchString(plugin.EventSource.Kind()) {
		return fmt.Errorf("%w: %s event source kind %q is not [a-z][a-z0-9-]*", ErrInvalidPlugin, plugin.ID, plugin.EventSource.Kind())
	}
	if plugin.AlertNormalizer != nil && plugin.EventSource == nil {
		return fmt.Errorf("%w: %s provides an alert normalizer without its event source", ErrInvalidPlugin, plugin.ID)
	}
	return nil
}

// validateToolEntry checks one contributed tool entry in isolation.
func validateToolEntry(entry ToolEntry, pluginID string) error {
	def := entry.Definition
	if !toolNamePattern.MatchString(def.Name) {
		return fmt.Errorf("%w: plugin %s tool name %q is not [a-z][a-z0-9_]*", ErrInvalidPlugin, pluginID, def.Name)
	}
	if def.Version == "" {
		return fmt.Errorf("%w: plugin %s tool %s carries no version", ErrInvalidPlugin, pluginID, def.Name)
	}
	if def.ExecutionMode != ModeQuoinRouted {
		return fmt.Errorf("%w: plugin %s tool %s must execute as %q", ErrInvalidPlugin, pluginID, def.Name, ModeQuoinRouted)
	}
	if def.FailureMode != FailureReturnToModel && def.FailureMode != FailureFailAttempt {
		return fmt.Errorf("%w: plugin %s tool %s failure mode %q is outside the vocabulary", ErrInvalidPlugin, pluginID, def.Name, def.FailureMode)
	}
	if def.Description == "" {
		return fmt.Errorf("%w: plugin %s tool %s carries no description", ErrInvalidPlugin, pluginID, def.Name)
	}
	if def.Parameters == nil {
		return fmt.Errorf("%w: plugin %s tool %s carries no parameter schema", ErrInvalidPlugin, pluginID, def.Name)
	}
	if entry.Invoke == nil {
		return fmt.Errorf("%w: plugin %s tool %s carries no invocation", ErrInvalidPlugin, pluginID, def.Name)
	}
	return nil
}

// toolDefinitionsEqual compares two manifests by canonical JSON bytes.
func toolDefinitionsEqual(left, right ToolDef) bool {
	type manifest struct {
		Name, Version, ExecutionMode, FailureMode, ResultSchemaKind, Description string
		Parameters                                                               map[string]any
	}
	leftBytes, err := json.Marshal(manifest{left.Name, left.Version, left.ExecutionMode, left.FailureMode, left.ResultSchemaKind, left.Description, left.Parameters})
	if err != nil {
		return false
	}
	rightBytes, err := json.Marshal(manifest{right.Name, right.Version, right.ExecutionMode, right.FailureMode, right.ResultSchemaKind, right.Description, right.Parameters})
	if err != nil {
		return false
	}
	return string(leftBytes) == string(rightBytes)
}

// ---------------------------------------------------------------------------
// Process default registry (blank-import assembly)
// ---------------------------------------------------------------------------

// defaultRegistry is the process-global assembly. Plugin packages call the
// package-level Register from init(); hosts reach the frozen result through
// Default.
var defaultRegistry = NewRegistry()

// Register adds one plugin to the process default registry. It is intended
// for plugin package init() functions; a registration failure is a
// programming error and panics (fail fast, before the process serves).
func Register(plugin Plugin) {
	if err := defaultRegistry.Register(plugin); err != nil {
		panic(fmt.Sprintf("plugin registration failed: %v", err))
	}
}

// Default returns the process default registry, frozen on first access.
func Default() *Registry { return defaultRegistry }
