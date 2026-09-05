package suites

import (
	"os"
	"os/exec"
	"time"
)

// helpers shared by the fault cell drivers.

// removeContainers force-removes owned containers idempotently.
func removeContainers(names ...string) {
	for _, name := range names {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	}
}

func runDocker(arguments ...string) error {
	return exec.Command("docker", arguments...).Run()
}

// parseDurationMS parses the rig's duration strings ("1.036s", "743ms").
func parseDurationMS(text string) time.Duration {
	duration, err := time.ParseDuration(text)
	if err != nil {
		return 0
	}
	return duration
}

// fileExists is a small guard for idempotent phase re-runs.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
