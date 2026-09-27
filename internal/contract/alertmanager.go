package contract

import "encoding/json"

// AlertmanagerWebhook is the shared event payload between the Alertmanager
// EventSource and Quoin's intake. Keep its JSON shape stable for queued events.
type AlertmanagerWebhook struct {
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

// ParseAlertmanagerWebhook decodes the shared shape; protocol admission and
// business validation remain with the respective consumers.
func ParseAlertmanagerWebhook(body []byte) (*AlertmanagerWebhook, error) {
	var webhook AlertmanagerWebhook
	if err := json.Unmarshal(body, &webhook); err != nil {
		return nil, err
	}
	return &webhook, nil
}
