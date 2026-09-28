package alertmanager_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/alerts"
	_ "github.com/Suknna/quoin/plugins/alertmanager"
)

func TestEventSourcePayloadIsIntakeWebhook(t *testing.T) {
	source, _, ok := plugins.Default().EventSource("alertmanager")
	if !ok {
		t.Fatal("alertmanager event source missing")
	}
	events, err := source.VerifyAndParse(context.Background(), plugins.InboundRequest{Body: []byte(`{
		"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"CPU"},"annotations":{"summary":"hot"},"startsAt":"2026-01-01T00:00:00Z","fingerprint":"0123456789abcdef","externalId":"forged-bypass"}],
		"groupKey":"group","truncatedAlerts":2,"unknownField":"ignored"}`)})
	if err != nil || len(events) != 1 {
		t.Fatalf("event source result: %v, %v", events, err)
	}
	webhook, err := alerts.ParseWebhook(events[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if webhook.GroupKey != "group" || webhook.TruncatedAlerts != 2 || len(webhook.Alerts) != 1 || webhook.Alerts[0].Annotations["summary"] != "hot" {
		t.Fatalf("intake lost event fields: %+v", webhook)
	}
	if webhook.Alerts[0].ExternalID != "" {
		t.Fatal("Alertmanager sender bypassed label/fingerprint verification with externalId")
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["unknownField"]; ok {
		t.Fatal("unknown source field leaked into normalized event")
	}
}

func TestAlertmanagerNormalizerMapsUnifiedSemantics(t *testing.T) {
	normalizer, pluginID, ok := plugins.Default().AlertNormalizer("alertmanager")
	if !ok || pluginID != plugins.AlertmanagerID || normalizer == nil {
		t.Fatalf("alertmanager normalizer missing: ok=%v plugin=%v", ok, pluginID)
	}
	payload := []byte(`{
		"status": "firing",
		"alerts": [
			{"status": "firing", "labels": {"alertname": "HighCPU", "severity": "critical", "instance": "node-1"},
			 "annotations": {"summary": "CPU 高", "description": "持续 5 分钟"}},
			{"status": "firing", "labels": {"alertname": "DiskFull", "severity": "page", "job": "db"},
			 "annotations": {"summary": "磁盘满"}},
			{"status": "resolved", "labels": {"alertname": "Flappy", "severity": "奇怪值"}}
		]
	}`)
	alerts, err := normalizer.NormalizeAlert(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 3 {
		t.Fatalf("normalized %d alerts, want 3", len(alerts))
	}
	first := alerts[0]
	if first.Severity != plugins.SeverityCritical || first.SeverityRaw != "critical" {
		t.Fatalf("first severity = %q (raw %q)", first.Severity, first.SeverityRaw)
	}
	if first.Title != "HighCPU" || first.Resource != "node-1" {
		t.Fatalf("first title/resource = %q/%q", first.Title, first.Resource)
	}
	if first.Annotations["summary"] != "CPU 高" {
		t.Fatalf("annotations not frozen: %v", first.Annotations)
	}
	if alerts[1].Severity != plugins.SeverityCritical || alerts[1].Resource != "db" {
		t.Fatalf("page severity mapped = %q, resource fallback = %q", alerts[1].Severity, alerts[1].Resource)
	}
	if alerts[2].Severity != plugins.SeverityInfo || alerts[2].SeverityRaw != "奇怪值" {
		t.Fatalf("unmapped severity degraded = %q (raw %q)", alerts[2].Severity, alerts[2].SeverityRaw)
	}
	if alerts[2].Title != "Flappy" {
		t.Fatalf("resolved alert title = %q", alerts[2].Title)
	}
}

func TestSeverityOrderIsComparable(t *testing.T) {
	ordered := []plugins.Severity{plugins.SeverityInfo, plugins.SeverityWarning, plugins.SeverityHigh, plugins.SeverityCritical}
	for index := 1; index < len(ordered); index++ {
		if plugins.SeverityOrder(ordered[index]) <= plugins.SeverityOrder(ordered[index-1]) {
			t.Fatalf("severity order broken at %v", ordered)
		}
	}
	if !plugins.ValidSeverity(plugins.SeverityCritical) || plugins.ValidSeverity("criticality") {
		t.Fatal("severity vocabulary membership wrong")
	}
}

func TestNormalizerRequiresEventSource(t *testing.T) {
	registry := plugins.NewRegistry()
	err := registry.Register(plugins.Plugin{
		ID: "orphan", Version: "1",
		AlertNormalizer: alertmanagerNormalizerShim{},
	})
	if err == nil {
		t.Fatal("normalizer without event source accepted")
	}
}

type alertmanagerNormalizerShim struct{}

func (alertmanagerNormalizerShim) NormalizeAlert([]byte) ([]plugins.NormalizedAlert, error) {
	return nil, nil
}
