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

// TestManifestDeliversSixRolesBehindOneTLSGateway protects the public topology:
// API/SSE/WebSocket traffic must reach Quoin before the SPA fallback, alert
// intake must reach Stele, and application credentials stay outside ConfigMaps.
func TestManifestDeliversSixRolesBehindOneTLSGateway(t *testing.T) {
	documents := loadDocuments(t, "quoin.yaml")
	deployments := map[string]map[string]any{}
	services := map[string]map[string]any{}
	configMaps := map[string]map[string]any{}
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
		}
	}

	for _, role := range []string{"gateway", "frontend", "quoin", "plinth", "lintel", "stele"} {
		if _, found := deployments[role]; !found {
			t.Fatalf("missing %s Deployment", role)
		}
	}
	for _, service := range []string{"gateway", "frontend", "quoin", "stele"} {
		if _, found := services[service]; !found {
			t.Fatalf("missing %s Service", service)
		}
	}
	if _, exposed := services["plinth"]; exposed {
		t.Fatal("Plinth must not have a public or cluster service")
	}
	if _, exposed := services["lintel"]; exposed {
		t.Fatal("Lintel must not have a public or cluster service")
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
	for _, role := range []string{"quoin", "plinth", "lintel", "stele"} {
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
