package app

import (
	"context"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
)

type catalogEventSource struct{}

func (catalogEventSource) Kind() string { return "synthetic-hook" }
func (catalogEventSource) VerifyAndParse(context.Context, plugins.InboundRequest) ([]plugins.Event, error) {
	return nil, nil
}

func TestPluginCatalogKeepsSourceKindSeparateFromPluginID(t *testing.T) {
	item := pluginCatalogEntry(plugins.Plugin{ID: "synthetic-plugin", Version: "1", EventSource: catalogEventSource{}, ConnectionKind: "synthetic-http", ConnectionTransport: plugins.ConnectionTransportHTTP, ConnectionAuthModes: []string{plugins.AuthModeNone, plugins.AuthModeBearer}, ConnectionProbePath: "/health"}, true)
	if item.ID != "synthetic-plugin" || item.SourceKind != "synthetic-hook" || item.ConnectionKind != "synthetic-http" || item.ConnectionProbePath != "/health" || len(item.ConnectionAuthModes) != 2 || !item.Enabled || len(item.Capabilities) != 2 {
		t.Fatalf("catalog lost distinct plugin/source identities: %+v", item)
	}
}
