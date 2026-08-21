package worker

// Sandbox acceptance (ARCH-WORKER-007/008): the real Landlock + seccomp
// establishment runs in a subprocess (Landlock restrictions are
// process-scoped and irreversible, so the test process must stay clean)
// and the adversarial self-check must pass there. This is the same path
// every agent worker runs before StartAttemptAck.

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestSandboxSubprocess drives the helper below.
func TestSandboxSubprocess(t *testing.T) {
	if os.Getenv("QUOIN_SANDBOX_HELPER") == "1" {
		sandboxHelper(t)
		return
	}
	workspace := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	paths, err := ReadOnlyRuntimePaths()
	if err != nil {
		t.Fatal(err)
	}
	// Landlock rules only anchor on paths that exist in this runtime
	// (the image is the authority for the tool catalog).
	existing := existingPaths(t, paths)
	// The Go runtime marks every supervisor-created fd CLOEXEC, so the
	// production worker inherits exactly 0/1/2. Test harnesses (this
	// one included) may leak a non-CLOEXEC fd into the helper; mark
	// strays CLOEXEC in the parent (closing would break the parent's
	// runtime) so the helper starts with a production-faithful fd table.
	markStrayFDsCloexec()
	cmd := exec.Command(executable, "-test.run", "TestSandboxSubprocess", "--", "--workspace", workspace, "--executable", executable, "--paths", strings.Join(existing, "\n"))
	cmd.Env = append(os.Environ(), "QUOIN_SANDBOX_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox helper failed: %v\n%s", err, out)
	}
}

func sandboxHelper(t *testing.T) {
	args := flag.NewFlagSet("sandbox", flag.ExitOnError)
	workspace := args.String("workspace", "", "attempt workspace")
	executable := args.String("executable", "", "worker executable path")
	paths := args.String("paths", "", "read-only runtime paths, newline separated")
	_ = args.Parse(flag.Args())
	var readOnly []string
	for _, path := range strings.Split(*paths, "\n") {
		if path != "" {
			readOnly = append(readOnly, path)
		}
	}
	sandbox := Sandbox{
		WorkspaceDir: *workspace, SupervisorPID: os.Getppid(),
		ReadOnlyPaths: readOnly, ExecutablePath: *executable,
	}
	if err := Establish(sandbox); err != nil {
		t.Fatalf("Establish: %v", err)
	}
	// Positive controls beyond SelfCheck: the workspace tools work and the
	// runtime paths stay readable.
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Fatalf("/bin/bash unreadable: %v", err)
	}
	t.Logf("sandbox established and self-checked (landlock + seccomp + no_new_privs)")
}

// markStrayFDsCloexec sets FD_CLOEXEC on every non-cloexec fd above
// stderr so the helper subprocess inherits only stdio, mirroring the
// production worker's fd table without closing the parent's runtime fds.
func markStrayFDsCloexec() {
	for fd := 3; fd < 256; fd++ {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno != 0 {
			continue
		}
		if flags&syscall.FD_CLOEXEC == 0 {
			syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, flags|syscall.FD_CLOEXEC)
		}
	}
}

func existingPaths(t *testing.T, paths []string) []string {
	var existing []string
	for _, path := range paths {
		resolved := path
		if target, err := filepath.EvalSymlinks(path); err == nil {
			resolved = target
		}
		if _, err := os.Stat(resolved); err == nil {
			existing = append(existing, path)
		}
	}
	return existing
}
