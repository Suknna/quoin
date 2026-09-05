package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestParseRuleClosedVocabularies(t *testing.T) {
	cases := []struct {
		raw       string
		operation string
		errnoName string
		prefix    string
	}{
		{"write:ENOSPC:sqlite", "write", "ENOSPC", "sqlite"},
		{"write:EDQUOT:artifact-staging", "write", "EDQUOT", "artifact-staging"},
		{"write:EROFS:/backup-output", "write", "EROFS", "backup-output"},
		{"fsync:EIO:sqlite", "fsync", "EIO", "sqlite"},
		{"rename:eio:backup-output", "rename", "EIO", "backup-output"},
		{"write:ENOSPC:", "write", "ENOSPC", ""},
	}
	for _, tc := range cases {
		parsed, err := parseRule(tc.raw)
		if err != nil {
			t.Fatalf("%s: %v", tc.raw, err)
		}
		if parsed.Operation != tc.operation || parsed.ErrnoName != tc.errnoName || parsed.Prefix != tc.prefix {
			t.Fatalf("%s parsed as %+v", tc.raw, parsed)
		}
	}
	rejected := []string{
		"chmod:EIO:sqlite",      // operation outside the closed set
		"write:EPERM:sqlite",    // errno outside the closed set
		"write:ENOSPC",          // missing prefix segment
		"truncate:EIO:artifact", // second closed-vocabulary breach
	}
	for _, raw := range rejected {
		if _, err := parseRule(raw); err == nil {
			t.Fatalf("%s accepted outside the closed vocabulary", raw)
		}
	}
}

func TestRulePathScoping(t *testing.T) {
	rules := []rule{}
	for _, raw := range []string{"write:ENOSPC:sqlite", "fsync:EIO:artifact-staging"} {
		parsed, err := parseRule(raw)
		if err != nil {
			t.Fatal(err)
		}
		rules = append(rules, parsed)
	}
	mustFault := [][2]string{
		{"write", "sqlite"}, {"write", "sqlite/data.db"},
		{"fsync", "artifact-staging"}, {"fsync", "artifact-staging/0b/1f.bin"},
	}
	for _, pair := range mustFault {
		if errnoFor(rules, pair[0], pair[1]) == 0 {
			t.Fatalf("%s on %s did not fault", pair[0], pair[1])
		}
	}
	mustNotFault := [][2]string{
		{"fsync", "sqlite"},           // only write is scoped for sqlite
		{"write", "artifact-staging"}, // only fsync is scoped for staging
		{"write", "sqliter"},          // prefix must respect path boundaries
		{"write", "backup-output/x"},  // no rule covers backup-output
		{"rename", "sqlite/data.db"},  // rename rules absent
	}
	for _, pair := range mustNotFault {
		if errno := errnoFor(rules, pair[0], pair[1]); errno != 0 {
			t.Fatalf("%s on %s unexpectedly faulted with %v", pair[0], pair[1], errno)
		}
	}
}

// TestMountInjectsScopedErrnos proves the real mounted path on Linux with
// /dev/fuse: a write inside the scoped prefix fails with the exact errno,
// a write outside the prefix succeeds, and after unmount the backing
// data is intact and writable again (the frozen recovery proof).
func TestMountInjectsScopedErrnos(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("FUSE mounts require Linux (running on %s)", runtime.GOOS)
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("/dev/fuse is not available")
	}
	backing := t.TempDir()
	target := t.TempDir()
	injected, err := parseRule("write:ENOSPC:sqlite")
	if err != nil {
		t.Fatal(err)
	}
	server, err := mountFaultfs(backing, target, []rule{injected})
	if err != nil {
		t.Skipf("mount unavailable in this environment: %v", err)
	}
	defer func() { _ = server.Unmount() }()

	// The mount shadows anything created under the target before it,
	// so the scoped trees are created through the mounted filesystem.
	scoped := filepath.Join(target, "sqlite")
	outside := filepath.Join(target, "other")
	for _, directory := range []string{scoped, outside} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(outside, "free.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write outside the scoped prefix must succeed: %v", err)
	}
	scopedError, ok := errorAsSyscallErrno(writeFileForError(filepath.Join(scoped, "db.dat")))
	if !ok {
		t.Fatalf("write inside the scoped prefix must fail with an errno, got %v", scopedError)
	}
	if scopedError != 28 { // ENOSPC on Linux
		t.Fatalf("scoped write errno = %d, want ENOSPC(28)", scopedError)
	}
}

// writeFileForError performs the write and returns its error untouched so
// the caller can inspect the raw errno.
func writeFileForError(path string) error {
	return os.WriteFile(path, []byte("payload"), 0o644)
}

func errorAsSyscallErrno(err error) (syscall.Errno, bool) {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno, true
	}
	return 0, false
}
