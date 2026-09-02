package helm

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/deploy/report"
)

// runner executes external commands while recording them in the verification
// report (OPS-HELPER-003: every path produces structured evidence).
type runner struct {
	report *report.Report
	stdout func() chan string
}

func newRunner(rep *report.Report) *runner {
	return &runner{report: rep}
}

// run executes one external command, records it under the stage and returns
// the combined output. The caller judges the error; the output is evidence.
func (r *runner) run(stage int, name string, arguments ...string) (string, error) {
	if len(arguments) == 0 {
		return "", fmt.Errorf("empty command %q", name)
	}
	started := time.Now()
	command := exec.Command(arguments[0], arguments[1:]...)
	output, runErr := command.CombinedOutput()
	duration := time.Since(started).Round(time.Millisecond)
	entry := report.Command{Argv: reportArguments(arguments), ExitCode: exitCode(runErr), Duration: duration.String()}
	r.report.RecordCommand(stage, entry)
	return string(output), runErr
}

// runInput executes one external command with the given stdin, recording it
// under the stage.
func (r *runner) runInteractive(stage int, name string, stdin io.Reader, stdout, stderr io.Writer, arguments ...string) (string, error) {
	if len(arguments) == 0 {
		return "", fmt.Errorf("empty command %q", name)
	}
	started := time.Now()
	command := exec.Command(arguments[0], arguments[1:]...)
	command.Stdin = stdin
	// kubectl attach detects terminal support from its own stdout file descriptor.
	// A MultiWriter turns that descriptor into a pipe, silently preventing the
	// remote TTY from forwarding restore prompts. Preserve the attached terminal
	// instead of capturing an interactive credential session in helper memory.
	command.Stdout = stdout
	command.Stderr = stderr
	runErr := command.Run()
	r.report.RecordCommand(stage, report.Command{Argv: reportArguments(arguments), ExitCode: exitCode(runErr), Duration: time.Since(started).Round(time.Millisecond).String()})
	return "", runErr
}

func (r *runner) runInput(stage int, name string, stdin string, arguments ...string) (string, error) {
	if len(arguments) == 0 {
		return "", fmt.Errorf("empty command %q", name)
	}
	started := time.Now()
	command := exec.Command(arguments[0], arguments[1:]...)
	command.Stdin = strings.NewReader(stdin)
	output, runErr := command.CombinedOutput()
	duration := time.Since(started).Round(time.Millisecond)
	r.report.RecordCommand(stage, report.Command{Argv: reportArguments(arguments), ExitCode: exitCode(runErr), Duration: duration.String()})
	return string(output), runErr
}

// kubectl assembles one kubectl invocation against the deployment namespace.
func kubectl(namespace string, arguments ...string) []string {
	return append([]string{"kubectl", "--namespace", namespace}, arguments...)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func buildPlatform() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

func defaultReportPath(stateDir string) string {
	return filepath.Join(stateDir, "verification-report.json")
}

// stateDirectory ensures the helper state root exists.
func stateDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create helper state directory: %w", err)
	}
	return nil
}

// reportArguments removes secret-bearing file identities from durable reports.
// Command execution still receives the original arguments; only evidence is
// redacted. Secret bytes are never passed in argv by this backend.
func reportArguments(arguments []string) []string {
	redacted := append([]string(nil), arguments...)
	for index, argument := range redacted {
		for _, key := range secretFileKeys {
			if strings.Contains(argument, key) {
				redacted[index] = "[REDACTED-SECRET-PATH]"
				break
			}
		}
	}
	return redacted
}

// componentSelector is the Chart's stable identity selector for one release workload.
func componentSelector(release, component string) string {
	return "app.kubernetes.io/name=quoin,app.kubernetes.io/instance=" + release + ",app.kubernetes.io/component=" + component
}
