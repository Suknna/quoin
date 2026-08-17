package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	composeprojection "github.com/Suknna/quoin/deploy/compose"
	"github.com/Suknna/quoin/internal/contract"
	"golang.org/x/term"
)

func main() {
	if len(os.Args) < 4 || os.Args[1] != "compose" || os.Args[2] != "install" {
		fail("usage: quoin-deploy compose install --config <path>")
	}
	flags := flag.NewFlagSet("compose install", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "strict compose-install YAML")
	if err := flags.Parse(os.Args[3:]); err != nil {
		os.Exit(2)
	}
	if *configPath == "" || flags.NArg() != 0 {
		fail("--config is required and positional arguments are not accepted")
	}
	var input contract.ComposeInstall
	if err := contract.DecodeFile(*configPath, &input); err != nil {
		failInput(err.Error())
	}
	stateDirectory, err := deploymentStateDirectory()
	if err != nil {
		fail(err.Error())
	}
	projection, err := composeprojection.Render(input, stateDirectory)
	if err != nil {
		failInput(err.Error())
	}
	// Read scripted bootstrap answers before any Docker child can inherit and
	// drain piped stdin; interactive runs keep passthrough stdin instead.
	var scripted *composeprojection.AdminAnswers
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		answers, inputErr := composeprojection.ReadAdminAnswers()
		if inputErr != nil {
			failInput(inputErr.Error())
		}
		scripted = &answers
	}
	composeArguments := []string{"compose", "--project-name", "quoin", "--file", projection.ComposeFile}
	fmt.Println("[1/4] Verifying Docker Compose and the generated projection")
	if err := run("docker", append(composeArguments, "version")...); err != nil {
		fail(fmt.Sprintf("Docker Compose is unavailable: %v", err))
	}
	if err := run("docker", append(composeArguments, "config", "--quiet")...); err != nil {
		fail(fmt.Sprintf("generated Compose projection is invalid: %v", err))
	}
	if running(projection.ComposeFile) {
		fmt.Println("[2/4] Existing Quoin is running; bootstrap state is already active")
		fmt.Println("[3/4] Preserving the existing administrator and persistent data")
	} else {
		fmt.Println("[2/4] Bootstrapping deployment secrets")
		// `compose run` is the only path that forwards the operator's stdin while
		// honouring the service's TTY allocation; `up` cannot attach stdin.
		if err := run("docker", append(composeArguments, "run", "--rm", "-T", "secret-bootstrap")...); err != nil {
			fail(fmt.Sprintf("secret bootstrap failed; persistent state was preserved: %v", err))
		}
		fmt.Println("[3/4] Creating or confirming the first administrator")
		if scripted == nil {
			// `compose run` is the only path that forwards the operator's stdin while
			// honouring the service's TTY allocation; `up` cannot attach stdin.
			if err := runInteractive("docker", append(composeArguments, "run", "--rm", "admin-bootstrap")...); err != nil {
				fail(fmt.Sprintf("administrator bootstrap failed; persistent state was preserved: %v", err))
			}
		} else {
			transcript, scriptErr := composeprojection.RunAdminBootstrapScripted(projection.ComposeFile, *scripted)
			fmt.Print(transcript)
			if scriptErr != nil {
				fail(fmt.Sprintf("administrator bootstrap failed; persistent state was preserved: %v", scriptErr))
			}
		}
	}
	fmt.Println("[4/4] Starting Quoin, Plinth, Lintel, and Stele")
	// No `--wait`: Compose re-creates exited one-shot dependencies while
	// waiting, which races the running Quoin for the data-directory flock.
	// The depends_on gates still enforce bootstrap ordering at start.
	if err := run("docker", append(composeArguments, "up", "--detach", "--remove-orphans", "quoin", "plinth", "lintel", "stele")...); err != nil {
		fail(fmt.Sprintf("long-lived components did not start: %v", err))
	}
	if err := awaitHealthy(composeArguments, 120*time.Second); err != nil {
		fail(fmt.Sprintf("long-lived components did not become healthy: %v", err))
	}
	fmt.Printf("Quoin is running. Public Origin: %s\n", input.PublicOrigin)
	fmt.Printf("Generated deployment projection: %s\n", projection.ComposeFile)
}

// awaitHealthy polls the health status of the four long-lived services so
// install only reports success once every component passes its own probe.
func awaitHealthy(composeArguments []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pending := []string{"quoin", "plinth", "lintel", "stele"}
	for time.Now().Before(deadline) {
		for index := 0; index < len(pending); index++ {
			command := exec.Command("docker", append(append([]string{}, composeArguments...), "ps", "--format", "{{.Service}} {{.Health}}", pending[index])...)
			output, err := command.Output()
			if err == nil && strings.HasPrefix(string(output), pending[index]+" healthy") {
				pending = append(pending[:index], pending[index+1:]...)
				index--
			}
		}
		if len(pending) == 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out waiting for %v", pending)
}

func deploymentStateDirectory() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "quoin", "compose"), nil
}

func running(composeFile string) bool {
	command := exec.Command("docker", "compose", "--project-name", "quoin", "--file", composeFile, "ps", "--status", "running", "--quiet", "quoin")
	output, err := command.Output()
	return err == nil && len(output) > 0
}

func run(name string, arguments ...string) error {
	command := exec.Command(name, arguments...)
	// Children never inherit quoin-deploy stdin: piped bootstrap answers must
	// not be drained by Docker clients. Interactive TTY runs use runInteractive.
	command.Stdin = nil
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func runInteractive(name string, arguments ...string) error {
	command := exec.Command(name, arguments...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, "quoin-deploy:", message)
	os.Exit(1)
}

// failInput reports invalid deployment input with exit code 2 so operators and
// acceptance tests can distinguish bad input from failed execution.
func failInput(message string) {
	fmt.Fprintln(os.Stderr, "quoin-deploy: invalid deployment input:", message)
	os.Exit(2)
}
