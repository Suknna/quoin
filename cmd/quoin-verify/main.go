// Command quoin-verify is the deterministic verification runner. It loads
// the frozen catalog, validates it, executes one layer's applicable
// scenarios cell by cell (setup/action/assert/teardown, no fail-fast) and
// emits the schema-validated in-toto Test Result plus the digest-referenced
// evidence index. Verdicts are computed by this program alone.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/verification/catalog"
	"github.com/Suknna/quoin/internal/verification/runner"
)

// Exit codes are part of the runner contract: CI consumes them directly.
const (
	exitUsage    = 1
	exitFailed   = 3
	exitWarned   = 4
	exitInternal = 2
)

const defaultCatalog = "docs/specs/quoin-v1/contracts/verification-catalog.yaml"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "quoin-verify:", err)
		os.Exit(exitUsage)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: quoin-verify <validate|run> [flags]")
	}
	switch args[0] {
	case "validate":
		return validateCommand(args[1:])
	case "run":
		return runCommand(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q (want validate or run)", args[0])
	}
}

func validateCommand(args []string) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	catalogPath := flags.String("catalog", "", "verification catalog path (default <repo>/"+defaultCatalog+")")
	if err := flags.Parse(args); err != nil {
		return err
	}
	path, err := resolveCatalog(*catalogPath)
	if err != nil {
		return err
	}
	if _, err := catalog.LoadAndValidate(path); err != nil {
		return err
	}
	fmt.Printf("catalog valid: %s\n", path)
	return nil
}

func runCommand(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	catalogPath := flags.String("catalog", "", "verification catalog path (default <repo>/"+defaultCatalog+")")
	layer := flags.String("layer", catalog.LayerContractGate, "verification layer to execute")
	output := flags.String("output", "", "invocation output directory (required)")
	repoRoot := flags.String("repo-root", "", "working directory for catalog phase commands (default repository root)")
	invocationID := flags.String("invocation-id", "", "stable invocation id (default generated)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *output == "" {
		return fmt.Errorf("--output is required")
	}
	if *layer != catalog.LayerContractGate && *layer != catalog.LayerReleaseQualification && *layer != catalog.LayerDeploymentAcceptance {
		return fmt.Errorf("unknown layer %q", *layer)
	}
	root, err := resolveRepoRoot(*repoRoot)
	if err != nil {
		return err
	}
	path, err := resolveCatalog(*catalogPath)
	if err != nil {
		return err
	}
	contractsDir := filepath.Dir(path)
	if *layer != catalog.LayerContractGate {
		return fmt.Errorf("layer %q execution is owned by its closure ticket; this build runs the contract gate", *layer)
	}
	subject, err := resolveSubject(root)
	if err != nil {
		return err
	}
	if *invocationID == "" {
		*invocationID = fmt.Sprintf("%s-%d", *layer, time.Now().UTC().Unix())
	}
	report, err := runner.Run(runner.Options{
		CatalogPath:  path,
		ProfilePath:  filepath.Join(contractsDir, "verification-result-profile.yaml"),
		ContractsDir: contractsDir,
		OutputDir:    *output,
		RepoRoot:     root,
		Layer:        *layer,
		InvocationID: *invocationID,
		Subject:      subject,
		ToolVersion:  "quoin-verify/dev (" + goVersion(root) + ")",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "quoin-verify:", err)
		os.Exit(exitInternal)
	}
	for _, item := range report.Index.Items {
		fmt.Printf("%-6s %-40s %s (%s)\n", strings.ToUpper(item.Outcome), item.TestName(), item.Category, report.Predicate.Quoin.InvocationID)
	}
	fmt.Printf("suite %s: evidence %s statement %s\n",
		report.Verdict,
		filepath.Join(*output, "evidence.json"),
		filepath.Join(*output, "test-result.json"))
	switch report.Verdict {
	case "PASSED":
		return nil
	case "WARNED":
		os.Exit(exitWarned)
	default:
		os.Exit(exitFailed)
	}
	return nil
}

// resolveSubject binds the invocation to the immutable source revision: the
// git commit plus the dirty-state digest. A clean tree and a dirty tree can
// never share a subject digest.
func resolveSubject(root string) (runner.SubjectBinding, error) {
	commit, err := gitOutput(root, "rev-parse", "HEAD")
	if err != nil {
		return runner.SubjectBinding{}, fmt.Errorf("subject resolution needs git HEAD: %w", err)
	}
	status, err := gitOutput(root, "status", "--porcelain")
	if err != nil {
		return runner.SubjectBinding{}, err
	}
	sum := sha256.Sum256([]byte(commit + "\n" + status))
	return runner.SubjectBinding{
		Name:   "quoin-source:" + commit[:12],
		Digest: hex.EncodeToString(sum[:]),
	}, nil
}

func gitOutput(root string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	body, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func goVersion(root string) string {
	cmd := exec.Command("go", "env", "GOVERSION")
	cmd.Dir = root
	if body, err := cmd.Output(); err == nil {
		return strings.TrimSpace(string(body))
	}
	return "go unknown"
}

func resolveCatalog(path string) (string, error) {
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		return path, nil
	}
	root, err := resolveRepoRoot("")
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(root, defaultCatalog)
	if _, err := os.Stat(resolved); err != nil {
		return "", fmt.Errorf("catalog: %w", err)
	}
	return resolved, nil
}

func resolveRepoRoot(explicit string) (string, error) {
	if explicit != "" {
		absolute, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		return absolute, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("repository root not found")
		}
		dir = parent
	}
}
