// Package backup owns the shared read-only ops-metrics observation contract for
// deployment backup helpers. Compose and Helm differ only in transport.
package backup

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Observation struct {
	Accepting, Maintenance, Active        bool
	ProcessStart, ManualSuccess, Failures float64
}

func (value Observation) Available() bool {
	return value.Accepting && !value.Maintenance && !value.Active
}

// UnavailabilityProven accepts only the typed terminal statement written by
// quoin-healthcheck after its own attempted ops-listener connection failed.
// Docker/Kubernetes/RBAC/image and launcher errors are deliberately irrelevant.
func UnavailabilityProven(output string, runErr error) bool {
	if runErr == nil {
		return false
	}
	var proof struct {
		Kind    string `json:"kind"`
		Source  string `json:"source"`
		Version int    `json:"version"`
	}
	for _, line := range strings.Split(output, "\n") {
		proof = struct {
			Kind    string `json:"kind"`
			Source  string `json:"source"`
			Version int    `json:"version"`
		}{}
		// Docker may decorate a PTY-attached child's stdout. Parse the complete
		// JSON object emitted by quoin-healthcheck, never generic error text.
		start, end := strings.IndexByte(line, '{'), strings.LastIndexByte(line, '}')
		if start < 0 || end < start {
			continue
		}
		if json.Unmarshal([]byte(line[start:end+1]), &proof) == nil && proof.Kind == "quoin_ops_unavailable" && proof.Source == "quoin-healthcheck" && proof.Version == 1 {
			return true
		}
	}
	return false
}

func ParseObservation(body string) (Observation, error) {
	var value Observation
	seen := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		number, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		switch name := fields[0]; {
		case name == "quoin_accepting_work":
			value.Accepting, seen["accepting"] = number == 1, true
		case strings.HasPrefix(name, "quoin_maintenance"):
			value.Maintenance = value.Maintenance || number != 0
			seen["maintenance"] = true
		case name == "quoin_backup_active":
			value.Active, seen["active"] = number == 1, true
		case name == "process_start_time_seconds":
			value.ProcessStart, seen["start"] = number, true
		case name == "quoin_backup_last_online_manual_success_timestamp_seconds":
			value.ManualSuccess, seen["manual"] = number, true
		case name == "quoin_backup_failures_total":
			value.Failures, seen["failures"] = number, true
		}
	}
	for _, key := range []string{"accepting", "maintenance", "active", "start", "manual", "failures"} {
		if !seen[key] {
			return Observation{}, fmt.Errorf("ops metrics missing %s", key)
		}
	}
	return value, nil
}

// OnlineOptions provides the backend-specific transport only. Every state
// transition, timeout, failure ordering, and process fence lives in Observe.
type OnlineOptions struct {
	Read       func(label string) (Observation, error)
	Now        func() time.Time
	Sleep      func(time.Duration)
	OnReady    func()
	ActiveWait time.Duration
	ObserveFor time.Duration
	PollEvery  time.Duration
}

// ObservationError contains the stable deployment report reason and operator
// action for the single online-backup observation state machine.
type ObservationError struct{ Code, Message, NextAction string }

func (e *ObservationError) Error() string { return e.Message }

func Observe(options OnlineOptions) error {
	if options.Read == nil {
		return &ObservationError{Code: "online_backup_unavailable", Message: "metrics reader is unavailable", NextAction: "repair the deployment verifier"}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Sleep == nil {
		options.Sleep = time.Sleep
	}
	if options.ActiveWait == 0 {
		options.ActiveWait = 2 * time.Minute
	}
	if options.ObserveFor == 0 {
		options.ObserveFor = 20 * time.Minute
	}
	if options.PollEvery == 0 {
		options.PollEvery = 5 * time.Second
	}
	baseline, err := options.Read("backup-metrics-baseline")
	if err != nil || !baseline.Accepting || baseline.Maintenance {
		message := "Quoin is not accepting work or is in maintenance"
		if err != nil {
			message = err.Error()
		}
		return &ObservationError{Code: "online_backup_unavailable", Message: message, NextAction: "ensure Quoin is ready and not in maintenance, or explicitly choose --offline"}
	}
	processStart := baseline.ProcessStart
	activeDeadline := options.Now().Add(options.ActiveWait)
	for baseline.Active && options.Now().Before(activeDeadline) {
		options.Sleep(options.PollEvery)
		baseline, err = options.Read("backup-wait-active")
		if err != nil || !baseline.Accepting || baseline.Maintenance {
			return &ObservationError{Code: "online_backup_unavailable", Message: "Quoin became unavailable while waiting for the active backup", NextAction: "retry after Quoin is ready; use --offline only when Quoin is unavailable"}
		}
		if baseline.ProcessStart != processStart {
			return &ObservationError{Code: "online_backup_observation_reset", Message: "Quoin restarted while waiting for the active backup", NextAction: "retry after Quoin is stably ready"}
		}
	}
	if baseline.Active {
		return &ObservationError{Code: "online_backup_unavailable", Message: "timed out waiting for the active backup", NextAction: "wait for the active Backup Run to finish, then retry"}
	}
	if options.OnReady != nil {
		options.OnReady()
	}
	// A run that was active at admission has a terminal signal as soon as it
	// disappears: a missing success and unchanged failure counter is a stable
	// failure, not an ambiguity that should consume the full observation window.
	observedActive := baseline.Active
	deadline := options.Now().Add(options.ObserveFor)
	for options.Now().Before(deadline) {
		options.Sleep(options.PollEvery)
		current, err := options.Read("backup-metrics-observe")
		if err != nil || current.ProcessStart != baseline.ProcessStart || !current.Accepting || current.Maintenance {
			return &ObservationError{Code: "online_backup_observation_reset", Message: "Quoin process/readiness changed during observation", NextAction: "retry after Quoin is stably ready"}
		}
		// Failure wins over a success scrape so a scrape containing both terminal
		// values cannot claim a successful manual run after a failure increment.
		if current.Failures > baseline.Failures {
			return &ObservationError{Code: "online_backup_failed", Message: "active backup ended without a new online-manual success", NextAction: "inspect the Backup Run in the Admin UI and retry after fixing it"}
		}
		if !current.Active && current.ManualSuccess > baseline.ManualSuccess {
			return nil
		}
		if observedActive && !current.Active {
			return &ObservationError{Code: "online_backup_failed", Message: "active backup ended without a new online-manual success", NextAction: "inspect the Backup Run in the Admin UI and retry after fixing it"}
		}
		observedActive = observedActive || current.Active
	}
	return &ObservationError{Code: "online_backup_timeout", Message: "timed out waiting for the Admin-triggered backup observation", NextAction: "inspect the Backup Run in the Admin UI, then retry observation or explicitly choose --offline if Quoin is unavailable"}
}
