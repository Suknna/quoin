package plugins

import (
	"encoding/json"
	"fmt"
)

// ValidateInspectionParams applies one registered template's closed parameter
// schema and its optional static semantic validator. Runtime validation uses
// the same frozen declaration as the management catalog and Run executor;
// unfamiliar plugins never need a new host-side template switch.
func (r *Registry) ValidateInspectionParams(pluginID, templateID, version string, params map[string]any) error {
	template, ok := r.InspectionTemplate(pluginID, templateID, version)
	if !ok {
		return fmt.Errorf("%w: inspection template %s/%s/%s is not registered", ErrUnknownPlugin, pluginID, templateID, version)
	}
	return r.validateTemplateParams(pluginID, template, params)
}

// validateTemplateParams also runs before a plugin is stored in the registry
// when checking its optional starter plan. That keeps an invalid starter
// declaration from failing only when the first connection is enabled.
func (r *Registry) validateTemplateParams(pluginID string, template InspectionTemplate, params map[string]any) error {
	templateID, version := template.ID, template.Version
	if params == nil {
		params = map[string]any{}
	}
	if template.ParamsSchema == nil {
		if len(params) != 0 {
			return fmt.Errorf("%w: template %s/%s declares no parameters", ErrInvalidSettings, pluginID, templateID)
		}
	} else {
		compiled, err := r.compiledConfigSchema(pluginID+":inspection:"+templateID+":"+version, template.ParamsSchema)
		if err != nil {
			return err
		}
		// Normalize numeric values as they arrive over JSON HTTP. Plan commands
		// issued directly by a host observe the same schema/validator semantics.
		body, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("%w: inspection params cannot be encoded: %v", ErrInvalidSettings, err)
		}
		var normalized map[string]any
		if err := json.Unmarshal(body, &normalized); err != nil {
			return fmt.Errorf("%w: inspection params are invalid JSON: %v", ErrInvalidSettings, err)
		}
		if err := compiled.Validate(normalized); err != nil {
			return fmt.Errorf("%w: inspection template %s/%s params: %v", ErrInvalidSettings, pluginID, templateID, err)
		}
		params = normalized
	}
	if template.ValidateParams != nil {
		if err := template.ValidateParams(params); err != nil {
			return fmt.Errorf("%w: inspection template %s/%s params: %v", ErrInvalidSettings, pluginID, templateID, err)
		}
	}
	return nil
}
