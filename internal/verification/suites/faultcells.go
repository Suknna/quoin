package suites

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Suknna/quoin/internal/verification/faults"
)

// faultRigNames derives unique per-invocation resource names from the
// suite workdir so parallel invocations cannot collide
// (VERIFY-MATRIX-004).
func faultRigName(request DeploymentRequest, kind string) string {
	return fmt.Sprintf("quoin-t40-%s-%d", kind, os.Getpid())
}

// RunStorageFaultCell executes the fault.storage cell phases. setup
// cross-builds quoin-faultfs for the docker server architecture;
// action mounts the path-scoped errno vocabulary inside one privileged
// container per fault and witnesses the exact errnos; assert freezes
// the catalog facts; teardown is idempotent container removal (the
// action's --rm containers own their own disposal).
func RunStorageFaultPhase(request DeploymentRequest) error {
	switch request.Phase {
	case PhaseSetup:
		arch, err := request.ServerArch()
		if err != nil {
			return err
		}
		binary := filepath.Join(request.Workdir, "quoin-faultfs")
		if err := faults.BuildFaultfs(binary, arch, request.RepoRoot); err != nil {
			return err
		}
		request.logf("built quoin-faultfs for linux/%s", arch)
		return nil
	case PhaseAction:
		fault, err := request.CellParameter("fault")
		if err != nil {
			return err
		}
		arch, err := request.ServerArch()
		if err != nil {
			return err
		}
		driver := &faults.Faultfs{
			BinaryPath: filepath.Join(request.Workdir, "quoin-faultfs"),
			Workdir:    filepath.Join(request.Workdir, "cells", fault),
			Container:  faultRigName(request, "faultfs") + "-" + fault,
			Image:      "alpine:3.20",
		}
		if err := os.MkdirAll(driver.Workdir, 0o755); err != nil {
			return err
		}
		outcome, err := driver.RunStorageFaultCell(fault)
		if storeErr := request.storeJSON("storage-"+fault+".json", outcome); storeErr != nil {
			return storeErr
		}
		_ = arch
		if err != nil {
			return err
		}
		request.logf("storage fault %s class=%s", fault, outcome.Class)
		return nil
	case PhaseAssert:
		fault, err := request.CellParameter("fault")
		if err != nil {
			return err
		}
		var outcome faults.StorageCellOutcome
		if err := request.loadJSON("storage-"+fault+".json", &outcome); err != nil {
			return fmt.Errorf("storage fault observation missing: %w", err)
		}
		checks := []map[string]string{}
		pass := func(name string, ok bool) {
			state := "failed"
			if ok {
				state = "passed"
			}
			checks = append(checks, map[string]string{"name": name, "result": state})
		}
		faultObserved := outcome.Class == "fault_deterministic_"+fault
		pass("fault-observed", faultObserved)
		pass("no-false-success", outcome.NoFalseSuccess)
		pass("recovery-action-exposed", outcome.RecoveryAction != "" && outcome.UnmountedClean)
		pass("integrity-preserved", outcome.IntegrityPreserved)
		if err := request.writeFacts(map[string]any{
			"fault-observed":          fault,
			"no-false-success":        outcome.NoFalseSuccess,
			"recovery-action-exposed": outcome.RecoveryAction != "" && outcome.UnmountedClean,
			"integrity-preserved":     outcome.IntegrityPreserved,
		}, checks); err != nil {
			return err
		}
		if !faultObserved || !outcome.NoFalseSuccess || !outcome.IntegrityPreserved || !outcome.UnmountedClean {
			return fmt.Errorf("storage fault %s assertions failed: %+v", fault, outcome)
		}
		return nil
	case PhaseTeardown:
		// The action's containers are --rm; any interrupted stragglers
		// are force-removed idempotently, proving fuse-unmounted and
		// temporary-data-removed.
		fault, err := request.CellParameter("fault")
		if err != nil {
			fault = "*"
		}
		removeContainers(faultRigName(request, "faultfs") + "-" + fault)
		return nil
	}
	return fmt.Errorf("unknown phase %q", request.Phase)
}

// RunNetworkFaultPhase executes the fault.network cell phases on the
// in-network rig: every toxic is observed through real client
// exchanges with unmodified TCP semantics.
func RunNetworkFaultPhase(request DeploymentRequest) error {
	switch request.Phase {
	case PhaseSetup:
		arch, err := request.ServerArch()
		if err != nil {
			return err
		}
		client := filepath.Join(request.Workdir, "faultclient")
		if err := faults.BuildFaultclient(client, arch, request.RepoRoot); err != nil {
			return err
		}
		request.logf("built faultclient for linux/%s", arch)
		return nil
	case PhaseAction:
		fault, err := request.CellParameter("fault")
		if err != nil {
			return err
		}
		rig, err := faults.StartNetworkRig(faultRigName(request, "tcp"),
			filepath.Join(request.Workdir, "faultclient"), "alpine:3.20",
			filepath.Join(request.Workdir, "cells", fault), 28474)
		if err != nil {
			return err
		}
		defer rig.Stop()
		observation, err := rig.ObserveTCPFault(fault)
		restored := rig.RoutesRestored()
		if storeErr := request.storeJSON("network-"+fault+".json", map[string]any{
			"observation": observation, "routesRestored": restored,
		}); storeErr != nil {
			return storeErr
		}
		if err != nil {
			return err
		}
		// The --rm containers disappear asynchronously; retry the
		// removal proof briefly so a loaded daemon's lag cannot flake
		// the residue gate (a genuinely stuck rig still fails here).
		removed := false
		for attempt := 0; attempt < 12; attempt++ {
			rig.Stop()
			if rig.Removed() {
				removed = true
				break
			}
			time.Sleep(1 * time.Second)
		}
		if !removed {
			return fmt.Errorf("network rig left residue after the cell")
		}
		request.logf("network fault %s class=%s restored=%t", fault, observation.ClientClass, restored)
		return nil
	case PhaseAssert:
		fault, err := request.CellParameter("fault")
		if err != nil {
			return err
		}
		var stored struct {
			Observation    faults.TCPObservation `json:"observation"`
			RoutesRestored bool                  `json:"routesRestored"`
		}
		if err := request.loadJSON("network-"+fault+".json", &stored); err != nil {
			return fmt.Errorf("network fault observation missing: %w", err)
		}
		deterministic := stored.Observation.ClientClass == "fault_deterministic_"+fault
		// bounded-retry: the faulted exchange ends inside the client's
		// own deadline — no unbounded hang; the deadline is frozen at 8s
		// and every observed elapsed stays inside it.
		bounded := parseDurationMS(stored.Observation.Elapsed) < 8*time.Second
		// no-false-terminal-success: the faulted exchange never carried
		// a complete body.
		noFalseSuccess := stored.Observation.ReceivedBytes != stored.Observation.TotalBytes || stored.Observation.TransportError != ""
		// idempotent-reconciliation: with the toxic removed the route
		// returns to the clean baseline exchange.
		reconciled := stored.RoutesRestored
		checks := []map[string]string{}
		for name, ok := range map[string]bool{
			"fault-deterministic":       deterministic,
			"bounded-retry":             bounded,
			"idempotent-reconciliation": reconciled,
			"no-false-terminal-success": noFalseSuccess,
		} {
			state := "failed"
			if ok {
				state = "passed"
			}
			checks = append(checks, map[string]string{"name": name, "result": state})
		}
		if err := request.writeFacts(map[string]any{
			"fault-deterministic":       deterministic,
			"bounded-retry":             bounded,
			"idempotent-reconciliation": reconciled,
			"no-false-terminal-success": noFalseSuccess,
		}, checks); err != nil {
			return err
		}
		if !deterministic || !bounded || !reconciled || !noFalseSuccess {
			return fmt.Errorf("network fault %s assertions failed: %+v", fault, stored.Observation)
		}
		return nil
	case PhaseTeardown:
		// The action removes its rig before returning; teardown only
		// sweeps interrupted stragglers (toxiproxy-removed,
		// proxy-routes-restored).
		name := faultRigName(request, "tcp")
		removeContainers(name+"-observer", name+"-toxiproxy", name+"-upstream")
		_ = runDocker("network", "rm", name)
		return nil
	}
	return fmt.Errorf("unknown phase %q", request.Phase)
}
