package journey

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CatalogRef is the frozen catalog binding of one Journey reference.
type CatalogRef struct {
	Digest  string `json:"digest"`
	Version string `json:"version"`
}

// Binding is the frozen Journey reference of one operation input.
type Binding struct {
	ID      string          `json:"id"`
	Version int64           `json:"version"`
	Params  json.RawMessage `json:"params"`
	Catalog CatalogRef      `json:"catalog"`
}

// Deps are the Lintel capabilities one Journey execution needs. The channel
// supplies them from the Browser Manager; the runner owns no global state.
type Deps struct {
	// PageEndpoint resolves the running operation's loopback DevTools base URL.
	PageEndpoint func(ctx context.Context) (string, error)
	// ProbeAuthenticated executes the identity's frozen authentication probe
	// journey against the operation's Chromium and reports the typed result.
	ProbeAuthenticated func(ctx context.Context, prefix string) (bool, error)
}

// Runner executes exactly one versioned Playwright Journey for one
// attempt/operation. The JavaScript runner is the sole executable Journey
// source; Go only owns fenced orchestration and result classification.
type Runner struct {
	Deps        Deps
	Trace       *Trace
	StartURL    string
	Journey     Binding
	Probe       Binding
	AttemptID   int64
	OperationID int64
}

// Outcome is the closed execution result of the runner. The channel converts
// it into the frozen browser_journey_result_v1 payload; classification stays
// with this fixed contract, never with model judgment.
type Outcome struct {
	Success        bool
	GapCode        string
	TerminalReason string
	OriginalGap    string
	ErrorDetail    string
	Output         map[string]any
	Probes         []ProbeObservation
	// TraceIntegrity is complete when the runner finished its own control flow
	// (including a business failure) and incomplete on cancellation/crash.
	TraceIntegrity string
}

type playwrightResponse struct {
	Output map[string]any `json:"output"`
	Trace  []struct {
		Kind   string `json:"kind"`
		Path   string `json:"path,omitempty"`
		Length int    `json:"length,omitempty"`
	} `json:"trace"`
}

// Run performs admission probe, exactly one fixed Playwright Journey, then the
// completion probe. There is no interpreter, generic action surface, whole-run
// retry, or mid-step retry in this process.
func (runner *Runner) Run(ctx context.Context) Outcome {
	if err := ValidateExecutableJourney(runner.Journey.ID, runner.Journey.Version); err != nil {
		runner.Trace.Append("journey_rejected", map[string]any{"journeyId": runner.Journey.ID, "detail": err.Error()})
		return Outcome{GapCode: "interrupted", TerminalReason: "protocol_error", ErrorDetail: bounded(err.Error(), 2000), TraceIntegrity: "complete"}
	}
	runner.Trace.Append("playwright_journey_started", map[string]any{"journeyId": runner.Journey.ID, "version": runner.Journey.Version})
	admission, outcome := runner.probe(ctx, "admission")
	if outcome != nil {
		return *outcome
	}
	response, err := runner.runPlaywright(ctx, map[string]any{
		"mode": "journey", "startUrl": runner.StartURL, "journey": map[string]any{
			"id": runner.Journey.ID, "version": runner.Journey.Version, "params": json.RawMessage(runner.Journey.Params),
		},
	})
	if err != nil {
		if ctx.Err() != nil {
			return runner.cancelled()
		}
		detail := "versioned Playwright Journey failed: " + err.Error()
		runner.Trace.Append("journey_failed", map[string]any{"detail": bounded(err.Error(), 500)})
		return Outcome{GapCode: "journey_failed", TerminalReason: "journey_failed", ErrorDetail: bounded(detail, 2000), Probes: []ProbeObservation{admission}, TraceIntegrity: "complete"}
	}
	for _, event := range response.Trace {
		fields := map[string]any{}
		if event.Path != "" {
			fields["path"] = event.Path
		}
		if event.Length > 0 {
			fields["length"] = event.Length
		}
		runner.Trace.Append(event.Kind, fields)
	}
	completion, outcome := runner.probe(ctx, "completion")
	if outcome != nil {
		if len(outcome.Probes) == 0 {
			outcome.Probes = []ProbeObservation{admission}
		} else {
			outcome.Probes = append([]ProbeObservation{admission}, outcome.Probes...)
		}
		return *outcome
	}
	runner.Trace.Append("journey_succeeded", map[string]any{"outputFields": len(response.Output)})
	return Outcome{Success: true, Output: response.Output, Probes: []ProbeObservation{admission, completion}, TraceIntegrity: "complete"}
}

func (runner *Runner) cancelled() Outcome {
	runner.Trace.Append("cancelled", nil)
	return Outcome{GapCode: "cancelled", TerminalReason: "cancelled", ErrorDetail: "journey execution was cancelled before completion", TraceIntegrity: "incomplete"}
}

func (runner *Runner) runPlaywright(ctx context.Context, request map[string]any) (playwrightResponse, error) {
	endpoint, err := runner.Deps.PageEndpoint(ctx)
	if err != nil {
		return playwrightResponse{}, err
	}
	request["endpoint"] = endpoint
	encoded, err := json.Marshal(request)
	if err != nil {
		return playwrightResponse{}, err
	}
	runnerPath := playwrightRunnerPath()
	if err := verifyPlaywrightRunner(runnerPath); err != nil {
		return playwrightResponse{}, err
	}
	command := exec.CommandContext(ctx, "node", runnerPath)
	command.Stdin = bytes.NewReader(encoded)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return playwrightResponse{}, fmt.Errorf("%s", bounded(detail, 2000))
	}
	var response playwrightResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		return playwrightResponse{}, fmt.Errorf("decode Playwright Journey result: %w", err)
	}
	return response, nil
}

func playwrightRunnerPath() string {
	if _, err := os.Stat("/web/journey-runner.mjs"); err == nil {
		return "/web/journey-runner.mjs"
	}
	// Source-tree execution is only for local Go tests; production images always
	// use the fixed /web path copied by build/package/Dockerfile.
	return "internal/lintel/browser/journey/playwright-runner.mjs"
}

// verifyPlaywrightRunner closes the catalog's steps_digest to the exact script
// Node will execute. There is deliberately no environment override: a Journey
// is a fixed, versioned program rather than an operator-provided script.
func verifyPlaywrightRunner(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read versioned Playwright Journey: %w", err)
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != StatusMarkerSourceDigest {
		return fmt.Errorf("versioned Playwright Journey source digest mismatch")
	}
	return nil
}

// probe runs one authentication probe phase. Unauthenticated is a business
// gap; technical faults are probe-unavailable and never unauthenticated. The
// fixed Playwright probe navigation revisits the identity's start URL first so
// the typed URL-prefix probe observes the profile's real landing page.
func (runner *Runner) probe(ctx context.Context, phase string) (ProbeObservation, *Outcome) {
	_, err := runner.runPlaywright(ctx, map[string]any{"mode": "probe", "startUrl": runner.StartURL})
	if err != nil && retryableProbeNavigation(err) && ctx.Err() == nil {
		// Chromium can expose DevTools before its first navigation can commit.
		// This one-shot readiness retry is limited to the probe's pre-commit
		// navigation: it never replays a Journey step or a completed navigation.
		runner.Trace.Append("probe_navigation_readiness_retry", map[string]any{"phase": phase})
		_, err = runner.runPlaywright(ctx, map[string]any{"mode": "probe", "startUrl": runner.StartURL})
	}
	if err != nil {
		if ctx.Err() != nil {
			cancelled := runner.cancelled()
			return ProbeObservation{}, &cancelled
		}
		return ProbeObservation{}, &Outcome{GapCode: "authentication_probe_unavailable", TerminalReason: "authentication_probe_unavailable", ErrorDetail: bounded("authentication probe could not reach the start URL: "+err.Error(), 2000), TraceIntegrity: "complete"}
	}
	var params struct {
		AuthenticatedURLPrefix string `json:"authenticatedUrlPrefix"`
	}
	if err := json.Unmarshal(runner.Probe.Params, &params); err != nil || params.AuthenticatedURLPrefix == "" {
		runner.Trace.Append("probe_invalid", map[string]any{"phase": phase})
		return ProbeObservation{}, &Outcome{GapCode: "interrupted", TerminalReason: "protocol_error", ErrorDetail: "frozen authentication probe binding is invalid", TraceIntegrity: "complete"}
	}
	observation := ProbeObservation{
		Phase: phase, JourneyID: runner.Probe.ID, JourneyVersion: runner.Probe.Version,
		Catalog:    runner.Probe.Catalog,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	authenticated, err := runner.Deps.ProbeAuthenticated(ctx, params.AuthenticatedURLPrefix)
	switch {
	case err != nil:
		if ctx.Err() != nil {
			cancelled := runner.cancelled()
			return ProbeObservation{}, &cancelled
		}
		reason := "url_probe_unavailable"
		observation.Result, observation.ReasonCode = "Indeterminate", &reason
		runner.Trace.Append("probe", map[string]any{"phase": phase, "result": "Indeterminate", "reason": reason})
		return observation, &Outcome{GapCode: "authentication_probe_unavailable", TerminalReason: "authentication_probe_unavailable", ErrorDetail: bounded("authentication probe could not observe the login state: "+err.Error(), 2000), Probes: []ProbeObservation{observation}, TraceIntegrity: "complete"}
	case !authenticated:
		observation.Result = "Unauthenticated"
		runner.Trace.Append("probe", map[string]any{"phase": phase, "result": "Unauthenticated"})
		return observation, &Outcome{GapCode: "authentication_required", TerminalReason: "authentication_required", ErrorDetail: "authentication probe observed the identity as logged out", Probes: []ProbeObservation{observation}, TraceIntegrity: "complete"}
	default:
		observation.Result = "Authenticated"
		runner.Trace.Append("probe", map[string]any{"phase": phase, "result": "Authenticated"})
		return observation, nil
	}
}

func retryableProbeNavigation(err error) bool {
	// `commit` proves that no request/redirect result was accepted, unlike a
	// timeout after a loaded page or a Journey selector/action. Only this
	// cold-CDP readiness signature is retried under the fixed read-only probe.
	detail := err.Error()
	return strings.Contains(detail, "page.goto: Timeout") && strings.Contains(detail, `waiting until "commit"`)
}

func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
