package alertmanager

// The alertmanager plugin (ADR-0011): a pure EventSource. Stele's webhook
// layer authenticates the bearer against Quoin's digest snapshot and hands
// the raw request here; this source owns the Alertmanager wire protocol and
// normalizes the payload. Business semantics (occurrence state machine,
// fingerprint recomputation, attribution) stay with Quoin's consumer.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/plugins"
)

// alertmanagerSource is the inbound EventSource for Alertmanager webhooks.
type alertmanagerSource struct{}

func (alertmanagerSource) Kind() string { return "alertmanager" }

// VerifyAndParse decodes the Alertmanager webhook shape and normalizes it
// into one "alerts.batch" event. Malformed bodies are refused before the
// gateway enqueues anything.
func (alertmanagerSource) VerifyAndParse(_ context.Context, req plugins.InboundRequest) ([]plugins.Event, error) {
	if len(req.Body) == 0 {
		return nil, errors.New("alertmanager webhook body is empty")
	}
	payload, err := contract.ParseAlertmanagerWebhook(req.Body)
	if err != nil {
		return nil, fmt.Errorf("alertmanager webhook body is not valid JSON: %w", err)
	}
	if payload.Status == "" || len(payload.Alerts) == 0 {
		return nil, errors.New("alertmanager webhook carries no alerts")
	}
	// A sender cannot opt out of Alertmanager's label/fingerprint consistency
	// check by injecting the normalized identity field used by other sources.
	for index := range payload.Alerts {
		payload.Alerts[index].ExternalID = ""
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []plugins.Event{{
		Type:           "alerts.batch",
		PayloadVersion: 1,
		Payload:        normalized,
	}}, nil
}

// alertmanagerSeverity maps the Alertmanager (Prometheus convention)
// severity label onto the unified vocabulary. Values outside the mapping
// degrade to info; the raw value travels alongside for audit.
func alertmanagerSeverity(raw string) plugins.Severity {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "critical", "crit", "page", "severe":
		return plugins.SeverityCritical
	case "high", "error":
		return plugins.SeverityHigh
	case "warning", "warn":
		return plugins.SeverityWarning
	default:
		// info / none / empty and every unmapped value.
		return plugins.SeverityInfo
	}
}

// alertmanagerNormalizer maps the alertmanager event payload (this source's
// normalized wire projection) onto the unified alert semantics: severity
// from labels.severity, title from labels.alertname, the full annotations
// map frozen verbatim, and the resource inferred from instance/job.
type alertmanagerNormalizer struct{}

func (alertmanagerNormalizer) NormalizeAlert(payload []byte) ([]plugins.NormalizedAlert, error) {
	document, err := contract.ParseAlertmanagerWebhook(payload)
	if err != nil {
		return nil, fmt.Errorf("alertmanager payload is not valid JSON: %w", err)
	}
	alerts := make([]plugins.NormalizedAlert, 0, len(document.Alerts))
	for _, alert := range document.Alerts {
		resource := alert.Labels["instance"]
		if resource == "" {
			resource = alert.Labels["job"]
		}
		raw := alert.Labels["severity"]
		alerts = append(alerts, plugins.NormalizedAlert{
			Severity:    alertmanagerSeverity(raw),
			SeverityRaw: raw,
			Title:       alert.Labels["alertname"],
			Annotations: alert.Annotations,
			Resource:    resource,
		})
	}
	return alerts, nil
}

func init() {
	plugins.Register(plugins.Plugin{
		ID:              plugins.AlertmanagerID,
		Version:         "2",
		DisplayName:     "Alertmanager",
		Description:     "接收上游 Alertmanager 告警来源：Stele 网关按来源认证并归一化入队，Quoin 事务性消费并归一化告警语义。",
		DefaultEnabled:  true,
		EventSource:     alertmanagerSource{},
		EventContracts:  []plugins.EventContract{{Type: "alerts.batch", Version: 1}},
		AlertNormalizer: alertmanagerNormalizer{},
		AlertIdentity:   plugins.AlertIdentityLabels,
	})
}
