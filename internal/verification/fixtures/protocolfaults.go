// Package fixtures owns the deterministic external-protocol fault corpus
// and the black-box model-provider fixture probe used by Release
// Qualification (VERIFY-EXTERNAL-001/002, VERIFY-FAULT-003). The corpus
// replays application-layer protocol failures — error statuses, half
// responses, malformed bodies, application timeouts and SSE framing
// faults — which must never be produced by the TCP fault primitives:
// the fixture, the proxy and the platform network policies own disjoint
// failure surfaces.
package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProtocolFault is one deterministic application-layer failure case.
// The closed behavior vocabulary mirrors VERIFY-EXTERNAL-001: error
// codes, half responses, malformed responses and application-layer
// timeouts, plus the SSE framing faults of HTTP-STREAM.
type ProtocolFault struct {
	ID       string            `json:"id"`
	Protocol string            `json:"protocol"` // http | sse
	Behavior string            `json:"behavior"` // closed vocabulary below
	Status   int               `json:"status,omitempty"`
	Body     string            `json:"body,omitempty"`
	Events   []string          `json:"events,omitempty"` // sse frames before the fault
	DelayMs  int               `json:"delayMs,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
}

// behaviors is the closed replay vocabulary. Each entry names what a real
// client must observe.
var behaviors = map[string]string{
	"error_status":    "answers a declared HTTP error status with a non-2xx body",
	"malformed_body":  "answers 200 with a body that violates the protocol schema",
	"truncated_body":  "streams a valid prefix, declares more bytes, then closes",
	"app_timeout":     "accepts the request and never answers within the client deadline",
	"mid_stream_cut":  "emits SSE frames then closes the connection before the terminal frame",
	"malformed_frame": "emits an SSE frame whose payload violates the event schema",
}

// protocols is the closed protocol vocabulary.
var protocols = map[string]bool{"http": true, "sse": true}

// CorpusDirDefault is the frozen corpus locator referenced by the
// verification catalog (integration.monitoring-stack fixtures).
const CorpusDirDefault = "testdata/external-protocol-faults"

// LoadCorpus loads and validates every fixture in the directory. The
// corpus is closed: unknown protocols, unknown behaviors, duplicate ids
// and files outside the *.json shape are rejected so a drifted corpus
// fails qualification instead of silently narrowing coverage.
func LoadCorpus(dir string) ([]ProtocolFault, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("protocol fault corpus: %w", err)
	}
	seen := map[string]bool{}
	corpus := make([]ProtocolFault, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var fault ProtocolFault
		if err := json.Unmarshal(body, &fault); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if fault.ID == "" || seen[fault.ID] {
			return nil, fmt.Errorf("%s: duplicate or empty id %q", entry.Name(), fault.ID)
		}
		seen[fault.ID] = true
		if !protocols[fault.Protocol] {
			return nil, fmt.Errorf("%s: protocol %q outside http|sse", fault.ID, fault.Protocol)
		}
		if _, known := behaviors[fault.Behavior]; !known {
			return nil, fmt.Errorf("%s: behavior %q outside the closed vocabulary", fault.ID, fault.Behavior)
		}
		if fault.Behavior == "error_status" && (fault.Status < 400 || fault.Status > 599) {
			return nil, fmt.Errorf("%s: error_status requires a 4xx/5xx status", fault.ID)
		}
		corpus = append(corpus, fault)
	}
	if len(corpus) == 0 {
		return nil, fmt.Errorf("protocol fault corpus %s is empty", dir)
	}
	sort.Slice(corpus, func(i, j int) bool { return corpus[i].ID < corpus[j].ID })
	return corpus, nil
}

// Serve replays one fault on an http.ResponseWriter. It is the single
// replay implementation: the monitoring-stack scenario and the corpus
// tests share it, so observed client behavior is identical everywhere.
// The request bounds the app_timeout replay: the handler holds the
// connection open until the client gives up, never past its own context.
func (fault ProtocolFault) Serve(writer http.ResponseWriter, request *http.Request) {
	for key, value := range fault.Headers {
		writer.Header().Set(key, value)
	}
	switch fault.Behavior {
	case "error_status":
		writer.WriteHeader(fault.Status)
		_, _ = io.WriteString(writer, fault.Body)
	case "malformed_body":
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, fault.Body)
	case "truncated_body":
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Content-Length", fmt.Sprint(len(fault.Body)+64))
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, fault.Body)
		if hijacker, ok := writer.(http.Hijacker); ok {
			if connection, _, err := hijacker.Hijack(); err == nil {
				_ = connection.Close()
				return
			}
		}
	case "app_timeout":
		// Hold the request open; the server never answers within any
		// sane application deadline. The handler returns only when the
		// client disconnects so no goroutine outlives the replay.
		select {
		case <-time.After(24 * time.Hour):
		case <-request.Context().Done():
		}
	case "mid_stream_cut":
		writer.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := writer.(http.Flusher)
		for _, event := range fault.Events {
			_, _ = io.WriteString(writer, event+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		if hijacker, ok := writer.(http.Hijacker); ok {
			if connection, _, err := hijacker.Hijack(); err == nil {
				_ = connection.Close()
				return
			}
		}
	case "malformed_frame":
		writer.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := writer.(http.Flusher)
		for _, event := range fault.Events {
			_, _ = io.WriteString(writer, event+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// FaultObservation is what a real client observed against one replay.
// It records the client-side facts qualification asserts on: the class
// of failure, not the server's intent.
type FaultObservation struct {
	Fixture     string `json:"fixture"`
	Behavior    string `json:"behavior"`
	Status      int    `json:"status,omitempty"`
	BodyPrefix  string `json:"bodyPrefix,omitempty"`
	BodyInvalid bool   `json:"bodyInvalidJSON,omitempty"` // malformed_body must fail to parse
	EventsSeen  int    `json:"eventsSeen,omitempty"`
	ReadFailure string `json:"readFailure,omitempty"` // unexpected EOF / connection reset
	DeadlineHit bool   `json:"deadlineHit,omitempty"` // client deadline fired first
	ClientClass string `json:"clientClass"`           // closed classification below
}

// classify maps the raw transport facts to the closed client-side class
// the scenario's facts report.
func classify(fault ProtocolFault, status int, readFailure string, deadlineHit bool, events int, bodyInvalid bool) string {
	switch fault.Behavior {
	case "error_status":
		if status == fault.Status {
			return "error_status_observed"
		}
		return "unexpected"
	case "malformed_body":
		if status == http.StatusOK && bodyInvalid {
			return "malformed_body_observed"
		}
		return "unexpected"
	case "truncated_body":
		if readFailure != "" {
			return "truncated_body_observed"
		}
		return "unexpected"
	case "app_timeout":
		if deadlineHit {
			return "app_timeout_observed"
		}
		return "unexpected"
	case "mid_stream_cut":
		if events > 0 && readFailure != "" {
			return "mid_stream_cut_observed"
		}
		return "unexpected"
	case "malformed_frame":
		if status == http.StatusOK && readFailure == "" {
			return "malformed_frame_observed"
		}
		return "unexpected"
	}
	return "unexpected"
}

// Observe drives one real client request against a replayed fault and
// returns the classified observation. The deadline is the client-side
// application timeout the scenario freezes; the request context bounds
// the attempt so an app_timeout replay cannot outlive its observation.
func Observe(client *http.Client, url string, fault ProtocolFault, deadline time.Duration) FaultObservation {
	observation := FaultObservation{Fixture: fault.ID, Behavior: fault.Behavior}
	bounded := *client
	if bounded.Timeout == 0 || bounded.Timeout > deadline {
		bounded.Timeout = deadline
	}
	attempt := func() (int, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, "", err
		}
		response, err := bounded.Do(request)
		if err != nil {
			return 0, "", err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return response.StatusCode, string(body), err
	}
	status, body, err := attempt()
	observation.Status = status
	observation.BodyPrefix = truncate(body, 120)
	observation.EventsSeen = strings.Count(body, "\n\n")
	observation.BodyInvalid = !json.Valid([]byte(body))
	if err != nil {
		observation.ReadFailure = err.Error()
	}
	if isDeadlineError(err) {
		observation.DeadlineHit = true
	}
	observation.ClientClass = classify(fault, observation.Status, observation.ReadFailure, observation.DeadlineHit, observation.EventsSeen, observation.BodyInvalid)
	return observation
}

func isDeadlineError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "Client.Timeout") ||
		strings.Contains(err.Error(), "context deadline exceeded")
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
