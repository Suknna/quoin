package suites

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DeploymentRequest is one `quoin-deploy <backend> verify --suite S
// --phase P` invocation context. The catalog phase commands reach this
// code through the deployment helper; the acceptance coordinator and
// the CI workflow invoke the same entrypoint.
type DeploymentRequest struct {
	Backend             string
	Suite               string
	Phase               string
	Scenario            string
	Cell                string
	Workdir             string
	FactsPath           string
	ConfigPath          string
	ReleaseManifestPath string
	RepoRoot            string
	Stdout              io.Writer
	Stderr              io.Writer
}

// ServerArch resolves the docker server architecture the fault actors
// must be built for.
func (request DeploymentRequest) ServerArch() (string, error) {
	output, err := exec.Command("docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		return "", fmt.Errorf("docker server architecture: %w", err)
	}
	arch := strings.TrimSpace(string(output))
	switch arch {
	case "amd64", "x86_64":
		return "amd64", nil
	case "arm64", "aarch64":
		return "arm64", nil
	}
	return "", fmt.Errorf("docker server architecture %q unsupported", arch)
}

// CellParameter reads one parameter of the executing cell from the
// recorded cell parameters file the helper writes next to the facts.
func (request DeploymentRequest) CellParameter(name string) (string, error) {
	body, err := os.ReadFile(filepath.Join(request.Workdir, "cell-parameters.json"))
	if err == nil {
		var parameters map[string]any
		if json.Unmarshal(body, &parameters) == nil {
			if value, present := parameters[name]; present {
				return fmt.Sprint(value), nil
			}
		}
	}
	// Fall back to the cell id vocabulary: <...>-<fault> suffixes name
	// the fault for the fault suites.
	if name == "fault" {
		segments := strings.Split(request.Cell, "-")
		if len(segments) > 0 {
			return segments[len(segments)-1], nil
		}
	}
	return "", fmt.Errorf("cell parameter %q unresolved", name)
}

// storeJSON persists one driver observation for the assert phase; the
// assert phase re-reads machine facts, never re-deriving them.
func (request DeploymentRequest) storeJSON(name string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(request.Workdir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(request.Workdir, name), body, 0o644)
}

// loadJSON reads one stored observation.
func (request DeploymentRequest) loadJSON(name string, target any) error {
	body, err := os.ReadFile(filepath.Join(request.Workdir, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, target)
}

// writeFacts persists the facts document for the coordinator's
// assertion comparison.
func (request DeploymentRequest) writeFacts(assertions map[string]any, checks []map[string]string) error {
	if request.FactsPath == "" {
		return nil
	}
	return WriteFacts(request.FactsPath, assertions, checks)
}

func (request DeploymentRequest) logf(format string, arguments ...any) {
	writer := request.Stderr
	if writer == nil {
		writer = os.Stderr
	}
	fmt.Fprintf(writer, "quoin-deploy suite %s/%s: ", request.Suite, request.Phase)
	fmt.Fprintf(writer, format+"\n", arguments...)
}
