package app

// ADR-0014 test fixture: the internal test package resolves connection kinds
// through the same registry view shape the boot wiring installs, declaring
// the legacy metrics kinds as enabled trusted HTTP kinds.

import (
	"github.com/Suknna/quoin/internal/plugins"
)

func fixtureConnectionKinds() *plugins.ConnectionKindView {
	registry := plugins.NewRegistry()
	for _, plugin := range []plugins.Plugin{
		{ID: "prometheus", Version: "1", DefaultEnabled: true, ConnectionKind: "prometheus",
			ConnectionTransport: plugins.ConnectionTransportHTTP,
			ConnectionAuthModes: []string{plugins.AuthModeNone, plugins.AuthModeBasic, plugins.AuthModeBearer}},
		{ID: "thanos", Version: "1", DefaultEnabled: true, ConnectionKind: "thanos",
			ConnectionTransport: plugins.ConnectionTransportHTTP,
			ConnectionAuthModes: []string{plugins.AuthModeNone, plugins.AuthModeBasic, plugins.AuthModeBearer}},
	} {
		if err := registry.Register(plugin); err != nil {
			panic("fixture plugin registration failed: " + err.Error())
		}
	}
	return registry.ConnectionKindView()
}
