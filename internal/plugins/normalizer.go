package plugins

// Alert normalization capability (ADR-0012): the plugin-side half of the
// alert normalization layer. The gateway's EventSource already normalizes the
// wire protocol into a per-source payload document; the AlertNormalizer maps
// that payload onto the unified alert semantics — severity, title and the
// canonical annotations — so alerts from different sources and different
// field structures land on one vocabulary. It is a pure function with zero
// business dependencies: business interpretation (occurrence state machine,
// enrichment, correlation) stays with Quoin's intake pipeline.

import "encoding/json"

// Severity is the closed unified severity vocabulary (ADR-0012): an ordered
// four-level scale. The numeric order makes comparisons cheap for the AI
// paths; unmapped source values degrade to Info and are recorded as intake
// issues by Quoin, never dropped.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// SeverityOrder is the comparable ordinal of the unified vocabulary
// (critical=4 > high=3 > warning=2 > info=1).
func SeverityOrder(severity Severity) int {
	switch severity {
	case SeverityCritical:
		return 4
	case SeverityHigh:
		return 3
	case SeverityWarning:
		return 2
	default:
		return 1
	}
}

// ValidSeverity reports membership in the closed vocabulary.
func ValidSeverity(severity Severity) bool {
	switch severity {
	case SeverityCritical, SeverityHigh, SeverityWarning, SeverityInfo:
		return true
	}
	return false
}

// NormalizedAlert is the unified semantic projection of one inbound alert
// (one element of an EventSource payload). Every field is frozen by Quoin at
// first observation; empty values are legal (the source simply did not
// carry them).
type NormalizedAlert struct {
	// Severity is the unified severity after the source's mapping.
	Severity Severity
	// SeverityRaw is the source-declared severity value verbatim (kept for
	// audit; the original labels also stay untouched).
	SeverityRaw string
	// Title is the human-facing alert name (Alertmanager: labels.alertname).
	Title string
	// Annotations is the full canonical annotation map of the alert
	// (summary/description and everything else the source carries).
	Annotations map[string]string
	// Resource is the best-effort affected-object identity (Alertmanager:
	// labels.instance, falling back to labels.job).
	Resource string
}

// AlertNormalizer maps one source-kind payload onto the unified alert
// semantics. Kind matches the plugin's EventSource kind; a plugin offering
// inbound alerts SHOULD provide a normalizer — Quoin's intake records a
// normalizer_missing intake issue for sources without one and freezes the
// degraded defaults.
type AlertNormalizer interface {
	// NormalizeAlert projects the EventSource payload (the normalized event
	// document the source produces) onto unified alert semantics, one
	// NormalizedAlert per alert element, in payload order.
	NormalizeAlert(payload []byte) ([]NormalizedAlert, error)
}

// annotationsJSON renders the canonical annotation map as deterministic
// JSON bytes (sorted keys) for frozen storage.
func AnnotationsJSON(annotations map[string]string) json.RawMessage {
	if annotations == nil {
		return json.RawMessage("{}")
	}
	encoded, err := json.Marshal(annotations)
	if err != nil {
		return json.RawMessage("{}")
	}
	return encoded
}
