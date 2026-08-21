package worker

// Fail-closed worker sandbox (ARCH-WORKER-007/008): no_new_privs,
// dumpable=0, Landlock ABI >= 6 with a deny-by-default file rule set
// (read-only runtime paths + the one attempt workspace; the network
// ruleset allows no port, so every TCP bind/connect fails), and a seccomp
// deny-list for cross-process, namespace, mount and module syscalls plus
// signaling the supervisor. Readiness and every StartAttemptAck run the
// adversarial self-checks: any failure rejects the attempt as
// sandbox_unavailable instead of silently downgrading.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// landlockABIRequired is the frozen minimum (ARCH-WORKER-007).
const landlockABIRequired = 6

// Sandbox describes the isolation applied to this worker process.
type Sandbox struct {
	WorkspaceDir   string
	SupervisorPID  int
	ReadOnlyPaths  []string
	ExecutablePath string
}

// Establish applies the fail-closed sandbox before any attempt input is
// processed (ARCH-WORKER-007). It returns an error the worker maps to a
// refused StartAttemptAck (sandbox_unavailable).
func Establish(sandbox Sandbox) error {
	// 1. no_new_privs + dumpable=0 first: nothing later may gain
	// privileges, and the /proc/mem/environ access gates close.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("set_dumpable: %w", err)
	}
	// 2. Landlock ABI check and deny-by-default ruleset.
	abi := landlockABI()
	if abi < landlockABIRequired {
		return fmt.Errorf("landlock abi %d < %d", abi, landlockABIRequired)
	}
	if err := applyLandlock(sandbox); err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	// 3. seccomp deny-list (network already denied by Landlock; the filter
	// blocks cross-process and namespace syscalls plus signaling the
	// supervisor).
	if err := applySeccomp(sandbox.SupervisorPID); err != nil {
		return fmt.Errorf("seccomp: %w", err)
	}
	// 4. Adversarial self-check (ARCH-WORKER-008): every forbidden path
	// must fail, every allowed path must succeed.
	return SelfCheck(sandbox)
}

// landlockABI probes the running kernel's Landlock ABI. The VERSION flag
// makes landlock_create_ruleset return the supported ABI VERSION NUMBER as
// its return value (not a file descriptor): closing it would corrupt an
// arbitrary fd of the calling process (this once closed the Go runtime's
// netpoll eventfd and killed the worker with "netpollBreak write failed").
func landlockABI() int {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno == 0 {
		return int(abi)
	}
	return 0
}

// applyLandlock installs the deny-by-default ruleset: read-only access to
// the frozen runtime paths and read/write access to the attempt workspace;
// no other path and no network port is allowed.
func applyLandlock(sandbox Sandbox) error {
	handledFS := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
		unix.LANDLOCK_ACCESS_FS_REFER |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE |
		unix.LANDLOCK_ACCESS_FS_IOCTL_DEV)
	handledNet := uint64(unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP)
	rulesetFd, err := landlockCreateRuleset(handledFS, handledNet)
	if err != nil {
		return err
	}
	defer unix.Close(rulesetFd)
	// Read-only runtime paths (the frozen catalog plus proc basics).
	// Landlock rules anchor on directories: directory entries anchor
	// themselves, file entries anchor on their parent directory. /dev
	// carries write rights so the fixed tool set can redirect to
	// /dev/null; /proc and /etc stay read-only (cross-process /proc/
	// environ/mem reads stay closed by dumpable=0 + the self-check).
	anchors := map[string]uint64{}
	readOnly := append([]string{}, sandbox.ReadOnlyPaths...)
	readOnly = append(readOnly, "/proc/self", "/proc", "/etc", "/dev/null", "/dev/urandom")
	if sandbox.ExecutablePath != "" {
		readOnly = append(readOnly, sandbox.ExecutablePath)
	}
	for _, entry := range readOnly {
		info, err := os.Stat(entry)
		if err != nil {
			continue
		}
		anchor := entry
		if !info.IsDir() {
			anchor = filepath.Dir(entry)
		}
		rights := readOnlyFSRights
		if anchor == "/dev" {
			rights = readOnlyFSRights | unix.LANDLOCK_ACCESS_FS_WRITE_FILE
		}
		if _, exists := anchors[anchor]; !exists {
			anchors[anchor] = rights
		}
	}
	for anchor, rights := range anchors {
		if err := landlockAddPath(rulesetFd, anchor, rights); err != nil {
			return err
		}
	}
	// The attempt workspace is the only writable tree.
	if err := landlockAddPath(rulesetFd, sandbox.WorkspaceDir, writeFSRights); err != nil {
		return err
	}
	// No network rule: with the network rights handled, every TCP
	// bind/connect is denied (ARCH-WORKER-007).
	return landlockRestrictSelf(rulesetFd)
}

// readOnlyFSRights allows executing and reading the frozen runtime tree.
var readOnlyFSRights = uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR)

// writeFSRights allows full manipulation inside the workspace.
var writeFSRights = uint64(
	unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_SYM | unix.LANDLOCK_ACCESS_FS_REFER |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE)

func landlockCreateRuleset(handledFS, handledNet uint64) (int, error) {
	attr := unix.LandlockRulesetAttr{
		Access_fs:  handledFS,
		Access_net: handledNet,
	}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return 0, errno
	}
	return int(fd), nil
}

func landlockAddPath(rulesetFd int, path string, rights uint64) error {
	body, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s for landlock: %w", path, err)
	}
	defer body.Close()
	attr := unix.LandlockPathBeneathAttr{
		Allowed_access: rights,
		Parent_fd:      int32(body.Fd()),
	}
	_, _, errno := unix.Syscall(unix.SYS_LANDLOCK_ADD_RULE, uintptr(rulesetFd), unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&attr)))
	if errno != 0 {
		return fmt.Errorf("add landlock rule for %s: %w", path, errno)
	}
	return nil
}

func landlockRestrictSelf(rulesetFd int) error {
	_, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(rulesetFd), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// ---- seccomp deny-list (classic BPF over seccomp_data) -----------------

const (
	bpfLdW            = 0x00 | 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJeq            = 0x05 | 0x10 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJset           = 0x05 | 0x40 // BPF_JMP | BPF_JSET | BPF_K
	bpfRet            = 0x06 | 0x00 // BPF_RET | BPF_K
	bpfK              = 0x00
	seccompRetAllow   = 0x7fff0000
	seccompRetKill    = 0x80000000
	seccompModeFilter = 2
)

// applySeccomp installs the deny-list filter (default allow): namespace,
// mount, module and cross-process syscalls are refused; kill-family calls
// targeting the supervisor are refused while ordinary process-group and
// child signaling keeps working (ARCH-WORKER-007).
func applySeccomp(supervisorPID int) error {
	if supervisorPID <= 0 {
		return errors.New("supervisor pid unknown")
	}
	var statements []unix.SockFilter
	loadNr := unix.SockFilter{Code: bpfLdW | bpfK, K: 0}
	loadArch := unix.SockFilter{Code: bpfLdW | bpfK, K: 4}
	loadArg0 := unix.SockFilter{Code: bpfLdW | bpfK, K: 16}                          // low 32 bits of arg0
	loadArg1 := unix.SockFilter{Code: bpfLdW | bpfK, K: 24}                          // low 32 bits of arg1
	deny := unix.SockFilter{Code: bpfRet | bpfK, K: 0x00050000 | uint32(unix.EPERM)} // SECCOMP_RET_ERRNO | EPERM
	allow := unix.SockFilter{Code: bpfRet | bpfK, K: seccompRetAllow}
	kill := unix.SockFilter{Code: bpfRet | bpfK, K: seccompRetKill}
	// Arch gate: only the compiled arch may proceed (kill otherwise).
	statements = append(statements, loadArch)
	statements = append(statements, unix.SockFilter{Code: bpfJeq | bpfK, Jt: 1, Jf: 0, K: nativeAuditArch()})
	statements = append(statements, kill)
	// Signal syscalls: deny only when the target is the supervisor. Each
	// non-matching syscall must skip the WHOLE group (load + pid test +
	// deny = 3 statements).
	for _, syscall := range []uint32{uint32(unix.SYS_KILL), uint32(unix.SYS_TKILL), uint32(unix.SYS_TGKILL)} {
		statements = append(statements, loadNr)
		statements = append(statements, unix.SockFilter{Code: bpfJeq | bpfK, Jt: 0, Jf: 3, K: syscall})
		statements = append(statements, loadArg0)
		statements = append(statements, unix.SockFilter{Code: bpfJeq | bpfK, Jt: 0, Jf: 1, K: uint32(supervisorPID)})
		statements = append(statements, deny)
	}
	// rt_sigqueueinfo / rt_tgsigqueueinfo take the target in arg1.
	for _, syscall := range []uint32{uint32(unix.SYS_RT_SIGQUEUEINFO), uint32(unix.SYS_RT_TGSIGQUEUEINFO)} {
		statements = append(statements, loadNr)
		statements = append(statements, unix.SockFilter{Code: bpfJeq | bpfK, Jt: 0, Jf: 3, K: syscall})
		statements = append(statements, loadArg1)
		statements = append(statements, unix.SockFilter{Code: bpfJeq | bpfK, Jt: 0, Jf: 1, K: uint32(supervisorPID)})
		statements = append(statements, deny)
	}
	// clone: namespace creation flags are refused.
	const namespaceFlags = uint32(unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWPID |
		unix.CLONE_NEWNET | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWCGROUP)
	statements = append(statements, loadNr)
	statements = append(statements, unix.SockFilter{Code: bpfJeq | bpfK, Jt: 0, Jf: 3, K: uint32(unix.SYS_CLONE)})
	statements = append(statements, loadArg0)
	statements = append(statements, unix.SockFilter{Code: bpfJset | bpfK, Jt: 0, Jf: 1, K: namespaceFlags})
	statements = append(statements, deny)
	// clone3, unshare, setns, mount family, keyrings, modules, bpf,
	// perf and cross-process syscalls are refused wholesale.
	deniedAlways := []uint32{
		uint32(unix.SYS_CLONE3), uint32(unix.SYS_UNSHARE), uint32(unix.SYS_SETNS),
		uint32(unix.SYS_MOUNT), uint32(unix.SYS_UMOUNT2), uint32(unix.SYS_PIVOT_ROOT),
		uint32(unix.SYS_CHROOT), uint32(unix.SYS_OPEN_TREE), uint32(unix.SYS_MOVE_MOUNT),
		uint32(unix.SYS_FSOPEN), uint32(unix.SYS_FSCONFIG), uint32(unix.SYS_FSMOUNT),
		uint32(unix.SYS_FSPICK), uint32(unix.SYS_MOUNT_SETATTR),
		uint32(unix.SYS_PTRACE), uint32(unix.SYS_PROCESS_VM_READV), uint32(unix.SYS_PROCESS_VM_WRITEV),
		uint32(unix.SYS_KCMP), uint32(unix.SYS_PIDFD_OPEN), uint32(unix.SYS_PIDFD_GETFD),
		uint32(unix.SYS_PIDFD_SEND_SIGNAL),
		uint32(unix.SYS_KEYCTL), uint32(unix.SYS_ADD_KEY), uint32(unix.SYS_REQUEST_KEY),
		uint32(unix.SYS_BPF), uint32(unix.SYS_USERFAULTFD), uint32(unix.SYS_PERF_EVENT_OPEN),
		uint32(unix.SYS_INIT_MODULE), uint32(unix.SYS_FINIT_MODULE), uint32(unix.SYS_DELETE_MODULE),
		uint32(unix.SYS_ACCT), uint32(unix.SYS_REBOOT), uint32(unix.SYS_KEXEC_LOAD),
		uint32(unix.SYS_KEXEC_FILE_LOAD), uint32(unix.SYS_SWAPON), uint32(unix.SYS_SWAPOFF),
	}
	for _, syscall := range deniedAlways {
		statements = append(statements, loadNr)
		statements = append(statements, unix.SockFilter{Code: bpfJeq | bpfK, Jt: 0, Jf: 1, K: syscall})
		statements = append(statements, deny)
	}
	statements = append(statements, allow)
	program := &unix.SockFprog{Len: uint16(len(statements)), Filter: &statements[0]}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, seccompModeFilter, uintptr(unsafe.Pointer(program)), 0, 0); err != nil {
		return fmt.Errorf("prctl seccomp: %w", err)
	}
	return nil
}

// nativeAuditArch returns the seccomp arch token of the compiled GOARCH.
func nativeAuditArch() uint32 {
	switch runtime.GOARCH {
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64
	default:
		return unix.AUDIT_ARCH_X86_64
	}
}

// SelfCheck runs the adversarial isolation verification (ARCH-WORKER-008).
func SelfCheck(sandbox Sandbox) error {
	// Reading the supervisor's sensitive /proc files must fail. The
	// dumpable/Yama gate plus the dropped capabilities of the deployment
	// close environ/mem/maps; the fd directory requires listing to
	// revalidate its entries, and a concrete namespace file exercises the
	// same gate for ns (the ns directory itself is world-listable).
	for _, name := range []string{"environ", "mem", "maps"} {
		target := filepath.Join("/proc", fmt.Sprint(sandbox.SupervisorPID), name)
		file, err := os.Open(target)
		if err == nil {
			file.Close()
			return fmt.Errorf("self-check: supervisor /proc/%s readable", name)
		}
	}
	// The fd directory listing only exposes fd numbers (metadata); the
	// content gate is opening a concrete fd, which must fail without the
	// deployment's dropped capabilities.
	fdLink := filepath.Join("/proc", fmt.Sprint(sandbox.SupervisorPID), "fd", "0")
	if file, err := os.Open(fdLink); err == nil {
		file.Close()
		return errors.New("self-check: supervisor fd target readable")
	}
	nsLink := filepath.Join("/proc", fmt.Sprint(sandbox.SupervisorPID), "ns", "mnt")
	if file, err := os.Open(nsLink); err == nil {
		file.Close()
		return errors.New("self-check: supervisor namespace readable")
	}
	// Signaling the supervisor must fail.
	if err := syscall.Kill(sandbox.SupervisorPID, 0); err == nil {
		return errors.New("self-check: supervisor signalable")
	} else if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("self-check: unexpected kill result: %w", err)
	}
	// External network must fail.
	connection, err := net.DialTimeout("tcp", "127.0.0.1:9", 2*time.Second)
	if err == nil {
		connection.Close()
		return errors.New("self-check: outbound network connection succeeded")
	}
	// Writing outside the workspace must fail.
	outside := filepath.Join(filepath.Dir(sandbox.WorkspaceDir), "sandbox-probe-"+fmt.Sprint(os.Getpid()))
	if file, err := os.Create(outside); err == nil {
		file.Close()
		os.Remove(outside)
		return errors.New("self-check: write outside workspace succeeded")
	}
	// Only stdio fds may be inherited: every fd above 2 must be a runtime-
	// internal descriptor (pipe/eventpoll/eventfd/socket) or the Go
	// runtime's own cgroup probe (GOMAXPROCS detection opens cpu.max at
	// startup and keeps it). A regular file or directory fd means the
	// supervisor leaked one through the spawn (ARCH-WORKER-002).
	entries, err := os.ReadDir("/proc/self/fd")
	if err == nil {
		for _, entry := range entries {
			number, parseErr := strconv.Atoi(entry.Name())
			if parseErr != nil || number <= 2 {
				continue
			}
			target, linkErr := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
			if linkErr != nil {
				continue
			}
			if !strings.HasPrefix(target, "pipe:") && !strings.HasPrefix(target, "anon_inode:") && !strings.HasPrefix(target, "socket:") && !strings.HasPrefix(target, "/sys/fs/cgroup/") {
				return fmt.Errorf("self-check: inherited fd %d points to %s", number, target)
			}
		}
	}
	// Workspace read/write must succeed.
	probe := filepath.Join(sandbox.WorkspaceDir, ".sandbox-probe")
	if err := os.WriteFile(probe, []byte("sandbox-ok"), 0o600); err != nil {
		return fmt.Errorf("self-check: workspace write failed: %w", err)
	}
	body, err := os.ReadFile(probe)
	if err != nil || string(body) != "sandbox-ok" {
		return errors.New("self-check: workspace read failed")
	}
	os.Remove(probe)
	// The frozen executables must exist.
	if _, err := os.Stat("/bin/bash"); err != nil {
		return fmt.Errorf("self-check: /bin/bash unavailable: %w", err)
	}
	_ = strings.TrimSpace
	return nil
}
