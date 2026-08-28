package journey

import (
	"encoding/json"

	"sync"
	"time"
)

// Trace is the mandatory whole-run sensitive trace of one Journey execution
// (DATA-BROWSER-006). It stays in memory until the channel uploads it as a
// sensitive trace Artifact owned by the Browser Operation; it never enters a
// non-secret log. Entries are bounded so a hostile page cannot grow it.
type Trace struct {
	mu      sync.Mutex
	entries []map[string]any
	seq     int
}

const traceMaxEntries = 2000

// Append records one bounded trace fact. Values must already be non-secret
// metadata or page projections the sensitive trace is allowed to carry.
func (trace *Trace) Append(event string, fields map[string]any) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if len(trace.entries) >= traceMaxEntries {
		return
	}
	trace.seq++
	entry := map[string]any{
		"seq":   trace.seq,
		"at":    time.Now().UTC().Format(time.RFC3339Nano),
		"event": event,
	}
	for key, value := range fields {
		if key == "seq" || key == "at" || key == "event" {
			continue
		}
		entry[key] = value
	}
	trace.entries = append(trace.entries, entry)
}

// Bytes renders the trace as canonical JSONL. The final seal line marks the
// integrity the channel will upload with.
func (trace *Trace) Bytes(integrity string) []byte {
	trace.mu.Lock()
	entries := make([]map[string]any, len(trace.entries))
	copy(entries, trace.entries)
	trace.mu.Unlock()
	out := make([]byte, 0, 4096)
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	seal, _ := json.Marshal(map[string]any{"seal": "terminal", "integrity": integrity, "seq": trace.seq, "at": time.Now().UTC().Format(time.RFC3339Nano)})
	out = append(out, seal...)
	out = append(out, '\n')
	return out
}

// probeObservation is one typed authentication probe fact of the run.
type ProbeObservation struct {
	Phase          string     `json:"phase"`
	Result         string     `json:"result"`
	JourneyID      string     `json:"journeyId"`
	JourneyVersion int64      `json:"journeyVersion"`
	Catalog        CatalogRef `json:"catalog"`
	ReasonCode     *string    `json:"reasonCode"`
	ObservedAt     string     `json:"observedAt"`
}
