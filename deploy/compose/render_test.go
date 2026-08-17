package compose_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/deploy/compose"
	"github.com/Suknna/quoin/internal/contract"
	"gopkg.in/yaml.v3"
)

func TestRenderProducesFourLongLivedServicesBehindBootstrapGate(t *testing.T) {
	root := t.TempDir()
	input := contract.ComposeInstall{Document: "compose-install", PublicOrigin: "https://quoin.test", PublishMode: "loopback", QuoinPublicHostPort: 18080, SteleWebhookHostPort: 18081, SecretDirectory: filepath.Join(root, "secrets"), LintelBrowserSlots: 1, LintelShmSizeBytes: 1 << 30}
	projection, err := compose.Render(input, filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(projection.ComposeFile)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Services map[string]struct {
			DependsOn map[string]struct {
				Condition string `yaml:"condition"`
			} `yaml:"depends_on"`
			Ports []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		service, exists := document.Services[component]
		if !exists {
			t.Fatalf("missing service %s", component)
		}
		if service.DependsOn["admin-bootstrap"].Condition != "service_completed_successfully" {
			t.Fatalf("%s bypasses Admin bootstrap: %+v", component, service.DependsOn)
		}
	}
	if len(document.Services["quoin"].Ports) != 1 || len(document.Services["stele"].Ports) != 1 {
		t.Fatal("loopback projection must publish only Quoin and Stele")
	}
	if len(document.Services["plinth"].Ports) != 0 || len(document.Services["lintel"].Ports) != 0 {
		t.Fatal("Runtime or ops ports leaked to the host")
	}
}
