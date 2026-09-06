package suites

// The backend seam of the suite deployment driver. Stack is the
// backend-agnostic contract every suite leg programs against; the
// compose and kubernetes implementations below own everything
// backend-specific. No leg ever branches on the backend kind — only
// this seam does (the seam IS the backend abstraction).
//
// Contract invariants both implementations must hold:
//   - ensureInstalled is an idempotent staged install through the real
//     deployment helper binary (os.Executable), with the four-line
//     scripted admin bootstrap answers on stdin.
//   - After ensureInstalled, the deployment answers HTTP on the local
//     QuoinPort and StelePort addresses (loopback publication is the
//     compose parity rule: clusters expose nothing persistent).
//   - runService is the one-shot stdin registration vehicle: a process
//     carrying exactly the long-running workload's image, environment
//     and mounts.
//   - down removes the deployment (and its data when dataRemoval) plus
//     every transport the implementation created (containers,
//     port-forwards, releases).

import (
	"strings"
	"time"
)

// Backend identifiers (the Stack.Backend vocabulary).
const (
	BackendCompose    = "compose"
	BackendKubernetes = "kubernetes"
)

// stackBackend is the set of backend-owned behaviors behind Stack.
type stackBackend interface {
	// helperVerb is the deployment helper subcommand ("compose"|"helm").
	helperVerb() string
	// opsBase is the in-deployment base URL of the Quoin ops listener
	// (the browser identities' start URL and authenticated prefix point
	// here; compose publishes one flat alias, Kubernetes splits the ops
	// listener into its release-scoped service).
	opsBase(stack *Stack) string
	// helperEnv is the environment helper subprocesses run with: it
	// isolates state per deployment identity (VERIFY-MATRIX-004).
	helperEnv(stack *Stack) []string
	// ensureInstalled performs the staged install and brings the public
	// listeners within reach of the suite process.
	ensureInstalled(stack *Stack) (string, error)
	// execCommand runs a one-shot command inside a running component.
	execCommand(stack *Stack, component string, arguments []string) (string, int, error)
	// runService runs the one-shot registration vehicle with stdin.
	runService(stack *Stack, component string, arguments []string, stdinPayload string) (string, int, error)
	// logs drains one component's recent log output.
	logs(stack *Stack, component string) (string, error)
	// down removes the deployment, its data when asked, and its
	// transports; it must be safe to call twice.
	down(stack *Stack, dataRemoval bool) (string, error)
	// refreshTransports re-establishes the suite's reachability after a
	// disruptive operation replaced workloads (a restore rebuilds pods;
	// pod-pinned transports die with the old pod). Compose publication
	// follows containers natively and the call is a no-op.
	refreshTransports(stack *Stack) error
	// stopComponent stops one component gracefully (the SIGTERM drain
	// vehicle) and reports the observed exit code plus the elapsed
	// stop time. stopQuoin below is the offline-fallback special case.
	stopComponent(stack *Stack, component string, timeout time.Duration) (exitCode string, elapsed time.Duration, err error)
	// startComponent returns a stopped component to service.
	startComponent(stack *Stack, component string) error
	// removeSecretAuthority destroys the deployment's secret authority
	// while preserving its data (the fail-closed proof's precondition):
	// compose removes the mounted secret directory, kubernetes deletes
	// the release-scoped Secret the helper treats as authoritative.
	removeSecretAuthority(stack *Stack) error
	// stopQuoin makes the Quoin workload unreachable so the offline
	// backup fallback path (OPS-BACKUP-006) is the only route left.
	stopQuoin(stack *Stack) error
	// runningWorkloadCount counts running plinth/lintel workloads — the
	// bootstrap-gating proof that a failed bootstrap never started them.
	runningWorkloadCount(stack *Stack) int
	// deploymentRoot is the deployment's on-disk root: explicit helper
	// report files (install-report.json, the legs' --report outputs)
	// live here, keyed by the deployment identity.
	deploymentRoot(stack *Stack) string
	// reportsDir is where the helper's internal stage reports live for
	// evidence collection (may not exist on every backend).
	reportsDir(stack *Stack) string
	// clone builds the disposable lifecycle deployment: a second,
	// fully-isolated deployment of the same subject (its own identity,
	// data, and loopback ports offset from the live matrix stack).
	clone(stack *Stack) *Stack
	// cloneRoot is the clone's on-disk root.
	cloneRoot(stack *Stack) string
}

// resolveBackend picks the implementation for the declared Backend.
func resolveBackend(backend string) stackBackend {
	if strings.EqualFold(backend, BackendKubernetes) {
		return &kubernetesBackend{}
	}
	return &composeBackend{}
}

// HelperVerb names the deployment helper subcommand for this backend.
func (stack *Stack) HelperVerb() string {
	return stack.backend().helperVerb()
}

// OpsBaseURL is the Quoin ops listener's in-deployment base URL.
func (stack *Stack) OpsBaseURL() string {
	return stack.backend().opsBase(stack)
}

// RefreshTransports re-establishes suite reachability after disruptive
// workload replacement (a restore rebuilds pods; pod-pinned transports
// die with the old pod).
func (stack *Stack) RefreshTransports() error {
	return stack.backend().refreshTransports(stack)
}

// HelperEnv is the helper-subprocess environment for this deployment.
func (stack *Stack) HelperEnv() []string {
	return stack.backend().helperEnv(stack)
}

// StopQuoin makes the Quoin workload unreachable (offline fallback
// proof vehicle).
func (stack *Stack) StopQuoin() error {
	return stack.backend().stopQuoin(stack)
}

// RemoveSecretAuthority destroys the deployment's secret authority while
// preserving its data.
func (stack *Stack) RemoveSecretAuthority() error {
	return stack.backend().removeSecretAuthority(stack)
}

// StopComponent stops one component gracefully and reports the drain
// exit code and elapsed time (OPS-SHUTDOWN-001).
func (stack *Stack) StopComponent(component string, timeout time.Duration) (string, time.Duration, error) {
	return stack.backend().stopComponent(stack, component, timeout)
}

// StartComponent returns a stopped component to service.
func (stack *Stack) StartComponent(component string) error {
	return stack.backend().startComponent(stack, component)
}

// LifecycleCloneRoot is the disposable clone's on-disk root (removed
// with the clone).
func (stack *Stack) LifecycleCloneRoot() string {
	return stack.backend().cloneRoot(stack)
}

// RunningWorkloadCount counts running plinth/lintl workloads.
func (stack *Stack) RunningWorkloadCount() int {
	return stack.backend().runningWorkloadCount(stack)
}

// ReportsDir is the helper's internal stage-report directory.
func (stack *Stack) ReportsDir() string {
	return stack.backend().reportsDir(stack)
}

// DeploymentRoot is the deployment's on-disk root for explicit helper
// reports.
func (stack *Stack) DeploymentRoot() string {
	return stack.backend().deploymentRoot(stack)
}

// LifecycleClone builds the disposable lifecycle deployment.
func (stack *Stack) LifecycleClone() *Stack {
	return stack.backend().clone(stack)
}

// backend lazily resolves and memoizes the implementation.
func (stack *Stack) backend() stackBackend {
	stack.backendOnce.Do(func() {
		stack.backendImpl = resolveBackend(stack.Backend)
	})
	return stack.backendImpl
}
