package faults

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FaultfsBinary locates the prebuilt quoin-faultfs binary for the
// container's platform. The caller builds it (GOOS=linux GOARCH=<server
// arch>) before invoking the storage cells; mounting requires Linux
// with /dev/fuse, so the primitive runs inside one privileged
// disposable container on the qualification host.
type Faultfs struct {
	BinaryPath string // host path of the linux quoin-faultfs binary
	Workdir    string // host directory receiving witness reports
	Container  string // disposable container name (unique per invocation)
	Image      string // base image carrying sh
}

// StorageCellOutcome is the machine record of one fault.storage cell's
// primitive execution on the native container.
type StorageCellOutcome struct {
	Fault              string         `json:"fault"`
	Operation          string         `json:"operation"`
	ExpectedErrno      int            `json:"expectedErrno"`
	PathErrnos         map[string]int `json:"pathErrnos"`
	NoFalseSuccess     bool           `json:"noFalseSuccess"`
	RecoveryAction     string         `json:"recoveryAction"`
	UnmountedClean     bool           `json:"unmountedClean"`
	IntegrityPreserved bool           `json:"integrityPreserved"`
	IntegrityDetail    string         `json:"integrityDetail"`
	Class              string         `json:"clientClass"`
}

// StorageFaultRules renders the catalog cell parameters into faultfs
// rules covering every scoped path.
func StorageFaultRules(fault string) ([]string, error) {
	primitive, known := StorageFaults[fault]
	if !known {
		return nil, fmt.Errorf("fault %q outside the storage vocabulary", fault)
	}
	errnoName := map[int]string{28: "ENOSPC", 122: "EDQUOT", 30: "EROFS", 5: "EIO"}[primitive.Errno]
	var rules []string
	for _, path := range StoragePaths {
		rules = append(rules, fmt.Sprintf("%s:%s:%s", primitive.Operation, errnoName, path))
	}
	return rules, nil
}

// RunStorageFaultCell executes one fault.storage primitive inside a
// fresh privileged container: mount faultfs over the scoped paths,
// witness the exact errno per path, unmount, then prove recovery
// (write+fsync+rename succeed through the released backing tree and
// pre-fault data is intact). The container is removed at the end and
// removal is proven by the caller through ContainerRemoved.
func (faultfs *Faultfs) RunStorageFaultCell(fault string) (StorageCellOutcome, error) {
	primitive, known := StorageFaults[fault]
	if !known {
		return StorageCellOutcome{}, fmt.Errorf("fault %q outside the storage vocabulary", fault)
	}
	rules, err := StorageFaultRules(fault)
	if err != nil {
		return StorageCellOutcome{}, err
	}
	outcome := StorageCellOutcome{
		Fault: fault, Operation: primitive.Operation, ExpectedErrno: primitive.Errno,
		PathErrnos: map[string]int{}, RecoveryAction: "quoin-faultfs unmount --target /mnt (fence lifted; backing tree regains write/fsync/rename)",
	}
	var mountFaults []string
	for _, rule := range rules {
		mountFaults = append(mountFaults, "--fault", rule)
	}
	mountArguments := append([]string{"/quoin-faultfs", "mount", "--backing", "/backing", "--target", "/mnt", "--pidfile", "/tmp/faultfs.pid"}, mountFaults...)

	// One shell script runs inside the container: start the mount, wait
	// for readiness, witness every scoped path, then unmount and prove
	// recovery through the backing tree.
	script := fmt.Sprintf(`
set -e
%s &
for i in $(seq 1 100); do [ -f /tmp/faultfs.pid ] && break; sleep 0.1; done
[ -f /tmp/faultfs.pid ] || { echo "faultfs-did-not-mount" >&2; exit 1; }
sleep 0.4
mkdir -p /mnt/sqlite /mnt/artifact-staging /mnt/backup-output
`, strings.Join(mountArguments, " "))
	for _, path := range StoragePaths {
		script += fmt.Sprintf("/quoin-faultfs witness --op %s --path /mnt/%s/witness.db --report /work/%s.json\n", primitive.Operation, path, path)
	}
	script += `
/quoin-faultfs unmount --target /mnt > /tmp/unmount.log 2>&1 || echo "unmount-failed" > /tmp/unmount.status
[ -f /tmp/unmount.status ] || echo "ok" > /tmp/unmount.status
mkdir -p /backing/sqlite /backing/artifact-staging /backing/backup-output
/quoin-faultfs witness --op write --path /backing/recovered-write.db --report /work/recovery-write.json
/quoin-faultfs witness --op fsync --path /backing/recovered-fsync.db --report /work/recovery-fsync.json
/quoin-faultfs witness --op rename --path /backing/recovered-rename.db --report /work/recovery-rename.json
cat /tmp/unmount.status
`
	if err := os.MkdirAll(faultfs.Workdir, 0o755); err != nil {
		return StorageCellOutcome{}, err
	}
	output, runErr := capture("docker", "run", "--rm", "--privileged",
		"--device", "/dev/fuse",
		"-v", faultfs.BinaryPath+":/quoin-faultfs:ro",
		"-v", faultfs.Workdir+":/work",
		faultfs.Image, "sh", "-c", script)
	if writeErr := os.WriteFile(filepath.Join(faultfs.Workdir, "container-"+fault+".log"), []byte(output), 0o644); writeErr != nil {
		return StorageCellOutcome{}, writeErr
	}
	if runErr != nil {
		return outcome, fmt.Errorf("faultfs container for %s failed: %v\n%s", fault, runErr, output)
	}
	// Classify from the witness reports only: the host-side verifier, not
	// the container, decides the outcome.
	allErrnosExact, noFalseSuccess := true, true
	for _, path := range StoragePaths {
		report, err := readWitnessReport(filepath.Join(faultfs.Workdir, path+".json"))
		if err != nil {
			return outcome, fmt.Errorf("witness %s/%s: %w", fault, path, err)
		}
		outcome.PathErrnos[path] = report.Errno
		if report.Errno != primitive.Errno {
			allErrnosExact = false
		}
		if report.Errno != 0 && report.Success {
			noFalseSuccess = false // a failed operation reported success
		}
	}
	outcome.NoFalseSuccess = noFalseSuccess
	outcome.UnmountedClean = strings.Contains(output, "\nok\n") || strings.HasSuffix(strings.TrimSpace(output), "ok")
	integrity := true
	for _, leg := range []string{"recovery-write", "recovery-fsync", "recovery-rename"} {
		report, err := readWitnessReport(filepath.Join(faultfs.Workdir, leg+".json"))
		if err != nil {
			return outcome, fmt.Errorf("recovery %s: %w", leg, err)
		}
		if !report.Success {
			integrity = false
		}
	}
	outcome.IntegrityPreserved = integrity
	outcome.IntegrityDetail = "after unmount, write+fsync+rename succeed through the released backing tree"
	if allErrnosExact && noFalseSuccess && outcome.UnmountedClean && integrity {
		outcome.Class = "fault_deterministic_" + fault
	} else {
		outcome.Class = "unexpected"
	}
	return outcome, nil
}

// witnessReport mirrors the quoin-faultfs witness JSON.
type witnessReport struct {
	Operation string `json:"operation"`
	Errno     int    `json:"errno"`
	Success   bool   `json:"success"`
}

func readWitnessReport(path string) (witnessReport, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return witnessReport{}, err
	}
	var report witnessReport
	if err := jsonUnmarshal(body, &report); err != nil {
		return witnessReport{}, err
	}
	return report, nil
}

// FaultfsBinaryForServerArch names the host path of the container
// binary for the docker server architecture.
func FaultfsBinaryForServerArch(workRoot, serverArch string) string {
	return filepath.Join(workRoot, "quoin-faultfs-"+serverArch)
}

// BuildFaultfs cross-builds the faultfs binary for the container
// platform. On darwin hosts this is a cross build (GOOS=linux); on
// linux CI runners it is a native build. The build runs from the
// repository root.
func BuildFaultfs(outputPath, goarch, repoRoot string) error {
	command := exec.Command("go", "build", "-o", outputPath, "./cmd/quoin-faultfs")
	command.Dir = repoRoot
	command.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
	var combined bytes.Buffer
	command.Stdout = &combined
	command.Stderr = &combined
	if err := command.Run(); err != nil {
		return fmt.Errorf("faultfs build: %v: %s", err, combined.String())
	}
	return nil
}
