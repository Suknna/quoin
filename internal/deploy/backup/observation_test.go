package backup

import (
	"errors"
	"testing"
	"time"
)

func TestObserveFailureWinsOverSuccessInSameScrape(t *testing.T) {
	values := []Observation{
		{Accepting: true, ProcessStart: 1, ManualSuccess: 10, Failures: 2},
		{Accepting: true, ProcessStart: 1, ManualSuccess: 11, Failures: 3},
	}
	clock := time.Unix(0, 0)
	err := Observe(OnlineOptions{
		Read:      func(string) (Observation, error) { value := values[0]; values = values[1:]; return value, nil },
		Now:       func() time.Time { return clock },
		Sleep:     func(duration time.Duration) { clock = clock.Add(duration) },
		PollEvery: time.Second, ObserveFor: time.Minute,
	})
	var observation *ObservationError
	if !errors.As(err, &observation) || observation.Code != "online_backup_failed" {
		t.Fatalf("error=%v; want online_backup_failed", err)
	}
}

func TestObserveRejectsUnavailableBeforePrompt(t *testing.T) {
	prompted := false
	err := Observe(OnlineOptions{
		Read:    func(string) (Observation, error) { return Observation{}, errors.New("connection refused") },
		OnReady: func() { prompted = true },
	})
	var observation *ObservationError
	if !errors.As(err, &observation) || observation.Code != "online_backup_unavailable" || prompted {
		t.Fatalf("error=%v prompted=%t", err, prompted)
	}
}

func TestUnavailabilityProvenAcceptsOnlyTypedVerifierProof(t *testing.T) {
	for _, scenario := range []struct {
		name, output string
		err          error
		want         bool
	}{
		{"typed", `{"kind":"quoin_ops_unavailable","source":"quoin-healthcheck","version":1}`, errors.New("exit status 1"), true},
		{"typed-followed-by-dns-error", "{\"kind\":\"quoin_ops_unavailable\",\"source\":\"quoin-healthcheck\",\"version\":1}\nGet \"http://quoin:9090/metrics\": dial tcp: lookup quoin on 127.0.0.11:53: server misbehaving", errors.New("exit status 1"), true},
		{"typed-with-terminal-decoration", "\x1b[31m{\"kind\":\"quoin_ops_unavailable\",\"source\":\"quoin-healthcheck\",\"version\":1}\x1b[0m", errors.New("exit status 1"), true},
		{"refused-text", "curl: (7) connection refused", errors.New("exit status 7"), false},
		{"timeout-text", "curl: (28) connection timed out", errors.New("exit status 28"), false},
		{"daemon", "Cannot connect to the Docker daemon", errors.New("exit status 1"), false},
		{"rbac", "Error from server (Forbidden): pods is forbidden", errors.New("exit status 1"), false},
		{"image", "ImagePullBackOff", errors.New("exit status 1"), false},
		{"config", "unknown flag: --metrics", errors.New("exit status 2"), false},
		{"empty", "", errors.New("exit status 1"), false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if got := UnavailabilityProven(scenario.output, scenario.err); got != scenario.want {
				t.Fatalf("UnavailabilityProven=%t want %t", got, scenario.want)
			}
		})
	}
}

func TestObserveFailsWhenObservedActiveRunDisappearsWithoutTerminalDelta(t *testing.T) {
	values := []Observation{
		{Accepting: true, Active: false, ProcessStart: 1, ManualSuccess: 10, Failures: 2},
		{Accepting: true, Active: true, ProcessStart: 1, ManualSuccess: 10, Failures: 2},
		{Accepting: true, Active: false, ProcessStart: 1, ManualSuccess: 10, Failures: 2},
	}
	clock := time.Unix(0, 0)
	err := Observe(OnlineOptions{
		Read: func(string) (Observation, error) { value := values[0]; values = values[1:]; return value, nil },
		Now:  func() time.Time { return clock }, Sleep: func(duration time.Duration) { clock = clock.Add(duration) },
		PollEvery: time.Second, ObserveFor: time.Hour,
	})
	var observation *ObservationError
	if !errors.As(err, &observation) || observation.Code != "online_backup_failed" {
		t.Fatalf("error=%v; want immediate online_backup_failed", err)
	}
	if !clock.Equal(time.Unix(0, 0).Add(2 * time.Second)) {
		t.Fatalf("observation waited %s; want two polls", clock.Sub(time.Unix(0, 0)))
	}
}

func TestObserveRejectsProcessRestartWhileWaitingForExistingActiveRun(t *testing.T) {
	values := []Observation{
		{Accepting: true, Active: true, ProcessStart: 1, ManualSuccess: 10, Failures: 2},
		{Accepting: true, Active: true, ProcessStart: 2, ManualSuccess: 10, Failures: 2},
	}
	clock := time.Unix(0, 0)
	err := Observe(OnlineOptions{
		Read: func(string) (Observation, error) { value := values[0]; values = values[1:]; return value, nil },
		Now:  func() time.Time { return clock }, Sleep: func(duration time.Duration) { clock = clock.Add(duration) },
		PollEvery: time.Second, ActiveWait: time.Minute,
	})
	var observation *ObservationError
	if !errors.As(err, &observation) || observation.Code != "online_backup_observation_reset" {
		t.Fatalf("error=%v; want online_backup_observation_reset", err)
	}
}
