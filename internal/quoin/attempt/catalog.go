package attempt

// Per-attempt frozen tool catalog (ADR-0004: the model may only call tools
// inside the catalog frozen for THIS execution). The catalog document is
// written once when the attempt is created — complete tool descriptions,
// provider-facing parameter schemas and result-schema references included —
// and is covered by the attempt input snapshot digest (tool_catalog_json
// carries the authoritative copy; the identical document is embedded in the
// canonical input so worker and supervisor render the same bytes without a
// second protocol). Authorization re-reads THIS document on every model/tool
// call, so a later enablement change can never drift a historical active
// attempt: recovery re-renders the original bytes, and a frozen tool whose
// installed implementation no longer matches is denied explicitly instead of
// silently reinterpreted.
//
// Catalog ASSEMBLY is registry-driven: the fixed platform tools come from
// this package's compiled table, and every plugin-contributed tool enters a
// generation's catalog exactly when its owning plugin is enabled and the
// generation accepts the tool's execution location. A new plugin therefore
// never changes a core map or switch here; the registry descriptors are the
// single ownership authority (duplicate tool names are rejected at
// registration, so a shared tool has exactly one owning plugin).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/plugins"
)

// FrozenCatalog is the immutable, self-contained tool catalog document of
// one attempt. Every field is fixed at creation: rendering the provider
// schema never consults the compiled implementation table.
type FrozenCatalog struct {
	// SchemaVersion names the catalog generation (model_calls provenance).
	SchemaVersion string `json:"schemaVersion"`
	// AgentVersion is the executor generation the catalog belongs to.
	AgentVersion string `json:"agentVersion"`
	// Plugins records the enabled plugin provenance (id + descriptor
	// version) whose tools this catalog actually carries.
	Plugins []FrozenPlugin `json:"plugins"`
	// Tools is the closed, ordered tool catalog with complete
	// provider-facing parameter schemas.
	Tools []FrozenTool `json:"tools"`
}

// FrozenPlugin is one contributing plugin's frozen identity.
type FrozenPlugin struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// FrozenTool is one tool's complete frozen contract.
type FrozenTool struct {
	Name             string `json:"name"`
	Version          string `json:"version"`
	ExecutionMode    string `json:"executionMode"`
	FailureMode      string `json:"failureMode"`
	ResultSchemaKind string `json:"resultSchemaKind,omitempty"`
	// ResultSchemaDigest, when set, freezes the exact generated result
	// contract document the installed validator enforces; a drifted schema
	// fails the installed-executor compatibility check instead of silently
	// sealing differently-shaped results.
	ResultSchemaDigest string         `json:"resultSchemaDigest,omitempty"`
	Description        string         `json:"description"`
	ProducesEvidence   bool           `json:"producesEvidence,omitempty"`
	Parameters         map[string]any `json:"parameters"`
}

// generationAccepts is the per-agent-generation execution-location strategy.
// A plugin tool joins a generation's catalog iff its declared execution
// location is accepted here; adding a plugin never touches this table.
var generationAccepts = map[string]map[plugins.ExecutionLocation]bool{
	"initial-analysis-v1": {
		plugins.LocationWorkerLocal:      true,
		plugins.LocationPlinthSupervisor: true,
	},
	"investigation-v1": {
		plugins.LocationWorkerLocal:      true,
		plugins.LocationPlinthSupervisor: true,
		plugins.LocationLintel:           true,
	},
}

// platformToolNames are the compiled tools no plugin owns (workspace and
// artifact tools). They are part of every generation's base catalog.
var platformToolNames = map[string]bool{
	"bash": true, "read": true, "write": true, "grep": true,
	"artifact_read": true, "artifact_grep": true,
}

// knownResultSchemaReferences pins the generated result contract documents
// by result schema kind. Kinds without a generated document are enforced by
// compiled validators alone and carry no frozen reference.
func knownResultSchemaReferences() map[string]string {
	browserSum := sha256.Sum256(gencontracts.BrowserToolSchema)
	return map[string]string{
		"browser_tool_result_v1": hex.EncodeToString(browserSum[:]),
	}
}

// Catalogs is the boot-frozen per-generation catalog set, assembled once by
// the wiring layer from the plugin registry and the resolved enablement.
// Attempts created afterwards freeze one of these documents; authorization
// re-reads the attempt's stored copy.
type Catalogs struct {
	generations map[string]*FrozenCatalog
}

// CatalogFor returns the frozen catalog of one agent generation.
func (catalogs *Catalogs) CatalogFor(agentVersion string) (*FrozenCatalog, error) {
	if catalogs == nil || catalogs.generations == nil {
		return nil, fmt.Errorf("frozen catalog source is not wired")
	}
	catalog, ok := catalogs.generations[agentVersion]
	if !ok {
		return nil, fmt.Errorf("agent generation %q has no frozen catalog", agentVersion)
	}
	return catalog, nil
}

// BuildCatalogs assembles the per-generation frozen catalogs. Platform tools
// come first (stable compiled order); enabled plugins contribute their
// declared tools in stable descriptor order, each verified against the
// compiled implementation so a declaration can never advertise a tool nobody
// executes.
func BuildCatalogs(registry *plugins.Registry, enabledPluginIDs []string) (*Catalogs, error) {
	if registry == nil {
		return nil, fmt.Errorf("plugin registry is not wired")
	}
	freezeImplementations()
	compiled := map[string]ToolDef{}
	for _, def := range ImplementationTools() {
		compiled[def.Name] = def
	}
	// Every registered descriptor — enabled or not — owns its declared tool
	// names; ownership is a declaration fact, not an enablement fact.
	pluginOwned := map[string]bool{}
	for _, descriptor := range registry.Descriptors() {
		for _, tool := range descriptor.Tools {
			pluginOwned[tool.Name] = true
		}
	}
	resultSchemaReferences := knownResultSchemaReferences()
	catalogs := &Catalogs{generations: map[string]*FrozenCatalog{}}
	for agentVersion, accepts := range generationAccepts {
		catalog := &FrozenCatalog{
			SchemaVersion: catalogSchemaVersionFor(agentVersion),
			AgentVersion:  agentVersion,
		}
		for _, def := range ImplementationTools() {
			if !platformToolNames[def.Name] && pluginOwned[def.Name] {
				continue // plugin-contributed: added below per enablement
			}
			if !platformToolNames[def.Name] && !pluginOwned[def.Name] {
				continue // unowned extra: refuse to offer unclaimed tools
			}
			catalog.Tools = append(catalog.Tools, frozenToolWithReference(def, resultSchemaReferences))
		}
		addedTools := map[string]bool{}
		for _, descriptor := range registry.Descriptors() {
			if !plugins.IsEnabled(enabledPluginIDs, descriptor.ID) {
				continue
			}
			contributed := false
			for _, declared := range descriptor.Tools {
				implementation, exists := compiled[declared.Name]
				if !exists {
					if accepts[declared.ExecutionLocation] {
						return nil, fmt.Errorf("plugin %s declares tool %s without a compiled implementation", descriptor.ID, declared.Name)
					}
					continue
				}
				if err := verifyDeclaredTool(descriptor, declared); err != nil {
					return nil, err
				}
				if accepts[declared.ExecutionLocation] {
					// The same contract tool may be declared by several
					// provider plugins (e.g. PromQL query over prometheus or
					// thanos connections): the catalog keeps ONE entry and
					// provenance lists every enabled contributing provider;
					// authorization resolves the actual source connection.
					if !addedTools[declared.Name] {
						catalog.Tools = append(catalog.Tools, frozenToolWithReference(implementation, resultSchemaReferences))
						addedTools[declared.Name] = true
					}
					contributed = true
				}
			}
			if contributed {
				catalog.Plugins = append(catalog.Plugins, FrozenPlugin{ID: descriptor.ID, Version: descriptor.Version})
			}
		}
		catalogs.generations[agentVersion] = catalog
	}
	return catalogs, nil
}

func catalogSchemaVersionFor(agentVersion string) string {
	if agentVersion == "investigation-v1" {
		// v3 carries quoin_browser v2: the breaking identityKey locator
		// (ADR-0004). The label keeps model-call provenance distinguishable
		// from catalogs frozen with the retired businessSystemKey locator.
		return "investigation-tools-v3"
	}
	return ToolSchemaVersion
}

// verifyDeclaredTool pins one declared tool against the compiled
// implementation table at build time.
func verifyDeclaredTool(descriptor plugins.Descriptor, declared plugins.Tool) error {
	implementation, exists := compiledToolByName(declared.Name)
	if !exists {
		return fmt.Errorf("plugin %s declares tool %s without a compiled implementation", descriptor.ID, declared.Name)
	}
	mode := LocationExecutionModes(declared.ExecutionLocation)
	if mode == "" || implementation.ExecutionMode != mode {
		return fmt.Errorf("plugin %s tool %s declares execution location %q, implementation runs as %q", descriptor.ID, declared.Name, declared.ExecutionLocation, implementation.ExecutionMode)
	}
	if implementation.Version != declared.Version {
		return fmt.Errorf("plugin %s tool %s declares version %s, implementation is %s", descriptor.ID, declared.Name, declared.Version, implementation.Version)
	}
	if implementation.FailureMode != declared.FailureMode {
		return fmt.Errorf("plugin %s tool %s declares failure mode %q, implementation uses %q", descriptor.ID, declared.Name, declared.FailureMode, implementation.FailureMode)
	}
	if implementation.Description != declared.Description {
		return fmt.Errorf("plugin %s tool %s description drifts from the compiled implementation", descriptor.ID, declared.Name)
	}
	return nil
}

// FrozenCatalogJSONForCreation marshals the catalog document an agent
// creation flow must freeze into attempt_input_snapshots.tool_catalog_json
// AND embed into the canonical input (the snapshot digest therefore covers
// the catalog; read and write share one semantic).
func FrozenCatalogJSONForCreation(catalogs *Catalogs, agentVersion string) (document []byte, catalog *FrozenCatalog, err error) {
	catalog, err = catalogs.CatalogFor(agentVersion)
	if err != nil {
		return nil, nil, err
	}
	document, err = json.Marshal(catalog)
	if err != nil {
		return nil, nil, err
	}
	return document, catalog, nil
}

// frozenToolFromDefinition freezes the compiled definition including its
// derived provider-facing parameters.
func frozenToolFromDefinition(def ToolDef) FrozenTool {
	return FrozenTool{
		Name: def.Name, Version: def.Version, ExecutionMode: def.ExecutionMode,
		FailureMode: def.FailureMode, ResultSchemaKind: def.ResultSchemaKind,
		Description: def.Description, ProducesEvidence: def.ProducesEvidence,
		Parameters: providerParameters(def),
	}
}

func frozenToolWithReference(def ToolDef, references map[string]string) FrozenTool {
	frozen := frozenToolFromDefinition(def)
	if reference, ok := references[def.ResultSchemaKind]; ok {
		frozen.ResultSchemaDigest = reference
	}
	return frozen
}

// providerParameters renders the complete provider-facing parameter schema
// of one compiled definition (the shared derivation of CanonicalToolsJSON).
func providerParameters(def ToolDef) map[string]any {
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

// ProviderToolsJSON renders the frozen catalog into the canonical
// provider-facing tool schema bytes. Creation and every later execution
// render from THIS document, so the bytes never drift with the installed
// implementation.
func (catalog *FrozenCatalog) ProviderToolsJSON() ([]byte, error) {
	if catalog == nil {
		return nil, fmt.Errorf("frozen tool catalog is absent")
	}
	tools := make([]any, 0, len(catalog.Tools))
	for _, tool := range catalog.Tools {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.Parameters,
			},
		})
	}
	return json.Marshal(tools)
}

// Digest is the SHA-256 hex of the frozen provider schema bytes — the value
// BeginModelCall verifies against the worker rendering and the value the
// model_calls provenance rows seal.
func (catalog *FrozenCatalog) Digest() (string, error) {
	body, err := catalog.ProviderToolsJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// Lookup resolves one tool inside THIS attempt's frozen catalog.
func (catalog *FrozenCatalog) Lookup(name string) (FrozenTool, bool) {
	if catalog == nil {
		return FrozenTool{}, false
	}
	for _, tool := range catalog.Tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return FrozenTool{}, false
}

// providerParametersEqual compares parameter schemas by canonical JSON
// bytes: a stored document round-trips numbers and arrays into generic
// forms, so structural equality is only meaningful post-normalisation.
func providerParametersEqual(def ToolDef, stored map[string]any) bool {
	installed, err := json.Marshal(providerParameters(def))
	if err != nil {
		return false
	}
	frozen, err := json.Marshal(stored)
	if err != nil {
		return false
	}
	return bytes.Equal(installed, frozen)
}

// InstalledDefinition verifies the frozen entry is still exactly what this
// binary compiles and returns the matching definition (its argument
// validator and execution plumbing). A drifted tool is denied at
// authorization: the frozen model-call bytes stay interpretable, and new
// attempts freeze the new catalog instead.
func (tool FrozenTool) InstalledDefinition() (ToolDef, error) {
	for _, def := range ImplementationTools() {
		if def.Name != tool.Name {
			continue
		}
		drift := ""
		switch {
		case def.Version != tool.Version:
			drift = fmt.Sprintf("version %s != frozen %s", def.Version, tool.Version)
		case def.ExecutionMode != tool.ExecutionMode:
			drift = fmt.Sprintf("execution mode %s != frozen %s", def.ExecutionMode, tool.ExecutionMode)
		case def.FailureMode != tool.FailureMode:
			drift = fmt.Sprintf("failure mode %s != frozen %s", def.FailureMode, tool.FailureMode)
		case def.ResultSchemaKind != tool.ResultSchemaKind:
			drift = "result schema kind drifted"
		case def.Description != tool.Description:
			drift = "description drifted"
		case def.ProducesEvidence != tool.ProducesEvidence:
			drift = "evidence behaviour drifted"
		case !providerParametersEqual(def, tool.Parameters):
			drift = "provider parameter schema drifted"
		}
		if drift != "" {
			return ToolDef{}, fmt.Errorf("frozen tool %s is incompatible with the installed executor: %s", tool.Name, drift)
		}
		return def, nil
	}
	return ToolDef{}, fmt.Errorf("frozen tool %s has no installed executor", tool.Name)
}

// legacyGenerationCatalog reproduces the catalog bytes of attempts created
// before per-attempt freezing: the full compiled generation catalog with the
// historical always-on plugin tools. It is the authorization fallback for a
// NULL tool_catalog_json only — never a creation path.
func legacyGenerationCatalog(agentVersion string) *FrozenCatalog {
	catalog := &FrozenCatalog{
		SchemaVersion: toolSchemaVersionFor(agentVersion),
		AgentVersion:  agentVersion,
	}
	references := knownResultSchemaReferences()
	for _, def := range ToolsForAgentVersion(agentVersion) {
		frozen := frozenToolFromDefinition(def)
		if reference, ok := references[def.ResultSchemaKind]; ok {
			frozen.ResultSchemaDigest = reference
		}
		catalog.Tools = append(catalog.Tools, frozen)
	}
	return catalog
}

// FrozenToolCatalog loads the attempt's frozen catalog document
// (attempt_input_snapshots.tool_catalog_json, ADR-0004).
func (service *Service) FrozenToolCatalog(ctx context.Context, attemptID int64) (*FrozenCatalog, error) {
	return frozenToolCatalogOn(ctx, service.db, attemptID)
}

// frozenToolCatalogOn is the transaction-composable form: callers inside a
// BEGIN IMMEDIATE ledger transaction must pass their own connection (the
// production pool is single-connection and a pool fetch would self-deadlock).
// A NULL document is the legacy shape: the attempt predates per-attempt
// freezing, and its catalog is the historical compiled generation catalog —
// the exact bytes its dispatch originally rendered.
func frozenToolCatalogOn(ctx context.Context, queries rowQuerier, attemptID int64) (*FrozenCatalog, error) {
	var document sql.NullString
	if err := queries.QueryRowContext(ctx, `SELECT tool_catalog_json FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&document); err != nil {
		return nil, err
	}
	if !document.Valid || document.String == "" {
		var agentVersion string
		if err := queries.QueryRowContext(ctx, `SELECT agent_version FROM execution_attempts WHERE id=?`, attemptID).Scan(&agentVersion); err != nil {
			return nil, err
		}
		return legacyGenerationCatalog(agentVersion), nil
	}
	var catalog FrozenCatalog
	if err := json.Unmarshal([]byte(document.String), &catalog); err != nil {
		return nil, fmt.Errorf("attempt %d frozen tool catalog is invalid: %w", attemptID, err)
	}
	return &catalog, nil
}

// FrozenToolCatalogDoc is the raw stored-catalog read for input REBUILDING:
// it returns nil (no document) instead of deriving a legacy catalog, because
// the rebuilt input bytes must reproduce the attempt's creation exactly —
// only attempts created with freezing carry the document.
func FrozenToolCatalogDoc(ctx context.Context, queries rowQuerier, attemptID int64) (*FrozenCatalog, error) {
	var document sql.NullString
	if err := queries.QueryRowContext(ctx, `SELECT tool_catalog_json FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&document); err != nil {
		return nil, err
	}
	if !document.Valid || document.String == "" {
		return nil, nil
	}
	var catalog FrozenCatalog
	if err := json.Unmarshal([]byte(document.String), &catalog); err != nil {
		return nil, fmt.Errorf("attempt %d frozen tool catalog is invalid: %w", attemptID, err)
	}
	return &catalog, nil
}

// CatalogFromInputDocument extracts the frozen catalog embedded in a
// canonical attempt input document. ok is false for legacy inputs created
// before per-attempt freezing; their consumers fall back to the historical
// generation rendering.
func CatalogFromInputDocument(canonical []byte) (catalog *FrozenCatalog, ok bool) {
	var document struct {
		ToolCatalog *FrozenCatalog `json:"toolCatalog"`
	}
	if err := json.Unmarshal(canonical, &document); err != nil || document.ToolCatalog == nil {
		return nil, false
	}
	return document.ToolCatalog, true
}

// CanonicalToolParameters renders the complete provider-facing parameter
// schema of one compiled definition. Hosts that adapt plugin execution
// bindings (e.g. the supervisor typed-executor registry) use it to verify a
// registering bundle against the frozen schema at registration time.
func CanonicalToolParameters(def ToolDef) map[string]any {
	return providerParameters(def)
}
