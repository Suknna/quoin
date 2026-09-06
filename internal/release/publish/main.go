// Command publish is the release-closure driver behind ci/finalize-release
// (T42). The finalize stage derives the final Release manifest from the
// validated subject inventory, the frozen contracts and the categorized
// signed qualification evidence, stages the release directory and packs the
// offline archive; the verify stage is the publish gate: schema validation,
// inventory equality, the exactly-one-bundle-per-signed-asset closure, the
// one-way evidence bindings, the full digest DAG with no self-reference,
// and the real offline archive import with digest readback
// (OPS-RELEASE-001, OPS-SUPPLY-002, OPS-OFFLINE-001/002, OPS-VERIFY-003).
//
// It never signs and never holds keys: signing is keyless cosign in CI and
// runs between finalize and verify.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "finalize-release: %v\n", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return fmt.Errorf("usage: publish <finalize|verify> [flags]")
	}
	switch arguments[0] {
	case "finalize":
		return finalizeMode(arguments[1:])
	case "verify":
		return verifyMode(arguments[1:])
	default:
		return fmt.Errorf("unknown stage %q (want finalize|verify)", arguments[0])
	}
}

// flagParser is the shared flat -flag parser used by the sibling release
// builder. Boolean flags never consume a following argument, so they may
// appear anywhere — including last — on the command line.
type flagParser struct {
	arguments []string
	index     int
	booleans  map[string]bool
}

func (parser *flagParser) next() (string, string, bool, error) {
	if parser.index >= len(parser.arguments) {
		return "", "", false, nil
	}
	flag := parser.arguments[parser.index]
	parser.index++
	if flag == "" || flag[0] != '-' {
		return "", "", false, fmt.Errorf("unexpected argument %q", flag)
	}
	if parser.booleans[flag] {
		return flag, "", false, nil
	}
	value := ""
	hasValue := false
	if parser.index < len(parser.arguments) {
		value = parser.arguments[parser.index]
		parser.index++
		hasValue = true
	}
	return flag, value, hasValue, nil
}

func (parser *flagParser) boolFlag(name string) bool {
	for _, argument := range parser.arguments {
		if argument == name {
			return true
		}
	}
	return false
}

// writeReport persists one structured stage report.
func writeReport(path string, document map[string]any) error {
	document["generatedAt"] = time.Now().UTC().Format(time.RFC3339)
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

// repoRoot walks up from the working directory to the module root.
func repoRoot() string {
	directory, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "."
		}
		directory = parent
	}
}
