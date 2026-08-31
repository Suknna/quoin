package helm

import (
	"encoding/json"
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

func startRegistry(t *testing.T, recorder *evidence) string {
	t.Helper()
	recorder.run("registry-pull", nil, nil, 0, "docker", "pull", "docker.io/library/registry:2")
	reference := strings.TrimSpace(recorder.output("docker", "image", "inspect", "docker.io/library/registry:2", "--format", "{{index .RepoDigests 0}}"))
	if !strings.Contains(reference, "@sha256:") {
		t.Fatalf("registry fixture is not digest-pinned: %q", reference)
	}
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

type releaseImages struct {
	Repository string `json:"repository"`
	AMD64      string `json:"linux/amd64"`
	ARM64      string `json:"linux/arm64"`
	Index      string `json:"index"`
}

// buildAndPushReleaseImages builds the four digest-pinned component images on
// both real platforms and assembles their immutable index (T30 recipe; the
// release manifest schema demands both platforms).
func buildAndPushReleaseImages(t *testing.T, recorder *evidence, workRoot string) map[string]*releaseImages {
	t.Helper()
	goproxy := strings.TrimSpace(recorder.output("go", "env", "GOPROXY"))
	images := map[string]*releaseImages{}
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
		entry := &releaseImages{Repository: registryHost + "/t31/" + build.component}
		for _, platform := range []string{"amd64", "arm64"} {
			tag := entry.Repository + ":" + platform
			arguments := []string{"build", "-f", build.dockerfile, "--platform", "linux/" + platform}
			if build.formal && build.component != "plinth" {
				arguments = append(arguments, "--build-arg", "DISTROLESS_REPO=gcr.m.daocloud.io/distroless/static-debian13")
			}
			if build.target != "" {
				arguments = append(arguments, "--target", build.target)
			}
			arguments = append(arguments, "--build-arg", "GOPROXY="+goproxy, "-t", tag, ".")
			recorder.run("build-"+build.component+"-"+platform, nil, nil, 0, append([]string{"docker"}, arguments...)...)
			recorder.run("push-"+build.component+"-"+platform, nil, nil, 0, "docker", "push", tag)
			if platform == "amd64" {
				entry.AMD64 = registryManifestDigest(t, entry.Repository, platform)
			} else {
				entry.ARM64 = registryManifestDigest(t, entry.Repository, platform)
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

// pushChartOCI packages the chart and pushes it to the disposable registry;
// the measured digest is the only chart reference the release manifest carries.
func pushChartOCI(t *testing.T, recorder *evidence, workRoot string) (digest, sha string) {
	t.Helper()
	packageDir := filepath.Join(workRoot, "chart")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// helm push appends the chart name to the OCI path: pushing to
	// oci://…/t31/charts stores the chart in t31/charts/quoin.
	recorder.run("chart-package", nil, nil, 0, "helm", "package", "deploy/helm/quoin", "--destination", packageDir)
	pushed := recorder.run("chart-push", nil, nil, 0, "helm", "push", filepath.Join(packageDir, "quoin-0.1.0.tgz"), "oci://"+registryHost+"/t31/charts")
	for _, line := range strings.Split(pushed, "\n") {
		if strings.HasPrefix(line, "Digest: ") {
			digest = strings.TrimSpace(strings.TrimPrefix(line, "Digest: "))
		}
	}
	if digest == "" {
		t.Fatalf("chart push did not report a digest:\n%s", pushed)
	}
	data, err := os.ReadFile(filepath.Join(packageDir, "quoin-0.1.0.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	return digest, sha256Hex(data)
}

func writeReleaseManifest(t *testing.T, recorder *evidence, workRoot string, images map[string]*releaseImages, chartDigest, chartSHA string) string {
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
	manifest := map[string]any{
		"manifest_version": 1,
		"release_version":  "v0.1.0-dev",
		"source_commit":    recorder.gitCommit,
		"generated_at":     time.Now().UTC().Format(time.RFC3339),
		"browser": map[string]any{
			"playwright_version": inputs.Playwright.Version,
			"chromium_revision":  inputs.Playwright.ChromiumRevision,
			"artifacts":          inputs.Playwright.Artifacts,
		},
		"helm":    map[string]any{"oci_repository": registryHost + "/t31/charts/quoin", "oci_digest": chartDigest, "tgz_asset_name": "quoin-0.1.0.tgz", "tgz_sha256": chartSHA},
		"compose": map[string]any{"asset_name": "quoin-compose-v0.1.0-t31.tar.gz", "bundle_sha256": strings.Repeat("20", 32)},
		"deployment_helper": map[string]any{"artifacts": map[string]any{
			"linux/amd64": map[string]any{"asset_name": "quoin-deploy-linux-amd64", "sha256": strings.Repeat("30", 32)},
			"linux/arm64": map[string]any{"asset_name": "quoin-deploy-linux-arm64", "sha256": strings.Repeat("31", 32)},
		}},
		"offline": map[string]any{"asset_name": "quoin-offline-v0.1.0-t31.tar.zst"},
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

// brokenImageManifest rewrites the quoin image to a well-formed but
// non-existent digest so the in-cluster bootstrap Job cannot pull it; this is
// the deterministic first-failure for the Job bootstrap retry proof.
func brokenImageManifest(t *testing.T, source, target string) string {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["images"].(map[string]any)["quoin"].(map[string]any)["index_digest"] = "sha256:" + strings.Repeat("ab", 32)
	writeFileT(t, target, mustJSON(t, manifest))
	return target
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

func writeInstallConfig(t *testing.T, workRoot, name string) string {
	t.Helper()
	path := filepath.Join(workRoot, name)
	writeFileT(t, path, `document: helm-install
publicOrigin: https://quoin.t31.example
publicIngress: {enabled: false}
steleIngress: {enabled: false}
storage:
  quoinData: {capacity: 2Gi, accessMode: ReadWriteOnce}
  quoinBackup: {capacity: 2Gi, accessMode: ReadWriteOnce}
  plinthState: {capacity: 2Gi, accessMode: ReadWriteOnce}
  lintelState: {capacity: 2Gi, accessMode: ReadWriteOnce}
lintelBrowserSlots: 1
lintelShmSize: 1Gi
`)
	return path
}
