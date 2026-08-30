package release_test

import (
	"os"
	"strings"
	"testing"
)

// TestMain extends the test binary deadline for the T30 acceptance run: it
// builds digest-pinned multi-architecture images and drives five real
// installs, which exceeds the default ten-minute envelope of `go test`. The
// testing package parses -test.timeout from the command line inside m.Run,
// so the evidence run rewrites its own argv (both flag forms); the ticket's
// acceptance command stays unchanged.
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
	os.Args = append(filtered, "-test.timeout=50m")
	os.Exit(m.Run())
}
