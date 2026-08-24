package compose

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/creack/pty"
)

// AdminAnswers carries the four attached-TTY first-admin bootstrap answers.
// Values only ever travel through the pseudo-terminal; they never enter
// argv, environment, compose values, or logs.
type AdminAnswers struct {
	Username     string
	DisplayName  string
	Password     string
	Confirmation string
}

type promptStep struct {
	marker string
	answer func(answers AdminAnswers) string
	label  string
}

var adminPrompts = []promptStep{
	{"Username: ", func(a AdminAnswers) string { return a.Username }, "username"},
	{"Display name: ", func(a AdminAnswers) string { return a.DisplayName }, "display name"},
	{"Temporary password: ", func(a AdminAnswers) string { return a.Password }, "temporary password"},
	{"Confirm temporary password: ", func(a AdminAnswers) string { return a.Confirmation }, "confirmation"},
}

// RunAdminBootstrapScripted drives the admin-bootstrap one-shot Compose service
// under a pseudo-terminal and answers each prompt only after it is observed.
// Answering prompt-by-prompt guarantees password bytes are written while the
// in-container ReadPassword has echo disabled, so the session transcript can
// never contain secret input. The returned transcript is sanitized: prompts,
// which input was accepted, and the JSON result lines only.
func RunAdminBootstrapScripted(composeFile string, answers AdminAnswers) (string, error) {
	if answers.Password != answers.Confirmation {
		return "", fmt.Errorf("the two typed temporary passwords do not match")
	}
	composeProject := os.Getenv("QUOIN_COMPOSE_PROJECT")
	if composeProject == "" {
		composeProject = "quoin"
	}
	command := exec.Command("docker", "compose", "--project-name", composeProject, "--file", composeFile, "run", "--rm", "admin-bootstrap")
	command.Env = append(os.Environ(), "DOCKER_CLI_HINTS=false")
	master, err := pty.Start(command)
	if err != nil {
		return "", fmt.Errorf("allocate bootstrap pseudo-terminal: %w", err)
	}
	defer master.Close()

	transcript := &strings.Builder{}
	result := make(chan error, 1)
	go func() { result <- command.Wait() }()

	step := 0
	deadline := time.After(60 * time.Second)
	buffer := make([]byte, 4096)
	session := &strings.Builder{}
	for {
		select {
		case <-deadline:
			command.Process.Kill()
			<-result
			return sanitizeTranscript(session.String()), fmt.Errorf("admin bootstrap timed out waiting for prompt %q; session so far:\n%s", adminPrompts[step].marker, debugSession(session.String()))
		default:
		}
		read, readErr := master.Read(buffer)
		if read > 0 {
			session.Write(buffer[:read])
		}
		for step < len(adminPrompts) && strings.Contains(session.String(), adminPrompts[step].marker) {
			fmt.Fprintf(transcript, "prompt observed: %s\n", strings.TrimSpace(adminPrompts[step].marker))
			if _, writeErr := io.WriteString(master, adminPrompts[step].answer(answers)+"\n"); writeErr != nil {
				waitErr := <-result
				return sanitizeTranscript(session.String()), fmt.Errorf("send %s: %w (wait: %v)", adminPrompts[step].label, writeErr, waitErr)
			}
			fmt.Fprintf(transcript, "input accepted over TTY (%s)\n", adminPrompts[step].label)
			step++
		}
		if readErr != nil {
			break
		}
	}
	waitErr := <-result
	final := sanitizeTranscript(session.String())
	for _, line := range strings.Split(transcript.String(), "\n") {
		if line != "" {
			final += line + "\n"
		}
	}
	if code := command.ProcessState.ExitCode(); code != 0 {
		return final, fmt.Errorf("admin bootstrap exited with code %d (wait: %v)", code, waitErr)
	}
	return final, nil
}

// sanitizeTranscript keeps only machine result lines from the raw session so
// echoed non-secret input can never be mistaken for a log contract.
// debugSession returns the raw session with control chars stripped, for the
// timeout error path only (never contains secret input).
func debugSession(session string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, session)
}

func sanitizeTranscript(session string) string {
	filtered := &strings.Builder{}
	scanner := bufio.NewScanner(strings.NewReader(session))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "\"code\"") {
			filtered.WriteString(line)
			filtered.WriteByte('\n')
		}
	}
	return filtered.String()
}

// ReadAdminAnswers reads the four scripted bootstrap answers from the piped
// stdin of quoin-deploy. It refuses to run without all four lines so a partial
// feed can never leave the bootstrap container waiting.
func ReadAdminAnswers() (AdminAnswers, error) {
	reader := bufio.NewReader(os.Stdin)
	lines := make([]string, 4)
	for index := range lines {
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return AdminAnswers{}, fmt.Errorf("scripted bootstrap needs four input lines (username, display name, password, confirmation): missing line %d", index+1)
		}
		lines[index] = strings.TrimRight(line, "\r\n")
	}
	answers := AdminAnswers{Username: lines[0], DisplayName: lines[1], Password: lines[2], Confirmation: lines[3]}
	if answers.Username == "" || answers.DisplayName == "" || answers.Password == "" {
		return AdminAnswers{}, fmt.Errorf("scripted bootstrap input must not be empty")
	}
	return answers, nil
}
