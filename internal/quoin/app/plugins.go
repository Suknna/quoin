package app

// Plugin management catalog and boot wiring (ADR-0004). The registry holds
// the authoritative description catalog of the compile-time registered
// built-in plugins; deployment YAML selects enablement; the resolved state
// feeds the management endpoint, the per-generation frozen model tool
// catalogs and the platform-fault origin eligibility in one pass.

import (
	"context"
	"net/http"
	"sort"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/danielgtaylor/huma/v2"
)

// initPluginRegistry installs the process default plugin registry
// (ADR-0011 blank-import assembly: cmd/quoin blank-imports
// internal/plugins/builtin, whose init() registrations populate the
// registry). The registry freezes on first read; a rejected registration
// panics at init — a compile-time fact, not a runtime condition.
func (application *apiServer) initPluginRegistry() {
	application.pluginRegistry = plugins.Default()
}

// configurePlugins resolves deployment enablement once at boot and projects
// it onto every consumer: the frozen per-generation tool catalogs of all
// agent slices and the platform-fault origin eligibility. Enablement is
// deployment-frozen; there is no runtime switch to re-verify, and each new
// attempt freezes the resolved catalog it was created with.
func (application *apiServer) configurePlugins(configured []string) ([]string, error) {
	enabled, err := application.pluginRegistry.ResolveEnabled(configured)
	if err != nil {
		return nil, err
	}
	catalogs, err := attempt.BuildCatalogs(application.pluginRegistry, enabled)
	if err != nil {
		return nil, err
	}
	application.analyses.Attempts().Catalogs = catalogs
	application.investigations.Attempts().Catalogs = catalogs
	application.inspections.Attempts().Catalogs = catalogs
	application.knowledgeService.Attempts().Catalogs = catalogs
	application.enabledPlugins = enabled
	return enabled, nil
}

type pluginCatalogItem struct {
	ID           string   `json:"id"`
	DisplayName  string   `json:"displayName"`
	Description  string   `json:"description"`
	Enabled      bool     `json:"enabled"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
}

type integrationsPluginsInput struct {
	Session string `cookie:"__Host-quoin-session"`
}

type integrationsPluginsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Pragma       string `header:"Pragma"`
	Body         struct {
		Items []pluginCatalogItem `json:"items"`
	} `json:"body"`
}

// registerPluginRoutes owns the management catalog surface.
func (application *apiServer) registerPluginRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/integrations/plugins", OperationID: "listIntegrationPlugins"}, application.integrationsPlugins)
}

// integrationsPlugins serves the authoritative plugin management catalog:
// every registered plugin (enabled and disabled) with its real enablement,
// in stable ID order.
func (application *apiServer) integrationsPlugins(ctx context.Context, input *integrationsPluginsInput) (*integrationsPluginsOutput, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "查看插件目录"); err != nil {
		return nil, err
	}
	enabled, err := application.pluginRegistry.ResolveEnabled(application.enabledPlugins)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取插件目录", err)
	}
	output := &integrationsPluginsOutput{CacheControl: "no-store", Pragma: "no-cache"}
	output.Body.Items = []pluginCatalogItem{}
	for _, plugin := range application.pluginRegistry.Plugins() {
		// ADR-0011 capability vocabulary: event_source (inbound gateway
		// capability), tools (outbound model tools), discover/inspection_
		// templates (declarative catalogs the schedulers consume).
		var capabilities []string
		if plugin.EventSource != nil {
			capabilities = append(capabilities, "event_source")
		}
		if plugin.Tools != nil {
			capabilities = append(capabilities, "tools")
		}
		if len(plugin.DiscoverObjects) > 0 {
			capabilities = append(capabilities, "discover")
		}
		if len(plugin.InspectionTemplates) > 0 {
			capabilities = append(capabilities, "inspection_templates")
		}
		sort.Strings(capabilities)
		output.Body.Items = append(output.Body.Items, pluginCatalogItem{
			ID:           plugin.ID,
			DisplayName:  plugin.DisplayName,
			Description:  plugin.Description,
			Enabled:      plugins.IsEnabled(enabled, plugin.ID),
			Version:      plugin.Version,
			Capabilities: capabilities,
		})
	}
	return output, nil
}
