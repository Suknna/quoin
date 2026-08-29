package knowledge

// suggestion.go renders the immutable original model suggestion for a
// diagnosis-source candidate: a structured, versioned projection of the
// model output that produced the source (the analysis output, report or
// assistant message). Program code owns the deterministic projection; the
// prose belongs to the model that wrote the source. The projection is
// frozen at candidate creation and never recomputed.

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const suggestionVersion = 1

const titleLimit = 100

// suggestionSource is the locator part of the projection: enough for the
// UI to link back to the immutable source without a second authority.
type suggestionSource struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	ModelID   string         `json:"modelId,omitempty"`
	CreatedAt string         `json:"createdAt,omitempty"`
	Locator   map[string]any `json:"locator,omitempty"`
}

// Suggestion is the full original suggestion projection (v1).
type Suggestion struct {
	V      int              `json:"v"`
	Source suggestionSource `json:"source"`
	Title  string           `json:"title"`
	Body   string           `json:"body"`
}

// deriveTitle produces the deterministic draft/suggestion title: the
// first non-empty line of the source content, bounded to 100 runes.
func deriveTitle(content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if utf8.RuneCountInString(trimmed) > titleLimit {
			runes := []rune(trimmed)
			trimmed = string(runes[:titleLimit])
		}
		return trimmed
	}
	return "未命名知识建议"
}

// suggestionJSON marshals the projection into the immutable column value.
func suggestionJSON(suggestion Suggestion) (string, error) {
	body, err := json.Marshal(suggestion)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
