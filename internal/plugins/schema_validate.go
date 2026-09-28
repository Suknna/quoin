package plugins

// Instance-settings validation (ADR-0004): a plugin's ConfigSchema is the
// closed contract every CONNECTION instance settings document must satisfy.
// ADR-0014 story 2 adds the parallel EventSourceConfigSchema: the closed
// contract every EVENT SOURCE instance settings document must satisfy. A nil
// schema means the capability truly takes no configuration — any non-empty
// settings document is rejected.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrInvalidSettings reports an instance settings document that fails the
// plugin's declared ConfigSchema (unknown field, missing required field,
// wrong type, or non-empty settings for a schema-less plugin).
var ErrInvalidSettings = errors.New("invalid plugin instance settings")

// MaxEventSourceSettingsBytes bounds one alert-source instance settings
// document (schema.sql CHECK mirrors it): bounded non-secret configuration,
// never an unbounded channel.
const MaxEventSourceSettingsBytes = 64 << 10

// forbiddenSettingsFields are the property names a closed event-source
// settings schema must never allow at any depth. The list mirrors the
// connection revision projection ban (password/bearerToken/apiKey) and adds
// the two generic carriers token/secret; names are matched case- and
// separator-insensitively on the exact word only (pageToken stays legal).
var forbiddenSettingsFields = []string{"password", "bearertoken", "bearer_token", "apikey", "api_key", "token", "secret"}

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

// validateClosedSettingsSchema enforces the closed-shape and secret-material
// rules of an event-source settings schema at registration (ADR-0014 story
// 2): top-level object with additionalProperties disabled, and no property
// name anywhere in the schema that could carry secret material. A schema
// that violates the closure or bans fails the process at boot — the
// declaration is trusted compiled code, so mis-assembly is a programming
// error, not a runtime condition.
func validateClosedSettingsSchema(pluginID, role string, schema map[string]any) error {
	if schema["type"] != "object" {
		return fmt.Errorf("%w: plugin %s %s schema must declare type object", ErrInvalidPlugin, pluginID, role)
	}
	if schema["additionalProperties"] != false {
		return fmt.Errorf("%w: plugin %s %s schema must disable additionalProperties", ErrInvalidPlugin, pluginID, role)
	}
	if err := rejectSecretSchemaFields(pluginID, role, "$", schema); err != nil {
		return err
	}
	return nil
}

// rejectSecretSchemaFields walks every nested schema shape. References and
// pattern-driven/open object keys cannot prove the no-secret guarantee at
// registration, so they fail closed instead of delegating that check to a
// runtime JSON Schema compiler which correctly resolves them but does not
// enforce our separate security vocabulary.
func rejectSecretSchemaFields(pluginID, role, path string, node any) error {
	switch value := node.(type) {
	case map[string]any:
		for _, keyword := range []string{"$ref", "$dynamicRef", "patternProperties", "unevaluatedProperties"} {
			if _, ok := value[keyword]; ok {
				return fmt.Errorf("%w: plugin %s %s schema cannot prove secret-free fields through %s.%s", ErrInvalidPlugin, pluginID, role, path, keyword)
			}
		}
		if value["type"] == "object" || value["properties"] != nil {
			if value["additionalProperties"] != false {
				return fmt.Errorf("%w: plugin %s %s schema must close object at %s", ErrInvalidPlugin, pluginID, role, path)
			}
		}
		if properties, ok := value["properties"].(map[string]any); ok {
			names := make([]string, 0, len(properties))
			for name := range properties {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if isForbiddenSettingsField(name) {
					return fmt.Errorf("%w: plugin %s %s schema must not allow secret material: property %s.%s", ErrInvalidPlugin, pluginID, role, path, name)
				}
				if err := rejectSecretSchemaFields(pluginID, role, path+"."+name, properties[name]); err != nil {
					return err
				}
			}
		}
		keys := make([]string, 0, len(value))
		for key := range value {
			if key != "properties" {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := rejectSecretSchemaFields(pluginID, role, path+"."+key, value[key]); err != nil {
				return err
			}
		}
	case []any:
		for index, item := range value {
			if err := rejectSecretSchemaFields(pluginID, role, fmt.Sprintf("%s[%d]", path, index), item); err != nil {
				return err
			}
		}
	}
	return nil
}

// isForbiddenSettingsField matches the exact word case- and
// separator-insensitively: password, bearerToken, apiKey, api_key, token and
// secret are banned; longer compounds (pageToken, passwordPolicy) stay legal.
func isForbiddenSettingsField(name string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(name, "_", ""))
	for _, forbidden := range forbiddenSettingsFields {
		if normalized == strings.ReplaceAll(forbidden, "_", "") {
			return true
		}
	}
	return false
}

// compiledEventSourceSchemas caches the compiled EventSourceConfigSchema per
// plugin ID, separately from the connection ConfigSchema cache: one plugin
// may declare both, and they are different authorities.
var (
	compiledEventSourceSchemasMu sync.Mutex
	compiledEventSourceSchemas   = map[*Registry]map[string]*jsonschema.Schema{}
)

// ValidateEventSourceConfig validates one EVENT SOURCE instance settings
// document against the owning plugin's declared EventSourceConfigSchema
// (Quoin calls this when authoring alert-source settings, ADR-0014 story 2).
// Semantics mirror ValidateConfig:
//
//   - plugin absent → ErrUnknownPlugin;
//   - nil schema → settings must be empty (nil, null, or {}): the plugin
//     declared its source takes no configuration;
//   - declared schema → the document must satisfy it (closed object:
//     unknown fields, missing required fields and wrong types are
//     rejected) and stay within MaxEventSourceSettingsBytes — wrapped in
//     ErrInvalidSettings.
func (r *Registry) ValidateEventSourceConfig(pluginID string, settings json.RawMessage) error {
	r.mu.Lock()
	r.ensureFrozen()
	plugin, exists := r.plugins[pluginID]
	configSchema := plugin.EventSourceConfigSchema
	validator := plugin.EventSourceValidator
	r.mu.Unlock()
	if !exists {
		return fmt.Errorf("%w: %s", ErrUnknownPlugin, pluginID)
	}
	if len(settings) > MaxEventSourceSettingsBytes {
		return fmt.Errorf("%w: plugin %s event source settings exceed %d bytes", ErrInvalidSettings, pluginID, MaxEventSourceSettingsBytes)
	}
	if configSchema == nil {
		if len(settings) == 0 || string(settings) == "null" || string(settings) == "{}" {
			return nil
		}
		return fmt.Errorf("%w: plugin %s declares no event source configuration but settings were supplied", ErrInvalidSettings, pluginID)
	}
	schema, err := r.compiledEventSourceSchema(pluginID, configSchema)
	if err != nil {
		return err
	}
	var instance any
	if len(settings) == 0 {
		instance = map[string]any{}
	} else if err := json.Unmarshal(settings, &instance); err != nil {
		return fmt.Errorf("%w: plugin %s event source settings are not valid JSON: %v", ErrInvalidSettings, pluginID, err)
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("%w: plugin %s event source settings: %v", ErrInvalidSettings, pluginID, err)
	}
	if validator != nil {
		if err := validator.ValidateConfig(settings); err != nil {
			return fmt.Errorf("%w: plugin %s event source settings: %v", ErrInvalidSettings, pluginID, err)
		}
	}
	return nil
}

// compiledEventSourceSchema compiles (once per plugin) the registered event
// source schema into its own cache namespace.
func (r *Registry) compiledEventSourceSchema(pluginID string, schema map[string]any) (*jsonschema.Schema, error) {
	compiledEventSourceSchemasMu.Lock()
	defer compiledEventSourceSchemasMu.Unlock()
	perRegistry, ok := compiledEventSourceSchemas[r]
	if !ok {
		perRegistry = map[string]*jsonschema.Schema{}
		compiledEventSourceSchemas[r] = perRegistry
	}
	if compiled, ok := perRegistry[pluginID]; ok {
		return compiled, nil
	}
	compiler := jsonschema.NewCompiler()
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(jsonBody(schema)))
	if err != nil {
		return nil, fmt.Errorf("%w: plugin %s event source config schema is not valid JSON: %v", ErrInvalidPlugin, pluginID, err)
	}
	const resourceID = "urn:quoin:plugin-event-source-config"
	if err := compiler.AddResource(resourceID, document); err != nil {
		return nil, fmt.Errorf("%w: plugin %s event source config schema: %v", ErrInvalidPlugin, pluginID, err)
	}
	compiled, err := compiler.Compile(resourceID)
	if err != nil {
		return nil, fmt.Errorf("%w: plugin %s event source config schema does not compile: %v", ErrInvalidPlugin, pluginID, err)
	}
	perRegistry[pluginID] = compiled
	return compiled, nil
}
