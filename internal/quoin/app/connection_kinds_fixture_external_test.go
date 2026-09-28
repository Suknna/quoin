package app_test

// ADR-0014 test fixture for the external app test package: the same registry
// view shape the boot wiring installs, with the legacy metrics kinds enabled.

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
