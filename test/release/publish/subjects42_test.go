package publish

// The T42 release subjects: the real T39 release builder produces the
// four dual-platform images (native amd64, emulated arm64 build evidence
// only — VERIFY-EXTERNAL-004), the merged OCI indexes, the Helm chart,
// the digest-pinned Compose bundle and both static helpers, ending with
// the validated subject inventory the final Release manifest references.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/release/offline"
	"github.com/Suknna/quoin/internal/release/supplychain"
)

const (
	t42Registry  = "t42-registry"
	t42Builder   = "t42-release-closure"
	t42Namespace = "t42"
)

var t42RegistryHost = "127.0.0.1:" + envOr42("QUOIN_T42_REGISTRY_PORT", "5142")

func envOr42(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

type subjectsResult struct {
	inventoryPath string
	work          string
	registryLive  bool
}

// ensureRegistry starts the invocation-local registry the whole closure
// consumes (VERIFY-MATRIX-04).
func ensureRegistry(t *testing.T, recorder *ticketEvidence) {
	t.Helper()
	if httpReady("http://127.0.0.1:"+portOf(t42RegistryHost)+"/v2/", 3*time.Second) {
		recorder.observe("registry-reused.json", map[string]string{"registry": t42RegistryHost, "owned": "pre-existing"})
		return
	}
	recorder.run(t, "registry-pull", nil, 0, "docker", "pull", "docker.io/library/registry:2")
	reference := strings.TrimSpace(dockerOutputCombined(t, "docker", "image", "inspect", "docker.io/library/registry:2", "--format", "{{index .RepoDigests 0}}"))
	if !strings.Contains(reference, "@sha256:") {
		t.Fatalf("registry fixture is not digest-pinned: %q", reference)
	}
	removeDocker(t42Registry)
	recorder.run(t, "registry-run", nil, 0, "docker", "run", "-d", "--name", t42Registry, "-p", t42RegistryHost+":5000", reference)
	if !httpReady("http://127.0.0.1:"+portOf(t42RegistryHost)+"/v2/", 60*time.Second) {
		t.Fatal("invocation-local registry did not become ready")
	}
}

func dockerOutputCombined(t *testing.T, argv ...string) string {
	t.Helper()
	output, err := execOutput(argv...)
	if err != nil {
		t.Fatalf("%v: %v", argv, err)
	}
	return output
}

// ensureBuilder provisions the test-owned buildx docker-container builder
// with the proven BuildKit mirror config; a healthy pre-existing builder
// is warm foreign infrastructure and stays.
func ensureBuilder(t *testing.T, recorder *ticketEvidence, workRoot string) bool {
	t.Helper()
	recorder.run(t, "builder-inspect-existing", nil, -1, "docker", "buildx", "inspect", t42Builder)
	if recorder.exitCodeOf("builder-inspect-existing") == 0 {
		recorder.observe("builder-reused.json", map[string]string{"builder": t42Builder, "owned": "pre-existing"})
		return false
	}
	config := fmt.Sprintf(`
[registry."docker.io"]
  mirrors = ["docker.m.daocloud.io"]
[registry."gcr.io"]
  mirrors = ["gcr.m.daocloud.io"]
[registry.%q]
  http = true
`, t42RegistryHost)
	configPath := filepath.Join(workRoot, "buildkitd.toml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder.run(t, "builder-create", nil, 0, "docker", "buildx", "create", "--name", t42Builder,
		"--driver", "docker-container", "--driver-opt", "network=host", "--config", configPath)
	recorder.run(t, "builder-bootstrap", nil, 0, "docker", "buildx", "inspect", "--bootstrap", t42Builder)
	return true
}

// buildSubjects42 drives the real T39 builder for both platforms and the
// assemble stage, returning the validated inventory. When the formal
// lintel recipe hits the documented frozen-Chromium lock drift, the
// canonical development recipe substitutes for lintel exactly as the T30/
// T40 acceptance routes prescribed (machine-visible disclosure below).
// QUOIN_T42_REUSE_SUBJECTS=1 reuses a prior invocation's still-live
// registry and inventory (fast reruns of the closure legs).
func buildSubjects42(t *testing.T, recorder *ticketEvidence, workRoot string) *subjectsResult {
	t.Helper()
	work := filepath.Join(workRoot, "subjects")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	result := &subjectsResult{inventoryPath: filepath.Join(work, "subjects-inventory.json"), work: work}
	if os.Getenv("QUOIN_T42_REUSE_SUBJECTS") == "1" {
		if body, err := os.ReadFile(result.inventoryPath); err == nil &&
			httpReady("http://127.0.0.1:"+portOf(t42RegistryHost)+"/v2/", 3*time.Second) {
			recorder.observe("subjects-reused.json", map[string]string{"inventory": result.inventoryPath})
			recorder.note("subjects-inventory.json", body)
			return result
		}
	}

	goproxy := strings.TrimSpace(dockerOutputCombined(t, "go", "env", "GOPROXY"))
	formal := recorder.run(t, "builder-images-formal", nil, -1,
		"go", "run", "./internal/release/build",
		"-registry", t42RegistryHost+"/"+t42Namespace,
		"-version", releaseVersion42,
		"-chart-oci", t42RegistryHost+"/"+t42Namespace+"/charts",
		"-builder", t42Builder,
		"-work", work,
		"-stage", "images",
		"-platform", "linux/amd64=native",
		"-platform", "linux/arm64=emulated",
	)
	formalOK := recorder.exitCodeOf("builder-images-formal") == 0
	if !formalOK {
		recorder.observe("formal-images-fallback.json", map[string]any{
			"exitCode": recorder.exitCodeOf("builder-images-formal"),
			"tail":     tailOf(formal, 25),
			"route":    "per-component formal buildx recipes (identical deploy/images sources, retried individually)",
		})
		buildSubjectsManually(t, recorder, work, goproxy)
	}
	recorder.run(t, "builder-assemble", nil, 0,
		"go", "run", "./internal/release/build",
		"-registry", t42RegistryHost+"/"+t42Namespace,
		"-version", releaseVersion42,
		"-chart-oci", t42RegistryHost+"/"+t42Namespace+"/charts",
		"-builder", t42Builder,
		"-work", work,
		"-out", result.inventoryPath,
		"-stage", "assemble",
	)
	body, err := os.ReadFile(result.inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	recorder.note("subjects-inventory.json", body)
	return result
}

// buildSubjectsManually reproduces the builder's images stage per
// component when the aggregate formal run fails: the same formal buildx
// recipes for all four components (cross-compiling build stages; the
// emulated final stage needs only binfmt). The fallback exists purely
// to retry a transient aggregate-stage failure — every component,
// including lintel, is built from its formal deploy/images recipe so
// the release subject is never a development substitute.
func buildSubjectsManually(t *testing.T, recorder *ticketEvidence, work, goproxy string) {
	t.Helper()
	for _, platform := range []string{"amd64", "arm64"} {
		mode := "native"
		if platform == "arm64" {
			mode = "emulated"
		}
		fragment := map[string]any{
			"platform": "linux/" + platform,
			"mode":     mode,
			"images":   map[string]any{},
		}
		for _, component := range []string{"quoin", "stele", "plinth", "lintel"} {
			repository := t42RegistryHost + "/" + t42Namespace + "/" + component
			tag := repository + ":" + platform
			recorder.run(t, "build-"+component+"-"+platform, nil, 0, "docker", "buildx", "build",
				"--builder", t42Builder, "--platform", "linux/"+platform,
				"--sbom=true", "--provenance=mode=min",
				"-f", "deploy/images/"+component+"/Dockerfile",
				"--build-arg", "GOPROXY="+goproxy,
				"-t", tag, "--push", ".")
			measureFragment(t, fragment, component, repository, platform)
		}
		body, err := json.MarshalIndent(fragment, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, "images-"+platform+".json"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// measureFragment reads one pushed per-arch tag and records the platform
// manifest digest plus attestation manifests into the fragment.
func measureFragment(t *testing.T, fragment map[string]any, component, repository, arch string) {
	t.Helper()
	reader := supplychain.RegistryReader{Host: t42RegistryHost}
	shortRepository := strings.TrimPrefix(repository, t42RegistryHost+"/")
	indexDigest, err := reader.TagDigest(shortRepository, arch)
	if err != nil {
		t.Fatalf("%s %s tag digest: %v", component, arch, err)
	}
	indexBody, err := reader.FetchIndex(shortRepository, indexDigest)
	if err != nil {
		t.Fatalf("%s %s index fetch: %v", component, arch, err)
	}
	platforms, err := offline.PlatformDigestsOf(indexBody)
	if err != nil {
		t.Fatal(err)
	}
	platformDigest, ok := platforms["linux/"+arch]
	if !ok {
		t.Fatalf("%s %s platform manifest missing from %s", component, arch, indexDigest)
	}
	attestations := attestationsOf(indexBody)
	fragment["images"].(map[string]any)[component] = map[string]any{
		"platform_manifest_digest": platformDigest,
		"attestation_manifests":    attestations,
	}
}

// attestationsOf extracts the unknown/unknown attestation descriptors.
func attestationsOf(indexBody []byte) []string {
	var shape struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform *struct {
				OS string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexBody, &shape); err != nil {
		return nil
	}
	var attestations []string
	for _, manifest := range shape.Manifests {
		if manifest.Platform != nil && manifest.Platform.OS == "unknown" {
			attestations = append(attestations, manifest.Digest)
		}
	}
	return attestations
}

func portOf(hostPort string) string {
	if index := strings.LastIndex(hostPort, ":"); index >= 0 {
		return hostPort[index+1:]
	}
	return hostPort
}

func execOutput(argv ...string) (string, error) {
	output, err := exec.Command(argv[0], argv[1:]...).Output()
	return string(output), err
}
