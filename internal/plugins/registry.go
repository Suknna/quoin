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
//   - cmd/quoin and cmd/stele blank-import the selected packages in plugins/; the
//     process default registry (Default) assembles from those init calls.
//   - cmd/plinth imports nothing from this package beyond the pure contract
//     types — Plinth is plugin-unaware by construction (ADR-0011).

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
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
	// collectionGrantPurposePattern pins the declared collection grant
	// vocabulary to the schema closure trigger's generic config_ prefix
	// convention (trg_attempt_connection_grants_collection_closure).
	collectionGrantPurposePattern = regexp.MustCompile(`^config_[a-z0-9_-]*$`)
)

// Registry is one process's frozen plugin assembly.
type Registry struct {
	mu      sync.Mutex
	plugins map[string]Plugin
	order   []string
	entries map[string]ToolEntry
	owners  map[string][]string
	sources map[string]string // event-source kind -> plugin ID
	kinds   map[string]string // connection kind -> plugin ID
	frozen  bool
}

// NewRegistry creates an empty registry (tests and explicit assemblies).
func NewRegistry() *Registry {
	return &Registry{
		plugins: map[string]Plugin{},
		entries: map[string]ToolEntry{},
		owners:  map[string][]string{},
		sources: map[string]string{},
		kinds:   map[string]string{},
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
	if err := validateConnectionDeclaration(plugin); err != nil {
		return err
	}
	for _, template := range plugin.InspectionTemplates {
		if template.ParamsSchema != nil {
			if _, err := r.compiledConfigSchema(plugin.ID+":inspection:"+template.ID+":"+template.Version, template.ParamsSchema); err != nil {
				return err // An invalid declaration must fail before the host starts.
			}
		}
	}
	if plugin.DefaultInspectionPlan != nil {
		for _, template := range plugin.InspectionTemplates {
			if template.ID == plugin.DefaultInspectionPlan.TemplateID {
				if err := r.validateTemplateParams(plugin.ID, template, plugin.DefaultInspectionPlan.Params); err != nil {
					return fmt.Errorf("%w: %s starter plan params: %v", ErrInvalidPlugin, plugin.ID, err)
				}
				break // first declared version is the current starter version
			}
		}
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
		if plugin.ConnectionKind != "" {
			if owner, exists := r.kinds[plugin.ConnectionKind]; exists {
				return fmt.Errorf("connection kind %q declared by both %s and %s", plugin.ConnectionKind, owner, id)
			}
			r.kinds[plugin.ConnectionKind] = id
		}
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
	for _, id := range ids {
		for _, template := range r.plugins[id].InspectionTemplates {
			entry, exists := r.entries[template.CollectToolName]
			if !exists || !entry.Internal || !slices.Contains(r.owners[template.CollectToolName], id) {
				return fmt.Errorf("%w: %s inspection template %s/%s must own internal tool %q", ErrInvalidPlugin, id, template.ID, template.Version, template.CollectToolName)
			}
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

// SourceEvent reports whether the named source declares the normalized event
// type and exact payload version. Source-kind ownership is checked at the same frozen assembly as
// EventSource so gateway admission and Quoin dispatch use one authority.
func (r *Registry) SourceEvent(kind, eventType string, payloadVersion uint32) bool {
	r.mu.Lock()
	r.ensureFrozen()
	pluginID, ok := r.sources[kind]
	if ok {
		for _, declared := range r.plugins[pluginID].EventContracts {
			if declared.Type == eventType && declared.Version == payloadVersion {
				r.mu.Unlock()
				return true
			}
		}
	}
	r.mu.Unlock()
	return false
}

// InboundManifestFingerprint is the deterministic versioned declaration of
// compiled inbound sources shared by Quoin and Stele. Deployment enablement
// remains a separate authorization decision; any compiled source divergence
// makes the inbound handshake fail closed before credential snapshots or
// queued events are accepted.
func (r *Registry) InboundManifestFingerprint() string {
	r.mu.Lock()
	r.ensureFrozen()
	type declaration struct {
		ID, Version, Kind, AlertIdentity string
		EventContracts                   []EventContract
		// EventSourceConfigSchema joins the manifest so two hosts cannot
		// disagree on which instance settings a source accepts (ADR-0014
		// story 2). Canonical JSON marshal sorts map keys, so the digest is
		// deterministic; a nil schema stays absent, preserving the fingerprint
		// of schema-less sources.
		EventSourceConfigSchema map[string]any `json:"eventSourceConfigSchema,omitempty"`
	}
	manifest := make([]declaration, 0, len(r.sources))
	for kind, id := range r.sources {
		plugin := r.plugins[id]
		events := append([]EventContract{}, plugin.EventContracts...)
		sort.Slice(events, func(i, j int) bool {
			if events[i].Type != events[j].Type {
				return events[i].Type < events[j].Type
			}
			return events[i].Version < events[j].Version
		})
		manifest = append(manifest, declaration{ID: id, Version: plugin.Version, Kind: kind, AlertIdentity: plugin.AlertIdentity, EventContracts: events, EventSourceConfigSchema: plugin.EventSourceConfigSchema})
	}
	r.mu.Unlock()
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Kind < manifest[j].Kind })
	encoded, _ := json.Marshal(manifest) // every field is a validated string
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum)
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

// AlertIdentity resolves a source's frozen alert identity contract. The
// caller never chooses identity semantics from untrusted event contents.
func (r *Registry) AlertIdentity(kind string) (string, bool) {
	r.mu.Lock()
	r.ensureFrozen()
	pluginID, ok := r.sources[kind]
	var mode string
	if ok {
		mode = r.plugins[pluginID].AlertIdentity
	}
	r.mu.Unlock()
	return mode, ok && mode != ""
}

// EventSourceSettings resolves one source kind's closed instance-settings
// schema and its owning plugin ID (ADR-0014 story 2). The Quoin control
// plane validates every alert-source settings document against this schema
// before persisting it; a nil schema means the source takes no settings and
// only the empty document is legal.
func (r *Registry) EventSourceSettings(kind string) (map[string]any, string, bool) {
	r.mu.Lock()
	r.ensureFrozen()
	pluginID, ok := r.sources[kind]
	var schema map[string]any
	if ok {
		schema = r.plugins[pluginID].EventSourceConfigSchema
	}
	r.mu.Unlock()
	return schema, pluginID, ok
}

// InspectionTemplate resolves one declared collection template by its
// frozen (plugin, id, version) identity. Unknown identities fail closed at
// the caller.
func (r *Registry) InspectionTemplate(pluginID, templateID, version string) (InspectionTemplate, bool) {
	plugin, ok := r.Plugin(pluginID)
	if !ok {
		return InspectionTemplate{}, false
	}
	for _, template := range plugin.InspectionTemplates {
		if template.ID == templateID && template.Version == version {
			return template, true
		}
	}
	return InspectionTemplate{}, false
}

// DiscoverObject resolves one declared discovery object type of one plugin.
func (r *Registry) DiscoverObject(pluginID, objectType string) (DiscoverObject, bool) {
	plugin, ok := r.Plugin(pluginID)
	if !ok {
		return DiscoverObject{}, false
	}
	for _, object := range plugin.DiscoverObjects {
		if object.ObjectType == objectType {
			return object, true
		}
	}
	return DiscoverObject{}, false
}

// IsCollectionGrantPurpose reports whether the grant purpose is declared by
// any registered plugin's collection contract (inspection templates and
// discovery objects). Enablement-independent by design: the declared
// vocabulary is a compile-time fact, so legacy attempts of a since-disabled
// plugin keep their credential-fulfillment defense.
func (r *Registry) IsCollectionGrantPurpose(purpose string) bool {
	for _, plugin := range r.Plugins() {
		for _, template := range plugin.InspectionTemplates {
			if template.GrantPurpose == purpose {
				return true
			}
		}
		for _, object := range plugin.DiscoverObjects {
			if object.GrantPurpose == purpose {
				return true
			}
		}
	}
	return false
}

// IsCollectionGrantPurpose reports whether the purpose is a declared
// collection grant purpose of the process default registry.
func IsCollectionGrantPurpose(purpose string) bool {
	return defaultRegistry.IsCollectionGrantPurpose(purpose)
}

// defaultEnabledIDs returns the DefaultEnabled plugin IDs of the frozen
// assembly (the silent-deployment default of ResolveEnabled).
func (r *Registry) defaultEnabledIDs() []string {
	r.mu.Lock()
	r.ensureFrozen()
	ids := make([]string, 0, len(r.plugins))
	for id, plugin := range r.plugins {
		if plugin.DefaultEnabled {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	sort.Strings(ids)
	return ids
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
	if plugin.EventSourceConfigSchema != nil {
		if plugin.EventSource == nil {
			return fmt.Errorf("%w: %s declares an event source config schema without an event source", ErrInvalidPlugin, plugin.ID)
		}
		if _, err := json.Marshal(plugin.EventSourceConfigSchema); err != nil {
			return fmt.Errorf("%w: %s event source config schema is not JSON: %v", ErrInvalidPlugin, plugin.ID, err)
		}
		if err := validateClosedSettingsSchema(plugin.ID, "event source config", plugin.EventSourceConfigSchema); err != nil {
			return err
		}
	}
	if plugin.EventSourceValidator != nil && plugin.EventSource == nil {
		return fmt.Errorf("%w: %s declares an event source config validator without an event source", ErrInvalidPlugin, plugin.ID)
	}
	if plugin.EventSource != nil && !sourceKindPattern.MatchString(plugin.EventSource.Kind()) {
		return fmt.Errorf("%w: %s event source kind %q is not [a-z][a-z0-9-]*", ErrInvalidPlugin, plugin.ID, plugin.EventSource.Kind())
	}
	if plugin.EventSource != nil && len(plugin.EventContracts) == 0 || plugin.EventSource == nil && len(plugin.EventContracts) > 0 {
		return fmt.Errorf("%w: %s event source and non-empty event contracts must be declared together", ErrInvalidPlugin, plugin.ID)
	}
	seenEvents := map[EventContract]bool{}
	for _, event := range plugin.EventContracts {
		if event.Type == "" || event.Version == 0 || seenEvents[event] {
			return fmt.Errorf("%w: %s has empty or duplicate event contract %q version %d", ErrInvalidPlugin, plugin.ID, event.Type, event.Version)
		}
		seenEvents[event] = true
	}
	seenTemplates := map[string]bool{}
	for _, template := range plugin.InspectionTemplates {
		if !pluginIDPattern.MatchString(template.ID) || template.Version == "" || seenTemplates[template.ID+"\x00"+template.Version] {
			return fmt.Errorf("%w: %s has an empty or duplicate inspection template identity %q/%q", ErrInvalidPlugin, plugin.ID, template.ID, template.Version)
		}
		seenTemplates[template.ID+"\x00"+template.Version] = true
		if !collectionGrantPurposePattern.MatchString(template.GrantPurpose) {
			return fmt.Errorf("%w: %s inspection template %s/%s grant purpose %q must be config_[a-z0-9_-]* (the schema closure trigger's declared vocabulary)", ErrInvalidPlugin, plugin.ID, template.ID, template.Version, template.GrantPurpose)
		}
		if !toolNamePattern.MatchString(template.CollectToolName) {
			return fmt.Errorf("%w: %s inspection template %s/%s needs a declared internal collection tool", ErrInvalidPlugin, plugin.ID, template.ID, template.Version)
		}
		if template.ResultKind != "promql" && template.ResultKind != "json" {
			return fmt.Errorf("%w: %s inspection template %s/%s must declare promql or json result kind", ErrInvalidPlugin, plugin.ID, template.ID, template.Version)
		}
		if template.ParamsSchema != nil {
			if err := validateClosedSettingsSchema(plugin.ID, "inspection template "+template.ID, template.ParamsSchema); err != nil {
				return err
			}
		}
	}
	if plugin.DefaultInspectionPlan != nil {
		if plugin.ConnectionKind == "" || plugin.DefaultInspectionPlan.TemplateID == "" {
			return fmt.Errorf("%w: %s starter inspection plan requires a connection kind and template", ErrInvalidPlugin, plugin.ID)
		}
		found := false
		for _, template := range plugin.InspectionTemplates {
			if template.ID == plugin.DefaultInspectionPlan.TemplateID {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: %s starter inspection plan references unknown template %q", ErrInvalidPlugin, plugin.ID, plugin.DefaultInspectionPlan.TemplateID)
		}
	}
	seenObjects := map[string]bool{}
	for _, object := range plugin.DiscoverObjects {
		if object.ObjectType == "" || seenObjects[object.ObjectType] {
			return fmt.Errorf("%w: %s has an empty or duplicate discovery object type %q", ErrInvalidPlugin, plugin.ID, object.ObjectType)
		}
		seenObjects[object.ObjectType] = true
		if !collectionGrantPurposePattern.MatchString(object.GrantPurpose) {
			return fmt.Errorf("%w: %s discovery object %q grant purpose %q must be config_[a-z0-9_-]* (the schema closure trigger's declared vocabulary)", ErrInvalidPlugin, plugin.ID, object.ObjectType, object.GrantPurpose)
		}
	}
	if plugin.AlertNormalizer != nil && plugin.EventSource == nil {
		return fmt.Errorf("%w: %s provides an alert normalizer without its event source", ErrInvalidPlugin, plugin.ID)
	}
	if plugin.AlertNormalizer != nil && plugin.AlertIdentity != AlertIdentityLabels && plugin.AlertIdentity != AlertIdentityExternal || plugin.AlertNormalizer == nil && plugin.AlertIdentity != "" {
		return fmt.Errorf("%w: %s must declare exactly one alert identity mode alongside its normalizer", ErrInvalidPlugin, plugin.ID)
	}
	if err := validatePostCommitDeclarations(plugin); err != nil {
		return err
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
	// The authorization plan is mandatory for connection-grant tools and
	// implies the flag; a dangling flag without a plan cannot be executed
	// generically and fails registration (fail closed at boot).
	if def.RequiresConnectionGrant && def.Grant == nil {
		return fmt.Errorf("%w: plugin %s tool %s requires a connection grant but declares no grant plan", ErrInvalidPlugin, pluginID, def.Name)
	}
	if def.Grant != nil {
		if !def.RequiresConnectionGrant {
			return fmt.Errorf("%w: plugin %s tool %s declares a grant plan without requiring a connection grant", ErrInvalidPlugin, pluginID, def.Name)
		}
		if def.Grant.Purpose == "" || def.Grant.SourceItemRole == "" {
			return fmt.Errorf("%w: plugin %s tool %s grant plan needs a purpose and a source item role", ErrInvalidPlugin, pluginID, def.Name)
		}
		// The declared purpose must stay inside the tool-call structural
		// class of the grant schema: never the core vocabulary, never a
		// probe purpose (<kind>_probe is derived by the core) and never a
		// collection purpose (config_ prefix belongs to templates).
		if !toolNamePattern.MatchString(def.Grant.Purpose) || strings.HasSuffix(def.Grant.Purpose, "_probe") || strings.HasPrefix(def.Grant.Purpose, "config_") {
			return fmt.Errorf("%w: plugin %s tool %s grant purpose %q must be [a-z][a-z0-9_]* and outside the _probe/config_ classes", ErrInvalidPlugin, pluginID, def.Name, def.Grant.Purpose)
		}
	}
	if def.EvidenceProjector != nil && !def.ProducesEvidence {
		return fmt.Errorf("%w: plugin %s tool %s declares an evidence projector without producing evidence", ErrInvalidPlugin, pluginID, def.Name)
	}
	return nil
}

// toolDefinitionsEqual compares two manifests by canonical JSON bytes. The
// authorization plan is part of the manifest: two contributions under one
// tool name must authorize identically, not merely render identically.
func toolDefinitionsEqual(left, right ToolDef) bool {
	type manifest struct {
		Name, Version, ExecutionMode, FailureMode, ResultSchemaKind, Description string
		Parameters                                                               map[string]any
		Grant                                                                    *GrantPlan
	}
	leftBytes, err := json.Marshal(manifest{left.Name, left.Version, left.ExecutionMode, left.FailureMode, left.ResultSchemaKind, left.Description, left.Parameters, left.Grant})
	if err != nil {
		return false
	}
	rightBytes, err := json.Marshal(manifest{right.Name, right.Version, right.ExecutionMode, right.FailureMode, right.ResultSchemaKind, right.Description, right.Parameters, right.Grant})
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
