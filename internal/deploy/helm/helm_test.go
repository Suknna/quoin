package helm

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
)

func TestLoadAcceptsFrozenHelmInstallInput(t *testing.T) {
	loaded, err := load(Request{ConfigPath: filepath.Join("..", "..", "..", "docs", "specs", "quoin-v1", "contracts", "examples", "helm-install.yaml")})
	if err != nil {
		t.Fatalf("load frozen helm-install input: %v", err)
	}
	if loaded.input.PublicOrigin != "https://quoin.internal.example" {
		t.Fatalf("public origin = %q", loaded.input.PublicOrigin)
	}
	if loaded.input.LintelShmSize != "1Gi" {
		t.Fatalf("default Lintel shm size = %q, want 1Gi", loaded.input.LintelShmSize)
	}
}

func TestChartValuesCarryDeploymentBinding(t *testing.T) {
	binding := &contract.DeploymentBinding{
		ReleaseVersion:          "v1.2.3",
		ReleaseSubjectDigest:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeploymentConfigDigest:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Backend:                 "kubernetes",
		Architecture:            "linux/amd64",
		BrowserChromiumRevision: "1200.0.6099.109",
	}
	values, err := chartValues(installInput{}, map[string]string{}, binding)
	if err != nil {
		t.Fatal(err)
	}
	text := string(values)
	for _, want := range []string{"deploymentBinding:", "releaseVersion: v1.2.3", "backend: kubernetes", binding.ReleaseSubjectDigest} {
		if !strings.Contains(text, want) {
			t.Fatalf("chart values missing %q:\n%s", want, text)
		}
	}
	without, err := chartValues(installInput{}, map[string]string{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "deploymentBinding") {
		t.Fatalf("development chart values must not claim a release subject:\n%s", without)
	}
}
