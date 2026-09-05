// Command quoin-faultfs mounts and unmounts the verification-only
// storage-fault filesystem (VERIFY-FAULT-002). Mount serves in the
// foreground until unmounted; SIGTERM triggers a clean unmount so
// qualification containers can stop it without leaving the backing
// directory fenced. Witness performs one write/fsync/rename against a
// path and reports the observed errno as JSON: qualification drives it
// inside the privileged container to record the exact primitive facts.
//
// Usage:
//
//	quoin-faultfs mount --backing DIR --target DIR \
//	    [--fault write:ENOSPC:sqlite]... [--pidfile FILE]
//	quoin-faultfs unmount --target DIR
//	quoin-faultfs witness --op write|fsync|rename --path FILE --report FILE
//
// Exit codes: 0 success (mount ended after a clean unmount, the
// unmount command succeeded, or the witness observed and reported),
// 1 mount/unmount failure, 2 usage error.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const usage = "usage: quoin-faultfs <mount|unmount|witness> [--backing DIR] [--target DIR] [--fault op:ERRNO:prefix]... [--pidfile FILE] | [--op write|fsync|rename --path FILE --report FILE]"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "mount":
		mountCommand(os.Args[2:])
	case "unmount":
		unmountCommand(os.Args[2:])
	case "witness":
		witnessCommand(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}

func mountCommand(arguments []string) {
	flags := flag.NewFlagSet("mount", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	backing := flags.String("backing", "", "backing directory whose contents the mount mirrors")
	target := flags.String("target", "", "mount point (created when missing)")
	pidfile := flags.String("pidfile", "", "file receiving the server pid once mounted")
	var faults []string
	flags.Var(&faultFlagSet{values: &faults}, "fault", "path-scoped fault op:ERRNO:prefix (repeatable; op in write|fsync|rename, ERRNO in ENOSPC|EDQUOT|EROFS|EIO)")
	if err := flags.Parse(arguments); err != nil || *backing == "" || *target == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	rules := make([]rule, 0, len(faults))
	for _, raw := range faults {
		parsed, err := parseRule(raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "quoin-faultfs:", err)
			os.Exit(2)
		}
		rules = append(rules, parsed)
	}
	// Resolve both directories to absolute paths before mounting: the
	// FUSE server outlives the caller's working directory.
	absoluteBacking, err := filepath.Abs(*backing)
	if err != nil {
		fmt.Fprintln(os.Stderr, "quoin-faultfs:", err)
		os.Exit(2)
	}
	absoluteTarget, err := filepath.Abs(*target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "quoin-faultfs:", err)
		os.Exit(2)
	}
	for _, directory := range []string{absoluteBacking, absoluteTarget} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "quoin-faultfs:", err)
			os.Exit(1)
		}
	}
	server, err := mountFaultfs(absoluteBacking, absoluteTarget, rules)
	if err != nil {
		fmt.Fprintf(os.Stderr, "quoin-faultfs: mount %s failed: %v\n", absoluteTarget, err)
		os.Exit(1)
	}
	if *pidfile != "" {
		if err := os.WriteFile(*pidfile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "quoin-faultfs:", err)
			_ = server.Unmount()
			os.Exit(1)
		}
	}
	described := make([]string, 0, len(rules))
	for _, r := range rules {
		described = append(described, fmt.Sprintf("%s:%s:%s", r.Operation, r.ErrnoName, r.Prefix))
	}
	fmt.Printf("quoin-faultfs: mounted %s on %s faults=[%s]\n", absoluteBacking, absoluteTarget, strings.Join(described, " "))

	// A SIGTERM must produce a clean unmount: an abrupt kill would leave
	// the mount point fenced and the backing directory unreachable
	// through it, which the storage-fault teardown proof rejects.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		_ = server.Unmount()
	}()
	server.Wait()
	fmt.Println("quoin-faultfs: unmounted")
}

func unmountCommand(arguments []string) {
	flags := flag.NewFlagSet("unmount", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	target := flags.String("target", "", "mount point to release")
	if err := flags.Parse(arguments); err != nil || *target == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	// fuse.Server.Unmount only exists inside the serving process; an
	// external unmount goes straight to umount(2), which is also what
	// the go-fuse server does under the hood.
	if err := unix.Unmount(*target, 0); err != nil {
		fmt.Fprintf(os.Stderr, "quoin-faultfs: unmount %s failed: %v\n", *target, err)
		os.Exit(1)
	}
	fmt.Printf("quoin-faultfs: unmounted %s\n", *target)
}

// faultFlagSet lets the repeated --fault flag accumulate values.
type faultFlagSet struct{ values *[]string }

func (set *faultFlagSet) String() string {
	if set.values == nil {
		return ""
	}
	return strings.Join(*set.values, ",")
}

func (set *faultFlagSet) Set(value string) error {
	*set.values = append(*set.values, value)
	return nil
}

// witnessCommand performs exactly one fault-scoped operation and writes
// the machine observation as JSON. It never interprets the errno: the
// deterministic verifier on the host compares the recorded number
// against the catalog's expectation. Exit 0 means the observation was
// recorded (including failures — the errno IS the observation), not
// that the operation succeeded.
func witnessCommand(arguments []string) {
	flags := flag.NewFlagSet("witness", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	operation := flags.String("op", "", "observed operation: write|fsync|rename")
	path := flags.String("path", "", "target file of the observation")
	report := flags.String("report", "", "where the JSON observation is written (default stdout)")
	if err := flags.Parse(arguments); err != nil || *operation == "" || *path == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if !faultOperations[*operation] {
		fmt.Fprintf(os.Stderr, "quoin-faultfs: witness op %q outside write|fsync|rename\n", *operation)
		os.Exit(2)
	}
	observation := witness(*operation, *path)
	body, err := jsonMarshalIndent(observation)
	if err != nil {
		fmt.Fprintln(os.Stderr, "quoin-faultfs:", err)
		os.Exit(1)
	}
	if *report == "" {
		fmt.Println(string(body))
		return
	}
	if err := os.WriteFile(*report, append(body, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "quoin-faultfs:", err)
		os.Exit(1)
	}
	fmt.Printf("quoin-faultfs: witness %s errno=%d success=%t\n", *operation, observation.Errno, observation.Success)
}
