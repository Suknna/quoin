package helm

import (
	"path/filepath"
	"testing"
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
