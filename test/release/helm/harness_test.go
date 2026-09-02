package helm

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The T31 acceptance mirrors the T01/T30 recorder contract: every command,
// artifact and observation lands under QUOIN_EVIDENCE_DIR with digests.

var (
	registryName       = "t31-registry"
	registryHost       = "127.0.0.1:5000"
	registryRepository = "t31"
	mainRelease        = "t31"
	mainNs             = "quoin-t31"
	retryRelease       = "t31r"
	retryNs            = "quoin-t31r"
)

type evidence struct {
	t          *testing.T
	dir        string
	commands   []commandRecord
	artifacts  []artifactRecord
	gitCommit  string
	dirtyState string
	toolInfo   map[string]string
	startedAt  time.Time
}

type commandRecord struct {
	Name     string   `json:"name"`
	Args     []string `json:"args"`
	ExitCode int      `json:"exitCode"`
	Duration string   `json:"duration"`
}

type artifactRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

func newEvidence(t *testing.T, dir string) *evidence {
	t.Helper()
	recorder := &evidence{t: t, dir: dir, startedAt: time.Now().UTC(), toolInfo: map[string]string{}}
	recorder.gitCommit = strings.TrimSpace(recorder.output("git", "rev-parse", "HEAD"))
	status, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	recorder.dirtyState = sha256Hex(status)
	for name, argv := range map[string][]string{
		"go":      {"go", "version"},
		"docker":  {"docker", "version", "--format", "{{.Server.Version}}"},
		"helm":    {"helm", "version", "--short"},
		"kubectl": {"kubectl", "version", "--client"},
		"k8s":     {"kubectl", "get", "-o", "jsonpath={.items[*].metadata.name}", "nodes"},
	} {
		output, err := exec.Command(argv[0], argv[1:]...).Output()
		if err != nil {
			t.Skipf("%s is not usable against a real cluster (%s): %v", name, strings.Join(argv, " "), err)
		}
		recorder.toolInfo[name] = strings.TrimSpace(string(output))
	}
	recorder.note("environment.json", mustJSON(t, map[string]any{
		"gitCommit": recorder.gitCommit, "dirtyStateDigest": recorder.dirtyState, "tools": recorder.toolInfo,
	}))
	return recorder
}

func (recorder *evidence) output(argv ...string) string {
	recorder.t.Helper()
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		recorder.t.Fatalf("%s: %v", strings.Join(argv, " "), err)
	}
	return string(out)
}

// run executes a command in the repository root, records its exit code and
// combined output, and returns the output. wantExit -1 accepts any code.
func (recorder *evidence) run(name string, env []string, stdin io.Reader, wantExit int, argv ...string) string {
	recorder.t.Helper()
	logPath := filepath.Join(recorder.dir, name+".log")
	started := time.Now()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = repoRoot(recorder.t)
	if env != nil {
		command.Env = env
	}
	command.Stdin = stdin
	var combined bytes.Buffer
	command.Stdout = &combined
	command.Stderr = &combined
	_ = command.Run()
	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	_ = os.WriteFile(logPath, combined.Bytes(), 0o644)
	recorder.commands = append(recorder.commands, commandRecord{Name: name, Args: append([]string(nil), argv...), ExitCode: exitCode, Duration: time.Since(started).Round(time.Millisecond).String()})
	recorder.artifacts = append(recorder.artifacts, artifactRecord{Path: logPath, SHA256: sha256Hex(combined.Bytes()), Bytes: combined.Len()})
	if wantExit >= 0 && exitCode != wantExit {
		recorder.t.Fatalf("%s: exit=%d want=%d output:\n%s", name, exitCode, wantExit, combined.String())
	}
	return combined.String()
}

func (recorder *evidence) note(name, content string) {
	recorder.t.Helper()
	path := filepath.Join(recorder.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		recorder.t.Fatal(err)
	}
	recorder.artifacts = append(recorder.artifacts, artifactRecord{Path: path, SHA256: sha256Hex([]byte(content)), Bytes: len(content)})
}

func (recorder *evidence) observe(name string, value any) {
	recorder.t.Helper()
	recorder.note(name, mustJSON(recorder.t, value))
}

// environmentBaseline snapshots the cluster surfaces this ticket touches so
// cleanup can prove pre-existing resources stayed untouched.
type environmentBaseline struct {
	Namespaces   string `json:"namespaces"`
	PVCs         string `json:"pvcs"`
	HelmReleases string `json:"helmReleases"`
	Containers   string `json:"containers"`
}

func captureEnvironmentBaseline(t *testing.T, recorder *evidence) environmentBaseline {
	t.Helper()
	baseline := environmentBaseline{
		Namespaces:   strings.TrimSpace(recorder.output("kubectl", "get", "namespaces", "--no-headers", "-o", "custom-columns=NAME:.metadata.name")),
		PVCs:         strings.TrimSpace(recorder.output("kubectl", "get", "pvc", "--all-namespaces", "--no-headers", "-o", "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")),
		HelmReleases: strings.TrimSpace(recorder.output("helm", "list", "--all-namespaces", "--output", "json")),
		Containers:   strings.TrimSpace(recorder.output("docker", "ps", "-a", "--format", "{{.ID}} {{.Names}}")),
	}
	recorder.note("baseline.json", mustJSON(t, baseline))
	return baseline
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, name := range []string{"docker", "go", "helm", "kubectl"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is not available: %v", name, err)
		}
	}
	// kubectl here may be the k3s multicall binary with its own kubeconfig
	// resolution; plain helm needs an explicit KUBECONFIG pointing at the
	// same cluster.
	if os.Getenv("KUBECONFIG") == "" {
		if _, err := os.Stat("/etc/rancher/k3s/k3s.yaml"); err == nil {
			if err := os.Setenv("KUBECONFIG", "/etc/rancher/k3s/k3s.yaml"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/qemu-aarch64"); err != nil {
		t.Skip("linux/arm64 emulation is not available (enable binfmt, e.g. docker run --privileged tonistiigi/binfmt --install arm64)")
	}
	if output, err := exec.Command("kubectl", "get", "--raw", "/readyz").Output(); err != nil {
		t.Skipf("no reachable Kubernetes cluster for the real acceptance path: %v (%s)", err, strings.TrimSpace(string(output)))
	}
}

// deployEnv isolates one helper invocation: state root, release and namespace.
func deployEnv(workRoot, release, namespace string) []string {
	return append(os.Environ(),
		"XDG_STATE_HOME="+filepath.Join(workRoot, release, "state"),
		"QUOIN_HELM_RELEASE="+release,
		"QUOIN_HELM_NAMESPACE="+namespace,
		"QUOIN_DEPLOY_SCRIPTED=1",
		"DOCKER_CLI_HINTS=false",
	)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(strings.TrimSpace(string(root)))
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func randomPassword(t *testing.T) string {
	t.Helper()
	buffer := make([]byte, 18)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("T31-%s", hex.EncodeToString(buffer))
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
