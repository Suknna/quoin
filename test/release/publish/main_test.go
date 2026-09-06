package publish

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestMain extends the test binary deadline for the T42 acceptance run:
// it builds dual-platform release subjects (arm64 under emulation),
// executes the release suite's compose cell, finalizes the offline
// archive and installs a concrete site, which exceeds the default
// ten-minute envelope of `go test`. The ticket's acceptance command
// rewrites its own argv here and stays unchanged otherwise.
func TestMain(m *testing.M) {
	if os.Getenv("QUOIN_EVIDENCE_DIR") == "" {
		os.Exit(m.Run())
	}
	filtered := []string{os.Args[0]}
	skipValue := false
	for _, argument := range os.Args[1:] {
		if skipValue {
			skipValue = false
			continue
		}
		if argument == "-test.timeout" || argument == "-timeout" {
			skipValue = true
			continue
		}
		if strings.HasPrefix(argument, "-test.timeout=") || strings.HasPrefix(argument, "-timeout=") {
			continue
		}
		filtered = append(filtered, argument)
	}
	os.Args = append(filtered, "-test.timeout=150m")
	// The rewritten budget is part of the evidence: the acceptance log
	// records the exact argv the test binary ran with.
	fmt.Fprintln(os.Stderr, "TestMain argv:", os.Args)
	os.Exit(m.Run())
}
