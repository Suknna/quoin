package fixtures

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProbeProviderFixtureAgainstRealBinary runs the real fixture
// process (go run ./test/fixtures/model-provider) with a completion
// delay and drives every black-box leg against it. This is the same
// pairing ci/verify-model-provider-fixture executes.
func TestProbeProviderFixtureAgainstRealBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("fixture process launch is skipped in short mode")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	fixture := exec.Command("go", "run", "./test/fixtures/model-provider", "-address", address, "-completion-delay", "3s")
	fixture.Dir = root
	var logs bytes.Buffer
	fixture.Stdout = &logs
	fixture.Stderr = &logs
	if err := fixture.Start(); err != nil {
		t.Fatalf("fixture could not start: %v", err)
	}
	t.Cleanup(func() {
		_ = fixture.Process.Kill()
		_, _ = fixture.Process.Wait()
	})

	// The fixture answers 401 without a bearer on /v1/models; that is
	// the readiness signal the poll can distinguish from connection
	// refused.
	base := "http://" + address
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(base + "/v1/models")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusUnauthorized {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := http.Get(base + "/v1/models"); err != nil {
		t.Fatalf("fixture did not become ready: %v\n%s", err, logs.String())
	}

	legs := ProbeProviderFixture(context.Background(), base)
	failed := []string{}
	for _, leg := range legs {
		if !leg.Passed {
			failed = append(failed, fmt.Sprintf("%s: %s", leg.Name, leg.Detail))
		}
	}
	if len(failed) != 0 {
		t.Fatalf("provider fixture legs failed:\n%s\nfixture logs:\n%s", strings.Join(failed, "\n"), logs.String())
	}
}
