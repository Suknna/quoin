// Package release hosts the T30 acceptance: the formal, digest-pinned
// Compose install exercised through real binaries, containers, the ops
// surface and the deployment helper, with structured runtime and cleanup
// evidence. Tests skip unless QUOIN_EVIDENCE_DIR is set so `go test ./...`
// stays cheap in ordinary development.
package release_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	"gopkg.in/yaml.v3"
)

// TestFormalImagesEqualMachineLocks is the OPS-PACKAGE-002 handwritten-mirror
// check: the formal Dockerfiles under deploy/images must carry exactly the
// digest-locked bases from contracts/release-inputs.yaml and the full
// per-architecture version-pinned tool packages from
// contracts/plinth-worker-tools.yaml. A lock change without a Dockerfile
// change (or vice versa) fails here.
func TestFormalImagesEqualMachineLocks(t *testing.T) {
	root := repoRoot(t)

	var releaseInputs struct {
		BaseImages map[string]struct {
			Repository  string `yaml:"repository"`
			IndexDigest string `yaml:"index_digest"`
		} `yaml:"base_images"`
		ComponentBases map[string]string `yaml:"component_bases"`
	}
	if err := yaml.Unmarshal(gen.ReleaseInputsYAML, &releaseInputs); err != nil {
		t.Fatal(err)
	}
	distroless := releaseInputs.BaseImages[releaseInputs.ComponentBases["quoin"]]
	debian := releaseInputs.BaseImages[releaseInputs.ComponentBases["plinth"]]
	if distroless.Repository == "" || debian.Repository == "" {
		t.Fatal("release inputs do not lock the four-component bases")
	}

	for _, component := range []string{"quoin", "stele"} {
		dockerfile := readDockerfile(t, root, component)
		assertContains(t, component, "distroless repository ARG", dockerfile, "ARG DISTROLESS_REPO="+distroless.Repository)
		assertContains(t, component, "distroless index digest ARG", dockerfile, "ARG DISTROLESS_INDEX_DIGEST="+distroless.IndexDigest)
		assertContains(t, component, "non-root user", dockerfile, "USER 65532:65532")
	}

	plinthDockerfile := readDockerfile(t, root, "plinth")
	assertContains(t, "plinth", "debian repository ARG", plinthDockerfile, "ARG DEBIAN_REPO="+debian.Repository)
	assertContains(t, "plinth", "debian index digest ARG", plinthDockerfile, "ARG DEBIAN_INDEX_DIGEST="+debian.IndexDigest)

	var tools struct {
		Packages []struct {
			Name     string            `yaml:"name"`
			Versions map[string]string `yaml:"versions"`
		} `yaml:"packages"`
	}
	if err := yaml.Unmarshal(gen.PlinthWorkerToolsYAML, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools.Packages) == 0 {
		t.Fatal("plinth worker tool catalog is empty")
	}
	// The Dockerfile carries the per-architecture projections of the catalog:
	// the arm64 branch installs the arm64 locks, the else branch the amd64
	// locks (the lock diverges for grep and gawk).
	arm64Branch := arm64BranchPattern.FindString(plinthDockerfile)
	if arm64Branch == "" {
		t.Fatal("plinth Dockerfile has no per-architecture install branches")
	}
	amd64Branch := strings.SplitN(strings.SplitN(plinthDockerfile, "else apt-get install", 2)[1], "\n    fi", 2)[0]
	branches := map[string]string{"linux/amd64": amd64Branch, "linux/arm64": arm64Branch}
	pattern := regexp.MustCompile(`([a-z0-9][a-z0-9+.-]*)=([^ \\]+)`)
	for platform, branch := range branches {
		pins := map[string]string{}
		for _, pin := range pattern.FindAllStringSubmatch(branch, -1) {
			pins[pin[1]] = strings.TrimSuffix(pin[2], ";")
		}
		if len(pins) != len(tools.Packages)+1 {
			t.Fatalf("%s branch pins %d packages, want the %d locked tools plus ca-certificates: %v", platform, len(pins), len(tools.Packages), pins)
		}
		for _, entry := range tools.Packages {
			version := entry.Versions[platform]
			if version == "" {
				t.Fatalf("tool %s has no %s version in the catalog", entry.Name, platform)
			}
			if pins[entry.Name] != version {
				t.Fatalf("%s branch pins %s=%q but the machine lock says %q", platform, entry.Name, pins[entry.Name], version)
			}
		}
	}
}

// arm64BranchPattern isolates the arm64 install branch in the formal plinth
// Dockerfile from the shared amd64 else-branch.
var arm64BranchPattern = regexp.MustCompile(`(?s)\[ "\$TARGETARCH" = "arm64" \].*?ca-certificates=\S+;`)

func readDockerfile(t *testing.T, root, component string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "deploy", "images", component, "Dockerfile"))
	if err != nil {
		t.Fatalf("formal %s image missing: %v", component, err)
	}
	return string(data)
}

func assertContains(t *testing.T, component, what, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s Dockerfile does not carry the %s: %s", component, what, needle)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found")
		}
		dir = parent
	}
}
