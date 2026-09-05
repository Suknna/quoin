package suites

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// RunDeploymentSuite dispatches one `quoin-deploy <backend> verify
// --suite S --phase P` invocation to its suite driver. Exit code 0
// means the phase completed and its observations were recorded; any
// non-zero exit is the phase failure the coordinator classifies.
func RunDeploymentSuite(request DeploymentRequest) int {
	if err := runDeploymentSuite(request); err != nil {
		if request.Stderr != nil {
			fmt.Fprintf(request.Stderr, "quoin-deploy: suite %s/%s %s phase failed: %v\n", request.Backend, request.Suite, request.Phase, err)
		}
		return 1
	}
	return 0
}

func runDeploymentSuite(request DeploymentRequest) error {
	switch request.Suite {
	case SuiteStorageFaults:
		return RunStorageFaultPhase(request)
	case SuiteNetworkFaults:
		return RunNetworkFaultPhase(request)
	case SuiteMonitoringStack:
		stack, password, err := stackFromEnvironment(request)
		if err != nil {
			return err
		}
		return RunMonitoringPhase(request, stack, password)
	case SuiteProductionTransport:
		stack, password, err := stackFromEnvironment(request)
		if err != nil {
			return err
		}
		return RunTransportPhase(request, stack, password)
	case SuiteReleaseQualification:
		stack, password, err := stackFromEnvironment(request)
		if err != nil {
			return err
		}
		return RunReleaseQualificationPhase(request, stack, password)
	}
	return fmt.Errorf("unknown suite %q", request.Suite)
}

// stackFromEnvironment assembles the deployment the product-driving
// suites observe. The invoking qualification passes the deployment
// coordinates through the environment so the helper process stays
// credential-free (OPS-HELPER-004: the verifier holds no product or
// connection credentials — the admin bootstrap password is the
// operator-supplied installer answer, transported out-of-band).
func stackFromEnvironment(request DeploymentRequest) (*Stack, string, error) {
	if request.Backend != "compose" {
		return nil, "", fmt.Errorf("suite %q on backend %q requires the cluster-backed adapter (compose cells execute this path natively)", request.Suite, request.Backend)
	}
	workRoot := os.Getenv("QUOIN_SUITE_WORK_ROOT")
	project := os.Getenv("QUOIN_SUITE_PROJECT")
	quoinPort, err := strconv.Atoi(envOr("QUOIN_SUITE_QUOIN_PORT", "0"))
	if err != nil {
		return nil, "", fmt.Errorf("QUOIN_SUITE_QUOIN_PORT: %w", err)
	}
	stelePort, err := strconv.Atoi(envOr("QUOIN_SUITE_STELE_PORT", "0"))
	if err != nil {
		return nil, "", fmt.Errorf("QUOIN_SUITE_STELE_PORT: %w", err)
	}
	if workRoot == "" || project == "" || quoinPort == 0 || stelePort == 0 {
		return nil, "", fmt.Errorf("suite %q requires QUOIN_SUITE_WORK_ROOT, QUOIN_SUITE_PROJECT, QUOIN_SUITE_QUOIN_PORT and QUOIN_SUITE_STELE_PORT", request.Suite)
	}
	password := os.Getenv("QUOIN_SUITE_ADMIN_PASSWORD")
	if shared, err := os.ReadFile(filepath.Join(os.TempDir(), "quoin-suite-"+project+"-admin-password")); err == nil && len(shared) > 0 {
		password = string(shared)
	}
	return &Stack{
		Project:                    project,
		WorkRoot:                   workRoot,
		ConfigPath:                 request.ConfigPath,
		ManifestPath:               request.ReleaseManifestPath,
		AdminPassword:              password,
		SharesInvocationCredential: true,
		QuoinPort:                  quoinPort,
		StelePort:                  stelePort,
		Stdout:                     request.Stdout,
		Stderr:                     request.Stderr,
		composeFile:                filepath.Join(workRoot, project, "state", "quoin", "compose", "generated", "compose.yaml"),
	}, password, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
