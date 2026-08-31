package helm

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	deploycompose "github.com/Suknna/quoin/deploy/compose"
	"github.com/Suknna/quoin/internal/deploy/report"
	"github.com/creack/pty"
)

// runAdminBootstrapInteractive attaches the operator's terminal to the admin
// pod so the four prompts are answered by a human.
func runAdminBootstrapInteractive(req Request, r *runner, stage int, namespace, release string) error {
	arguments := kubectl(namespace, "attach", "-it", "pod/"+release+"-admin-bootstrap", "--container", "admin")
	command := exec.Command(arguments[0], arguments[1:]...)
	command.Stdin = req.Stdin
	command.Stdout = req.Stdout
	command.Stderr = req.Stderr
	runErr := command.Run()
	r.report.RecordCommand(stage, report.Command{Argv: arguments, ExitCode: exitCode(runErr)})
	if runErr != nil {
		return fmt.Errorf("admin bootstrap attach exited: %w", runErr)
	}
	// kubectl attach's local success says only that its stream closed; it does
	// not propagate the remote container status. The pod terminal state is the
	// sole authority before Install may persist the completed stage.
	return judgeAdminOutcome(req, r, stage, namespace, release, "", "")
}

// runAdminBootstrapScripted drives the admin pod through a local
// pseudo-terminal running `kubectl attach -it` (the pod owns the remote TTY
// and kubectl requires a local TTY). The container runtime does not replay
// TTY output printed before the attach client connects, so the first
// (non-secret) username answer is sent blind; every later marker is only
// printed after our own previous answer and is therefore observable, which
// keeps password bytes traveling while the container-side ReadPassword has
// echo disabled — the same prompt-by-prompt contract as the Compose backend.
// A dedicated reader goroutine feeds the pty into a channel: pty masters are
// blocking file descriptors, so a select loop is the only deadline-safe
// structure.
func runAdminBootstrapScripted(req Request, r *runner, stage int, namespace, release string, answers deploycompose.AdminAnswers) error {
	if answers.Password != answers.Confirmation {
		return fmt.Errorf("the two typed temporary passwords do not match")
	}
	arguments := kubectl(namespace, "attach", "-it", "pod/"+release+"-admin-bootstrap", "--container", "admin")
	command := exec.Command(arguments[0], arguments[1:]...)
	master, err := pty.Start(command)
	if err != nil {
		return fmt.Errorf("allocate attach pseudo-terminal: %w", err)
	}
	defer master.Close()

	type chunk struct {
		data []byte
		err  error
	}
	stream := make(chan chunk, 32)
	go func() {
		buffer := make([]byte, 4096)
		for {
			read, readErr := master.Read(buffer)
			if read > 0 {
				message := make([]byte, read)
				copy(message, buffer[:read])
				stream <- chunk{data: message}
			}
			if readErr != nil {
				stream <- chunk{err: readErr}
				return
			}
		}
	}()
	result := make(chan error, 1)
	go func() { result <- command.Wait() }()

	session := &strings.Builder{}
	transcript := &strings.Builder{}
	// markers[0] ("Username: ") predates the attach stream; it is answered
	// blind because a username is not secret. Everything after it is
	// observable and answered strictly after its marker.
	markers := []string{"", "Display name: ", "Temporary password: ", "Confirm temporary password: "}
	steps := []func(deploycompose.AdminAnswers) string{
		func(a deploycompose.AdminAnswers) string { return a.Username },
		func(a deploycompose.AdminAnswers) string { return a.DisplayName },
		func(a deploycompose.AdminAnswers) string { return a.Password },
		func(a deploycompose.AdminAnswers) string { return a.Confirmation },
	}
	time.Sleep(1500 * time.Millisecond)
	fmt.Fprintln(transcript, "username sent over TTY (blind; non-secret)")
	if _, writeErr := io.WriteString(master, steps[0](answers)+"\r"); writeErr != nil {
		waitErr := <-result
		if outcomeErr := judgeAdminOutcome(req, r, stage, namespace, release, transcript.String(), session.String()); outcomeErr != nil {
			return fmt.Errorf("send username: %w (wait: %v; outcome: %w)", writeErr, waitErr, outcomeErr)
		}
		return nil
	}
	step := 1
	deadline := time.After(90 * time.Second)
	for {
		select {
		case <-deadline:
			_ = command.Process.Kill()
			<-result
			return fmt.Errorf("admin bootstrap timed out waiting for prompt %q; session so far:\n%s", markers[step], debugSession(session.String()))
		case <-result:
			// kubectl attach does not propagate the container exit status;
			// the pod's terminal state is the authority.
			return judgeAdminOutcome(req, r, stage, namespace, release, transcript.String(), session.String())
		case piece := <-stream:
			if piece.err != nil && len(piece.data) == 0 {
				// The pty closed; the result channel decides the outcome.
				continue
			}
			session.Write(piece.data)
			for step < len(markers) && strings.Contains(session.String(), markers[step]) {
				fmt.Fprintf(transcript, "prompt observed: %s\n", strings.TrimSpace(markers[step]))
				if _, writeErr := io.WriteString(master, steps[step](answers)+"\r"); writeErr != nil {
					waitErr := <-result
					return fmt.Errorf("send step %d: %w (wait: %v)", step, writeErr, waitErr)
				}
				fmt.Fprintf(transcript, "input accepted over TTY (step %d)\n", step)
				step++
			}
		}
	}
}

// judgeAdminOutcome waits for the admin pod to reach a terminal phase and
// judges the bootstrap by the CONTAINER exit status (kubectl attach exits 0
// regardless of the container outcome).
func judgeAdminOutcome(req Request, r *runner, stage int, namespace, release, transcript, session string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		output, err := r.run(stage, "admin-outcome", kubectl(namespace, "get", "pod", release+"-admin-bootstrap", "--output", "jsonpath={.status.phase} {.status.containerStatuses[0].state.terminated.exitCode}")...)
		if err == nil {
			fields := strings.Fields(strings.TrimSpace(output))
			if len(fields) == 2 && (fields[0] == "Succeeded" || fields[0] == "Failed") {
				if fields[0] == "Succeeded" && fields[1] == "0" {
					fmt.Fprint(req.Stdout, transcript)
					return nil
				}
				return fmt.Errorf("admin bootstrap container %s with exit code %s", fields[0], fields[1])
			}
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("admin pod did not reach a terminal phase in time; session:\n%s", debugSession(session))
}

// debugSession strips control characters for error paths; it can contain
// echoed non-secret input only because password echoes are disabled
// prompt-by-prompt.
func debugSession(session string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, session)
}

// adminAnswersFromStdin reads the four scripted answers from the helper's
// piped stdin (same frozen behaviour as the Compose backend).
func adminAnswersFromStdin(stdin io.Reader) (deploycompose.AdminAnswers, error) {
	reader := bufio.NewReader(stdin)
	lines := make([]string, 4)
	for index := range lines {
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return deploycompose.AdminAnswers{}, fmt.Errorf("scripted bootstrap needs four input lines (username, display name, password, confirmation): missing line %d", index+1)
		}
		lines[index] = strings.TrimRight(line, "\r\n")
	}
	answers := deploycompose.AdminAnswers{Username: lines[0], DisplayName: lines[1], Password: lines[2], Confirmation: lines[3]}
	if answers.Username == "" || answers.DisplayName == "" || answers.Password == "" {
		return deploycompose.AdminAnswers{}, fmt.Errorf("scripted bootstrap input must not be empty")
	}
	return answers, nil
}
