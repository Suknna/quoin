package appconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	quoinconfig "github.com/Suknna/quoin/internal/quoin/config"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"gopkg.in/yaml.v3"
)

// TestRemovedLabelContractRoutesAreNotRedirected verifies that the retired
// contract surface is absent from this API, rather than being silently routed
// to a replacement endpoint. The old paths must remain ordinary 404s.
// TestBusinessUploadDoesNotRequireTargetLabelContractVersion keeps the public
// multipart contract focused on the YAML document: omission of the retired
// field reaches the business service rather than failing adapter validation.
func TestBusinessUploadDoesNotRequireTargetLabelContractVersion(t *testing.T) {
	document := []byte("system_key: payments\n")
	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)
	if err := form.WriteField("clientCommandId", "upload-000001"); err != nil {
		t.Fatal(err)
	}
	file, err := form.CreateFormFile("file", "business-system.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(document); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}

	handler := &Handler{AuthenticateAdmin: func(context.Context, string) (int64, error) { return 1, nil }}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/business-systems", body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.AddCookie(&http.Cookie{Name: "__Host-quoin-session", Value: "admin-session"})
	response := httptest.NewRecorder()

	// The invalid YAML fails before the intentionally unwired service needs its
	// database. Its validation response proves the removed field is not required
	// by the multipart adapter.
	handler.ServeBusinessSystemUpload(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422: %s", response.Code, response.Body.String())
	}
	var problem struct {
		FieldErrors []struct {
			Path string `json:"path"`
		} `json:"fieldErrors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	for _, field := range problem.FieldErrors {
		if field.Path == "targetLabelContractVersion" {
			t.Fatal("retired field remained required")
		}
	}
}

// TestBusinessSystemTemplateIsAnOfficialParseableDeclaration ensures download
// clients receive the same quoin/v1 shape accepted by the upload parser. Its
// placeholder reference names are intentionally not resolved in this test.
func TestBusinessSystemTemplateIsAnOfficialParseableDeclaration(t *testing.T) {
	declaration, fields := quoinconfig.ParseBusinessSystem([]byte(businessSystemTemplateYAML), quoinconfig.Limits{})
	if len(fields) != 0 {
		t.Fatalf("template failed ParseBusinessSystem: %#v", fields)
	}
	if declaration.APIVersion != "quoin/v1" || declaration.Kind != "BusinessSystem" {
		t.Fatalf("template identity=%s/%s, want quoin/v1 BusinessSystem", declaration.APIVersion, declaration.Kind)
	}
	if declaration.Spec.Metrics.ConnectionRef != "observability-main" {
		t.Fatalf("template metrics connection=%q", declaration.Spec.Metrics.ConnectionRef)
	}
}

func TestOpenAPIContractsRetireLabelContractsAndExposeResourceRefresh(t *testing.T) {
	// The generated Huma document proves the app has the real POST route; the
	// checked-in OpenAPI declaration remains the public contract authority and
	// must make exactly the same retirement/addition visible to clients.
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "test"))
	(&Handler{}).Register(api)
	generatedPaths := api.OpenAPI().Paths
	if generatedPaths["/api/v1/business-systems/{systemKey}/resources:refresh"] == nil {
		t.Fatal("runtime OpenAPI omitted startResourceRefresh")
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
	if _, ok := contract.Paths["/api/v1/business-systems/{systemKey}/resources:refresh"]; !ok {
		t.Fatal("checked-in OpenAPI omitted startResourceRefresh")
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
