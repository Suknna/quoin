package app

import (
	"context"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/test/plugins/synthetic"
)

type catalogEventSource struct{}

func (catalogEventSource) Kind() string { return "synthetic-hook" }
func (catalogEventSource) VerifyAndParse(context.Context, plugins.InboundRequest) ([]plugins.Event, error) {
	return nil, nil
}

func TestPluginCatalogPublishesVersionedTemplateParameterContract(t *testing.T) {
	item := pluginCatalogEntry(synthetic.Plugin(), true)
	if len(item.InspectionTemplates) != 1 || item.InspectionTemplates[0].ID != synthetic.TemplateID ||
		item.InspectionTemplates[0].Version != synthetic.TemplateVersion || item.InspectionTemplates[0].ResultKind != "json" || item.InspectionTemplates[0].ParamsSchema == nil {
		t.Fatalf("catalog omitted synthetic inspection schema: %+v", item.InspectionTemplates)
	}
}

func TestPluginCatalogKeepsSourceKindSeparateFromPluginID(t *testing.T) {
	item := pluginCatalogEntry(plugins.Plugin{ID: "synthetic-plugin", Version: "1", EventSource: catalogEventSource{}, ConnectionKind: "synthetic-http", ConnectionTransport: plugins.ConnectionTransportHTTP, ConnectionAuthModes: []string{plugins.AuthModeNone, plugins.AuthModeBearer}, ConnectionProbePath: "/health"}, true)
	if item.ID != "synthetic-plugin" || item.SourceKind != "synthetic-hook" || item.ConnectionKind != "synthetic-http" || item.ConnectionProbePath != "/health" || len(item.ConnectionAuthModes) != 2 || !item.Enabled || len(item.Capabilities) != 2 {
		t.Fatalf("catalog lost distinct plugin/source identities: %+v", item)
	}
}
