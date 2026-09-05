// Package environments resolves and freezes the Release Qualification
// environment matrix (VERIFY-MATRIX-001..004): which deployment backend
// and architecture this host can execute natively, the exact toolchain
// versions a qualification freezes in its evidence configuration, the
// maintained-Kubernetes window, and the digest-pinned external stack
// images. It records facts; it never widens a native claim — an emulated
// server architecture is reported as non-native and qualification cells
// for it are delegated to genuinely native runners.
package environments

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Runner executes probe commands; production uses exec.Command.
type Runner interface {
	Output(name string, arguments ...string) (string, error)
}

// execRunner adapts os/exec.
type execRunner struct{}

func (execRunner) Output(name string, arguments ...string) (string, error) {
	body, err := exec.Command(name, arguments...).CombinedOutput()
	return string(body), err
}

// Native is the resolved native execution capability of one host.
type Native struct {
	// Backend is the deployment backend present on this host:
	// "compose" (a Docker Engine with Compose) or "kubernetes" (a
	// reachable cluster). Empty when neither is usable.
	Backend string `json:"backend"`
	// Architecture is the server-side platform the backend executes
	// ("linux/amd64", "linux/arm64").
	Architecture string `json:"architecture"`
	// NativeExecution is true only when the backend architecture equals
	// the host architecture: QEMU-emulated containers may serve as build
	// or diagnostic evidence but never satisfy a native required cell
	// (VERIFY-EXTERNAL-004).
	NativeExecution bool `json:"nativeExecution"`
	// Emulated is true when the backend runs a foreign architecture
	// through binfmt/QEMU.
	Emulated bool `json:"emulated"`
	// HostGOARCH is runtime.GOARCH of the resolving process.
	HostGOARCH string `json:"hostGoarch"`
	// ServerVersion / ServerPlatform carry the raw docker observations.
	ServerVersion  string `json:"serverVersion,omitempty"`
	ServerPlatform string `json:"serverPlatform,omitempty"`
}

// DockerArchOf maps a runtime.GOARCH value to the linux platform triple.
func DockerArchOf(goarch string) string {
	return "linux/" + goarch
}

// ResolveNative probes Docker for the compose backend. A missing Docker
// is not an error: the returned Native reports an unusable backend so the
// caller records delegation instead of failing the qualification.
func ResolveNative(probe Runner) Native {
	resolved := Native{HostGOARCH: runtime.GOARCH}
	version, err := probe.Output("docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}} {{.Server.Version}}")
	if err != nil {
		return resolved
	}
	fields := strings.Fields(strings.TrimSpace(version))
	if len(fields) != 2 {
		return resolved
	}
	resolved.Backend = "compose"
	resolved.ServerPlatform = fields[0]
	resolved.ServerVersion = fields[1]
	resolved.Architecture = fields[0]
	// The Docker client runs on the host itself, so its platform is the
	// host truth even when the CLI is a wrapper.
	if platform, err := probe.Output("docker", "version", "--format", "{{.Client.Os}}/{{.Client.Arch}}"); err == nil {
		resolved.NativeExecution = strings.TrimSpace(platform) == resolved.ServerPlatform
		resolved.Emulated = !resolved.NativeExecution
	} else {
		// Without the client probe the claim cannot be proven native.
		resolved.NativeExecution = false
		resolved.Emulated = true
	}
	return resolved
}

// Toolchain carries the exact verification-environment versions a
// qualification freezes (VERIFY-MATRIX-002). These versions must never
// enter the product supply-chain release inputs.
type Toolchain struct {
	DockerEngineVersion string `json:"dockerEngineVersion"`
	ComposeVersion      string `json:"composeVersion"`
	GoVersion           string `json:"goVersion"`
}

// ResolveToolchain freezes the exact Docker Engine and Compose CLI
// versions plus the Go toolchain building the runner.
func ResolveToolchain(probe Runner) Toolchain {
	toolchain := Toolchain{}
	if version, err := probe.Output("docker", "version", "--format", "{{.Server.Version}}"); err == nil {
		toolchain.DockerEngineVersion = strings.TrimSpace(version)
	}
	if version, err := probe.Output("docker", "compose", "version", "--short"); err == nil {
		toolchain.ComposeVersion = strings.TrimSpace(version)
	}
	if version, err := probe.Output("go", "env", "GOVERSION"); err == nil {
		toolchain.GoVersion = strings.TrimSpace(version)
	}
	return toolchain
}

// KubernetesCell is one resolved maintained-Kubernetes matrix cell.
type KubernetesCell struct {
	Selector string `json:"selector"` // maintained_minor_N.latest_patch
	Version  string `json:"version"`  // resolved v1.minor.patch
}

// KubernetesWindow is the frozen six-cell window: the three most
// recently maintained minors with their latest patch, each on amd64 and
// arm64 (VERIFY-MATRIX-001).
type KubernetesWindow struct {
	ResolvedAt string           `json:"resolvedAt"`
	Source     string           `json:"source"`
	Cells      []KubernetesCell `json:"cells"`
}

// SelectMaintainedWindow computes the three latest maintained minors
// from the complete set of published patch versions. Keys are full
// "v1.<minor>.<patch>" strings.
func SelectMaintainedWindow(published map[string]bool, now time.Time) []KubernetesCell {
	latestPatch := map[int]int{}
	for version := range published {
		minor, patch, ok := parseVersion(version)
		if !ok {
			continue
		}
		if patch > latestPatch[minor] {
			latestPatch[minor] = patch
		}
	}
	minors := make([]int, 0, len(latestPatch))
	for minor := range latestPatch {
		minors = append(minors, minor)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(minors)))
	window := make([]KubernetesCell, 0, 3)
	for index, minor := range minors {
		if index >= 3 {
			break
		}
		window = append(window, KubernetesCell{
			Selector: fmt.Sprintf("maintained_minor_%d.latest_patch", index),
			Version:  fmt.Sprintf("v1.%d.%d", minor, latestPatch[minor]),
		})
	}
	return window
}

func parseVersion(version string) (minor, patch int, ok bool) {
	fields := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(fields) != 3 {
		return 0, 0, false
	}
	minor, errA := strconv.Atoi(fields[1])
	patch, errB := strconv.Atoi(fields[2])
	if errA != nil || errB != nil {
		return 0, 0, false
	}
	return minor, patch, true
}

// kubernetesSources are the official version endpoints the resolver
// reads (https://kubernetes.io/releases/): stable.txt gives the newest
// release; the per-minor endpoint gives that minor's latest patch. They
// are variables so the resolver's own tests can reroute them to a local
// server; production callers never mutate them.
var (
	stableEndpoint   = "https://dl.k8s.io/release/stable.txt"
	minorEndpointFmt = "https://dl.k8s.io/release/stable-1.%d.txt"
)

// ResolveKubernetesWindow resolves the maintained window online and
// freezes the resolution time. It fails closed: a partial or unreachable
// resolution returns an error and the qualification must delegate the
// Kubernetes cells rather than freezing an invented window.
func ResolveKubernetesWindow(ctx context.Context, client *http.Client) (KubernetesWindow, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	window := KubernetesWindow{ResolvedAt: time.Now().UTC().Format(time.RFC3339Nano), Source: stableEndpoint}
	stable, err := fetchVersion(ctx, client, stableEndpoint)
	if err != nil {
		return KubernetesWindow{}, fmt.Errorf("kubernetes stable: %w", err)
	}
	minor, _, ok := parseVersion(stable)
	if !ok {
		return KubernetesWindow{}, fmt.Errorf("kubernetes stable %q is not a v1.minor.patch", stable)
	}
	published := map[string]bool{stable: true}
	for candidate := minor; candidate >= minor-2 && candidate > 0; candidate-- {
		version, err := fetchVersion(ctx, client, fmt.Sprintf(minorEndpointFmt, candidate))
		if err != nil {
			return KubernetesWindow{}, fmt.Errorf("kubernetes minor 1.%d: %w", candidate, err)
		}
		published[version] = true
	}
	window.Cells = SelectMaintainedWindow(published, time.Now())
	if len(window.Cells) != 3 {
		return KubernetesWindow{}, fmt.Errorf("resolved %d maintained minors, want 3", len(window.Cells))
	}
	return window, nil
}

func fetchVersion(ctx context.Context, client *http.Client, url string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64))
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(body))
	if !strings.HasPrefix(version, "v1.") {
		return "", fmt.Errorf("%s answered %q", url, version)
	}
	return version, nil
}

// ImageDigest freezes one qualification-resolved external image.
type ImageDigest struct {
	Reference string `json:"reference"` // repository@sha256 digest form
	Tag       string `json:"tag"`
	Digest    string `json:"digest"`
}

// ResolveImageDigest pulls one image and freezes its repository digest.
// Qualification fixes the monitoring stack and Toxiproxy through exact
// digests, never mutable tags (VERIFY-FAULT-001, VERIFY-EXTERNAL-001).
func ResolveImageDigest(probe Runner, tag string) (ImageDigest, error) {
	if _, err := probe.Output("docker", "pull", tag); err != nil {
		return ImageDigest{}, fmt.Errorf("pull %s: %w", tag, err)
	}
	inspect, err := probe.Output("docker", "image", "inspect", tag, "--format", "{{json .RepoDigests}}")
	if err != nil {
		return ImageDigest{}, fmt.Errorf("inspect %s: %w", tag, err)
	}
	var digests []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(inspect)), &digests); err != nil || len(digests) == 0 {
		return ImageDigest{}, fmt.Errorf("image %s exposes no repo digest", tag)
	}
	sort.Strings(digests)
	return ImageDigest{Reference: digests[0], Tag: tag, Digest: digests[0]}, nil
}
