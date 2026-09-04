package subjects_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/release/inputs"
	"github.com/Suknna/quoin/internal/release/subjects"
)

// TestFormalImagesEqualMachineLocks is the T39 OPS-PACKAGE-002 mirror check:
// every formal Dockerfile under deploy/images must carry exactly the locked
// base digests and the full per-architecture version-pinned package sets
// from the machine locks. The builder passes the same locks as build
// arguments; this test pins the Dockerfile text itself so a lock change
// without a Dockerfile change (or vice versa) fails.
func TestFormalImagesEqualMachineLocks(t *testing.T) {
	root := repoRoot(t)
	lock, err := inputs.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, component := range []string{"quoin", "stele", "plinth", "lintel"} {
		raw, err := os.ReadFile(filepath.Join(root, "deploy", "images", component, "Dockerfile"))
		if err != nil {
			t.Fatalf("formal %s image missing: %v", component, err)
		}
		dockerfile := string(raw)
		base, err := lock.Base(component)
		if err != nil {
			t.Fatal(err)
		}
		switch base.Repository {
		case "gcr.io/distroless/static-debian13":
			assertText(t, component, "distroless repository ARG", dockerfile, "ARG DISTROLESS_REPO="+base.Repository)
			assertText(t, component, "distroless index digest ARG", dockerfile, "ARG DISTROLESS_INDEX_DIGEST="+base.IndexDigest)
		case "docker.io/library/debian":
			assertText(t, component, "debian repository ARG", dockerfile, "ARG DEBIAN_REPO="+base.Repository)
			assertText(t, component, "debian index digest ARG", dockerfile, "ARG DEBIAN_INDEX_DIGEST="+base.IndexDigest)
		default:
			t.Fatalf("%s base %s is not a locked formal base", component, base.Repository)
		}
		assertText(t, component, "non-root user", dockerfile, "USER 65532:65532")

		if component == "lintel" {
			// The Chromium download pins are part of the mirror contract:
			// the Dockerfile ARG defaults must equal the locked Playwright
			// artifacts exactly (OPS-IMAGE-002).
			for _, argument := range lock.ChromiumBuildArgs() {
				name, value, _ := strings.Cut(argument, "=")
				assertText(t, component, "chromium lock ARG "+name, dockerfile, "ARG "+name+"="+value)
			}
		}

		if component != "plinth" && component != "lintel" {
			continue
		}
		specs, err := lock.DebianSpecs(component)
		if err != nil {
			t.Fatal(err)
		}
		arm64Branch := lastMatch(regexp.MustCompile(`(?s)\[ "\$TARGETARCH" = "arm64" \].*?\n\s*(fi|else)`), dockerfile)
		if arm64Branch == "" {
			t.Fatalf("%s Dockerfile has no per-architecture install branches", component)
		}
		amd64Start := strings.LastIndex(dockerfile, "else apt-get install")
		amd64End := strings.Index(dockerfile[amd64Start:], "\n    fi") + amd64Start
		if amd64Start < 0 || amd64End < amd64Start {
			t.Fatalf("%s Dockerfile has no amd64 install branch", component)
		}
		amd64Branch := dockerfile[amd64Start:amd64End]
		for platform, branch := range map[string]string{"linux/amd64": amd64Branch, "linux/arm64": arm64Branch} {
			pins := aptPins(branch)
			names := make([]string, 0, len(specs[platform]))
			for name := range specs[platform] {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if pins[name] != specs[platform][name] {
					t.Fatalf("%s %s pins %s=%q but the machine lock says %q", component, platform, name, pins[name], specs[platform][name])
				}
			}
			if len(pins) != len(specs[platform]) {
				t.Fatalf("%s %s pins %d packages, want exactly the %d locked", component, platform, len(pins), len(specs[platform]))
			}
		}
	}
}

func aptPins(branch string) map[string]string {
	pattern := regexp.MustCompile(`([a-z0-9][a-z0-9+.-]*)=([^ \\;]+)`)
	pins := map[string]string{}
	for _, pin := range pattern.FindAllStringSubmatch(branch, -1) {
		pins[pin[1]] = pin[2]
	}
	return pins
}

func assertText(t *testing.T, component, what, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s Dockerfile does not carry the %s: %s", component, what, needle)
	}
}

// lastMatch returns the last occurrence so multi-stage Dockerfiles (whose
// earlier build stages may carry their own TARGETARCH conditionals) resolve
// to the final-stage package branches.
func lastMatch(pattern *regexp.Regexp, text string) string {
	all := pattern.FindAllString(text, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1]
}

// runLockEqualityAssertion proves inside the acceptance path that the formal
// Dockerfiles equal the machine locks and that the browser artifacts the
// lintel image baked are exactly the locked hashes.
func runLockEqualityAssertion(t *testing.T, recorder *evidence, inventory *subjects.Inventory) map[string]any {
	t.Helper()
	// The mirror check is the same deterministic proof as the unit test,
	// executed inside the acceptance evidence trail.
	t.Run("formal-images-equal-machine-locks", func(t *testing.T) {
		TestFormalImagesEqualMachineLocks(t)
	})
	lock, err := inputs.Load()
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]string{}
	for platform, artifact := range lock.Playwright.Artifacts {
		subject := inventory.Browser.Artifacts[platform]
		if subject.SHA256 != artifact.SHA256 {
			t.Fatalf("browser artifact %s: inventory %s != lock %s", platform, subject.SHA256, artifact.SHA256)
		}
		actual[platform] = subject.SHA256
	}
	verifyPlaywrightUpstreamLocks(t, recorder, lock)
	recorder.observe("browser-locks.json", map[string]any{
		"playwright_version": lock.Playwright.Version,
		"chromium_revision":  lock.Playwright.ChromiumRevision,
		"artifacts":          actual,
	})
	return map[string]any{
		"expected": "formal Dockerfiles equal release-inputs/plinth-worker-tools locks; baked Chromium artifacts equal the locked SHA-256 hashes; the pinned upstream Playwright sources still hash to the lock",
		"actual":   "mirror check passed; both architecture artifacts match the lock",
	}
}

// verifyPlaywrightUpstreamLocks re-fetches the two pinned upstream Playwright
// sources at the frozen tag and proves both SHA-256 hashes and the artifact
// URL mapping still equal the machine lock (OPS-IMAGE-002: the release
// verifier derives the platform URLs from the pinned upstream files).
func verifyPlaywrightUpstreamLocks(t *testing.T, recorder *evidence, lock inputs.Lock) {
	t.Helper()
	const upstream = "https://raw.githubusercontent.com/microsoft/playwright/"
	for _, pinned := range []struct {
		label string
		file  inputs.SourceFile
	}{
		{"browsers.json", lock.Playwright.BrowsersJSON},
		{"registry-source", lock.Playwright.RegistrySource},
	} {
		response, err := httpGet(upstream + lock.Playwright.SourceTag + "/" + pinned.file.Path)
		if err != nil {
			t.Fatalf("upstream %s: %v", pinned.label, err)
		}
		sum := sha256.Sum256(response)
		actual := hex.EncodeToString(sum[:])
		if actual != pinned.file.SHA256 {
			t.Fatalf("upstream %s sha256 %s does not equal the lock %s", pinned.label, actual, pinned.file.SHA256)
		}
		recorder.observe("upstream-"+pinned.label+".sha256", map[string]string{"path": pinned.file.Path, "sha256": actual})
	}
	amd64 := lock.Playwright.Artifacts["linux/amd64"].URL
	arm64 := lock.Playwright.Artifacts["linux/arm64"].URL
	if !strings.Contains(amd64, lock.Playwright.ChromiumVersion) {
		t.Fatalf("amd64 artifact URL %s does not carry the locked chromium version %s", amd64, lock.Playwright.ChromiumVersion)
	}
	if !strings.Contains(arm64, lock.Playwright.ChromiumRevision) {
		t.Fatalf("arm64 artifact URL %s does not carry the locked chromium revision %s", arm64, lock.Playwright.ChromiumRevision)
	}
}
