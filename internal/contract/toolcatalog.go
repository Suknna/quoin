// Provider-tool-catalog rendering (ADR-0004 per-attempt frozen catalogs,
// ADR-0011 component isolation). Quoin (creation, BeginModelCall digest
// verification) and the Plinth worker (canonical-input rendering for the
// model call) MUST produce byte-identical provider schema bytes from the
// same frozen document: the digest the worker seals into BeginModelCall is
// re-derived by Quoin from the stored catalog, so any divergence is a hard
// runtime failure. Both ends therefore delegate to THESE pure functions —
// the byte shape lives once, in the neutral package both components already
// depend on. The package stays free of DB, business and plugin types so the
// Plinth compile-level ignorance of the plugin system (ADR-0011) is
// unaffected; callers project their own frozen documents into the neutral
// input. The rendering itself is part of the frozen contract: any change to
// the emitted bytes is a digest-breaking generation change for in-flight
// attempts and must be a conscious contract decision (pinned by the shared
// cross-end fixture under testdata/).

package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ProviderTool is the neutral projection of one frozen tool as the provider
// schema needs it: identity, model-facing description and the complete
// frozen parameter schema. Everything else in a frozen document (versions,
// failure modes, provenance) is host-side authority and never rendered.
type ProviderTool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ProviderToolsJSON renders frozen tools into the canonical provider-facing
// tool schema bytes (nested OpenAI function objects; deterministic because
// encoding/json orders map keys). It is the single rendering authority for
// both ends; an empty tool set renders as an empty JSON array.
func ProviderToolsJSON(tools []ProviderTool) ([]byte, error) {
	rendered := make([]any, 0, len(tools))
	for _, tool := range tools {
		rendered = append(rendered, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.Parameters,
			},
		})
	}
	return json.Marshal(rendered)
}

// ProviderToolsDigest is the SHA-256 hex of ProviderToolsJSON — the value
// BeginModelCall seals and re-verifies (tool_schema_digest).
func ProviderToolsDigest(tools []ProviderTool) (string, error) {
	body, err := ProviderToolsJSON(tools)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
