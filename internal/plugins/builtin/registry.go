package builtin

// The process registry assembly entry: every host builds its
// plugins.Registry from THIS source, so no process maintains a second
// plugin registration path (ADR-0004). Registration failures are
// compile-time facts pinned by tests, so a rejected built-in descriptor is
// a programming error that must fail the process, not degrade into a lying
// catalog.

import (
	"github.com/Suknna/quoin/internal/plugins"
)

// Registry returns a new plugins.Registry with every built-in descriptor
// registered.
func Registry() *plugins.Registry {
	registry := plugins.NewRegistry()
	for _, descriptor := range Descriptors() {
		if err := registry.RegisterDescriptor(descriptor); err != nil {
			panic("built-in plugin descriptor " + descriptor.ID + " failed registration: " + err.Error())
		}
	}
	return registry
}
