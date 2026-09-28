package plugins

// Trusted HTTP connection kinds (ADR-0014): the bounded, registry-declared
// class of outbound connection kinds the Stele gateway may execute. A plugin
// declares its connection kind, the HTTP transport and the closed subset of
// auth modes it needs; Quoin's connections domain resolves declarations
// through the frozen registry (never a host-side type switch), and Stele
// keeps network execution, TLS and credential injection generic over the
// class. Non-HTTP transports and new auth modes are deliberate contract
// extensions — they fail closed until designed, never masquerade as HTTP.

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// ConnectionTransportHTTP is the only connection transport vocabulary today.
// It promises exactly the bounded gateway contract Quoin validates and Stele
// executes: absolute http(s) base URL, closed TLS options, GET|POST methods.
const ConnectionTransportHTTP = "http"

// The bounded HTTP auth-mode vocabulary (ADR-0014). Auth mode is non-secret
// revision metadata; credential material never appears in any declaration.
const (
	AuthModeNone   = "none"
	AuthModeBasic  = "basic"
	AuthModeBearer = "bearer"
)

// reservedConnectionKinds are connection identities Quoin owns outside the
// plugin gateway class. model_provider credentials travel only to Plinth
// (FetchCredentialGrant) and must never become gateway-executable.
var reservedConnectionKinds = map[string]bool{"model_provider": true}

// HTTPProbeContract is the frozen, bounded read-only probe contract of one
// trusted HTTP connection kind: one GET request against a declared absolute
// path, expected to answer with a single declared 2xx status. The contract is
// compiled plugin declaration (never instance configuration); Quoin freezes
// it into every probe attempt of the kind and executes it through the generic
// gateway, so enabling a connection always rests on a real bounded request.
type HTTPProbeContract struct {
	// Method is fixed to GET — the probe vocabulary is read-only by
	// construction; state-changing verbs need an explicit contract extension.
	Method string
	// Path is the absolute request path (starts with "/", may carry a query
	// string, no spaces or control characters).
	Path string
	// ExpectStatus is the single 2xx status the probe treats as success.
	ExpectStatus int
}

// HTTPConnectionKind is the frozen declaration of one trusted HTTP
// connection kind: a plugin's promise that instances of this kind speak the
// generic gateway HTTP contract within the declared auth modes.
type HTTPConnectionKind struct {
	// Kind is the connections.type identity (e.g. "prometheus").
	Kind string
	// Transport is the declared transport; only ConnectionTransportHTTP
	// exists.
	Transport string
	// AuthModes is the closed auth-mode subset the kind accepts.
	AuthModes []string
	// Probe is the bounded read-only probe contract, when the plugin
	// declares one. A kind without a probe contract is still gateway-
	// executable for its tools but can never start or close a probe attempt.
	Probe *HTTPProbeContract
}

// validAuthMode reports whether the mode is inside the bounded vocabulary.
func validAuthMode(mode string) bool {
	return mode == AuthModeNone || mode == AuthModeBasic || mode == AuthModeBearer
}

// validateProbePath checks one probe-path declaration: it must be an
// absolute path of bounded printable ASCII with no space or control byte,
// suitable as a fixed URL path (+ optional query) appended to the connection
// base URL. Credential material never belongs in a declaration; the check
// keeps the probe deterministic and URL-safe rather than scanning secrets.
func validateProbePath(plugin Plugin) error {
	path := plugin.ConnectionProbePath
	if path == "" {
		return nil
	}
	if plugin.ConnectionKind == "" {
		return fmt.Errorf("%w: %s declares a connection probe path without a connection kind", ErrInvalidPlugin, plugin.ID)
	}
	if len(path) > 2048 {
		return fmt.Errorf("%w: %s connection probe path exceeds 2048 bytes", ErrInvalidPlugin, plugin.ID)
	}
	if path[0] != '/' {
		return fmt.Errorf("%w: %s connection probe path %q must be absolute (start with '/')", ErrInvalidPlugin, plugin.ID, path)
	}
	for i := 0; i < len(path); i++ {
		if path[i] <= 0x20 || path[i] == 0x7f {
			return fmt.Errorf("%w: %s connection probe path must be printable ASCII without spaces or control characters", ErrInvalidPlugin, plugin.ID)
		}
	}
	return nil
}

// probeDeclaration folds the plugin's declared probe path onto the frozen
// contract shape (GET, expected status 200). The expected-status vocabulary
// is exactly 200 — the bounded read-only probe asks the platform whether its
// endpoint answers; any narrower acceptance is a future contract extension.
func probeDeclaration(plugin Plugin) *HTTPProbeContract {
	if plugin.ConnectionProbePath == "" {
		return nil
	}
	return &HTTPProbeContract{Method: http.MethodGet, Path: plugin.ConnectionProbePath, ExpectStatus: http.StatusOK}
}

// validateConnectionDeclaration checks one plugin's connection declaration in
// isolation. An empty kind means the plugin needs no platform connection and
// imposes no requirements.
func validateConnectionDeclaration(plugin Plugin) error {
	if err := validateProbePath(plugin); err != nil {
		return err
	}
	if plugin.ConnectionKind == "" {
		if plugin.ConnectionTransport != "" || len(plugin.ConnectionAuthModes) > 0 {
			return fmt.Errorf("%w: %s declares connection transport/auth modes without a connection kind", ErrInvalidPlugin, plugin.ID)
		}
		return nil
	}
	if reservedConnectionKinds[plugin.ConnectionKind] {
		return fmt.Errorf("%w: %s connection kind %q is reserved by the core", ErrInvalidPlugin, plugin.ID, plugin.ConnectionKind)
	}
	if !sourceKindPattern.MatchString(plugin.ConnectionKind) {
		return fmt.Errorf("%w: %s connection kind %q is not [a-z][a-z0-9-]*", ErrInvalidPlugin, plugin.ID, plugin.ConnectionKind)
	}
	if plugin.ConnectionTransport != ConnectionTransportHTTP {
		return fmt.Errorf("%w: %s connection kind %q must declare the %q transport; non-HTTP transports need an explicit gateway contract extension", ErrInvalidPlugin, plugin.ID, plugin.ConnectionKind, ConnectionTransportHTTP)
	}
	if len(plugin.ConnectionAuthModes) == 0 {
		return fmt.Errorf("%w: %s connection kind %q declares no auth modes", ErrInvalidPlugin, plugin.ID, plugin.ConnectionKind)
	}
	seen := map[string]bool{}
	for _, mode := range plugin.ConnectionAuthModes {
		if !validAuthMode(mode) {
			return fmt.Errorf("%w: %s connection kind %q declares auth mode %q outside the bounded none/basic/bearer vocabulary", ErrInvalidPlugin, plugin.ID, plugin.ConnectionKind, mode)
		}
		if seen[mode] {
			return fmt.Errorf("%w: %s connection kind %q declares auth mode %q twice", ErrInvalidPlugin, plugin.ID, plugin.ConnectionKind, mode)
		}
		seen[mode] = true
	}
	return nil
}

// HTTPConnectionKind resolves the frozen declaration for one connection kind
// and its owning plugin ID. Unknown kinds are not gateway-executable; callers
// must fail closed.
func (r *Registry) HTTPConnectionKind(kind string) (HTTPConnectionKind, string, bool) {
	r.mu.Lock()
	r.ensureFrozen()
	owner, ok := r.kinds[kind]
	var declaration HTTPConnectionKind
	if ok {
		plugin := r.plugins[owner]
		declaration = HTTPConnectionKind{Kind: kind, Transport: plugin.ConnectionTransport, AuthModes: append([]string(nil), plugin.ConnectionAuthModes...), Probe: probeDeclaration(plugin)}
	}
	r.mu.Unlock()
	return declaration, owner, ok
}

// HTTPConnectionKinds returns every declared trusted HTTP connection kind
// ordered by kind.
func (r *Registry) HTTPConnectionKinds() []HTTPConnectionKind {
	r.mu.Lock()
	r.ensureFrozen()
	declarations := make([]HTTPConnectionKind, 0, len(r.kinds))
	for kind, owner := range r.kinds {
		plugin := r.plugins[owner]
		declarations = append(declarations, HTTPConnectionKind{Kind: kind, Transport: plugin.ConnectionTransport, AuthModes: append([]string(nil), plugin.ConnectionAuthModes...), Probe: probeDeclaration(plugin)})
	}
	r.mu.Unlock()
	sort.Slice(declarations, func(i, j int) bool { return declarations[i].Kind < declarations[j].Kind })
	return declarations
}

// ConnectionKindView resolves trusted HTTP connection kinds against a
// deployment enablement set (ADR-0004 enablement × ADR-0014 kind registry).
// It is the seam Quoin's connections domain consumes: a kind is trusted only
// while its owning plugin stays enabled, so a plugin removed from the
// deployment enablement revokes its kind everywhere (create and gateway
// material acquire fail closed). Before the deployment resolution runs, the
// view falls back to the registry's DefaultEnabled plugins — the same silent
// default the enablement resolver applies.
type ConnectionKindView struct {
	mu       sync.RWMutex
	registry *Registry
	enabled  []string
	resolved bool
}

// ConnectionKindView builds the deployment-facing view over the frozen
// registry.
func (r *Registry) ConnectionKindView() *ConnectionKindView {
	return &ConnectionKindView{registry: r}
}

// SetEnabled installs the deployment-resolved enablement set. It replaces
// the silent DefaultEnabled fallback; an explicit empty set closes every
// kind.
func (view *ConnectionKindView) SetEnabled(enabled []string) {
	view.mu.Lock()
	defer view.mu.Unlock()
	view.enabled = enabled
	view.resolved = true
}

// LookupHTTPConnectionKind reports the trusted HTTP connection declaration
// for kind while its owning plugin is enabled.
func (view *ConnectionKindView) LookupHTTPConnectionKind(kind string) (HTTPConnectionKind, bool) {
	view.mu.RLock()
	registry, enabled, resolved := view.registry, view.enabled, view.resolved
	view.mu.RUnlock()
	declaration, owner, ok := registry.HTTPConnectionKind(kind)
	if !ok {
		return HTTPConnectionKind{}, false
	}
	if !resolved {
		enabled = registry.defaultEnabledIDs()
	}
	if !IsEnabled(enabled, owner) {
		return HTTPConnectionKind{}, false
	}
	return declaration, true
}
