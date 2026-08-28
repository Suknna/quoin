package journey

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateExecutableJourneyAcceptsEmbeddedStatusMarker(t *testing.T) {
	if err := ValidateExecutableJourney(StatusMarkerID, 2); err != nil {
		t.Fatalf("validate executable Journey: %v", err)
	}
}

func TestValidateExecutableJourneyRejectsVersionMismatch(t *testing.T) {
	if err := ValidateExecutableJourney(StatusMarkerID, 3); err == nil {
		t.Fatalf("a Journey version without a fixed Playwright implementation must fail closed")
	}
}

func TestValidateExecutableJourneyRejectsUnknownJourney(t *testing.T) {
	if err := ValidateExecutableJourney("unknown.journey.v1", 1); err == nil {
		t.Fatalf("an unregistered Journey cannot execute")
	}
}

func TestRetryableProbeNavigationOnlyAcceptsPreCommitTimeout(t *testing.T) {
	if !retryableProbeNavigation(errors.New(`page.goto: Timeout 30000ms exceeded. Call log: waiting until "commit"`)) {
		t.Fatal("pre-commit CDP readiness timeout must be retryable")
	}
	for _, detail := range []string{
		`page.goto: Timeout 30000ms exceeded. Call log: waiting until "load"`,
		`locator.waitFor: Timeout 15000ms exceeded`,
		`connection refused`,
	} {
		if retryableProbeNavigation(errors.New(detail)) {
			t.Fatalf("non-pre-commit error must not retry: %q", detail)
		}
	}
}

func TestVerifyPlaywrightRunnerRejectsSubstitutedProgram(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replacement.mjs")
	if err := os.WriteFile(path, []byte("process.exit(0)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyPlaywrightRunner(path); err == nil {
		t.Fatal("a program whose bytes are not the catalog-bound Journey source must fail closed")
	}
}
