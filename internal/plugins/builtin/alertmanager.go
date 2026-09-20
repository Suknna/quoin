package builtin

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

	"github.com/Suknna/quoin/internal/plugins"
)

// alertmanagerEventPayload is the normalized event document this source
// produces and Quoin's alert consumer parses: the cleaned Alertmanager
// webhook projection. It intentionally mirrors the historical wire shape so
// the intake transaction's machine semantics are unchanged.
type alertmanagerEventPayload struct {
	Status string `json:"status"`
	Alerts []struct {
		Status       string            `json:"status"`
		Labels       map[string]string `json:"labels"`
		Annotations  map[string]string `json:"annotations"`
		StartsAt     string            `json:"startsAt"`
		EndsAt       string            `json:"endsAt"`
		Fingerprint  string            `json:"fingerprint"`
		GeneratorURL string            `json:"generatorURL"`
	} `json:"alerts"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
}

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
	var payload alertmanagerEventPayload
	decoder := json.NewDecoder(strings.NewReader(string(req.Body)))
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("alertmanager webhook body is not valid JSON: %w", err)
	}
	if payload.Status == "" || len(payload.Alerts) == 0 {
		return nil, errors.New("alertmanager webhook carries no alerts")
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []plugins.Event{{
		Type:    "alerts.batch",
		Payload: normalized,
	}}, nil
}

func init() {
	plugins.Register(plugins.Plugin{
		ID:             plugins.AlertmanagerID,
		Version:        "1",
		DisplayName:    "Alertmanager",
		Description:    "接收上游 Alertmanager 告警来源：Stele 网关按来源认证并归一化入队，Quoin 事务性消费。",
		DefaultEnabled: true,
		EventSource:    alertmanagerSource{},
	})
}
