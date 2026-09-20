// Package operations owns Quoin's unified HTTP operation registration and
// admission guard for automatic access auditing (ADR-0006 /
// docs/audit-design.md §2 "统一入口"). Every admitted HTTP entry — Huma
// operations and raw mux wrappers alike — is declared once with its exact
// identity and access classification. Missing or contradicting declarations
// fail construction validation; the guard never guesses semantics from method
// or path.
//
// The admission guard centralizes the declared access boundary: it resolves
// the session or authentication-flow identity, enforces the declared level
// (401/403/503) before any handler runs, and records automatic access facts.
// Handlers keep their finer, per-object checks; the declared level replaces
// only the coarse session/role gate. Business commit facts, command ledgers
// and same-transaction success audit stay with the execution runner
// (internal/quoin/execution) and the domain handlers.
package operations

import (
	"fmt"
	"net/http"
)

// Level is the declared access boundary of an operation. The guard enforces
// it centrally before dispatch; handlers retain finer per-object checks.
type Level string

const (
	// LevelPublic marks the unauthenticated entry point of the authentication
	// flow surface (the operation that starts a flow). Anonymous traffic on
	// these operations is deliberately NOT written to the business audit
	// table (docs/audit-design.md §4: 匿名爆破与限速是有界安全日志/指标); the
	// auth flow records its own bounded phase events.
	LevelPublic Level = "public"
	// LevelSession marks operations reachable with any valid session,
	// including restricted (password-change-required) sessions — the password
	// change and logout themselves must stay reachable while restricted.
	LevelSession Level = "session"
	// LevelFull marks operations requiring a full session: authenticated,
	// past the restricted-password stage and initialized. Missing sessions
	// are rejected with 401; restricted or uninitialized sessions with 403.
	LevelFull Level = "full"
	// LevelAdmin marks operations requiring a full admin session; non-admin
	// sessions are rejected with 403.
	LevelAdmin Level = "admin"
)

// Kind is the declared access category used for audit presentation and
// coverage review. It never changes admission by itself.
type Kind string

const (
	KindQuery         Kind = "query"          // read without releasing stored content bytes
	KindCommand       Kind = "command"        // state change; authoritative result stays with the execution runner
	KindSensitiveRead Kind = "sensitive-read" // releases secret or stored content bytes
	KindStream        Kind = "stream"         // long-lived connection; audited once, not per frame
	KindSystem        Kind = "system"         // maintenance/system surfaces outside normal work
)

// Declaration is the fixed access contract of one operation. ID, Method and
// Path must match the registered Huma operation exactly — validation fails on
// any drift, and no classification is ever inferred from method or path.
type Declaration struct {
	// ID is the exact huma.OperationID (or the stable identifier of a raw
	// mux route, e.g. "downloadArtifactContent").
	ID string
	// Method and Path restate the exact route contract. Raw wrappers use
	// their ServeMux pattern (Path without method, e.g. "/api/v1/...");
	// any-method raw patterns leave Method empty.
	Method string
	Path   string
	Level  Level
	Kind   Kind
	// ObjectType names the audited domain object family (audit_events
	// domain_ref_type) when the operation targets one; empty otherwise.
	ObjectType string
	// Raw marks a declaration for a raw mux wrapper wired explicitly through
	// Admission.Wrap. Raw declarations are excluded from the Huma OpenAPI
	// presence check (they are not registered through huma.Register); Wrap
	// itself fails wiring for unknown ids.
	Raw bool
	// Planned marks a declaration reserved for an operation that is not (yet)
	// registered on a live surface. Planned declarations do not fail the
	// reverse coverage check but are still validated and, once their route
	// appears, enforced exactly. Planned is a transient state, never a final
	// escape hatch: adoption completes only when every desired operation is
	// registered and RemainingPlanned is asserted empty.
	Planned bool
}

// validate checks one declaration's internal consistency.
func (d Declaration) validate() error {
	if d.ID == "" {
		return fmt.Errorf("operations: declaration is missing its operation id")
	}
	if d.Path == "" || d.Path[0] != '/' {
		return fmt.Errorf("operations: declaration %q must declare an absolute path", d.ID)
	}
	switch d.Level {
	case LevelPublic, LevelSession, LevelFull, LevelAdmin:
	default:
		return fmt.Errorf("operations: declaration %q must declare a known level, got %q", d.ID, d.Level)
	}
	switch d.Kind {
	case KindQuery, KindCommand, KindSensitiveRead, KindStream, KindSystem:
	default:
		return fmt.Errorf("operations: declaration %q must declare a known kind, got %q", d.ID, d.Kind)
	}
	if d.Level == LevelPublic && d.Kind == KindSensitiveRead {
		return fmt.Errorf("operations: declaration %q cannot release sensitive content on the public surface", d.ID)
	}
	if d.Method != "" {
		switch d.Method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
		default:
			return fmt.Errorf("operations: declaration %q declares unknown HTTP method %q", d.ID, d.Method)
		}
	}
	return nil
}
