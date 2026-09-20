package plugins

// The compiled model-tool contract (ADR-0004, reworked by ADR-0011). One
// authority per tool: the type-safe generic Tool derives its manifest
// (ToolDef) from the typed definition — description, failure mode and the
// provider-facing parameter schema reflect off the typed argument struct —
// so declaration and implementation cannot drift by construction. JSON
// crosses the boundary exactly once: arguments decode into A at the rim,
// the handler works on typed values, the result R marshals back at the rim.
//
// Platform tools owned by the attempt core (workspace + artifact tools) keep
// using ToolDef directly; they are not plugin tools. Plugin tools always
// execute as quoin_routed: Quoin authorizes, audits and dispatches; the
// actual platform call runs through the injected PlatformCaller at the Stele
// gateway (credential injection + rate limiting). Handlers never touch
// credentials, URLs or TLS material.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Execution modes of the closed compiled vocabulary (wire value of
// ToolExecutionMode.QUOIN_ROUTED is "quoin_routed"; WORKER_LOCAL stays).
const (
	// ModeWorkerLocal: disposable Plinth worker sandbox (platform tools only).
	ModeWorkerLocal = "worker_local"
	// ModeQuoinRouted: Quoin permission/audit then Stele-gateway execution.
	ModeQuoinRouted = "quoin_routed"
)

// Failure modes (unchanged closed vocabulary).
const (
	FailureReturnToModel = "return_to_model"
	FailureFailAttempt   = "fail_attempt"
)

// rawMessageType special-cases json.RawMessage fields (embedded JSON
// documents) in derived schemas.
var rawMessageType = reflect.TypeOf(json.RawMessage{})

// ArgumentKind is the JSON kind of one argument (platform tool table).
type ArgumentKind string

const (
	KindString ArgumentKind = "string"
	KindNumber ArgumentKind = "number"
)

// ToolDef is the compiled manifest of one tool: everything the frozen
// catalogs, ingress validation and drift checks consume. Plugin tools derive
// theirs from Tool[A, R]; platform tools are authored directly.
type ToolDef struct {
	Name             string
	Version          string
	ExecutionMode    string // worker_local | quoin_routed
	FailureMode      string // return_to_model | fail_attempt
	ResultSchemaKind string // exact ResultPayload.schema_kind accepted at runtime ingress
	Description      string
	// Arguments lists the accepted top-level argument keys with their
	// required kind; "required" keys must be present. Derived automatically
	// for plugin tools.
	Arguments map[string]ArgumentKind
	Required  []string
	// ProducesEvidence marks an observation tool: a succeeded execution
	// commits deterministic Evidence together with the Tool Call terminal
	// state (ARCH-TOOL-005, DATA-EVIDENCE-001).
	ProducesEvidence bool
	// RequiresConnectionGrant marks a tool whose authorization freezes
	// connection grants inside the Tool Call persistence transaction
	// (ARCH-INPUT-003); the model never selects the connection.
	RequiresConnectionGrant bool
	// Parameters, when non-nil, is the complete frozen provider-facing JSON
	// Schema.
	Parameters map[string]any
	// ValidateArguments is the matching ingress validator for Parameters.
	// It must reject unknown fields and unsupported members.
	ValidateArguments func([]byte) error
	// ValidateResult, when set, is the ingress validator for the tool's
	// sealed result payload (dispatched by ResultSchemaKind).
	ValidateResult func([]byte) error
}

// ProviderParameters renders the complete provider-facing parameter schema of
// one compiled definition (the shared derivation of every catalog rendering).
func (def ToolDef) ProviderParameters() map[string]any {
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

// ---------------------------------------------------------------------------
// Gateway execution seam
// ---------------------------------------------------------------------------

// PlatformRequest is one authorized platform call: method + relative path
// against the resolved connection endpoint. scheme/host never appear here —
// the gateway resolves the endpoint from the configured connection, which
// keeps the SSRF surface closed (ADR-0011).
type PlatformRequest struct {
	Method  string // GET | POST
	Path    string // relative path, starts with "/"
	Query   url.Values
	Header  http.Header // extra non-credential headers; auth is injected by the gateway
	Body    []byte      // GET must carry none
	Timeout time.Duration
}

// PlatformResponse is the platform's raw HTTP answer. Any status code —
// including the platform's own 4xx/5xx — is a transport success; the handler
// interprets the semantics.
type PlatformResponse struct {
	StatusCode int
	Body       []byte
}

// Gateway-level failure classes. The PlatformCaller returns (wrapped) one of
// these; Quoin's executor maps them onto stable tool error codes.
var (
	// ErrPlatformRateLimited: the Stele-side quota refused the call.
	ErrPlatformRateLimited = errors.New("platform rate limited")
	// ErrPlatformUnreachable: the platform was unreachable or timed out.
	ErrPlatformUnreachable = errors.New("platform unreachable")
	// ErrCredentialUnavailable: connection material is missing, revoked or
	// could not be acquired.
	ErrCredentialUnavailable = errors.New("credential unavailable")
)

// PlatformCaller executes one authorized platform request through the Stele
// gateway (credential injection, rate limiting, transport). Quoin's executor
// implements it over the gateway stream; the Stele process is the only
// implementation that touches real platforms.
type PlatformCaller interface {
	Call(ctx context.Context, req PlatformRequest) (*PlatformResponse, error)
}

// SpillFunc commits one long raw body as a tool_result Artifact and returns
// the committed artifact id (wired by the executing host).
type SpillFunc func(ctx context.Context, body []byte, mediaType string) (int64, error)

// ---------------------------------------------------------------------------
// Generic tool assembly
// ---------------------------------------------------------------------------

// ToolContext is the handler-side view of one outbound execution: the frozen
// connection context, the gateway caller and the optional long-body spill
// seam. It never carries credentials.
type ToolContext struct {
	Context  context.Context
	Conn     Connection
	Platform PlatformCaller
	// Spill, when non-nil, commits one long raw body into the tool_result
	// Artifact store (long outputs spill instead of inflating the model
	// context).
	Spill SpillFunc
}

// Handler is the typed tool implementation: business logic runs here (in
// Quoin), platform I/O only through Platform, long bodies through Spill.
type Handler[A any, R any] func(t *ToolContext, args A) (R, error)

// Tool is one type-safe outbound tool. A is the typed argument struct; its
// json tags drive the derived parameter schema (a field without omitempty is
// required) and an optional `doc:"…"` tag contributes the property
// description. R is the typed result; its marshaled bytes are the sealed
// result payload.
type Tool[A any, R any] struct {
	Name        string
	Version     string
	FailureMode string
	ResultKind  string // ResultPayload.schema_kind
	Description string
	// Schema, when non-nil, replaces the reflection-derived parameter schema.
	Schema map[string]any
	// ProducesEvidence / RequiresConnectionGrant mirror the ToolDef flags.
	ProducesEvidence        bool
	RequiresConnectionGrant bool
	// Internal marks tools Quoin's schedulers call directly (probe, discover,
	// collect): they never render into model catalogs.
	Internal bool
	// Timeout bounds one execution; zero means the executor default.
	Timeout time.Duration
	// RateLimitPerMinute is the default per-connection quota the Stele
	// gateway enforces; zero means the gateway default.
	RateLimitPerMinute int

	Handler Handler[A, R]
}

// ToolEntry is the registered, type-erased form of one Tool: the derived
// manifest plus the invocation closure. Both halves come from the same
// generic definition — there is no second tool protocol.
type ToolEntry struct {
	Definition ToolDef
	// Owner is the contributing plugin ID. Several plugins may contribute
	// the same shared-contract tool (identical definitions); the catalog
	// keeps one entry and provenance lists every contributor.
	Owner string
	// Internal tools are callable by Quoin's schedulers only.
	Internal bool
	// Timeout / RateLimitPerMinute are the gateway execution hints.
	Timeout            time.Duration
	RateLimitPerMinute int

	// Invoke decodes the canonical arguments, runs the typed handler and
	// marshals the typed result. The returned bytes are the sealed result
	// payload (the handler shapes structured failures itself); a non-nil
	// error is an execution-level failure mapped onto stable error codes.
	Invoke ToolInvoker
}

// ToolExecution carries one authorized outbound execution into the
// type-erased invoker.
type ToolExecution struct {
	Arguments json.RawMessage
	Conn      Connection
	Platform  PlatformCaller
	Spill     SpillFunc
}

// ToolInvoker is the type-erased invocation seam.
type ToolInvoker func(ctx context.Context, exec ToolExecution) (json.RawMessage, error)

// Entry derives the compile-time assembly unit of one typed tool. Calling it
// is cheap and pure; registries dedupe shared contracts on manifest equality.
func (t Tool[A, R]) Entry(owner string) ToolEntry {
	parameters := t.Schema
	if parameters == nil {
		parameters = deriveParameters[A]()
	}
	definition := ToolDef{
		Name:                    t.Name,
		Version:                 t.Version,
		ExecutionMode:           ModeQuoinRouted,
		FailureMode:             t.FailureMode,
		ResultSchemaKind:        t.ResultKind,
		Description:             t.Description,
		Arguments:               argumentKinds(parameters),
		ProducesEvidence:        t.ProducesEvidence,
		RequiresConnectionGrant: t.RequiresConnectionGrant,
		Parameters:              parameters,
		ValidateArguments: func(raw []byte) error {
			var args A
			if err := decodeToolArguments(raw, &args); err != nil {
				return err
			}
			return requireDerivedArguments(parameters, raw)
		},
	}
	return ToolEntry{
		Definition:         definition,
		Owner:              owner,
		Internal:           t.Internal,
		Timeout:            t.Timeout,
		RateLimitPerMinute: t.RateLimitPerMinute,
		Invoke: func(ctx context.Context, exec ToolExecution) (json.RawMessage, error) {
			var args A
			if err := decodeToolArguments(exec.Arguments, &args); err != nil {
				return nil, err
			}
			result, err := t.Handler(&ToolContext{
				Context: ctx, Conn: exec.Conn, Platform: exec.Platform, Spill: exec.Spill,
			}, args)
			if err != nil {
				return nil, err
			}
			return json.Marshal(result)
		},
	}
}

// requireDerivedArguments enforces the derived required set: a required
// property must be present, and required strings must be non-empty (the
// historical ingress semantics of the platform tool table).
func requireDerivedArguments(parameters map[string]any, raw []byte) error {
	required := argumentNames(parameters["required"])
	if len(required) == 0 {
		return nil
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return fmt.Errorf("arguments are not a JSON object: %w", err)
	}
	for _, name := range required {
		value, exists := object[name]
		if !exists || value == nil {
			return fmt.Errorf("argument %q is required", name)
		}
		if text, isString := value.(string); isString && text == "" {
			return fmt.Errorf("argument %q must be a non-empty string", name)
		}
	}
	return nil
}

// argumentNames accepts both the derived []string form and the JSON-style
// []any form of a required list (explicit Schema overrides).
func argumentNames(value any) []string {
	switch list := value.(type) {
	case []string:
		return list
	case []any:
		names := make([]string, 0, len(list))
		for _, item := range list {
			if name, ok := item.(string); ok {
				names = append(names, name)
			}
		}
		return names
	}
	return nil
}

// decodeToolArguments decodes the canonical arguments object strictly:
// unknown fields are rejected so the frozen schema stays closed.
func decodeToolArguments(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("arguments do not satisfy the tool schema: %w", err)
	}
	if decoder.More() {
		return errors.New("arguments carry trailing content")
	}
	return nil
}

// deriveParameters reflects the typed argument struct into the closed
// provider-facing JSON Schema: properties from json tags, required from the
// absence of omitempty, descriptions from `doc` tags. Unsupported field
// kinds fail registration (the vocabulary stays closed, like the historical
// string/number table).
func deriveParameters[A any]() map[string]any {
	properties := map[string]any{}
	var required []string
	var reflectErr error
	instance := reflect.TypeOf((*A)(nil)).Elem()
	if instance.Kind() != reflect.Struct {
		panic(fmt.Sprintf("tool argument type %s must be a struct", instance))
	}
	for i := 0; i < instance.NumField(); i++ {
		field := instance.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		property, err := propertySchema(field.Type)
		if err != nil {
			reflectErr = fmt.Errorf("argument %q: %w", name, err)
			break
		}
		if doc := field.Tag.Get("doc"); doc != "" {
			if _, ok := property.(map[string]any); ok {
				property.(map[string]any)["description"] = doc
			}
		}
		properties[name] = property
		if !strings.Contains(field.Tag.Get("json"), "omitempty") {
			required = append(required, name)
		}
	}
	if reflectErr != nil {
		panic(fmt.Sprintf("tool argument schema derivation failed: %v", reflectErr))
	}
	sort.Strings(required)
	return map[string]any{"type": "object", "properties": properties, "required": required}
}

// propertySchema maps one Go field kind onto the closed schema vocabulary.
func propertySchema(t reflect.Type) (any, error) {
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.Slice:
		if t == rawMessageType {
			return map[string]any{"type": "object"}, nil
		}
		if t.Elem().Kind() == reflect.String {
			return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, nil
		}
		if t.Elem().Kind() == reflect.Float32 || t.Elem().Kind() == reflect.Float64 ||
			isIntKind(t.Elem().Kind()) {
			return map[string]any{"type": "array", "items": map[string]any{"type": "number"}}, nil
		}
	case reflect.Pointer:
		return propertySchema(t.Elem())
	}
	return nil, fmt.Errorf("field kind %s is outside the closed schema vocabulary", t)
}

func isIntKind(kind reflect.Kind) bool {
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

// argumentKinds projects the derived schema back onto the simple argument
// table (kept for ingress validators that consume Arguments/Required).
func argumentKinds(parameters map[string]any) map[string]ArgumentKind {
	kinds := map[string]ArgumentKind{}
	properties, _ := parameters["properties"].(map[string]any)
	for name, property := range properties {
		schema, _ := property.(map[string]any)
		switch schema["type"] {
		case "number", "boolean":
			kinds[name] = KindNumber
		default:
			kinds[name] = KindString
		}
	}
	return kinds
}
