package offline

import (
	"bytes"
	"path/filepath"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// timeEpoch is the fixed archive mtime: deterministic tar bytes.
var timeEpoch = time.Unix(0, 0)

// commandOutput runs one external command and returns its exact stdout;
// stderr is only surfaced inside the error for diagnostics. Digest
// readback hashes stdout alone so incidental stderr warnings can never
// perturb the measurement.
func commandOutput(name string, argv ...string) (string, error) {
	command := exec.Command(name, argv...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		code = -1
	}
	if err != nil {
		return stdout.String(), fmt.Errorf("%s exited %d: %s", name, code, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// PullLayout copies one digest-pinned multi-platform index from a
// registry into a local OCI layout with standard OCI tooling, preserving
// every digest (OPS-OFFLINE-002). The source reference must be
// repository@digest; loopback HTTP registries disable source TLS.
func PullLayout(runner Runner, reference, destDir string, insecure bool) error {
	// The oci: transport refuses destinations whose parent directory is
	// missing; create it before the copy.
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return err
	}
	argv := []string{"copy", "--all"}
	if insecure {
		argv = append(argv, "--src-tls-verify=false")
	}
	argv = append(argv, "docker://"+reference, "oci:"+destDir)
	if output, err := runner.Run("skopeo", argv...); err != nil {
		return fmt.Errorf("skopeo pull %s: %v: %s", reference, err, output)
	}
	return VerifyLayout(destDir, digestOfReference(reference))
}

// digestOfReference extracts the sha256:<hex> part of repository@digest.
func digestOfReference(reference string) string {
	for index := len(reference) - 1; index >= 0; index-- {
		if reference[index] == '@' {
			return reference[index+1:]
		}
	}
	return reference
}

// ImportLayout copies an extracted OCI layout into a target registry
// preserving digests, exactly as the offline install path does before the
// per-item digest readback (OPS-OFFLINE-002). The destination format is
// pinned to OCI so the index bytes travel unmodified.
func ImportLayout(runner Runner, layoutDir, targetReference string, insecure bool) error {
	argv := []string{"copy", "--all", "--preserve-digests", "--format", "oci"}
	if insecure {
		argv = append(argv, "--dest-tls-verify=false")
	}
	argv = append(argv, "oci:"+layoutDir, "docker://"+targetReference)
	if output, err := runner.Run("skopeo", argv...); err != nil {
		return fmt.Errorf("skopeo import %s: %v: %s", targetReference, err, output)
	}
	return nil
}

// ReadBackDigest fetches the raw manifest bytes of a digest-pinned
// reference and proves the registry serves exactly the pinned content.
func ReadBackDigest(runner Runner, reference string, insecure bool) (string, error) {
	argv := []string{"inspect", "--raw"}
	if insecure {
		argv = append(argv, "--tls-verify=false")
	}
	argv = append(argv, "docker://"+reference)
	raw, err := runner.Run("skopeo", argv...)
	if err != nil {
		return "", fmt.Errorf("skopeo inspect %s: %v", reference, err)
	}
	sum := sha256.Sum256([]byte(raw))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ReadBackIndex fetches the raw index bytes and returns the digest plus
// the per-platform manifest digests it lists.
func ReadBackIndex(runner Runner, reference string, insecure bool) (string, map[string]string, error) {
	digest, err := ReadBackDigest(runner, reference, insecure)
	if err != nil {
		return "", nil, err
	}
	platforms, err := platformDigests(runner, reference, insecure)
	if err != nil {
		return "", nil, err
	}
	return digest, platforms, nil
}

func platformDigests(runner Runner, reference string, insecure bool) (map[string]string, error) {
	argv := []string{"inspect", "--raw"}
	if insecure {
		argv = append(argv, "--tls-verify=false")
	}
	argv = append(argv, "docker://"+reference)
	raw, err := runner.Run("skopeo", argv...)
	if err != nil {
		return nil, err
	}
	return PlatformDigestsOf([]byte(raw))
}

// EnsureArtifacts is a helper for callers staging archive bytes on disk.
func EnsureArtifacts(dir string) error {
	return os.MkdirAll(dir, 0o755)
}
