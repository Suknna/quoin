package faults

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
)

// osGetenv isolates environment reads for tests.
func osGetenv(key string) string { return os.Getenv(key) }

// jsonUnmarshal isolates the report decoding so classification stays
// free of transport concerns.
func jsonUnmarshal(body []byte, target any) error {
	return json.Unmarshal(body, target)
}

// runCommand executes one command with an environment override map.
func runCommandEnvironment(environment map[string]string, name string, arguments ...string) error {
	command := exec.Command(name, arguments...)
	command.Env = os.Environ()
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	var combined bytes.Buffer
	command.Stdout = &combined
	command.Stderr = &combined
	if err := command.Run(); err != nil {
		return err
	}
	return nil
}
