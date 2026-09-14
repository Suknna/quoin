package appconfig

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"gopkg.in/yaml.v3"
)

func TestOpenAPIContractsKeepDeclarationHistoryWithoutWriteRoutes(t *testing.T) {
	// Both public and runtime contracts keep history without the retired producer.
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "test"))
	(&Handler{}).Register(api)
	generatedPaths := api.OpenAPI().Paths
	if generatedPaths["/api/v1/business-systems/{systemKey}/resources:refresh"] != nil {
		t.Fatal("runtime OpenAPI retained retired business resource refresh")
	}
	for _, path := range []string{
		"/api/v1/label-contracts",
		"/api/v1/label-contracts/{contractVersion}",
		"/api/v1/label-contracts/{contractVersion}/readiness",
		"/api/v1/label-contracts/{contractVersion}/activate",
	} {
		if generatedPaths[path] != nil {
			t.Fatalf("runtime OpenAPI retained retired path %q", path)
		}
	}

	contractPath := filepath.Join("..", "..", "..", "..", "docs", "specs", "quoin-v1", "contracts", "openapi.yaml")
	body, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Paths      map[string]any `yaml:"paths"`
		Components struct {
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(body, &contract); err != nil {
		t.Fatal(err)
	}
	if _, ok := contract.Paths["/api/v1/business-systems/{systemKey}/resources:refresh"]; ok {
		t.Fatal("checked-in OpenAPI retained retired business resource refresh")
	}
	for _, path := range []string{
		"/api/v1/label-contracts",
		"/api/v1/label-contracts/{contractVersion}",
		"/api/v1/label-contracts/{contractVersion}/readiness",
		"/api/v1/label-contracts/{contractVersion}/activate",
		"/api/v1/templates/label-contract",
	} {
		if _, ok := contract.Paths[path]; ok {
			t.Fatalf("checked-in OpenAPI retained retired path %q", path)
		}
	}
	for _, schema := range []string{"LabelContractSummary", "LabelContractDetail", "ActivateLabelContractRequest", "LabelContractReadiness"} {
		if _, ok := contract.Components.Schemas[schema]; ok {
			t.Fatalf("checked-in OpenAPI retained retired schema %q", schema)
		}
	}
}
