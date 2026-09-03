package compose_test

import (
	"os"
	"path/filepath"
	"strings"
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

func TestRenderWithPinnedImagesAndVerifyOverlay(t *testing.T) {
	root := t.TempDir()
	input := contract.ComposeInstall{Document: "compose-install", PublicOrigin: "https://quoin.test", PublishMode: "loopback", QuoinPublicHostPort: 18080, SteleWebhookHostPort: 18081, SecretDirectory: filepath.Join(root, "secrets"), LintelBrowserSlots: 1, LintelShmSizeBytes: 1 << 30}
	projection, err := compose.RenderWithOptions(input, filepath.Join(root, "state"), compose.Options{Images: map[string]string{
		"quoin":  "127.0.0.1:5000/quoin@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"plinth": "127.0.0.1:5000/plinth@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"lintel": "127.0.0.1:5000/lintel@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"stele":  "127.0.0.1:5000/stele@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(projection.ComposeFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "${QUOIN_IMAGE_NAMESPACE") || strings.Contains(text, ":v0.1.0-dev") {
		t.Fatalf("pinned projection must not carry mutable dev image references:\n%s", text)
	}
	for _, pinned := range []string{
		"127.0.0.1:5000/quoin@sha256:aaaa",
		"127.0.0.1:5000/plinth@sha256:bbbb",
		"127.0.0.1:5000/lintel@sha256:cccc",
		"127.0.0.1:5000/stele@sha256:dddd",
	} {
		if !strings.Contains(text, pinned) {
			t.Fatalf("projection missing pinned reference %s", pinned)
		}
	}
	overlay, err := compose.RenderVerifyOverlay(projection, compose.Options{Images: map[string]string{"quoin": "127.0.0.1:5000/quoin@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}})
	if err != nil {
		t.Fatal(err)
	}
	overlayData, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Services map[string]struct {
			Image      string   `yaml:"image"`
			Networks   []string `yaml:"networks"`
			Ports      []string `yaml:"ports"`
			Volumes    []string `yaml:"volumes"`
			Entrypoint []string `yaml:"entrypoint"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(overlayData, &document); err != nil {
		t.Fatal(err)
	}
	verifier := document.Services["quoin-verifier"]
	if verifier.Image != "127.0.0.1:5000/quoin@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("verifier must use the pinned quoin image: %q", verifier.Image)
	}
	if len(verifier.Volumes) != 0 || len(verifier.Ports) != 0 {
		t.Fatal("verifier must not mount state or publish ports")
	}
	if len(verifier.Networks) != 1 || verifier.Networks[0] != "internal" {
		t.Fatalf("verifier must join only the internal network: %v", verifier.Networks)
	}
	if len(verifier.Entrypoint) != 1 || verifier.Entrypoint[0] != "/quoin-healthcheck" {
		t.Fatalf("verifier entrypoint must be the healthcheck binary: %v", verifier.Entrypoint)
	}
}

func TestRenderDeploymentBinding(t *testing.T) {
	root := t.TempDir()
	input := contract.ComposeInstall{Document: "compose-install", PublicOrigin: "https://quoin.test", PublishMode: "loopback", QuoinPublicHostPort: 18080, SteleWebhookHostPort: 18081, SecretDirectory: filepath.Join(root, "secrets"), LintelBrowserSlots: 1, LintelShmSizeBytes: 1 << 30}
	binding := &contract.DeploymentBinding{
		ReleaseVersion:          "v1.2.3",
		ReleaseSubjectDigest:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeploymentConfigDigest:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Backend:                 "compose",
		Architecture:            "linux/amd64",
		BrowserChromiumRevision: "1200.0.6099.109",
	}
	projection, err := compose.RenderWithOptions(input, filepath.Join(root, "state"), compose.Options{DeploymentBinding: binding})
	if err != nil {
		t.Fatal(err)
	}
	quoin, err := os.ReadFile(filepath.Join(projection.Directory, "quoin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var rendered contract.QuoinConfig
	if err := contract.Decode(quoin, &rendered); err != nil {
		t.Fatalf("rendered quoin config must validate: %v\n%s", err, quoin)
	}
	if rendered.DeploymentBinding == nil || rendered.DeploymentBinding.ReleaseSubjectDigest != binding.ReleaseSubjectDigest || rendered.DeploymentBinding.Backend != "compose" || rendered.DeploymentBinding.BrowserChromiumRevision != binding.BrowserChromiumRevision {
		t.Fatalf("rendered quoin config lost the deployment binding: %s", quoin)
	}

	// A local development projection (no release manifest) keeps the same
	// generated file shape without a binding; Deployment Acceptance is simply
	// unavailable there.
	projection, err = compose.Render(input, filepath.Join(root, "state2"))
	if err != nil {
		t.Fatal(err)
	}
	quoin, err = os.ReadFile(filepath.Join(projection.Directory, "quoin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(quoin), "deploymentBinding") {
		t.Fatalf("development projection must not claim a release subject:\n%s", quoin)
	}
	var plain contract.QuoinConfig
	if err := contract.Decode(quoin, &plain); err != nil {
		t.Fatal(err)
	}
	if plain.DeploymentBinding != nil {
		t.Fatalf("development projection decoded a binding: %+v", plain.DeploymentBinding)
	}
}
