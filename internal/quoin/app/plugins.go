package app

// Plugin management catalog and boot wiring (ADR-0004). The registry holds
// the authoritative description catalog of the compile-time registered
// built-in plugins; deployment YAML selects enablement; the resolved state
// feeds the management endpoint, the per-generation frozen model tool
// catalogs and the platform-fault origin eligibility in one pass.

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/pluginevents"
	"github.com/danielgtaylor/huma/v2"
)

// initPluginRegistry installs the process default plugin registry
// (ADR-0011 blank-import assembly: cmd/quoin blank-imports
// plugins/alertmanager and plugins/metrics, whose init() registrations
// populate the registry). The registry freezes on first read; a rejected registration
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
	// The resolved enablement is also the connection-kind trust boundary:
	// a plugin outside it has its connection kinds revoked everywhere
	// (create and gateway material acquire fail closed).
	application.connectionKinds.SetEnabled(enabled)
	catalogs, err := attempt.BuildCatalogs(application.pluginRegistry, enabled)
	if err != nil {
		return nil, err
	}
	application.analyses.Attempts().Catalogs = catalogs
	application.investigations.Attempts().Catalogs = catalogs
	application.inspections.Attempts().Catalogs = catalogs
	application.knowledgeService.Attempts().Catalogs = catalogs
	// The deployment-resolved source scope plan (ADR-0014): registry-declared
	// roles × connection kinds of enabled plugins drive the frozen agent
	// input integrations and every grant resolution. A new trusted plugin
	// joins the frozen source authority through this generic derivation —
	// no per-plugin wiring.
	scopes := attempt.SourceScopes(application.pluginRegistry, enabled)
	application.analyses.UseSourceScopes(scopes)
	application.investigations.UseSourceScopes(scopes)
	application.alerts.UseEnabledPlugins(enabled)
	application.enabledPlugins = enabled
	return enabled, nil
}

type pluginCatalogItem struct {
	ID           string   `json:"id"`
	SourceKind   string   `json:"sourceKind,omitempty"`
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
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/integrations/plugin-events/deadletters", OperationID: "listPluginEventDeadletters"}, application.listPluginEventDeadletters)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/integrations/plugin-events/deadletters/{deliveryId}/replay", OperationID: "replayPluginEventDeadletter", DefaultStatus: http.StatusNoContent}, application.replayPluginEventDeadletter)
}

type pluginEventDeadlettersInput struct {
	Session string `cookie:"__Host-quoin-session"`
}

type pluginEventDeadlettersOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Count int64                       `json:"count"`
		Items []pluginevents.DeadDelivery `json:"items"`
	} `json:"body"`
}

func (application *apiServer) listPluginEventDeadletters(ctx context.Context, input *pluginEventDeadlettersInput) (*pluginEventDeadlettersOutput, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "查看插件事件死信"); err != nil {
		return nil, err
	}
	if application.pluginEvents == nil {
		return nil, huma.Error503ServiceUnavailable("插件事件派发器尚未就绪")
	}
	count, err := application.pluginEvents.DeadCount(ctx)
	if err != nil {
		return nil, huma.Error503ServiceUnavailable("无法读取插件事件死信", err)
	}
	items, err := application.pluginEvents.DeadDeliveries(ctx, 100)
	if err != nil {
		return nil, huma.Error503ServiceUnavailable("无法读取插件事件死信", err)
	}
	output := &pluginEventDeadlettersOutput{CacheControl: "no-store"}
	output.Body.Count = count
	output.Body.Items = items
	return output, nil
}

type replayPluginEventDeadletterInput struct {
	Session    string `cookie:"__Host-quoin-session"`
	DeliveryID int64  `path:"deliveryId" minimum:"1"`
}

type replayPluginEventDeadletterOutput struct {
	Status int `header:"-"`
}

func (application *apiServer) replayPluginEventDeadletter(ctx context.Context, input *replayPluginEventDeadletterInput) (*replayPluginEventDeadletterOutput, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "重放插件事件死信"); err != nil {
		return nil, err
	}
	if application.pluginEvents == nil {
		return nil, huma.Error503ServiceUnavailable("插件事件派发器尚未就绪")
	}
	if err := application.pluginEvents.ReplayDeadletterAsAdmin(ctx, input.DeliveryID); err != nil {
		var rejection *execution.Rejection
		switch {
		case errors.As(err, &rejection):
			if rejection.Code == "delivery_not_found" {
				return nil, huma.Error404NotFound("插件事件投递不存在")
			}
			return nil, huma.Error409Conflict("只有死信可重放", err)
		default:
			return nil, huma.Error503ServiceUnavailable("插件事件死信重放失败", err)
		}
	}
	application.pluginEvents.Kick()
	return &replayPluginEventDeadletterOutput{Status: http.StatusNoContent}, nil
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
		output.Body.Items = append(output.Body.Items, pluginCatalogEntry(plugin, plugins.IsEnabled(enabled, plugin.ID)))
	}
	return output, nil
}

func pluginCatalogEntry(plugin plugins.Plugin, enabled bool) pluginCatalogItem {
	// A plugin ID is not necessarily its inbound protocol kind. These two
	// identities must remain separate at the management API boundary.
	item := pluginCatalogItem{
		ID: plugin.ID, DisplayName: plugin.DisplayName, Description: plugin.Description,
		Enabled: enabled, Version: plugin.Version, Capabilities: []string{},
	}
	if plugin.EventSource != nil {
		item.SourceKind = plugin.EventSource.Kind()
		item.Capabilities = append(item.Capabilities, "event_source")
	}
	if plugin.AlertNormalizer != nil {
		item.Capabilities = append(item.Capabilities, "alert_normalizer")
	}
	if plugin.Tools != nil {
		item.Capabilities = append(item.Capabilities, "tools")
	}
	if len(plugin.DiscoverObjects) > 0 {
		item.Capabilities = append(item.Capabilities, "discover")
	}
	if len(plugin.InspectionTemplates) > 0 {
		item.Capabilities = append(item.Capabilities, "inspection_templates")
	}
	sort.Strings(item.Capabilities)
	return item
}
