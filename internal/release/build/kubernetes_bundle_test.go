package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/release/subjects"
)

func TestBuildKubernetesBundlePinsEveryApplicationImage(t *testing.T) {
	inventory := &subjects.Inventory{Images: map[string]subjects.ImageSubject{}}
	for index, component := range subjects.Components {
		inventory.Images[component] = subjects.ImageSubject{
			Repository:  "registry.example/release/" + component,
			IndexDigest: "sha256:" + strings.Repeat(string(rune('a'+index)), 64),
		}
	}
	options := &options{version: "v1.2.3", work: t.TempDir()}
	if err := buildKubernetesBundle(options, inventory); err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(options.work, inventory.Kubernetes.AssetName)
	for _, required := range []string{"bootstrap.yaml", "ops-services.yaml", "quoin.yaml"} {
		if readBundleEntry(t, bundlePath, required) == nil {
			t.Fatalf("bundle misses required manifest %s", required)
		}
	}
	body := readBundleEntry(t, bundlePath, "quoin.yaml")
	bootstrap := readBundleEntry(t, bundlePath, "bootstrap.yaml")
	body = append(body, bootstrap...)
	for _, component := range subjects.Components {
		image := inventory.Images[component]
		want := image.Repository + "@" + image.IndexDigest
		if !strings.Contains(string(body), want) {
			t.Fatalf("bundle misses %s image %q", component, want)
		}
	}
	for _, forbidden := range []string{"quoin/web:v0.1.0-dev", "quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("bundle retains development image %q", forbidden)
		}
	}
}

func readBundleEntry(t *testing.T, path, want string) []byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == want {
			body, err := io.ReadAll(archive)
			if err != nil {
				t.Fatal(err)
			}
			return body
		}
	}
	t.Fatalf("bundle entry %q missing", want)
	return nil
}
