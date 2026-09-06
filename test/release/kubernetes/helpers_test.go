package kubernetes

// The T43 acceptance's shared fixtures: the evidence recorder (the
// T39/T40/T42 contract), the invocation-local registry, the formal
// subject build, the measured chart, the qualification inventory and
// the owned-resource cleanup.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/verification/suites"
)

type ticketEvidence struct {
	dir       string
	commands  []commandRecord
	artifacts []artifactRecord
	startedAt time.Time
}

type commandRecord struct {
	Name     string   `json:"name"`
	Args     []string `json:"args"`
	ExitCode int      `json:"exitCode"`
	Duration string   `json:"duration"`
	Log      string   `json:"log"`
}

type artifactRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

func newEvidence(dir string) *ticketEvidence {
	return &ticketEvidence{dir: dir, startedAt: time.Now().UTC()}
}

func (recorder *ticketEvidence) run(t *testing.T, name string, env []string, wantExit int, argv ...string) string {
	t.Helper()
	logPath := filepath.Join(recorder.dir, name+".log")
	started := time.Now()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = repoRoot()
	if env != nil {
		command.Env = env
	}
	var combined bytes.Buffer
	command.Stdout, command.Stderr = &combined, &combined
	_ = command.Run()
	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	_ = os.WriteFile(logPath, combined.Bytes(), 0o644)
	recorder.commands = append(recorder.commands, commandRecord{
		Name: name, Args: argv, ExitCode: exitCode,
		Duration: time.Since(started).Round(time.Millisecond).String(),
		Log:      name + ".log",
	})
	recorder.artifacts = append(recorder.artifacts, artifactRecord{Path: logPath, SHA256: sha256Hex(combined.Bytes()), Bytes: combined.Len()})
	fmt.Printf("t43: %s exit=%d %.1fs\n", name, exitCode, time.Since(started).Seconds())
	if wantExit >= 0 && exitCode != wantExit {
		t.Fatalf("%s exited %d (want %d):\n%s", name, exitCode, wantExit, tailOf(combined.String(), 40))
	}
	return combined.String()
}

func (recorder *ticketEvidence) exitCodeOf(name string) int {
	for _, entry := range recorder.commands {
		if entry.Name == name {
			return entry.ExitCode
		}
	}
	return -1
}

func (recorder *ticketEvidence) note(name string, content []byte) {
	path := filepath.Join(recorder.dir, name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return
	}
	recorder.artifacts = append(recorder.artifacts, artifactRecord{Path: path, SHA256: sha256Hex(content), Bytes: len(content)})
}

func (recorder *ticketEvidence) observe(name string, value any) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return
	}
	recorder.note(name, body)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func tailOf(text string, lines int) string {
	all := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(all) <= lines {
		return text
	}
	return strings.Join(all[len(all)-lines:], "\n")
}

// ensureRegistry starts (or reuses) the invocation-local registry the
// cluster pulls the digest-pinned subjects from.
func ensureRegistry(t *testing.T, recorder *ticketEvidence) {
	t.Helper()
	if httpReady("http://127.0.0.1:5142/v2/", 3*time.Second) {
		recorder.observe("registry-reused.json", map[string]string{"registry": registryHostPort, "owned": "pre-existing"})
		return
	}
	recorder.run(t, "registry-pull", nil, 0, "docker", "pull", "docker.io/library/registry:2")
	reference := strings.TrimSpace(dockerInspect(t, "docker", "image", "inspect", "docker.io/library/registry:2", "--format", "{{index .RepoDigests 0}}"))
	_ = exec.Command("docker", "rm", "-f", registryName).Run()
	recorder.run(t, "registry-run", nil, 0, "docker", "run", "-d", "--name", registryName, "-p", registryHostPort+":5000", reference)
	if !httpReady("http://127.0.0.1:5142/v2/", 60*time.Second) {
		t.Fatal("invocation-local registry did not become ready")
	}
}

func dockerInspect(t *testing.T, argv ...string) string {
	t.Helper()
	output, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		t.Fatalf("%v: %v", argv, err)
	}
	return string(output)
}

func httpReady(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		if response, err := client.Get(url); err == nil {
			response.Body.Close()
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// buildSubjects builds the four formal component images (native
// architecture) into the registry through the proven buildx path with
// SBOM and provenance attestations.
func buildSubjects(t *testing.T, recorder *ticketEvidence) {
	t.Helper()
	builder := "t43-builder"
	recorder.run(t, "builder-inspect-existing", nil, -1, "docker", "buildx", "inspect", builder)
	if recorder.exitCodeOf("builder-inspect-existing") != 0 {
		recorder.run(t, "builder-create", nil, 0, "docker", "buildx", "create", "--name", builder,
			"--driver", "docker-container", "--driver-opt", "network=host", "--bootstrap")
	}
	goproxy := strings.TrimSpace(runOutput(t, "go", "env", "GOPROXY"))
	arch := hostArch()
	for _, component := range []string{"quoin", "stele", "plinth", "lintel"} {
		repository := registryHostPort + "/t43/" + component
		recorder.run(t, "build-"+component, nil, 0, "docker", "buildx", "build",
			"--builder", builder, "--platform", "linux/"+arch,
			"--sbom=true", "--provenance=mode=min",
			"-f", "deploy/images/"+component+"/Dockerfile",
			"--build-arg", "GOPROXY="+goproxy,
			"-t", repository+":"+arch, "--push", ".")
	}
}

// chartAndInventory packages and pushes the chart, measures its OCI
// digest, and freezes the qualification inventory of the four images.
func chartAndInventory(t *testing.T, recorder *ticketEvidence, workRoot string) (string, string, map[string]suites.SubjectImage) {
	t.Helper()
	chartRoot := filepath.Join(workRoot, "chart")
	_ = os.MkdirAll(chartRoot, 0o755)
	recorder.run(t, "chart-package", nil, 0, "helm", "package", "deploy/helm/quoin",
		"--version", strings.TrimPrefix(releaseVersion, "v"), "--destination", chartRoot)
	pushOutput := recorder.run(t, "chart-push", nil, 0, "helm", "push",
		filepath.Join(chartRoot, "quoin-"+strings.TrimPrefix(releaseVersion, "v")+".tgz"),
		"oci://"+registryHostPort+"/t43/charts")
	chartDigest := ""
	for _, line := range strings.Split(pushOutput, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Digest: ") {
			chartDigest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Digest: "))
		}
	}
	if chartDigest == "" {
		t.Fatal("chart push reported no digest")
	}
	inventory := map[string]suites.SubjectImage{}
	for _, component := range []string{"quoin", "stele", "plinth", "lintel"} {
		repository := registryHostPort + "/t43/" + component
		summary := recorder.run(t, "digest-"+component, nil, -1, "docker", "buildx", "imagetools", "inspect", repository+":"+hostArch())
		indexDigest := ""
		for _, line := range strings.Split(summary, "\n") {
			if strings.HasPrefix(line, "Digest:") {
				indexDigest = strings.TrimSpace(strings.TrimPrefix(line, "Digest:"))
			}
		}
		if indexDigest == "" {
			t.Fatalf("%s index digest unresolved", component)
		}
		inventory[component] = suites.SubjectImage{
			Repository: repository, Index: indexDigest,
			Platforms: map[string]string{"linux/" + hostArch(): indexDigest},
		}
	}
	recorder.observe("subjects.json", map[string]any{"chart": chartDigest, "images": inventory})
	return registryHostPort + "/t43/charts/quoin", chartDigest, inventory
}

// cleanup removes the owned namespace and registry; the release and
// port-forwards are proven gone by the teardown-zero assertion.
func cleanup(t *testing.T, recorder *ticketEvidence) {
	t.Helper()
	recorder.run(t, "namespace-delete", nil, -1, "kubectl", "delete", "namespace", namespace, "--ignore-not-found=true", "--wait=true", "--timeout=180s")
	_ = exec.Command("docker", "rm", "-f", registryName).Run()
}

func cleanupRecord() map[string]any {
	return map[string]any{
		"ownedResources": map[string]string{
			"helm-release":     "uninstalled and proven gone (teardown-zero.json)",
			"namespace":        "kubectl delete namespace executed",
			"port-forwards":    "killed with the stack; zero proven by pgrep",
			"registry":         "docker rm -f t43-registry",
			"disposable-clone": "second release uninstalled with its PVCs by its Down",
		},
		"preExistingUntouched": "the cluster's other namespaces and releases are never addressed by name",
		"result":               "owned-resource zero; see teardown-zero.json",
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
