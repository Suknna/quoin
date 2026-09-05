package ui

// TestMain widens the Go test timeout for the T41 acceptance run: the
// first action phase boots the real Compose qualification site (image
// builds included) before the browser matrix executes, which is far
// beyond the default 10m budget. The rewritten argv is echoed to stderr
// so the evidence logs record the actual timeout contract.

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("QUOIN_EVIDENCE_DIR") != "" {
		kept := []string{}
		for _, arg := range os.Args {
			if strings.HasPrefix(arg, "-test.timeout=") || strings.HasPrefix(arg, "-timeout=") {
				continue
			}
			kept = append(kept, arg)
		}
		kept = append(kept, "-test.timeout=170m")
		fmt.Fprintf(os.Stderr, "t41 acceptance argv rewritten: %v\n", kept)
		os.Args = kept
	}
	os.Exit(m.Run())
}
