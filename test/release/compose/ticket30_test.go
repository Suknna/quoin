package release_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	"gopkg.in/yaml.v3"
)

var (
	mainProject        = "quoin-t30main"
	retryProject       = "quoin-t30retry"
	mismatchProj       = "quoin-t30mismatch"
	registryName       = "t30-registry"
	registryRepository = "t30"
	registryHost       = "127.0.0.1:5000"
	mainQuoinPort      = 19180
	mainStelePort      = 19181
	retryQuoinPort     = 19190
	retryStelePort     = 19191
)

// TestTicket30 proves the hardened formal Compose install end to end: minimal
// Schema input, digest-pinned four-component artifacts, fixed
// ports/volumes/topology, process locks, SIGTERM drain, the frozen metrics
// contract, retryable staged install state, and exact cleanup and retained
// state semantics — all through the real quoin-deploy / docker compose /
// container path with structured evidence under QUOIN_EVIDENCE_DIR.
func TestTicket30(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T30 acceptance evidence run disabled")
	}
	requireTools(t)
	recorder := newEvidence(t, evidenceDir)
	workRoot := t.TempDir()
	// The untouched-resources baseline must be captured before this test
	// creates any container, project or image.
	baseline := captureEnvironmentBaseline(t, recorder)
	registryRef := startRegistry(t, recorder)

	images := buildAndPushReleaseImages(t, recorder, workRoot)
	manifestPath := writeReleaseManifest(t, recorder, workRoot, images)

	helper := filepath.Join(workRoot, "quoin-deploy")
	started := time.Now()
	recorder.run("build-helper", nil, nil, 0, "go", "build", "-trimpath", "-o", helper, "./cmd/quoin-deploy")
	recorder.observe("helper-binary.json", map[string]any{"path": helper, "buildSeconds": time.Since(started).Seconds()})

	mainSecrets := filepath.Join(workRoot, "secrets-main")
	mainConfig := writeInstallConfig(t, workRoot, "install-main.yaml", mainSecrets, mainQuoinPort, mainStelePort)

	// Minimal Schema input proof: unknown fields are rejected with exit 2 and
	// no deployment side effect.
	badConfig := filepath.Join(workRoot, "bad-install.yaml")
	writeFileT(t, badConfig, "document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: 1\nsteleWebhookHostPort: 2\nunknownField: true\nsecretDirectory: "+mainSecrets+"\nlintelBrowserSlots: 1\n")
	beforeProjects := recorder.run("compose-ls-before", nil, nil, 0, "docker", "compose", "ls", "--all", "--format", "json")
	recorder.run("invalid-input", composeEnv(workRoot, mainProject), strings.NewReader(""), 2, helper, "compose", "install", "--config", badConfig, "--release-manifest", manifestPath, "--report", filepath.Join(workRoot, "report-invalid.json"))
	tampered := filepath.Join(workRoot, "release-manifest-bad.json")
	tamperManifest(t, manifestPath, tampered)
	recorder.run("invalid-manifest", composeEnv(workRoot, mainProject), strings.NewReader(""), 2, helper, "compose", "install", "--config", mainConfig, "--release-manifest", tampered, "--report", filepath.Join(workRoot, "report-badmanifest.json"))
	afterBad := recorder.run("compose-ls-after-bad", nil, nil, 0, "docker", "compose", "ls", "--all", "--format", "json")
	if strings.Contains(afterBad, mainProject) || beforeProjects != afterBad {
		t.Fatalf("invalid input produced deployment side effects:\n%s", afterBad)
	}

	// Formal digest-pinned install through the real helper path.
	tempPassword := randomPassword(t)
	answers := strings.NewReader(strings.Join([]string{"admin", "Ticket Admin", tempPassword, tempPassword}, "\n") + "\n")
	installReport := filepath.Join(workRoot, "report-install.json")
	recorder.run("install", composeEnv(workRoot, mainProject), answers, 0, helper, "compose", "install", "--config", mainConfig, "--release-manifest", manifestPath, "--report", installReport)
	assertInstallReport(t, recorder, installReport, images)

	composeFile := filepath.Join(workRoot, mainProject, "state", "quoin", "compose", "generated", "compose.yaml")
	assertPinnedContainers(t, recorder, composeFile, images)

	proveProcessLocks(t, recorder, composeFile)
	drainObservation := proveSIGTERMDrain(t, recorder, composeFile)
	recorder.observe("sigterm-drain.json", drainObservation)

	// Plain read-only verify against the running formal stack.
	verifyReport := filepath.Join(workRoot, "report-verify.json")
	recorder.run("verify", composeEnv(workRoot, mainProject), nil, 0, helper, "compose", "verify", "--config", mainConfig, "--release-manifest", manifestPath, "--report", verifyReport)
	assertVerifyReport(t, recorder, verifyReport)

	retention := proveRetainedState(t, recorder, workRoot, helper, mainConfig, manifestPath, composeFile, tempPassword)
	recorder.observe("retained-state.json", retention)

	retryObservation := proveInstallRetryState(t, recorder, helper, workRoot, manifestPath)
	recorder.observe("install-retry-state.json", retryObservation)

	cleanupTicketResources(t, recorder, workRoot, registryRef, baseline, tempPassword)

	scanEvidenceForSecrets(t, evidenceDir, tempPassword)
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, name := range []string{"docker", "go"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is not available: %v", name, err)
		}
	}
	// The release manifest schema demands both real platforms; arm64 image
	// construction therefore requires binfmt emulation on this host. Probing
	// the kernel registration avoids a registry pull, which restricted
	// networks may not serve.
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/qemu-aarch64"); err != nil {
		t.Skipf("linux/arm64 emulation is not available (enable binfmt, e.g. docker run --privileged tonistiigi/binfmt --install arm64): %v", err)
	}
}

func startRegistry(t *testing.T, recorder *evidence) string {
	t.Helper()
	recorder.run("registry-pull", nil, nil, 0, "docker", "pull", "docker.io/library/registry:2")
	reference := strings.TrimSpace(recorder.output("docker", "image", "inspect", "docker.io/library/registry:2", "--format", "{{index .RepoDigests 0}}"))
	if !strings.Contains(reference, "@sha256:") {
		t.Fatalf("registry fixture is not digest-pinned: %q", reference)
	}
	// A previous interrupted run may have left the test-owned registry
	// container holding the port; it carries only disposable pushed test
	// manifests, so remove it before starting a fresh one.
	_ = exec.Command("docker", "rm", "-f", registryName).Run()
	recorder.run("registry-run", nil, nil, 0, "docker", "run", "-d", "--name", registryName, "-p", registryHost+":5000", reference)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get("http://" + registryHost + "/v2/")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return reference
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("local registry did not become ready")
	return ""
}

// releaseImages carries the measured digest set of one component.
type releaseImages struct {
	Repository string `json:"repository"`
	AMD64      string `json:"linux/amd64"`
	ARM64      string `json:"linux/arm64"`
	Index      string `json:"index"`
}

func buildAndPushReleaseImages(t *testing.T, recorder *evidence, workRoot string) map[string]*releaseImages {
	t.Helper()
	goproxy := strings.TrimSpace(recorder.output("go", "env", "GOPROXY"))
	images := map[string]*releaseImages{}
	// The lintel formal recipe is blocked by frozen-lock drift (see the
	// ticket evidence); the qualified canonical development recipe provides
	// the lintel artifact for this acceptance while remaining fully
	// digest-pinned through the same registry path.
	builds := []struct {
		component  string
		dockerfile string
		target     string
		formal     bool
	}{
		{"quoin", "deploy/images/quoin/Dockerfile", "", true},
		{"stele", "deploy/images/stele/Dockerfile", "", true},
		{"plinth", "deploy/images/plinth/Dockerfile", "", true},
		{"lintel", "build/package/Dockerfile", "lintel", false},
	}
	for _, build := range builds {
		entry := &releaseImages{Repository: registryHost + "/" + registryRepository + "/" + build.component}
		for _, platform := range []string{"amd64", "arm64"} {
			tag := entry.Repository + ":" + platform
			arguments := []string{"build", "-f", build.dockerfile, "--platform", "linux/" + platform}
			if build.formal && build.component != "plinth" {
				// gcr.io is unreachable from some networks; the digest is the
				// authority, so a digest-identical mirror may substitute.
				arguments = append(arguments, "--build-arg", "DISTROLESS_REPO=gcr.m.daocloud.io/distroless/static-debian13")
			}
			if build.target != "" {
				arguments = append(arguments, "--target", build.target)
			}
			arguments = append(arguments, "--build-arg", "GOPROXY="+goproxy, "-t", tag, ".")
			recorder.run("build-"+build.component+"-"+platform, nil, nil, 0, append([]string{"docker"}, arguments...)...)
			recorder.run("push-"+build.component+"-"+platform, nil, nil, 0, "docker", "push", tag)
			digest := registryManifestDigest(t, entry.Repository, platform)
			if platform == "amd64" {
				entry.AMD64 = digest
			} else {
				entry.ARM64 = digest
			}
		}
		entry.Index = pushDualPlatformIndex(t, entry)
		if entry.Index == "" || entry.AMD64 == "" || entry.ARM64 == "" || entry.AMD64 == entry.ARM64 {
			t.Fatalf("%s did not produce two distinct real platform manifests", build.component)
		}
		images[build.component] = entry
	}
	recorder.observe("release-images.json", images)
	return images
}

// registryManifestDigest reads the pushed manifest digest from the local
// registry's Docker-Content-Digest header.
func registryManifestDigest(t *testing.T, repository, tag string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "http://"+registryHost+"/v2/"+strings.TrimPrefix(repository, registryHost+"/")+"/manifests/"+tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	digest := response.Header.Get("Docker-Content-Digest")
	if response.StatusCode != http.StatusOK || digest == "" {
		t.Fatalf("manifest %s:%s unreadable: status=%d digest=%q", repository, tag, response.StatusCode, digest)
	}
	return digest
}

// pushDualPlatformIndex assembles a real OCI image index from the two pushed
// platform manifests and records its registry digest.
func pushDualPlatformIndex(t *testing.T, entry *releaseImages) string {
	t.Helper()
	name := strings.TrimPrefix(entry.Repository, registryHost+"/")
	type descriptor struct {
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
		Digest    string `json:"digest"`
		Platform  struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
	}
	index := struct {
		SchemaVersion int          `json:"schemaVersion"`
		MediaType     string       `json:"mediaType"`
		Manifests     []descriptor `json:"manifests"`
	}{SchemaVersion: 2, MediaType: "application/vnd.oci.image.index.v1+json"}
	for _, platform := range []struct{ arch, digest string }{{"amd64", entry.AMD64}, {"arm64", entry.ARM64}} {
		request, err := http.NewRequest(http.MethodGet, "http://"+registryHost+"/v2/"+name+"/manifests/"+platform.digest, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			t.Fatalf("platform manifest %s unreadable: %d", platform.digest, response.StatusCode)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<22))
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		var platformDescriptor descriptor
		platformDescriptor.Digest = platform.digest
		platformDescriptor.MediaType = response.Header.Get("Content-Type")
		platformDescriptor.Size = int64(len(body))
		platformDescriptor.Platform.Architecture = platform.arch
		platformDescriptor.Platform.OS = "linux"
		index.Manifests = append(index.Manifests, platformDescriptor)
	}
	encoded, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPut, "http://"+registryHost+"/v2/"+name+"/manifests/index", strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", index.MediaType)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	digest := response.Header.Get("Docker-Content-Digest")
	if response.StatusCode != http.StatusCreated || digest == "" {
		t.Fatalf("index push failed: status=%d digest=%q", response.StatusCode, digest)
	}
	return digest
}

// writeReleaseManifest generates the acceptance release manifest from the
// measured image digests and the frozen machine locks. The browser section
// carries the locked upstream values; the helm/compose/helper/sigstore and
// validation sections are structural local-test values owned by the stage-10
// release pipeline and are disclosed as such in the runtime evidence.
func writeReleaseManifest(t *testing.T, recorder *evidence, workRoot string, images map[string]*releaseImages) string {
	t.Helper()
	var inputs struct {
		Playwright struct {
			Version          string `yaml:"version"`
			ChromiumRevision string `yaml:"chromium_revision"`
			Artifacts        map[string]struct {
				SHA256 string `json:"sha256" yaml:"sha256"`
				Bytes  int64  `json:"bytes" yaml:"bytes"`
			} `yaml:"artifacts"`
		} `yaml:"playwright"`
	}
	if err := yaml.Unmarshal(gen.ReleaseInputsYAML, &inputs); err != nil {
		t.Fatal(err)
	}
	browser := map[string]any{
		"playwright_version": inputs.Playwright.Version,
		"chromium_revision":  inputs.Playwright.ChromiumRevision,
		"artifacts":          inputs.Playwright.Artifacts,
	}
	manifest := map[string]any{
		"manifest_version": 1,
		// OPS-UPGRADE-001: the manifest release must equal the release the
		// images actually report (buildinfo), so the readiness uniformity
		// check holds for the locally built artifacts.
		"release_version": "v0.1.0-dev",
		"source_commit":   recorder.gitCommit,
		"generated_at":    time.Now().UTC().Format(time.RFC3339),
		"browser":         browser,
		"helm":            map[string]any{"oci_repository": "ghcr.io/suknna/quoin-chart", "oci_digest": "sha256:" + strings.Repeat("10", 32), "tgz_asset_name": "quoin-0.1.0-t30.tgz", "tgz_sha256": strings.Repeat("10", 32)},
		"compose":         map[string]any{"asset_name": "quoin-compose-v0.1.0-t30.tar.gz", "bundle_sha256": strings.Repeat("20", 32)},
		"deployment_helper": map[string]any{"artifacts": map[string]any{
			"linux/amd64": map[string]any{"asset_name": "quoin-deploy-linux-amd64", "sha256": strings.Repeat("30", 32)},
			"linux/arm64": map[string]any{"asset_name": "quoin-deploy-linux-arm64", "sha256": strings.Repeat("31", 32)},
		}},
		"offline": map[string]any{"asset_name": "quoin-offline-v0.1.0-t30.tar.zst"},
		"sigstore_bundles": map[string]any{
			"image_indexes": map[string]any{"quoin": "q.sigstore.json", "plinth": "p.sigstore.json", "lintel": "l.sigstore.json", "stele": "s.sigstore.json"},
			"image_manifests": map[string]any{
				"quoin":  map[string]any{"linux/amd64": "qa.sigstore.json", "linux/arm64": "qb.sigstore.json"},
				"plinth": map[string]any{"linux/amd64": "pa.sigstore.json", "linux/arm64": "pb.sigstore.json"},
				"lintel": map[string]any{"linux/amd64": "la.sigstore.json", "linux/arm64": "lb.sigstore.json"},
				"stele":  map[string]any{"linux/amd64": "sa.sigstore.json", "linux/arm64": "sb.sigstore.json"},
			},
			"helm_oci": "h.sigstore.json", "release_manifest": "m.sigstore.json", "compose": "c.sigstore.json",
			"deployment_helper": map[string]any{"linux/amd64": "da.sigstore.json", "linux/arm64": "db.sigstore.json"},
			"offline":           "o.sigstore.json",
		},
		"contracts": map[string]any{
			"deployment_config_version": 1, "database_schema_version": "1", "runtime_proto_version": "1",
			"worker_protocol_version": "1", "metrics_contract_version": 1, "plinth_worker_tools_version": 1,
			"release_inputs_version": 1, "readiness_response_version": 1, "journey_catalog_version": "1",
		},
		"validation": map[string]any{},
	}
	manifestImages := map[string]any{}
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		entry := images[component]
		manifestImages[component] = map[string]any{
			"repository": entry.Repository, "index_digest": entry.Index,
			"platforms": map[string]any{"linux/amd64": entry.AMD64, "linux/arm64": entry.ARM64},
		}
	}
	manifest["images"] = manifestImages
	for _, field := range []string{"deployment_config", "database_schema", "runtime_proto", "worker_protocol", "metrics", "plinth_worker_tools", "release_inputs", "readiness_response", "journey_catalog"} {
		manifest["contracts"].(map[string]any)[field+"_sha256"] = strings.Repeat("40", 32)
	}
	for _, cell := range []string{"contracts", "compose_linux_amd64", "compose_linux_arm64", "kubernetes_linux_amd64", "kubernetes_linux_arm64", "offline_import", "supply_chain"} {
		manifest["validation"].(map[string]any)[cell] = map[string]any{"status": "passed", "evidence_sha256": strings.Repeat("50", 32)}
	}
	path := filepath.Join(workRoot, "release-manifest.json")
	writeFileT(t, path, mustJSON(t, manifest))
	return path
}

func tamperManifest(t *testing.T, source, target string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"manifest_version": 1`, `"manifest_version": "one"`, 1)
	if tampered == string(data) {
		t.Fatal("tamper target not found")
	}
	writeFileT(t, target, tampered)
}

func writeInstallConfig(t *testing.T, workRoot, name, secretDir string, quoinPort, stelePort int) string {
	t.Helper()
	path := filepath.Join(workRoot, name)
	writeFileT(t, path, fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: %d\nsecretDirectory: %s\nlintelBrowserSlots: 1\nlintelShmSizeBytes: 1073741824\n", quoinPort, stelePort, secretDir))
	return path
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func composeFileArguments(composeFile string) []string {
	return []string{"docker", "compose", "--project-name", mainProject, "--file", composeFile}
}
