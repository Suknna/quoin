// Package exploration owns the closed, model-facing Browser Tool vocabulary.
// It deliberately parses JSON into a finite action type instead of accepting a
// selector script, CDP method, Playwright command, URL fetch request, or any
// other executable browser instruction.
package exploration

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

var ErrInvalidInput = errors.New("invalid browser exploration action")

// Action is the complete, validated input that an Exploration executor may
// receive. Raw holds the original frozen canonical bytes for provenance only;
// Value and other input content must never be put into a trace.
type Action struct {
	Name      string
	SessionID string
	Raw       json.RawMessage
	Fields    map[string]json.RawMessage
}

var allowed = map[string]struct{}{
	"open": {}, "close_session": {}, "switch_page": {}, "close_page": {},
	"goto": {}, "back": {}, "forward": {}, "reload": {}, "click": {},
	"fill": {}, "select": {}, "check": {}, "uncheck": {}, "press": {},
	"scroll": {}, "read": {}, "screenshot": {}, "wait_for": {},
	"accept_dialog": {}, "dismiss_dialog": {},
}

// Parse verifies the runtime snapshot digest and every closed action shape.
// It is deliberately equivalent to the frozen request oneOf: unknown fields,
// untyped locators, malformed URLs, and omitted action-specific requirements
// are rejected before any browser process is contacted.
func Parse(canonical, digest []byte) (Action, error) {
	if len(canonical) == 0 || len(digest) != sha256.Size {
		return Action{}, fmt.Errorf("%w: missing canonical action or digest", ErrInvalidInput)
	}
	computed := sha256.Sum256(canonical)
	if string(computed[:]) != string(digest) {
		return Action{}, fmt.Errorf("%w: canonical digest mismatch", ErrInvalidInput)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &fields); err != nil || len(fields) == 0 {
		return Action{}, fmt.Errorf("%w: action must be a non-empty JSON object", ErrInvalidInput)
	}
	var name string
	if err := json.Unmarshal(fields["action"], &name); err != nil {
		return Action{}, fmt.Errorf("%w: action is required", ErrInvalidInput)
	}
	if _, ok := allowed[name]; !ok {
		return Action{}, fmt.Errorf("%w: action %q is not in the fixed tool vocabulary", ErrInvalidInput, name)
	}
	allowedFields, required := actionFields(name)
	for field := range fields {
		if !allowedFields[field] {
			return Action{}, fmt.Errorf("%w: action %s does not allow %q", ErrInvalidInput, name, field)
		}
	}
	for _, field := range required {
		if len(fields[field]) == 0 {
			return Action{}, fmt.Errorf("%w: action %s requires %s", ErrInvalidInput, name, field)
		}
	}
	var sessionID string
	if name != "open" {
		if err := json.Unmarshal(fields["sessionId"], &sessionID); err != nil || sessionID == "" || len(sessionID) > 200 {
			return Action{}, fmt.Errorf("%w: action %s requires a bounded sessionId", ErrInvalidInput, name)
		}
	}
	if err := validateFields(name, fields); err != nil {
		return Action{}, err
	}
	return Action{Name: name, SessionID: sessionID, Raw: append(json.RawMessage(nil), canonical...), Fields: fields}, nil
}

func actionFields(name string) (map[string]bool, []string) {
	base := map[string]bool{"action": true}
	with := func(fields ...string) map[string]bool {
		copy := make(map[string]bool, len(base)+len(fields))
		for key := range base {
			copy[key] = true
		}
		for _, key := range fields {
			copy[key] = true
		}
		return copy
	}
	session := []string{"action", "sessionId"}
	switch name {
	case "open":
		return with("businessSystemKey"), []string{"action", "businessSystemKey"}
	case "close_session", "back", "forward", "reload":
		return with("sessionId"), session
	case "read":
		// The frozen schema permits both a whole-session read and a targeted
		// read. A locator, when provided, must still be fully typed.
		return with("sessionId", "locator"), session
	case "switch_page", "close_page":
		return with("sessionId", "pageId"), append(session, "pageId")
	case "goto":
		return with("sessionId", "url"), append(session, "url")
	case "click", "check", "uncheck":
		return with("sessionId", "locator"), append(session, "locator")
	case "fill":
		return with("sessionId", "locator", "value"), append(session, "locator", "value")
	case "select":
		return with("sessionId", "locator", "values"), append(session, "locator", "values")
	case "press":
		return with("sessionId", "locator", "key"), append(session, "locator", "key")
	case "scroll":
		return with("sessionId", "deltaX", "deltaY"), append(session, "deltaX", "deltaY")
	case "screenshot":
		return with("sessionId", "fullPage"), session
	case "wait_for":
		return with("sessionId", "locator", "state"), append(session, "locator", "state")
	case "accept_dialog":
		return with("sessionId", "promptText"), session
	case "dismiss_dialog":
		return with("sessionId"), session
	default:
		return base, []string{"action"}
	}
}

func validateFields(name string, fields map[string]json.RawMessage) error {
	stringField := func(field string, max int, required bool) (string, error) {
		raw := fields[field]
		if len(raw) == 0 && !required {
			return "", nil
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || (required && value == "") || len(value) > max {
			return "", fmt.Errorf("%w: action %s has invalid %s", ErrInvalidInput, name, field)
		}
		return value, nil
	}
	switch name {
	case "open":
		_, err := stringField("businessSystemKey", 200, true)
		return err
	case "switch_page", "close_page":
		_, err := stringField("pageId", 100, true)
		return err
	case "goto":
		raw, err := stringField("url", 4096, true)
		if err != nil {
			return err
		}
		parsed, parseErr := url.ParseRequestURI(raw)
		if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("%w: goto URL must be http(s)", ErrInvalidInput)
		}
	case "click", "check", "uncheck", "fill", "select", "press", "wait_for":
		if err := validateLocator(fields["locator"]); err != nil {
			return err
		}
	case "read":
		if len(fields["locator"]) != 0 {
			if err := validateLocator(fields["locator"]); err != nil {
				return err
			}
		}
	}
	if name == "fill" {
		_, err := stringField("value", 16384, false)
		return err
	}
	if name == "press" {
		_, err := stringField("key", 100, true)
		return err
	}
	if name == "select" {
		var values []string
		if err := json.Unmarshal(fields["values"], &values); err != nil || len(values) == 0 || len(values) > 100 {
			return fmt.Errorf("%w: select requires 1..100 values", ErrInvalidInput)
		}
		seen := map[string]bool{}
		for _, value := range values {
			if len(value) > 1000 || seen[value] {
				return fmt.Errorf("%w: select values must be bounded and unique", ErrInvalidInput)
			}
			seen[value] = true
		}
	}
	if name == "scroll" {
		for _, field := range []string{"deltaX", "deltaY"} {
			var value int
			if err := json.Unmarshal(fields[field], &value); err != nil || value < -100000 || value > 100000 {
				return fmt.Errorf("%w: scroll %s is invalid", ErrInvalidInput, field)
			}
		}
	}
	if name == "wait_for" {
		state, err := stringField("state", 20, true)
		if err != nil || (state != "visible" && state != "hidden" && state != "enabled" && state != "disabled") {
			return fmt.Errorf("%w: wait_for state is invalid", ErrInvalidInput)
		}
	}
	if name == "screenshot" && len(fields["fullPage"]) != 0 {
		var fullPage bool
		if err := json.Unmarshal(fields["fullPage"], &fullPage); err != nil {
			return fmt.Errorf("%w: screenshot fullPage must be boolean", ErrInvalidInput)
		}
	}
	if name == "accept_dialog" && len(fields["promptText"]) != 0 {
		_, err := stringField("promptText", 16384, false)
		return err
	}
	return nil
}

func validateLocator(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		return fmt.Errorf("%w: locator must be an object", ErrInvalidInput)
	}
	var kind string
	if err := json.Unmarshal(fields["kind"], &kind); err != nil {
		return fmt.Errorf("%w: locator kind is required", ErrInvalidInput)
	}
	allowed, required := map[string]map[string]bool{
		"elementRef": {"kind": true, "ref": true, "observationVersion": true},
		"role":       {"kind": true, "role": true, "name": true, "exact": true},
		"label":      {"kind": true, "label": true, "exact": true},
		"text":       {"kind": true, "text": true, "exact": true},
		"testId":     {"kind": true, "testId": true},
	}[kind], ""
	if allowed == nil {
		return fmt.Errorf("%w: unsupported locator kind %q", ErrInvalidInput, kind)
	}
	for field := range fields {
		if !allowed[field] {
			return fmt.Errorf("%w: locator %s does not allow %s", ErrInvalidInput, kind, field)
		}
	}
	switch kind {
	case "elementRef":
		required = "ref"
		var version int
		if err := json.Unmarshal(fields["observationVersion"], &version); err != nil || version < 1 {
			return fmt.Errorf("%w: elementRef requires observationVersion", ErrInvalidInput)
		}
	case "role":
		required = "role"
	case "label":
		required = "label"
	case "text":
		required = "text"
	case "testId":
		required = "testId"
	}
	max := map[string]int{"elementRef": 200, "role": 100, "label": 500, "text": 1000, "testId": 300}[kind]
	var value string
	if err := json.Unmarshal(fields[required], &value); err != nil || strings.TrimSpace(value) == "" || len(value) > max {
		return fmt.Errorf("%w: locator %s requires bounded %s", ErrInvalidInput, kind, required)
	}
	if kind == "role" && len(fields["name"]) != 0 {
		var name string
		if err := json.Unmarshal(fields["name"], &name); err != nil || len(name) > 500 {
			return fmt.Errorf("%w: role locator name is invalid", ErrInvalidInput)
		}
	}
	if rawExact := fields["exact"]; len(rawExact) != 0 {
		var exact bool
		if err := json.Unmarshal(rawExact, &exact); err != nil {
			return fmt.Errorf("%w: locator exact must be boolean", ErrInvalidInput)
		}
	}
	return nil
}
