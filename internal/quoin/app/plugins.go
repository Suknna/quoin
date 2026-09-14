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
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"github.com/danielgtaylor/huma/v2"
)

// initPluginRegistry registers the built-in descriptors. Registration
// failures are programming errors pinned by contract tests, so they abort
// construction instead of degrading into a lying catalog.
func (application *apiServer) initPluginRegistry() {
	registry := plugins.NewRegistry()
	for _, descriptor := range attempt.BuiltinDescriptors() {
		if err := registry.RegisterDescriptor(descriptor); err != nil {
			panic("built-in plugin descriptor rejected: " + err.Error())
		}
	}
	if err := verifyPluginAgreement(registry); err != nil {
		panic(err.Error())
	}
	application.pluginRegistry = registry
}

// verifyPluginAgreement pins the descriptor/implementation agreement
// (声明不能伪装不存在的实现) for every registered descriptor, enabled or not.
func verifyPluginAgreement(registry *plugins.Registry) error {
	for _, descriptor := range registry.Descriptors() {
		if err := attempt.VerifyDescriptorTools(descriptor); err != nil {
			return err
		}
	}
	return nil
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
	// A disabled plugin's component absence is not a fault: Lintel
	// disconnections only open new faults while the browser plugin is
	// enabled, and a plugin disabled mid-life converges the existing fault
	// lifecycle instead of firing forever.
	browserEnabled := plugins.IsEnabled(enabled, plugins.BrowserID)
	application.platformFaults.FaultOriginEligible = func(component string) bool {
		if component == qruntime.SlotLintel {
			return browserEnabled
		}
		return true
	}
	application.enabledPlugins = enabled
	return enabled, nil
}

// browserPluginEnabled reports the resolved browser-plugin enablement;
// unknown (unwired maintenance surfaces) resolve to the defaults.
func (application *apiServer) browserPluginEnabled() bool {
	enabled, err := application.pluginRegistry.ResolveEnabled(application.enabledPlugins)
	if err != nil {
		return false
	}
	return plugins.IsEnabled(enabled, plugins.BrowserID)
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
	for _, descriptor := range application.pluginRegistry.Descriptors() {
		capabilities := make([]string, 0, len(descriptor.Capabilities))
		for _, capability := range descriptor.Capabilities {
			capabilities = append(capabilities, string(capability))
		}
		sort.Strings(capabilities)
		output.Body.Items = append(output.Body.Items, pluginCatalogItem{
			ID:           descriptor.ID,
			DisplayName:  descriptor.DisplayName,
			Description:  descriptor.Description,
			Enabled:      plugins.IsEnabled(enabled, descriptor.ID),
			Version:      descriptor.Version,
			Capabilities: capabilities,
		})
	}
	return output, nil
}
