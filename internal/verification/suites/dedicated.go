package suites

import "fmt"

// restoreIsolationObservation is deliberately limited to the two dedicated
// catalog assertions. It is derived from an isolated native Stack clone, not
// from release-qualification facts that happen to cover related behavior.
type restoreIsolationObservation struct {
	RestoreComplete         bool `json:"restoreComplete"`
	IdentityIsolationProved bool `json:"identityIsolationProved"`
}

// RunRestoreIsolationPhase drives the existing native backup/restore lifecycle
// on a disposable Stack namespace. NativeRestore invokes the core restore
// command in its offline PVC Pod; the old authenticated session is then probed
// against the rebuilt public surface before facts are emitted.
func RunRestoreIsolationPhase(request DeploymentRequest) error {
	stack, password, err := stackFromEnvironment(request)
	if err != nil {
		return err
	}
	switch request.Phase {
	case PhaseSetup:
		return nil
	case PhaseAction:
		detail := map[string]string{}
		_, _, restored, invalidated, _, _, _, _ := driveDisposableLifecycle(request, stack, password, detail)
		return request.storeJSON("restore-isolation-"+request.Cell+".json", restoreIsolationObservation{
			RestoreComplete: restored, IdentityIsolationProved: invalidated,
		})
	case PhaseAssert:
		var observed restoreIsolationObservation
		if err := request.loadJSON("restore-isolation-"+request.Cell+".json", &observed); err != nil {
			return fmt.Errorf("restore isolation observations missing: %w", err)
		}
		facts := map[string]any{"restore-complete": observed.RestoreComplete, "identity-isolation-proved": observed.IdentityIsolationProved}
		checks := []map[string]string{}
		for id, actual := range facts {
			result := "failed"
			if actual == true {
				result = "passed"
			}
			checks = append(checks, map[string]string{"name": id, "result": result})
		}
		return request.writeFacts(facts, checks)
	default:
		return fmt.Errorf("unknown restore-isolation phase %q", request.Phase)
	}
}

// RunLintelRecoveryPhase reserves the Kubernetes-native acceptance seam for
// the core maintenance recovery protocol. The protocol's issue/await/finalize
// exchange must retain the private registration envelope and observe the
// resulting receipt; it cannot truthfully reuse generic release facts.
// <--Waiting for Implementation--> Port the private core recovery exchange to
// the native offline Pod runner and emit facts only from its durable receipt.
func RunLintelRecoveryPhase(request DeploymentRequest) error {
	return fmt.Errorf("NOT_RUN: Kubernetes lintel-recovery requires a native maintenance recovery observation; no recovery receipt was produced")
}
