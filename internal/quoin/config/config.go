// Package config owns the shared strict-configuration machinery for the two
// human-edited YAML documents (Business System configuration and Label
// Contract, CFG-YAML-001): the strict single-document YAML lexical parse, the
// frozen JSON Schema structural validation, the official PromQL AST ownership
// rules, the cron/timezone/unique-key semantics and the embedded Journey
// Catalog static validation. The field inventory itself is owned only by
// contracts/schemas/*.schema.json; this package never re-declares it in Go
// struct tags (CFG-YAML-001).
package config

import "strings"

// FieldError is one item of the frozen 422 fieldErrors list
// (HTTP-ERROR-001): a document path, an ordinary-language reason and an
// actionable remediation hint.
type FieldError struct {
	Path        string `json:"path"`
	Reason      string `json:"reason"`
	Remediation string `json:"remediation,omitempty"`
}

// ValidationError carries every deterministic field error found during the
// parse-once validation pipeline; the HTTP layer maps it to 422 with the
// complete list (no partial reporting).
type ValidationError struct {
	Errors []FieldError
}

func (err *ValidationError) Error() string {
	parts := make([]string, 0, len(err.Errors))
	for _, item := range err.Errors {
		parts = append(parts, item.Path+": "+item.Reason)
	}
	return strings.Join(parts, "; ")
}

func validationError(path, reason, remediation string) *ValidationError {
	return &ValidationError{Errors: []FieldError{{Path: path, Reason: reason, Remediation: remediation}}}
}

// ParserVersion identifies the strict YAML parse mechanism persisted with
// every uploaded document (CFG-YAML-001: parser/schema versions ride along
// the immutable row).
const ParserVersion = "quoin-strict-yaml-1"

// SchemaVersion returns the frozen schema identity ($id) persisted with a
// document validated against the named schema.
func SchemaVersion(name string) string {
	if schema, ok := compiledSchemas[name]; ok {
		return schema.version
	}
	return name
}
