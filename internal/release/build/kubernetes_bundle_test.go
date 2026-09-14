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
	for _, required := range []string{"ops-services.yaml", "quoin.yaml"} {
		if readBundleEntry(t, bundlePath, required) == nil {
			t.Fatalf("bundle misses required manifest %s", required)
		}
	}
	pinned := func(component string) string {
		image := inventory.Images[component]
		return image.Repository + "@" + image.IndexDigest
	}
	// The default manifest is browser-free (ADR 0004): it pins exactly the
	// four mainline components and must not reference the browser runtime at
	// all — the retired Lintel overlay (deploy/retired/browser/) ships in no
	// bundle, so a plain kubectl apply of quoin.yaml + ops-services.yaml can
	// never start Lintel.
	body := readBundleEntry(t, bundlePath, "quoin.yaml")
	for _, component := range []string{"frontend", "plinth", "quoin", "stele"} {
		if !strings.Contains(string(body), pinned(component)) {
			t.Fatalf("default manifest misses %s image %q", component, pinned(component))
		}
	}
	if strings.Contains(string(body), pinned("lintel")) {
		t.Fatal("default manifest must not deploy the browser runtime; it is retired")
	}
	rendered := string(readBundleEntry(t, bundlePath, "quoin.yaml"))
	for _, forbidden := range []string{"quoin/web:v0.1.0-dev", "quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("quoin.yaml retains development image %q", forbidden)
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
