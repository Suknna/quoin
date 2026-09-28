package plugins_test

// ADR-0014 coverage for the trusted HTTP connection-kind declaration:
// registration validates the bounded vocabulary, freeze enforces per-kind
// ownership, and the deployment view revokes kinds whose plugin leaves the
// enablement set.

import (
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
)

func httpKindPlugin(id, kind string, modes ...string) plugins.Plugin {
	if len(modes) == 0 {
		modes = []string{plugins.AuthModeNone, plugins.AuthModeBasic, plugins.AuthModeBearer}
	}
	return plugins.Plugin{
		ID: id, Version: "1", DefaultEnabled: true,
		ConnectionKind: kind, ConnectionTransport: plugins.ConnectionTransportHTTP,
		ConnectionAuthModes: modes,
	}
}

func TestHTTPConnectionKindDeclarationAndLookup(t *testing.T) {
	registry := plugins.NewRegistry()
	if err := registry.Register(httpKindPlugin("prometheus", "prometheus")); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(httpKindPlugin("synthmetrics", "synth-http", plugins.AuthModeBasic)); err != nil {
		t.Fatal(err)
	}
	declaration, owner, ok := registry.HTTPConnectionKind("synth-http")
	if !ok || owner != "synthmetrics" {
		t.Fatalf("synth-http lookup = %+v owner=%s ok=%v", declaration, owner, ok)
	}
	if declaration.Transport != plugins.ConnectionTransportHTTP || len(declaration.AuthModes) != 1 || declaration.AuthModes[0] != plugins.AuthModeBasic {
		t.Fatalf("synth-http declaration = %+v", declaration)
	}
	kinds := registry.HTTPConnectionKinds()
	if len(kinds) != 2 || kinds[0].Kind != "prometheus" || kinds[1].Kind != "synth-http" {
		t.Fatalf("declared kinds = %+v", kinds)
	}
	if _, _, ok := registry.HTTPConnectionKind("model_provider"); ok {
		t.Fatal("reserved kind must not resolve through the plugin registry")
	}
}

func TestHTTPConnectionKindDeclarationValidation(t *testing.T) {
	cases := []struct {
		name    string
		plugin  plugins.Plugin
		message string
	}{
		{"transport required", plugins.Plugin{ID: "p", Version: "1", ConnectionKind: "k", ConnectionAuthModes: []string{plugins.AuthModeBasic}}, `must declare the "http" transport`},
		{"unknown transport", plugins.Plugin{ID: "p", Version: "1", ConnectionKind: "k", ConnectionTransport: "grpc", ConnectionAuthModes: []string{plugins.AuthModeBasic}}, `must declare the "http" transport`},
		{"reserved kind", httpKindPlugin("p", "model_provider"), "reserved by the core"},
		{"no auth modes", plugins.Plugin{ID: "p", Version: "1", ConnectionKind: "k", ConnectionTransport: plugins.ConnectionTransportHTTP}, "declares no auth modes"},
		{"mode outside vocabulary", httpKindPlugin("p", "k", plugins.AuthModeBasic, "digest"), "outside the bounded none/basic/bearer vocabulary"},
		{"duplicate mode", httpKindPlugin("p", "k", plugins.AuthModeBasic, plugins.AuthModeBasic), "twice"},
		{"bad kind name", httpKindPlugin("p", "Prometheus"), "is not [a-z][a-z0-9-]*"},
		{"orphan transport", plugins.Plugin{ID: "p", Version: "1", ConnectionTransport: plugins.ConnectionTransportHTTP, ConnectionAuthModes: []string{plugins.AuthModeBasic}}, "without a connection kind"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			registry := plugins.NewRegistry()
			err := registry.Register(testCase.plugin)
			if err == nil || !strings.Contains(err.Error(), testCase.message) {
				t.Fatalf("register err = %v, want containing %q", err, testCase.message)
			}
		})
	}
}

func TestDuplicateConnectionKindFailsFreeze(t *testing.T) {
	registry := plugins.NewRegistry()
	if err := registry.Register(httpKindPlugin("alpha", "shared-kind")); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(httpKindPlugin("beta", "shared-kind")); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("expected freeze to panic on duplicate connection kind")
		}
	}()
	registry.Plugins()
}

func TestConnectionKindViewRevokesDisabledPluginKind(t *testing.T) {
	registry := plugins.NewRegistry()
	if err := registry.Register(httpKindPlugin("prometheus", "prometheus")); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(httpKindPlugin("synthmetrics", "synth-http")); err != nil {
		t.Fatal(err)
	}
	view := registry.ConnectionKindView()

	// Before the deployment resolution runs, the view falls back to the
	// silent DefaultEnabled default (both fixtures are default-enabled).
	if _, ok := view.LookupHTTPConnectionKind("prometheus"); !ok {
		t.Fatal("silent default must keep default-enabled kinds trusted")
	}
	if _, ok := view.LookupHTTPConnectionKind("unknown"); ok {
		t.Fatal("unknown kind must never resolve")
	}

	// The resolved deployment set revokes the synthetic kind's plugin.
	view.SetEnabled([]string{"prometheus"})
	if _, ok := view.LookupHTTPConnectionKind("prometheus"); !ok {
		t.Fatal("enabled plugin kind must resolve")
	}
	if _, ok := view.LookupHTTPConnectionKind("synth-http"); ok {
		t.Fatal("kind of a plugin outside the enablement set must be revoked")
	}

	// An explicit empty set closes every kind.
	view.SetEnabled(nil)
	if _, ok := view.LookupHTTPConnectionKind("prometheus"); ok {
		t.Fatal("explicit empty enablement must close every kind")
	}
}
