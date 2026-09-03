// Package compose owns the deployment helper's Compose backend orchestration:
// the staged install state machine with its persisted retry state and the
// read-only verifier that judges the deployed operational surface
// (OPS-HELPER-002..004, OPS-VERIFY-003).
package compose

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	deploycompose "github.com/Suknna/quoin/deploy/compose"
	"github.com/Suknna/quoin/internal/buildinfo"
	"github.com/Suknna/quoin/internal/contract"
	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
)

// Exit codes follow OPS-HELPER-003: 0 success, 2 input/schema/state errors
// without deployment side effects, 1 platform or behavioral failures.
const (
	ExitSuccess  = 0
	ExitPlatform = 1
	ExitInput    = 2
)

// InputError marks invalid helper input: schema, digest or retry-state
// mismatches. No deployment side effect has occurred.
type InputError struct{ Message string }

func (err *InputError) Error() string { return err.Message }

// PlatformError marks a failed external command or behavioral check with a
// stable code and the next retryable action.
type PlatformError struct {
	Code       string
	Message    string
	NextAction string
}

func (err *PlatformError) Error() string {
	return fmt.Sprintf("%s: %s", err.Code, err.Message)
}

const (
	stagePreflight       = "preflight"
	stageSecretBootstrap = "secret-bootstrap"
	stageAdminBootstrap  = "admin-bootstrap"
	stageWorkloads       = "workloads"
	stageVerify          = "verify"
)

type Request struct {
	ConfigPath          string
	ReleaseManifestPath string
	ReportPath          string
	Stdin               io.Reader
	Stdout              io.Writer
	Stderr              io.Writer
}

func (req Request) stdout() io.Writer {
	if req.Stdout == nil {
		return os.Stdout
	}
	return req.Stdout
}

func (req Request) stderr() io.Writer {
	if req.Stderr == nil {
		return os.Stderr
	}
	return req.Stderr
}

// loadedRequest carries the validated inputs shared by install and verify.
type loadedRequest struct {
	input            contract.ComposeInstall
	manifest         *deployconfig.ReleaseManifest
	images           map[string]string
	binding          *contract.DeploymentBinding
	stateDir         string
	projection       deploycompose.Projection
	composeArguments []string
	project          string
}

// load validates the minimal input and (when given) the release manifest,
// then renders the canonical projection.
func load(req Request) (*loadedRequest, error) {
	var input contract.ComposeInstall
	if err := contract.DecodeFile(req.ConfigPath, &input); err != nil {
		return nil, &InputError{err.Error()}
	}
	stateDir, err := deployconfig.StateDirectory()
	if err != nil {
		return nil, &InputError{err.Error()}
	}
	loaded := &loadedRequest{input: input, stateDir: stateDir, images: map[string]string{}}
	if req.ReleaseManifestPath != "" {
		manifest, err := deployconfig.LoadReleaseManifest(req.ReleaseManifestPath)
		if err != nil {
			return nil, &InputError{err.Error()}
		}
		loaded.manifest = manifest
		binding, err := deployconfig.DeploymentBinding(manifest, req.ConfigPath, req.ReleaseManifestPath, "compose")
		if err != nil {
			return nil, &InputError{err.Error()}
		}
		loaded.binding = binding
		for _, component := range deployconfig.Components {
			reference, err := manifest.ImageReference(component)
			if err != nil {
				return nil, &InputError{err.Error()}
			}
			loaded.images[component] = reference
		}
	}
	projection, err := deploycompose.RenderWithOptions(input, stateDir, deploycompose.Options{Images: loaded.images, DeploymentBinding: loaded.binding})
	if err != nil {
		return nil, &InputError{err.Error()}
	}
	loaded.projection = projection
	loaded.project = os.Getenv("QUOIN_COMPOSE_PROJECT")
	if loaded.project == "" {
		loaded.project = "quoin"
	}
	loaded.composeArguments = []string{"compose", "--project-name", loaded.project, "--file", projection.ComposeFile}
	return loaded, nil
}

func (loaded *loadedRequest) release() string {
	if loaded.manifest != nil {
		return loaded.manifest.ReleaseVersion
	}
	return buildinfo.Release
}

func isTerminal(stdin io.Reader) bool {
	file, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func shortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// secretsPresent is the fast resume probe for a completed secret stage.
func secretsPresent(secretDirectory string) bool {
	for _, name := range []string{"root-key", "stele-service-token", "runtime-ca.pem", "runtime-ca.key", "runtime-tls.crt", "runtime-tls.key"} {
		if info, err := os.Lstat(filepath.Join(secretDirectory, name)); err != nil || info.Size() == 0 {
			return false
		}
	}
	return true
}

// firstContainer returns the leading container id of a compose service.
func (helper *runner) firstContainer(loaded *loadedRequest, component string) (string, error) {
	containers, err := helper.capture(dockerize(append(append([]string{}, loaded.composeArguments...), "ps", "-aq", component))...)
	if err != nil || strings.TrimSpace(containers) == "" {
		return "", &PlatformError{Code: "component_not_deployed", Message: fmt.Sprintf("no container for %s", component), NextAction: "rerun the install command to resume"}
	}
	return strings.Fields(strings.TrimSpace(containers))[0], nil
}

// awaitHealthy polls the health status of the four long-lived services so
// install only reports success once every component passes its own probe.
func (helper *runner) awaitHealthy(loaded *loadedRequest, timeout time.Duration, stage int) error {
	deadline := time.Now().Add(timeout)
	pending := []string{"quoin", "plinth", "lintel", "stele"}
	for time.Now().Before(deadline) {
		for index := 0; index < len(pending); index++ {
			output, err := helper.capture(dockerize(append(append([]string{}, loaded.composeArguments...), "ps", "--format", "{{.Service}} {{.Health}}", pending[index]))...)
			if err == nil && strings.HasPrefix(output, pending[index]+" healthy") {
				pending = append(pending[:index], pending[index+1:]...)
				index--
			}
		}
		if len(pending) == 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out waiting for %v", pending)
}
