// Package main implements quoin-faultfs, the verification-only FUSE
// filesystem behind the frozen catalog's fault.storage scenario
// (VERIFY-FAULT-002). It is a loopback mount built directly on
// github.com/hanwen/go-fuse/v2@v2.9.0 whose only capability is
// path-scoped `write|fsync|rename -> errno` injection plus mount and
// unmount; it deliberately implements nothing else. Each injected rule
// matches every file whose path relative to the mount root carries the
// rule's prefix, so a qualification cell can scope ENOSPC/EDQUOT/EROFS
// or EIO to exactly the sqlite/artifact-staging/backup-output trees the
// catalog names without touching any other path.
package main

import (
	"context"
	"fmt"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// faultVocabulary is the closed errno vocabulary of the storage fault
// primitives. The Linux errno numbers are recorded next to the symbols
// because the frozen catalog's evidence asserts them on native Linux
// cells; off-Linux the symbols still resolve but mounting is refused.
var faultVocabulary = map[string]syscall.Errno{
	"ENOSPC": syscall.ENOSPC, // 28 on Linux
	"EDQUOT": syscall.EDQUOT, // 122 on Linux
	"EROFS":  syscall.EROFS,  // 30 on Linux
	"EIO":    syscall.EIO,    // 5 on Linux
}

// faultOperations is the closed operation vocabulary.
var faultOperations = map[string]bool{
	"write": true, "fsync": true, "rename": true,
}

// rule is one parsed `--fault op:ERRNO:prefix` declaration.
type rule struct {
	Operation string
	Errno     syscall.Errno
	ErrnoName string
	Prefix    string
}

// parseRule parses one `op:ERRNO:prefix` declaration. The prefix is a
// path relative to the mount root and matches every deeper path.
func parseRule(raw string) (rule, error) {
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) != 3 {
		return rule{}, fmt.Errorf("fault %q must be op:ERRNO:prefix", raw)
	}
	operation := strings.ToLower(parts[0])
	if !faultOperations[operation] {
		return rule{}, fmt.Errorf("fault operation %q outside the closed write|fsync|rename vocabulary", parts[0])
	}
	errno, known := faultVocabulary[strings.ToUpper(parts[1])]
	if !known {
		return rule{}, fmt.Errorf("fault errno %q outside the closed ENOSPC|EDQUOT|EROFS|EIO vocabulary", parts[1])
	}
	prefix := strings.Trim(path.Clean("/"+parts[2]), "/")
	return rule{Operation: operation, Errno: errno, ErrnoName: strings.ToUpper(parts[1]), Prefix: prefix}, nil
}

// matches reports whether a relative path falls under the rule prefix.
// The empty prefix (the mount root itself) matches everything.
func (r rule) matches(relative string) bool {
	if r.Prefix == "" {
		return true
	}
	return relative == r.Prefix || strings.HasPrefix(relative, r.Prefix+"/")
}

// errnoFor returns the injected errno for one operation on one relative
// path, or 0 when no rule applies. First matching rule wins; rules are
// evaluated in declaration order so the recorded argv is the authority.
func errnoFor(rules []rule, operation, relative string) syscall.Errno {
	for _, r := range rules {
		if r.Operation == operation && r.matches(relative) {
			return r.Errno
		}
	}
	return 0
}

// faultSystem couples the wrapped loopback nodes with the injected rules.
type faultSystem struct {
	rules []rule
}

// faultNode wraps one loopback node, overriding exactly the three
// fault-scoped operations. The go-fuse bridge dispatches Write and Fsync
// to the node first when it implements NodeWriter/NodeFsyncer, which
// covers handles produced by both Open and Create; every other operation
// stays the untouched loopback behavior.
type faultNode struct {
	*fs.LoopbackNode
	system *faultSystem
}

// WrapChild wraps every child the loopback creates so the whole tree, not
// just the root, carries the fault overrides.
func (n *faultNode) WrapChild(ctx context.Context, ops fs.InodeEmbedder) fs.InodeEmbedder {
	return &faultNode{LoopbackNode: ops.(*fs.LoopbackNode), system: n.system}
}

// relative returns this node's path relative to the mount root, which is
// what the rules scope against.
func (n *faultNode) relative() string {
	root := n.RootData.RootNode.EmbeddedInode()
	return n.EmbeddedInode().Path(root)
}

// writeScoped reports whether a write rule covers this node's path.
func (n *faultNode) writeScoped() bool {
	return errnoFor(n.system.rules, "write", n.relative()) != 0
}

// Write injects the scoped write errno; otherwise it forwards to the
// loopback file handle exactly as the bridge default would.
func (n *faultNode) Write(ctx context.Context, handle fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	if errno := errnoFor(n.system.rules, "write", n.relative()); errno != 0 {
		return 0, errno
	}
	if writer, ok := handle.(fs.FileWriter); ok {
		return writer.Write(ctx, data, off)
	}
	return 0, syscall.ENOSYS
}

// Fsync injects the scoped fsync errno; otherwise it forwards to the
// loopback file handle exactly as the bridge default would.
func (n *faultNode) Fsync(ctx context.Context, handle fs.FileHandle, flags uint32) syscall.Errno {
	if errno := errnoFor(n.system.rules, "fsync", n.relative()); errno != 0 {
		return errno
	}
	if fsyncer, ok := handle.(fs.FileFsyncer); ok {
		return fsyncer.Fsync(ctx, flags)
	}
	return syscall.ENOSYS
}

// Rename injects the rename errno when either the old or the new path is
// scoped by a rule; otherwise it delegates to the loopback rename.
func (n *faultNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	from := path.Join(n.relative(), name)
	root := n.RootData.RootNode.EmbeddedInode()
	to := path.Join(newParent.EmbeddedInode().Path(root), newName)
	if errno := errnoFor(n.system.rules, "rename", from); errno != 0 {
		return errno
	}
	if errno := errnoFor(n.system.rules, "rename", to); errno != 0 {
		return errno
	}
	return n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
}

// Open reuses the loopback open and only adds FOPEN_DIRECT_IO for
// write-scoped files: a buffered write would sit in the kernel page cache
// and reach the FUSE daemon only after close, so the injected errno would
// never be observable at write(2) time — the deterministic primitive the
// storage-fault cells require.
func (n *faultNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	handle, fuseFlags, errno := n.LoopbackNode.Open(ctx, flags)
	if errno == 0 && n.writeScoped() {
		fuseFlags |= fuse.FOPEN_DIRECT_IO
	}
	return handle, fuseFlags, errno
}

// Create mirrors Open for O_CREAT opens, which the loopback answers
// without calling Open. The scope check uses the created child's path
// (Create dispatches on the parent directory with the new name).
func (n *faultNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	inode, handle, fuseFlags, errno := n.LoopbackNode.Create(ctx, name, flags, mode, out)
	if errno == 0 && errnoFor(n.system.rules, "write", path.Join(n.relative(), name)) != 0 {
		fuseFlags |= fuse.FOPEN_DIRECT_IO
	}
	return inode, handle, fuseFlags, errno
}

// newFaultRoot builds the wrapped loopback root. The returned node is the
// mount root; every descendant created by the loopback is wrapped through
// WrapChild.
func newFaultRoot(backing string, rules []rule) (fs.InodeEmbedder, error) {
	loopbackRoot, err := fs.NewLoopbackRoot(backing)
	if err != nil {
		return nil, err
	}
	system := &faultSystem{rules: rules}
	return &faultNode{LoopbackNode: loopbackRoot.(*fs.LoopbackNode), system: system}, nil
}

// mountFaultfs mounts the wrapped loopback at target and returns the
// server. DirectMount prefers the mount(2) syscall over executing
// fusermount, which qualification containers (which carry /dev/fuse and
// CAP_SYS_ADMIN but often no fusermount binary) rely on.
func mountFaultfs(backing, target string, rules []rule) (*fuse.Server, error) {
	root, err := newFaultRoot(backing, rules)
	if err != nil {
		return nil, err
	}
	oneSecond := time.Second
	options := &fs.Options{
		AttrTimeout:  &oneSecond,
		EntryTimeout: &oneSecond,
		MountOptions: fuse.MountOptions{
			FsName:      "quoin-faultfs",
			Name:        "faultfs",
			DirectMount: true,
		},
	}
	return fs.Mount(target, root, options)
}
