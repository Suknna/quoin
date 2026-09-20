package plugins

// Instance-settings validation (ADR-0004): a plugin's ConfigSchema is the
// closed contract every instance settings document must satisfy. The
// registry validates the schema STRUCTURE at registration; this file
// enforces the schema against INSTANCES. A nil schema means the plugin truly
// takes no configuration — any non-empty settings document is rejected.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrInvalidSettings reports an instance settings document that fails the
// plugin's declared ConfigSchema (unknown field, missing required field,
// wrong type, or non-empty settings for a schema-less plugin).
var ErrInvalidSettings = errors.New("invalid plugin instance settings")

// compiledSchemas caches the compiled ConfigSchema per plugin ID. The plugin
// table is immutable after freeze, so the cache never invalidates.
var (
	compiledSchemasMu sync.Mutex
	compiledSchemas   = map[*Registry]map[string]*jsonschema.Schema{}
)

// ValidateConfig validates one instance settings document against the
// plugin's declared ConfigSchema (Quoin calls this when authoring connection
// revisions). Semantics:
//
//   - plugin absent → ErrUnknownPlugin;
//   - nil schema → settings must be empty (nil, null, or {}): the plugin
//     declared it takes no configuration;
//   - declared schema → the document must satisfy it (closed object:
//     unknown fields, missing required fields and wrong types are
//     rejected) — wrapped in ErrInvalidSettings.
func (r *Registry) ValidateConfig(pluginID string, settings json.RawMessage) error {
	r.mu.Lock()
	r.ensureFrozen()
	plugin, exists := r.plugins[pluginID]
	configSchema := plugin.ConfigSchema
	validator := plugin.Validator
	r.mu.Unlock()
	if !exists {
		return fmt.Errorf("%w: %s", ErrUnknownPlugin, pluginID)
	}
	if configSchema == nil {
		if len(settings) == 0 || string(settings) == "null" || string(settings) == "{}" {
			return nil
		}
		return fmt.Errorf("%w: plugin %s declares no configuration but settings were supplied", ErrInvalidSettings, pluginID)
	}
	schema, err := r.compiledConfigSchema(pluginID, configSchema)
	if err != nil {
		return err
	}
	var instance any
	if len(settings) == 0 {
		instance = map[string]any{}
	} else if err := json.Unmarshal(settings, &instance); err != nil {
		return fmt.Errorf("%w: plugin %s settings are not valid JSON: %v", ErrInvalidSettings, pluginID, err)
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("%w: plugin %s settings: %v", ErrInvalidSettings, pluginID, err)
	}
	if validator != nil {
		if err := validator.ValidateConfig(settings); err != nil {
			return fmt.Errorf("%w: plugin %s settings: %v", ErrInvalidSettings, pluginID, err)
		}
	}
	return nil
}

// compiledConfigSchema compiles (once per plugin) the registered schema.
func (r *Registry) compiledConfigSchema(pluginID string, schema map[string]any) (*jsonschema.Schema, error) {
	compiledSchemasMu.Lock()
	defer compiledSchemasMu.Unlock()
	perRegistry, ok := compiledSchemas[r]
	if !ok {
		perRegistry = map[string]*jsonschema.Schema{}
		compiledSchemas[r] = perRegistry
	}
	if compiled, ok := perRegistry[pluginID]; ok {
		return compiled, nil
	}
	compiler := jsonschema.NewCompiler()
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(jsonBody(schema)))
	if err != nil {
		return nil, fmt.Errorf("%w: plugin %s config schema is not valid JSON: %v", ErrInvalidPlugin, pluginID, err)
	}
	const resourceID = "urn:quoin:plugin-config"
	if err := compiler.AddResource(resourceID, document); err != nil {
		return nil, fmt.Errorf("%w: plugin %s config schema: %v", ErrInvalidPlugin, pluginID, err)
	}
	compiled, err := compiler.Compile(resourceID)
	if err != nil {
		return nil, fmt.Errorf("%w: plugin %s config schema does not compile: %v", ErrInvalidPlugin, pluginID, err)
	}
	perRegistry[pluginID] = compiled
	return compiled, nil
}

// jsonBody re-encodes the plugin's schema map as canonical JSON bytes for
// the validator.
func jsonBody(schema map[string]any) []byte {
	body, _ := json.Marshal(schema)
	return body
}
