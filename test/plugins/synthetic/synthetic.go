// Package synthetic is a compiled test-only alert source. Its plugin ID is
// intentionally distinct from its wire kind, and it uses a stable upstream
// externalId rather than Alertmanager's label fingerprint. Hosts assemble it
// through their registry just like production plugins; no Quoin domain branch
// names this source.
package synthetic

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/plugins"
)

const Kind = "synthetic-hook"

func Plugin() plugins.Plugin {
	return plugins.Plugin{
		ID: "synthetic-plugin", Version: "1", DisplayName: "Synthetic webhook",
		EventSource: source{}, EventTypes: []string{"alerts.batch"},
		AlertNormalizer: normalizer{}, AlertIdentity: plugins.AlertIdentityExternal,
	}
}

type source struct{}

func (source) Kind() string { return Kind }

func (source) VerifyAndParse(_ context.Context, req plugins.InboundRequest) ([]plugins.Event, error) {
	batch, err := contract.ParseAlertmanagerWebhook(req.Body)
	if err != nil {
		return nil, err
	}
	if len(batch.Alerts) == 0 {
		return nil, errors.New("synthetic alert batch is empty")
	}
	// Malformed individual items are Quoin intake issues, not a reason to
	// discard other valid members of this accepted delivery at the gateway.
	encoded, err := json.Marshal(batch)
	if err != nil {
		return nil, err
	}
	return []plugins.Event{{Type: "alerts.batch", Payload: encoded}}, nil
}

type normalizer struct{}

func (normalizer) NormalizeAlert(body []byte) ([]plugins.NormalizedAlert, error) {
	batch, err := contract.ParseAlertmanagerWebhook(body)
	if err != nil {
		return nil, err
	}
	result := make([]plugins.NormalizedAlert, 0, len(batch.Alerts))
	for _, item := range batch.Alerts {
		result = append(result, plugins.NormalizedAlert{
			Severity:    plugins.SeverityHigh,
			Title:       "Synthetic source",
			Resource:    item.Labels["instance"],
			Annotations: item.Annotations,
		})
	}
	return result, nil
}
