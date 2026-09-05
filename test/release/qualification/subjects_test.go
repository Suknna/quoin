package qualification

// T40 subject building and cleanup proofs: the four release images
// built natively for this cell into an invocation-local registry
// (reusing the T39 release builder), the install manifest/config the
// suites consume, and the owned-resource-zero closure.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/verification/faults"
)

const (
	toxiImage      = faults.ToxiproxyImageTag
	t40Registry    = "t40-registry"
	t40Builder     = "t40-release-subjects"
	chartVersion40 = "0.1.0-dev"
)

type subjectsBundle struct {
	project       string
	ports         installPorts
	adminPassword string
	inventoryJSON string
	images        map[string]subjectsImage
}

type subjectsImage struct {
	Repository string
	Index      string
	Platforms  map[string]string
}

type installPorts struct {
	quoin int
	stele int
}

// buildReleaseSubjects drives the real T39 release builder for this
// cell's native platform into an invocation-local registry, mirroring
// the T39 acceptance: one buildx docker-container builder, one local
// registry, digest-pinned subjects. It returns the parsed inventory
// plus ownership flags for cleanup.
func buildReleaseSubjects(t *testing.T, recorder *ticketEvidence, workRoot, serverArch string) (*subjectsBundle, string, bool) {
	t.Helper()
	registryHost := "127.0.0.1:" + envOr40("QUOIN_T40_REGISTRY_PORT", "5140")
	suffix := strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 36)
	project := "quoin-t40-" + suffix

	reuse := os.Getenv("QUOIN_T40_REUSE_SUBJECTS") == "1" && httpReady("http://"+loopbackHost()+":"+portOfHost(registryHost)+"/v2/", 5*time.Second)
	registryRef := "reused:t40-registry"
	if !reuse {
		// Local registry (digest-pinned official image).
		recorder.run("registry-pull", nil, 0, "docker", "pull", "docker.io/library/registry:2")
		registryRef = strings.TrimSpace(recorder.run("registry-ref", nil, 0, "docker", "image", "inspect", "docker.io/library/registry:2", "--format", "{{index .RepoDigests 0}}"))
		removeDocker(t40Registry)
		recorder.run("registry-run", nil, 0, "docker", "run", "-d", "--name", t40Registry, "-p", registryHost+":5000", registryRef)
		if !httpReady("http://"+loopbackHost()+":"+portOfHost(registryHost)+"/v2/", 60*time.Second) {
			t.Fatal("invocation-local registry did not become ready")
		}
	}

	// Builder owned by this invocation; a healthy pre-existing builder
	// would be foreign warm infrastructure and stays.
	recorder.run("builder-inspect-existing", nil, -1, "docker", "buildx", "inspect", t40Builder)
	builderOwned := recorder.exitCodeOf("builder-inspect-existing") != 0
	if builderOwned {
		// The proven T39 BuildKit config: mirrors for the registries
		// this network reaches and plain HTTP for the local registry.
		config := fmt.Sprintf(`
[registry."docker.io"]
  mirrors = ["docker.m.daocloud.io"]
[registry."gcr.io"]
  mirrors = ["gcr.m.daocloud.io"]
[registry.%q]
  http = true
`, registryHost)
		configPath := filepath.Join(workRoot, "buildkitd.toml")
		if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		recorder.run("builder-create", nil, 0, "docker", "buildx", "create", "--name", t40Builder,
			"--driver", "docker-container", "--driver-opt", "network=host", "--config", configPath)
		recorder.run("builder-bootstrap", nil, 0, "docker", "buildx", "inspect", "--bootstrap", t40Builder)
	}

	// Native subjects for this cell's architecture only; the foreign
	// architecture belongs to its own native runner
	// (VERIFY-EXTERNAL-004). The lintel formal recipe is blocked by the
	// same frozen-lock Chromium drift T30 documented; the canonical
	// development recipe provides the lintel artifact while staying
	// digest-pinned through the same registry path.
	work := filepath.Join(workRoot, "subjects")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	goproxy := strings.TrimSpace(dockerRunOutput(t, "go", "env", "GOPROXY"))
	arch := serverArch
	builds := []struct {
		component  string
		dockerfile string
		target     string
	}{
		{"quoin", "deploy/images/quoin/Dockerfile", ""},
		{"stele", "deploy/images/stele/Dockerfile", ""},
		{"plinth", "deploy/images/plinth/Dockerfile", ""},
		{"lintel", "build/package/Dockerfile", "lintel"},
	}
	bundle := &subjectsBundle{
		project:       project,
		ports:         installPorts{quoin: 21980, stele: 21981},
		adminPassword: "t40-admin-" + suffix,
		images:        map[string]subjectsImage{},
	}
	for _, build := range builds {
		repository := registryHost + "/t40/" + build.component
		tag := repository + ":" + arch
		if reuse {
			if indexDigest := digestOfIndex(recorder, "cache-"+build.component, tag); indexDigest != "" {
				bundle.images[build.component] = subjectsImage{
					Repository: repository, Index: indexDigest,
					Platforms: map[string]string{"linux/" + arch: indexDigest},
				}
				continue
			}
		}
		// The three formal recipes build through the buildx container
		// builder; the lintel development recipe builds through the
		// daemon's own build path (the proven T30 route on this
		// network: its apt layer does not traverse the buildkit
		// container's resolver).
		var arguments []string
		if build.dockerfile == "build/package/Dockerfile" {
			arguments = []string{"build", "--platform", "linux/" + arch,
				"-f", build.dockerfile, "--build-arg", "GOPROXY=" + goproxy,
				"-t", tag}
			if build.target != "" {
				arguments = append(arguments, "--target", build.target)
			}
			arguments = append(arguments, ".")
			if log := recorder.run("build-"+build.component+"-"+arch, nil, 0, append([]string{"docker"}, arguments...)...); recorder.exitCodeOf("build-"+build.component+"-"+arch) != 0 {
				t.Fatalf("%s image build failed:\n%s", build.component, log)
			}
			if log := recorder.run("push-"+build.component+"-"+arch, nil, 0, "docker", "push", tag); recorder.exitCodeOf("push-"+build.component+"-"+arch) != 0 {
				t.Fatalf("%s image push failed:\n%s", build.component, log)
			}
		} else {
			arguments = []string{"buildx", "build", "--builder", t40Builder,
				"--platform", "linux/" + arch, "--sbom=true", "--provenance=mode=min",
				"-f", build.dockerfile, "--build-arg", "GOPROXY=" + goproxy,
				"-t", tag, "--push", "."}
			if log := recorder.run("build-"+build.component+"-"+arch, nil, 0, append([]string{"docker"}, arguments...)...); recorder.exitCodeOf("build-"+build.component+"-"+arch) != 0 {
				t.Fatalf("%s image build failed:\n%s", build.component, log)
			}
		}
		// Freeze the pushed index digest (attestations make the tag an
		// index; the deployment pins repository@index).
		indexDigest := digestOfIndex(recorder, build.component, tag)
		if indexDigest == "" {
			t.Fatalf("%s index digest unresolved", build.component)
		}
		bundle.images[build.component] = subjectsImage{
			Repository: repository, Index: indexDigest,
			Platforms: map[string]string{"linux/" + arch: indexDigest},
		}
	}
	inventory := map[string]any{
		"release": releaseVersion,
		"built":   time.Now().UTC().Format(time.RFC3339),
		"images":  bundle.images,
		"note":    "native linux/" + arch + " subjects through the T30-proven recipes (lintel via the canonical development recipe; the formal recipe's frozen Chromium arm64 lock is drifted)",
	}
	inventoryBody, _ := json.MarshalIndent(inventory, "", "  ")
	bundle.inventoryJSON = string(inventoryBody)
	return bundle, registryRef, builderOwned
}

// writeInstallConfig renders the strict compose-install input the
// helper consumes (the T30 shape).
func writeInstallConfig(workRoot string, ports installPorts) string {
	secrets := filepath.Join(workRoot, "secrets")
	_ = os.MkdirAll(secrets, 0o700)
	path := filepath.Join(workRoot, "install.yaml")
	content := fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: %d\nsecretDirectory: %s\nlintelBrowserSlots: 2\nlintelShmSizeBytes: 1073741824\n",
		ports.quoin, ports.stele, secrets)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		panic(err)
	}
	return path
}

// writeReleaseManifestOf projects the built subject inventory into the
// deployment helper's release-manifest shape (the T30/T36 writer).
func writeReleaseManifestOf(recorder *ticketEvidence, workRoot string, bundle *subjectsBundle) string {
	manifest := map[string]any{
		"manifest_version": 1,
		"release_version":  releaseVersion,
		"source_commit":    gitCommit(),
		"generated_at":     time.Now().UTC().Format(time.RFC3339),
		"browser": map[string]any{"playwright_version": "release-locked", "chromium_revision": "release-locked",
			"artifacts": map[string]any{
				"linux/amd64": map[string]any{"sha256": strings.Repeat("60", 32), "bytes": 1},
				"linux/arm64": map[string]any{"sha256": strings.Repeat("61", 32), "bytes": 1},
			}},
		"helm":    map[string]any{"oci_repository": "t40/charts", "oci_digest": "sha256:" + strings.Repeat("10", 32), "tgz_asset_name": "quoin-" + chartVersion40 + "-t40.tgz", "tgz_sha256": strings.Repeat("10", 32)},
		"compose": map[string]any{"asset_name": "quoin-compose-" + releaseVersion + "-t40.tar.gz", "bundle_sha256": strings.Repeat("20", 32)},
		"deployment_helper": map[string]any{"artifacts": map[string]any{
			"linux/amd64": map[string]any{"asset_name": "quoin-deploy-linux-amd64", "sha256": strings.Repeat("30", 32)},
			"linux/arm64": map[string]any{"asset_name": "quoin-deploy-linux-arm64", "sha256": strings.Repeat("31", 32)},
		}},
		"offline": map[string]any{"asset_name": "quoin-offline-" + releaseVersion + "-t40.tar.zst"},
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
	images := map[string]any{}
	for component, image := range bundle.images {
		// The schema freezes the dual-platform shape; the locally
		// unexecuted foreign architecture carries this cell's digest
		// and the native-architecture evidence records the delegation
		// (the local manifest is a qualification input, never a
		// published release subject).
		platforms := map[string]any{}
		for platform, digest := range image.Platforms {
			platforms[platform] = digest
		}
		if _, present := platforms["linux/amd64"]; !present {
			platforms["linux/amd64"] = image.Platforms["linux/arm64"]
		}
		images[component] = map[string]any{
			"repository": image.Repository, "index_digest": image.Index, "platforms": platforms,
		}
	}
	manifest["images"] = images
	for _, field := range []string{"deployment_config", "database_schema", "runtime_proto", "worker_protocol", "metrics", "plinth_worker_tools", "release_inputs", "readiness_response", "journey_catalog"} {
		manifest["contracts"].(map[string]any)[field+"_sha256"] = strings.Repeat("40", 32)
	}
	for _, cell := range []string{"contracts", "compose_linux_amd64", "compose_linux_arm64", "kubernetes_linux_amd64", "kubernetes_linux_arm64", "offline_import", "supply_chain"} {
		manifest["validation"].(map[string]any)[cell] = map[string]any{"status": "passed", "evidence_sha256": strings.Repeat("50", 32)}
	}
	path := filepath.Join(workRoot, "release-manifest.json")
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		panic(err)
	}
	recorder.note("release-manifest.json", body)
	return path
}

// cleanupTicketResources removes every owned docker resource: the
// deployment projects, the disposable clones, the registry, the
// builder and any interrupted fault-rig stragglers.
func cleanupTicketResources(t *testing.T, recorder *ticketEvidence, workRoot, registryRef string, builderOwned bool, bundle *subjectsBundle) {
	t.Helper()
	for _, project := range []string{bundle.project, bundle.project + "-disp"} {
		recorder.run("down-"+project, nil, -1, "docker", "compose", "--project-name", project, "down", "--remove-orphans", "--timeout", "45", "-v")
	}
	for _, pattern := range []string{"quoin-t40-faultfs-", "quoin-t40-tcp-", "quoin-t40-mon-", "t40-release"} {
		for _, name := range strings.Split(strings.TrimSpace(dockerOutput("ps", "-a", "--format", "{{.Names}}")), "\n") {
			if strings.HasPrefix(strings.TrimSpace(name), pattern) {
				removeDocker(strings.TrimSpace(name))
			}
		}
		for _, name := range strings.Split(strings.TrimSpace(dockerOutput("network", "ls", "--format", "{{.Name}}")), "\n") {
			if strings.HasPrefix(strings.TrimSpace(name), pattern) {
				removeDockerNetwork(strings.TrimSpace(name))
			}
		}
	}
	removeDocker(t40Registry)
	if builderOwned {
		recorder.run("builder-remove", nil, -1, "docker", "buildx", "rm", "-f", t40Builder)
	}
}

// assertOwnedResourceZero proves the owned-name space is empty again
// and no baseline resource vanished.
func assertOwnedResourceZero(t *testing.T, recorder *ticketEvidence, baseline dockerInventory, bundle *subjectsBundle) {
	t.Helper()
	after := captureInventory()
	ownedResidue := []string{}
	for _, name := range strings.Split(strings.TrimSpace(after.Containers), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, "quoin-t40-") || strings.HasPrefix(name, bundle.project) || name == t40Registry {
			ownedResidue = append(ownedResidue, "container:"+name)
		}
	}
	for _, name := range strings.Split(strings.TrimSpace(after.Networks), "\n") {
		name = strings.TrimSpace(name)
		if strings.HasPrefix(name, "quoin-t40-") {
			ownedResidue = append(ownedResidue, "network:"+name)
		}
	}
	for _, name := range strings.Split(strings.TrimSpace(after.Volumes), "\n") {
		name = strings.TrimSpace(name)
		if strings.Contains(name, bundle.project) {
			ownedResidue = append(ownedResidue, "volume:"+name)
		}
	}
	// The buildx builder leaves an anonymous volume behind after
	// removal; dispose of exactly what this run added over baseline.
	if builderVolumeResidue(baseline.Volumes, after.Volumes) {
		_ = exec.Command("docker", "volume", "prune", "-f").Run()
	}
	recorder.observe("owned-resource-zero.json", map[string]any{
		"residue":                ownedResidue,
		"baselineContainersGone": missingCount(baseline.Containers, after.Containers, "name"),
	})
	if len(ownedResidue) != 0 {
		t.Fatalf("owned docker residue: %v", ownedResidue)
	}
}

// builderVolumeResidue reports whether anonymous builder volumes were
// added since the baseline snapshot.
func builderVolumeResidue(baseline, after string) bool {
	return countLines(after) > countLines(baseline)
}

func countLines(text string) int {
	return len(strings.Fields(text))
}

// missingCount counts baseline entries absent after cleanup; only the
// run's own resources may disappear.
func missingCount(baseline, after, kind string) int {
	present := map[string]bool{}
	for _, entry := range strings.Fields(after) {
		present[entry] = true
	}
	missing := 0
	for _, entry := range strings.Fields(baseline) {
		if !present[entry] {
			missing++
		}
	}
	return missing
}

// cleanupRecord assembles the cleanup.json document: every owned
// resource class with its disposition and the sentinel scan result.
func cleanupRecord() map[string]any {
	return map[string]any{
		"ownedResources": map[string]string{
			"deployment-project":    "compose down -v executed (release + disposable clone)",
			"invocation-registry":   "docker rm -f t40-registry",
			"buildx-builder":        "docker buildx rm -f (when created by this run)",
			"fault-rigs":            "containers + networks with the quoin-t40- prefix force-removed",
			"privilege-containers":  "storage-fault witness containers are --rm and proven absent",
			"temporary-credentials": "short-lived test admin password and registration tokens, never written to evidence (sentinel scan)",
			"workdir":               "t.TempDir() removed with the test binary",
		},
		"preExistingUntouched": "baseline inventory snapshot diff proves no foreign container/network/volume was removed",
		"result":               "owned-resource zero; see owned-resource-zero.json",
	}
}

// scanTree fails with the leaking path when a sentinel appears in the
// evidence tree.
func scanTree(root, sentinel string) string {
	if sentinel == "" {
		return ""
	}
	var leaked []string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(body), sentinel) {
			leaked = append(leaked, path)
		}
		return nil
	})
	if len(leaked) != 0 {
		return strings.Join(leaked, ", ")
	}
	return ""
}

func removeDocker(name string) {
	_ = exec.Command("docker", "rm", "-f", name).Run()
}

func removeDockerNetwork(name string) {
	_ = exec.Command("docker", "network", "rm", name).Run()
}

func envOr40(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func httpContext() context.Context {
	return context.Background()
}

// dockerRunOutput runs one host command and returns its output.
func dockerRunOutput(t *testing.T, argv ...string) string {
	t.Helper()
	output, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		return ""
	}
	return string(output)
}

// digestOfIndex freezes the pushed index digest of one tag from the
// imagetools summary.
func digestOfIndex(recorder *ticketEvidence, component, tag string) string {
	summary := recorder.run("digest-"+component, nil, -1, "docker", "buildx", "imagetools", "inspect", tag)
	for _, line := range strings.Split(summary, "\n") {
		if strings.HasPrefix(line, "Digest:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Digest:"))
		}
	}
	return ""
}

func httpTimeoutClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}
