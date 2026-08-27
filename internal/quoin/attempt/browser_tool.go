package attempt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var browserActions = map[string]bool{"open": true, "close_session": true, "switch_page": true, "close_page": true, "goto": true, "back": true, "forward": true, "reload": true, "click": true, "fill": true, "select": true, "check": true, "uncheck": true, "press": true, "scroll": true, "read": true, "screenshot": true, "wait_for": true, "accept_dialog": true, "dismiss_dialog": true}

// browserToolParameters is deliberately a finite oneOf rather than a generic
// object. The model cannot use this tool to smuggle an arbitrary browser/CDP
// command through provider-side schema validation.
func browserToolParameters() map[string]any {
	request := func(action any, required []string, properties map[string]any) map[string]any {
		properties["action"] = action
		return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
	}
	session := map[string]any{"type": "string", "minLength": 1, "maxLength": 200}
	page := map[string]any{"type": "string", "minLength": 1, "maxLength": 100}
	locator := browserToolLocatorSchema()
	return map[string]any{"oneOf": []any{
		request(map[string]any{"const": "open"}, []string{"action", "businessSystemKey"}, map[string]any{"businessSystemKey": map[string]any{"type": "string", "minLength": 1, "maxLength": 200}}),
		request(map[string]any{"enum": []string{"close_session", "back", "forward", "reload", "read"}}, []string{"action", "sessionId"}, map[string]any{"sessionId": session}),
		// The frozen request union permits both a whole-session read and an
		// explicitly targeted read. Keep these separate so the provider schema
		// cannot silently discard a model-supplied locator.
		request(map[string]any{"const": "read"}, []string{"action", "sessionId", "locator"}, map[string]any{"sessionId": session, "locator": locator}),
		request(map[string]any{"enum": []string{"switch_page", "close_page"}}, []string{"action", "sessionId", "pageId"}, map[string]any{"sessionId": session, "pageId": page}),
		request(map[string]any{"const": "goto"}, []string{"action", "sessionId", "url"}, map[string]any{"sessionId": session, "url": map[string]any{"type": "string", "minLength": 8, "maxLength": 4096, "pattern": `^https?://[^/?#\s\u0000-\u001F\u007F]+(?:[/?#]|$)`}}),
		request(map[string]any{"enum": []string{"click", "check", "uncheck"}}, []string{"action", "sessionId", "locator"}, map[string]any{"sessionId": session, "locator": locator}),
		request(map[string]any{"const": "fill"}, []string{"action", "sessionId", "locator", "value"}, map[string]any{"sessionId": session, "locator": locator, "value": map[string]any{"type": "string", "maxLength": 16384}}),
		request(map[string]any{"const": "select"}, []string{"action", "sessionId", "locator", "values"}, map[string]any{"sessionId": session, "locator": locator, "values": map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "uniqueItems": true, "items": map[string]any{"type": "string", "maxLength": 1000}}}),
		request(map[string]any{"const": "press"}, []string{"action", "sessionId", "locator", "key"}, map[string]any{"sessionId": session, "locator": locator, "key": map[string]any{"type": "string", "minLength": 1, "maxLength": 100}}),
		request(map[string]any{"const": "scroll"}, []string{"action", "sessionId", "deltaX", "deltaY"}, map[string]any{"sessionId": session, "deltaX": map[string]any{"type": "integer", "minimum": -100000, "maximum": 100000}, "deltaY": map[string]any{"type": "integer", "minimum": -100000, "maximum": 100000}}),
		request(map[string]any{"const": "screenshot"}, []string{"action", "sessionId"}, map[string]any{"sessionId": session, "fullPage": map[string]any{"type": "boolean"}}),
		request(map[string]any{"const": "wait_for"}, []string{"action", "sessionId", "locator", "state"}, map[string]any{"sessionId": session, "locator": locator, "state": map[string]any{"enum": []string{"visible", "hidden", "enabled", "disabled"}}}),
		request(map[string]any{"const": "accept_dialog"}, []string{"action", "sessionId"}, map[string]any{"sessionId": session, "promptText": map[string]any{"type": "string", "maxLength": 16384}}),
		request(map[string]any{"const": "dismiss_dialog"}, []string{"action", "sessionId"}, map[string]any{"sessionId": session}),
	}}
}

// browserToolLocatorSchema mirrors the frozen Browser Tool contract in the
// provider-facing schema. A bare object here would let a provider accept CSS or
// arbitrary fields that Quoin later rejects, splitting the one frozen contract.
func browserToolLocatorSchema() map[string]any {
	stringField := func(minimum, maximum int) map[string]any {
		field := map[string]any{"type": "string", "maxLength": maximum}
		if minimum > 0 {
			field["minLength"] = minimum
		}
		return field
	}
	return map[string]any{"description": "A frozen role, label, text, testId or observation elementRef locator; never CSS, XPath or JavaScript.", "oneOf": []any{
		map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "ref", "observationVersion"}, "properties": map[string]any{"kind": map[string]any{"const": "elementRef"}, "ref": stringField(1, 200), "observationVersion": map[string]any{"type": "integer", "minimum": 1}}},
		map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "role"}, "properties": map[string]any{"kind": map[string]any{"const": "role"}, "role": stringField(1, 100), "name": stringField(0, 500), "exact": map[string]any{"type": "boolean", "default": true}}},
		map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "label"}, "properties": map[string]any{"kind": map[string]any{"const": "label"}, "label": stringField(1, 500), "exact": map[string]any{"type": "boolean", "default": true}}},
		map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "text"}, "properties": map[string]any{"kind": map[string]any{"const": "text"}, "text": stringField(1, 1000), "exact": map[string]any{"type": "boolean", "default": true}}},
		map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "testId"}, "properties": map[string]any{"kind": map[string]any{"const": "testId"}, "testId": stringField(1, 300)}},
	}}
}

func validateBrowserToolArguments(body []byte) error {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return fmt.Errorf("quoin_browser arguments must be a JSON object")
	}
	schema, err := loadBrowserToolSchema()
	if err != nil {
		return fmt.Errorf("load browser tool schema: %w", err)
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("validate frozen browser request schema: %w", err)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(body, &values); err != nil || values == nil {
		return fmt.Errorf("quoin_browser arguments must be a JSON object")
	}
	var action string
	if err := json.Unmarshal(values["action"], &action); err != nil {
		return fmt.Errorf("quoin_browser requires action")
	}
	allowed := map[string]map[string]bool{
		"open":          {"action": true, "businessSystemKey": true},
		"close_session": {"action": true, "sessionId": true}, "back": {"action": true, "sessionId": true}, "forward": {"action": true, "sessionId": true}, "reload": {"action": true, "sessionId": true}, "read": {"action": true, "sessionId": true, "locator": true},
		"switch_page": {"action": true, "sessionId": true, "pageId": true}, "close_page": {"action": true, "sessionId": true, "pageId": true},
		"goto":  {"action": true, "sessionId": true, "url": true},
		"click": {"action": true, "sessionId": true, "locator": true}, "check": {"action": true, "sessionId": true, "locator": true}, "uncheck": {"action": true, "sessionId": true, "locator": true},
		"fill": {"action": true, "sessionId": true, "locator": true, "value": true}, "select": {"action": true, "sessionId": true, "locator": true, "values": true}, "press": {"action": true, "sessionId": true, "locator": true, "key": true},
		"scroll": {"action": true, "sessionId": true, "deltaX": true, "deltaY": true}, "screenshot": {"action": true, "sessionId": true, "fullPage": true}, "wait_for": {"action": true, "sessionId": true, "locator": true, "state": true},
		"accept_dialog": {"action": true, "sessionId": true, "promptText": true}, "dismiss_dialog": {"action": true, "sessionId": true},
	}
	fields, ok := allowed[action]
	if !ok {
		return fmt.Errorf("quoin_browser action %q is not allowed", action)
	}
	for key := range values {
		if !fields[key] {
			return fmt.Errorf("quoin_browser action %s does not allow %q", action, key)
		}
	}
	need := func(key string) error {
		var s string
		if raw := values[key]; len(raw) == 0 || json.Unmarshal(raw, &s) != nil || strings.TrimSpace(s) == "" {
			return fmt.Errorf("quoin_browser action %s requires non-empty %s", action, key)
		}
		return nil
	}
	if action == "open" {
		return need("businessSystemKey")
	}
	if err := need("sessionId"); err != nil {
		return err
	}
	switch action {
	case "switch_page", "close_page":
		return need("pageId")
	case "goto":
		if err := need("url"); err != nil || !strings.HasPrefix(strings.TrimSpace(string(values["url"])), `"http`) {
			return fmt.Errorf("quoin_browser goto requires an http(s) URL")
		}
	case "click", "check", "uncheck", "fill", "select", "press", "wait_for":
		if err := validateBrowserLocator(values["locator"]); err != nil {
			return err
		}
	}
	if action == "dismiss_dialog" && values["promptText"] != nil {
		return fmt.Errorf("quoin_browser dismiss_dialog does not allow promptText")
	}
	return nil
}

func validateBrowserLocator(raw json.RawMessage) error {
	var locator map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &locator) != nil {
		return fmt.Errorf("quoin_browser locator must be an object")
	}
	var kind string
	if json.Unmarshal(locator["kind"], &kind) != nil {
		return fmt.Errorf("quoin_browser locator requires kind")
	}
	allowed := map[string]map[string]bool{
		"elementRef": {"kind": true, "ref": true, "observationVersion": true},
		"role":       {"kind": true, "role": true, "name": true, "exact": true},
		"label":      {"kind": true, "label": true, "exact": true},
		"text":       {"kind": true, "text": true, "exact": true},
		"testId":     {"kind": true, "testId": true},
	}
	fields, ok := allowed[kind]
	if !ok {
		return fmt.Errorf("quoin_browser locator kind %q is not allowed", kind)
	}
	for key := range locator {
		if !fields[key] {
			return fmt.Errorf("quoin_browser locator does not allow %q", key)
		}
	}
	required := map[string]string{"elementRef": "ref", "role": "role", "label": "label", "text": "text", "testId": "testId"}[kind]
	var value string
	if json.Unmarshal(locator[required], &value) != nil || strings.TrimSpace(value) == "" {
		return fmt.Errorf("quoin_browser locator %s requires %s", kind, required)
	}
	if kind == "elementRef" {
		var version int
		if json.Unmarshal(locator["observationVersion"], &version) != nil || version < 1 {
			return fmt.Errorf("quoin_browser elementRef requires observationVersion")
		}
	}
	return nil
}

var (
	browserToolSchemaOnce sync.Once
	browserToolSchema     *jsonschema.Schema
	browserToolSchemaErr  error
)

func loadBrowserToolSchema() (*jsonschema.Schema, error) {
	browserToolSchemaOnce.Do(func() {
		document, err := jsonschema.UnmarshalJSON(strings.NewReader(string(contracts.BrowserToolSchema)))
		if err != nil {
			browserToolSchemaErr = err
			return
		}
		compiler := jsonschema.NewCompiler()
		compiler.UseRegexpEngine(browserToolECMARegexp)
		const resource = "https://github.com/Suknna/quoin/schemas/browser-tool.schema.json"
		if err := compiler.AddResource(resource, document); err != nil {
			browserToolSchemaErr = err
			return
		}
		browserToolSchema, browserToolSchemaErr = compiler.Compile(resource)
	})
	return browserToolSchema, browserToolSchemaErr
}

func browserToolECMARegexp(pattern string) (jsonschema.Regexp, error) {
	translated := regexp.MustCompile(`\\u([0-9a-fA-F]{4})`).ReplaceAllStringFunc(pattern, func(token string) string {
		return `\x` + token[len(token)-2:]
	})
	return regexp.Compile(translated)
}

// validateBrowserToolResult validates the complete frozen browser-tool JSON
// schema, including bounded Observation/Event/Error substructures. The local
// checks below only enforce result facts that span the envelope and schema.
func validateBrowserToolResult(canonical []byte) error {
	var value any
	if err := json.Unmarshal(canonical, &value); err != nil {
		return fmt.Errorf("browser result must be JSON: %w", err)
	}
	schema, err := loadBrowserToolSchema()
	if err != nil {
		return fmt.Errorf("load browser tool schema: %w", err)
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("validate browser result schema: %w", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &body); err != nil {
		return fmt.Errorf("browser result must be JSON: %w", err)
	}
	allowed := map[string]bool{"outcome": true, "action": true, "sessionId": true, "observation": true, "error": true, "screenshotArtifactId": true}
	for key := range body {
		if !allowed[key] {
			return fmt.Errorf("browser result field %q is not allowed", key)
		}
	}
	var outcome, action string
	if err := json.Unmarshal(body["outcome"], &outcome); err != nil || (outcome != "success" && outcome != "recoverable_error" && outcome != "session_closed") {
		return fmt.Errorf("browser result requires valid outcome")
	}
	if err := json.Unmarshal(body["action"], &action); err != nil || !browserActions[action] {
		return fmt.Errorf("browser result requires valid action")
	}
	_, hasError := body["error"]
	if (outcome == "success") == hasError {
		return fmt.Errorf("browser result outcome/error combination is invalid")
	}
	if action != "open" {
		var session string
		if err := json.Unmarshal(body["sessionId"], &session); err != nil || session == "" {
			return fmt.Errorf("browser result requires sessionId")
		}
	}
	if hasError {
		var failure struct {
			Code, Message string
			Retryable     bool `json:"retryableInSession"`
		}
		if err := json.Unmarshal(body["error"], &failure); err != nil || failure.Code == "" || failure.Message == "" {
			return fmt.Errorf("browser result requires a bounded error")
		}
	}
	return nil
}
