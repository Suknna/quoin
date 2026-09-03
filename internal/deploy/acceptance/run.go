package acceptance

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	genericreport "github.com/Suknna/quoin/internal/deploy/report"
)

const (
	ExitSuccess  = 0
	ExitPlatform = 1
	ExitInput    = 2
)

// RunRequest supplies the backend-owned verifier as a narrow closure. Keeping
// the verifier behind this seam avoids a second Compose/Helm implementation:
// helper mode calls the ordinary read-only verifier exactly once.
type RunRequest struct {
	Backend             string
	ConfigPath          string
	ReleaseManifestPath string
	HelperRequestPath   string
	ReportPath          string
	Stdout              io.Writer
	Stderr              io.Writer
	Verify              func(genericReportPath string) int
}

// Run validates the offline request before the generic verifier is invoked,
// then projects its single operational result onto every verify-only item.
func Run(input RunRequest) int {
	request, requestBytes, err := ReadRequest(input.HelperRequestPath)
	if err != nil {
		return inputError(input, err)
	}
	if input.Backend != request.Backend {
		return inputError(input, fmt.Errorf("helper request backend %q does not match %q command", request.Backend, input.Backend))
	}
	reportPath := input.ReportPath
	if reportPath == "" {
		reportPath = filepath.Join(filepath.Dir(input.HelperRequestPath), "deployment-verification-report.yaml")
	}
	if input.ConfigPath == "" {
		return inputError(input, fmt.Errorf("helper mode requires --config"))
	}
	configDigest, err := deployconfig.DigestFile(input.ConfigPath)
	if err != nil {
		return writeUnexecutedReport(input, request, requestBytes, reportPath, "config_unreadable")
	}
	if configDigest != request.DeploymentConfigDigest {
		return writeUnexecutedReport(input, request, requestBytes, reportPath, "subject_drift")
	}
	if request.Architecture != runtime.GOOS+"/"+runtime.GOARCH {
		return inputError(input, fmt.Errorf("helper request architecture %q does not match this helper %s/%s", request.Architecture, runtime.GOOS, runtime.GOARCH))
	}
	if input.Verify == nil {
		return inputError(input, fmt.Errorf("no backend verifier configured"))
	}

	started := time.Now().UTC()
	verifyItems := hasVerifyOnly(request.Items)
	genericPath := reportPath + ".generic.json"
	verifyExit := ExitSuccess
	var generic genericreport.Report
	if verifyItems {
		verifyExit = input.Verify(genericPath)
		generic = readGenericReport(genericPath)
	}
	finished := time.Now().UTC()

	logBody := compactLog(request, verifyExit, generic)
	logPath := reportPath + ".log"
	if err := writeFileAtomically(logPath, logBody); err != nil {
		fmt.Fprintf(stderr(input), "quoin-deploy: write helper log: %v\n", err)
		return ExitInput
	}
	logHash := sha256Hex(logBody)
	attachment := Attachment{Kind: "log", SHA256: logHash, SizeBytes: len(logBody), MediaType: "text/plain"}
	argv := sanitizedArgv(input)
	report := reportFromRequest(request, requestBytes, started, finished, argv, attachment, verifyExit, generic)
	body, err := WriteReport(reportPath, report)
	if err != nil {
		fmt.Fprintf(stderr(input), "quoin-deploy: write helper report: %v\n", err)
		return ExitInput
	}
	// The ordinary helper envelope is intentionally one-way: it records the
	// exact standalone payload digest, while the importable payload only names
	// its own stdout/log attachments (OPS-HELPER-003).
	if verifyItems {
		if err := linkGenericReport(genericPath, sha256Hex(body)); err != nil {
			fmt.Fprintf(stderr(input), "quoin-deploy: link generic report: %v\n", err)
			return ExitInput
		}
	}
	fmt.Fprintf(stdout(input), "Deployment Acceptance helper report: %s\n", reportPath)

	if !verifyItems {
		return ExitSuccess
	}
	if verifyExit == ExitSuccess {
		return ExitSuccess
	}
	if isEnvironmentUnavailable(verifyExit, generic) {
		return ExitInput
	}
	return ExitPlatform
}

func hasVerifyOnly(items []RequestItem) bool {
	for _, item := range items {
		if item.ScenarioID == "deployment.verify-only" {
			return true
		}
	}
	return false
}

func reportFromRequest(request Request, requestBytes []byte, started, finished time.Time, argv []string, attachment Attachment, verifyExit int, generic genericreport.Report) Report {
	report := Report{
		SchemaVersion: 1, DocumentType: "helper_report", InvocationID: request.InvocationID,
		ManifestDigest: request.ManifestDigest, ItemSetDigest: request.ItemSetDigest,
		ReleaseSubjectDigest: request.ReleaseSubjectDigest, CatalogDigest: request.CatalogDigest,
		ResultProfileDigest: request.ResultProfileDigest, DeploymentConfigDigest: request.DeploymentConfigDigest,
		PublicOriginDigest: request.PublicOriginDigest, Backend: request.Backend, Architecture: request.Architecture,
		HelperRequestDigest: sha256Hex(requestBytes), StartedAt: started.Format(time.RFC3339Nano),
		FinishedAt: finished.Format(time.RFC3339Nano), Items: make([]ReportItem, 0, len(request.Items)),
	}
	assertions := assertionsFromGeneric(generic)
	for _, item := range request.Items {
		outcome, category, cleanup, itemExit := "warned", "not_run", "not_run", 0
		itemAssertions := []Assertion{}
		if item.ScenarioID == "deployment.verify-only" {
			outcome, category, cleanup, itemExit = verifyOutcome(verifyExit, generic)
			itemAssertions = assertions
		}
		// Result digests deliberately exclude wall-clock values and raw verifier
		// observations; report timestamps and check text may contain timestamps.
		// The digest binds only immutable item identity plus the verdict lattice.
		type assertionDigest struct{ ID, Result string }
		digestAssertions := make([]assertionDigest, 0, len(itemAssertions))
		for _, assertion := range itemAssertions {
			digestAssertions = append(digestAssertions, assertionDigest{assertion.ID, assertion.Result})
		}
		semantic := struct {
			ItemID, ScenarioID, CellID, InputDigest, Outcome, Category, Cleanup string
			ExitCode                                                            int
			Argv                                                                []string
			Assertions                                                          []assertionDigest
		}{item.ItemID, item.ScenarioID, item.CellID, item.InputDigest, outcome, category, cleanup, itemExit, argv, digestAssertions}
		encoded, _ := json.Marshal(semantic)
		report.Items = append(report.Items, ReportItem{
			ItemID: item.ItemID, ScenarioID: item.ScenarioID, CellID: item.CellID, InputDigest: item.InputDigest,
			ResultDigest: sha256Hex(encoded), Outcome: outcome, Category: category,
			StartedAt: started.Format(time.RFC3339Nano), FinishedAt: finished.Format(time.RFC3339Nano),
			ArgvSanitized: append([]string(nil), argv...), ExitCode: itemExit,
			Assertions: append([]Assertion(nil), itemAssertions...), Attachments: []Attachment{attachment}, CleanupOutcome: cleanup,
		})
	}
	return report
}

func verifyOutcome(exitCode int, generic genericreport.Report) (outcome, category, cleanup string, itemExit int) {
	switch {
	case exitCode == ExitSuccess:
		return "passed", "passed", "clean", 0
	case generic.Failure != nil && generic.Failure.Code == "subject_drift":
		return "warned", "subject_drift", "not_run", ExitInput
	case isEnvironmentUnavailable(exitCode, generic):
		return "warned", "environment_unavailable", "not_run", ExitInput
	default:
		return "failed", "functional_assertion_failed", "clean", ExitPlatform
	}
}

func isEnvironmentUnavailable(exitCode int, generic genericreport.Report) bool {
	if exitCode == ExitInput {
		return true
	}
	if generic.Failure == nil {
		return false
	}
	code := generic.Failure.Code
	return strings.Contains(code, "unavailable") || strings.Contains(code, "unreadable") ||
		strings.Contains(code, "preflight") || strings.Contains(code, "invalid_input")
}

func assertionsFromGeneric(generic genericreport.Report) []Assertion {
	assertions := make([]Assertion, 0, len(generic.Checks))
	for _, check := range generic.Checks {
		result := check.Result
		if result != "passed" {
			result = "failed"
		}
		assertions = append(assertions, Assertion{ID: check.ID, Expected: check.Expected, Actual: check.Actual, Result: result})
	}
	if len(assertions) == 0 && generic.Failure != nil {
		assertions = append(assertions, Assertion{ID: "verification", Expected: "operational surface available", Actual: generic.Failure.Code, Result: "failed"})
	}
	return assertions
}

func compactLog(request Request, exitCode int, generic genericreport.Report) []byte {
	lines := []string{"deployment acceptance helper", "backend=" + request.Backend, fmt.Sprintf("verifier_exit_code=%d", exitCode)}
	for _, check := range generic.Checks {
		lines = append(lines, fmt.Sprintf("check=%s result=%s code=%s", check.ID, check.Result, check.Code))
	}
	if generic.Failure != nil {
		lines = append(lines, "failure_code="+generic.Failure.Code)
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func sanitizedArgv(input RunRequest) []string {
	argv := []string{"quoin-deploy", input.Backend, "verify", "--helper-request", filepath.Clean(input.HelperRequestPath), "--config", filepath.Clean(input.ConfigPath)}
	if input.ReleaseManifestPath != "" {
		argv = append(argv, "--release-manifest", filepath.Clean(input.ReleaseManifestPath))
	}
	return argv
}

func readGenericReport(path string) genericreport.Report {
	body, err := os.ReadFile(path)
	if err != nil {
		return genericreport.Report{}
	}
	var report genericreport.Report
	_ = json.Unmarshal(body, &report)
	return report
}

func linkGenericReport(path, payloadDigest string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return err
	}
	document["deployment_acceptance_payload_sha256"] = payloadDigest
	updated, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomically(path, append(updated, '\n'))
}

func writeFileAtomically(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, body, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// writeUnexecutedReport preserves a valid request's evidence even when the
// local deployment config cannot be read or has drifted. No verifier (and no
// Docker/Kubernetes command) is launched on this path.
func writeUnexecutedReport(input RunRequest, request Request, requestBytes []byte, reportPath, code string) int {
	started := time.Now().UTC()
	generic := genericreport.Report{Failure: &genericreport.Failure{Code: code}}
	logBody := compactLog(request, ExitInput, generic)
	if err := writeFileAtomically(reportPath+".log", logBody); err != nil {
		fmt.Fprintf(stderr(input), "quoin-deploy: write helper log: %v\n", err)
		return ExitInput
	}
	attachment := Attachment{Kind: "log", SHA256: sha256Hex(logBody), SizeBytes: len(logBody), MediaType: "text/plain"}
	report := reportFromRequest(request, requestBytes, started, time.Now().UTC(), sanitizedArgv(input), attachment, ExitInput, generic)
	if _, err := WriteReport(reportPath, report); err != nil {
		fmt.Fprintf(stderr(input), "quoin-deploy: write helper report: %v\n", err)
		return ExitInput
	}
	fmt.Fprintf(stdout(input), "Deployment Acceptance helper report: %s\n", reportPath)
	return ExitInput
}

func inputError(input RunRequest, err error) int {
	fmt.Fprintf(stderr(input), "quoin-deploy: invalid deployment acceptance helper request: %v\n", err)
	return ExitInput
}

func stdout(input RunRequest) io.Writer {
	if input.Stdout != nil {
		return input.Stdout
	}
	return os.Stdout
}
func stderr(input RunRequest) io.Writer {
	if input.Stderr != nil {
		return input.Stderr
	}
	return os.Stderr
}
