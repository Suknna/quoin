package kubernetes

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestManifestDeliversBrowserFreeStackBehindOneTLSGateway protects the default
// public topology: API/SSE/WebSocket traffic must reach Quoin before the SPA
// fallback, alert intake must reach Stele, and application credentials stay
// outside ConfigMaps. The browser plugin and its Lintel runtime are retired
// (deploy/retired/browser/): the manifest must not contain, reference, or
// provision any Lintel resource.
func TestManifestDeliversBrowserFreeStackBehindOneTLSGateway(t *testing.T) {
	documents := loadDocuments(t, "quoin.yaml")
	deployments := map[string]map[string]any{}
	services := map[string]map[string]any{}
	configMaps := map[string]map[string]any{}
	claims := map[string]map[string]any{}
	for _, document := range documents {
		metadata := document["metadata"].(map[string]any)
		name := metadata["name"].(string)
		switch document["kind"] {
		case "Deployment":
			deployments[name] = document
		case "Service":
			services[name] = document
		case "ConfigMap":
			configMaps[name] = document
		case "PersistentVolumeClaim":
			claims[name] = document
		}
	}

	for _, role := range []string{"gateway", "frontend", "quoin", "plinth", "stele"} {
		if _, found := deployments[role]; !found {
			t.Fatalf("missing %s Deployment", role)
		}
	}
	for _, service := range []string{"gateway", "frontend", "quoin", "stele"} {
		if _, found := services[service]; !found {
			t.Fatalf("missing %s Service", service)
		}
	}
	for _, claim := range []string{"quoin-data", "quoin-backups", "plinth-state"} {
		if _, found := claims[claim]; !found {
			t.Fatalf("missing %s PersistentVolumeClaim", claim)
		}
	}
	// The default stack is browser-free: no Lintel Deployment, ConfigMap,
	// PVC, or Service may appear in the default manifest.
	for name := range deployments {
		if name == "lintel" {
			t.Fatal("default manifest must not deploy Lintel; the browser runtime is retired")
		}
	}
	for name := range configMaps {
		if name == "lintel-config" {
			t.Fatal("default manifest must not ship lintel-config; the browser runtime is retired")
		}
	}
	for name := range claims {
		if name == "lintel-state" {
			t.Fatal("default manifest must not provision lintel-state; the browser runtime is retired")
		}
	}
	for _, exposed := range []string{"plinth", "lintel"} {
		if _, found := services[exposed]; found {
			t.Fatalf("%s must not have a public or cluster service in the default manifest", exposed)
		}
	}
	if rendered := mustMarshal(t, documents); strings.Contains(rendered, "lintel") {
		t.Fatalf("default manifest must not reference Lintel at all: %s", rendered)
	}

	// ADR 0004: the deployment YAML selects the enabled plugin set. The
	// browser-free default stack must enable exactly the four source plugins
	// and must never enable the browser plugin.
	quoinComponent := loadComponentYAML(t, configMaps["quoin-config"]["data"].(map[string]any)["component.yaml"].(string))
	enabled, ok := quoinComponent["enabledPlugins"].([]any)
	if !ok {
		t.Fatalf("quoin-config must declare enabledPlugins: %v", quoinComponent)
	}
	got := make([]string, 0, len(enabled))
	for _, id := range enabled {
		got = append(got, id.(string))
	}
	want := []string{"alertmanager", "kubernetes", "prometheus", "thanos"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("default enabledPlugins must be exactly %v, got %v", want, got)
	}

	gateway := configMaps["gateway-config"]["data"].(map[string]any)["caddy.json"].(string)
	apiRoute := strings.Index(gateway, `"/api/*"`)
	steleRoute := strings.Index(gateway, `"/stele/*"`)
	frontendRoute := strings.LastIndex(gateway, `"frontend:8080"`)
	if apiRoute < 0 || steleRoute < 0 || frontendRoute < 0 || apiRoute > frontendRoute || steleRoute > frontendRoute {
		t.Fatalf("gateway must route API and Stele before frontend fallback: %s", gateway)
	}
	for _, required := range []string{"quoin:8080", `"/api/*"`, "stele:8080", "Strict-Transport-Security", `"tls_connection_policies": [{}]`, `"automatic_https": {"disable": true, "disable_redirects": true}`, "/etc/caddy/tls/tls.crt"} {
		if !strings.Contains(gateway, required) {
			t.Fatalf("gateway config missing %q", required)
		}
	}
	if deployments["frontend"] == nil || !strings.Contains(mustMarshal(t, deployments["frontend"]), "quoin/frontend:v0.1.0-dev") {
		t.Fatal("frontend must be an independently deployed image")
	}
	for _, secret := range []string{"gateway-tls", "quoin-secrets"} {
		if !strings.Contains(mustMarshal(t, documents), secret) {
			t.Fatalf("manifest must mount deployer-provided %s", secret)
		}
	}
	gatewayContainer := deployments["gateway"]["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if command := gatewayContainer["command"].([]any); len(command) != 1 || command[0] != "caddy" {
		t.Fatalf("gateway must explicitly execute stock Caddy: %v", command)
	}
	capabilities := gatewayContainer["securityContext"].(map[string]any)["capabilities"].(map[string]any)
	if additions := capabilities["add"].([]any); len(additions) != 1 || additions[0] != "NET_BIND_SERVICE" {
		t.Fatalf("gateway must retain only Caddy's required file capability: %v", additions)
	}
	for _, role := range []string{"quoin", "plinth", "stele"} {
		if !strings.Contains(mustMarshal(t, deployments[role]), "terminationGracePeriodSeconds: 60") {
			t.Fatalf("%s must preserve the bounded graceful shutdown window", role)
		}
	}
	frontendPod := deployments["frontend"]["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	frontendSecurity, ok := frontendPod["securityContext"].(map[string]any)
	if !ok || frontendSecurity["runAsNonRoot"] != true || frontendSecurity["runAsUser"] != 65532 {
		t.Fatal("frontend must explicitly enforce its non-root identity")
	}
	seccomp, ok := frontendSecurity["seccompProfile"].(map[string]any)
	if !ok || seccomp["type"] != "RuntimeDefault" {
		t.Fatal("frontend must enforce the default seccomp profile")
	}
	strategy := deployments["stele"]["spec"].(map[string]any)["strategy"].(map[string]any)
	if strategy["type"] != "Recreate" {
		t.Fatalf("Stele must use Recreate because its relay is single-active: %v", strategy)
	}
}

// TestComposeQuoinConfigDefaultsMatchThePluginContract protects the Compose
// side of the same coordination: the default component config enables exactly
// the four source plugins and never the browser plugin, which is retired
// (historical variant in deploy/retired/browser/quoin-browser.yaml).
func TestComposeQuoinConfigDefaultsMatchThePluginContract(t *testing.T) {
	defaults := parseConfigFile(t, "../config/quoin.yaml")

	want := []any{"alertmanager", "kubernetes", "prometheus", "thanos"}
	if got := defaults["enabledPlugins"]; !equalStrings(got, want) {
		t.Fatalf("config/quoin.yaml must enable exactly %v, got %v", want, got)
	}
}

func parseConfigFile(t *testing.T, path string) map[string]any {
	t.Helper()
	file, err := os.Open(filepath.Join(".", path))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(false)
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	if document == nil {
		t.Fatalf("%s must contain one YAML document", path)
	}
	var trailing map[string]any
	if err := decoder.Decode(&trailing); err == nil {
		t.Fatalf("%s must contain exactly one YAML document, found at least two", path)
	} else if !errors.Is(err, io.EOF) {
		t.Fatalf("parse %s: %v", path, err)
	}
	return document
}

func equalStrings(got any, want []any) bool {
	list, ok := got.([]any)
	if !ok || len(list) != len(want) {
		return false
	}
	for index, value := range list {
		if value != want[index] {
			return false
		}
	}
	return true
}

func loadComponentYAML(t *testing.T, component string) map[string]any {
	t.Helper()
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(component), &parsed); err != nil {
		t.Fatalf("embedded component.yaml must parse: %v", err)
	}
	return parsed
}

func loadDocuments(t *testing.T, name string) []map[string]any {
	t.Helper()
	file, err := os.Open(filepath.Join(".", name))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	var documents []map[string]any
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return documents
		}
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, document)
	}
}

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	data, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
