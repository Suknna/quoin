package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestComposeDeliversSixRolesBehindOneTLSGateway guards the direct auxiliary
// deployment file so it cannot silently regress to the former four-service
// projection or expose Runtime and operational listeners on the host.
func TestComposeDeliversSixRolesBehindOneTLSGateway(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Services map[string]struct {
			Image   string   `yaml:"image"`
			Ports   []string `yaml:"ports"`
			Volumes []string `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"gateway", "frontend", "quoin", "plinth", "lintel", "stele"} {
		if _, found := document.Services[role]; !found {
			t.Fatalf("missing %s service", role)
		}
	}
	if document.Services["gateway"].Image != "caddy:2.10.2-alpine" {
		t.Fatalf("gateway must use the fixed stock Caddy image: %q", document.Services["gateway"].Image)
	}
	if !strings.Contains(document.Services["frontend"].Image, "quoin/frontend") {
		t.Fatal("frontend must use its independent image")
	}
	for _, role := range []string{"quoin", "plinth", "lintel", "stele"} {
		if len(document.Services[role].Ports) != 0 {
			t.Fatalf("%s must not publish a host port: %v", role, document.Services[role].Ports)
		}
	}
	for _, volume := range document.Services["quoin"].Volumes {
		if strings.Contains(volume, ":/run/quoin-secrets") && !strings.HasSuffix(volume, ":ro") {
			t.Fatal("the long-running Quoin service must mount startup secrets read-only")
		}
	}
	if len(document.Services["gateway"].Ports) != 1 || document.Services["gateway"].Ports[0] != "443:8443" {
		t.Fatalf("gateway must be the only public Compose service: %v", document.Services["gateway"].Ports)
	}
	for _, route := range []string{"/api", "/api/*", "/stele", "/stele/*", "quoin:8080", "stele:8080", "frontend:8080", "stream_timeout", "Strict-Transport-Security", "tls_connection_policies", "automatic_https"} {
		if !strings.Contains(string(data), route) {
			t.Fatalf("gateway Caddy JSON must contain %q", route)
		}
	}
}
