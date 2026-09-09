package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// selectMode is the CI boundary for changed-file image selection. Repeating
// -changed keeps workflow quoting unambiguous; -changed-file accepts a
// newline-separated git diff list. Its JSON is intentionally stable so a
// workflow can consume .components without interpreting policy itself.
func selectMode(arguments []string) error {
	var changed []string
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "-changed":
			index++
			if index >= len(arguments) {
				return fmt.Errorf("-changed needs a path")
			}
			changed = append(changed, arguments[index])
		case "-changed-file":
			index++
			if index >= len(arguments) {
				return fmt.Errorf("-changed-file needs a path")
			}
			body, err := os.ReadFile(arguments[index])
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(body), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					changed = append(changed, line)
				}
			}
		default:
			return fmt.Errorf("unknown select argument %q", arguments[index])
		}
	}
	components := selectBuildSubjects(changed)
	// JSON null is not iterable in a GitHub Actions matrix. An empty array
	// explicitly represents a change that affects no application image.
	if components == nil {
		components = []string{}
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Components []string `json:"components"`
	}{Components: components})
}
