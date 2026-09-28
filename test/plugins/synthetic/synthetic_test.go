package synthetic

import (
	"context"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
)

func TestSyntheticPluginKeepsMixedBatchForPerItemIntake(t *testing.T) {
	plugin := Plugin()
	if plugin.ID == plugin.EventSource.Kind() {
		t.Fatal("plugin ID and source kind should exercise distinct identity spaces")
	}
	events, err := plugin.EventSource.VerifyAndParse(context.Background(), plugins.InboundRequest{Body: []byte(`{
		"alerts":[
			{"externalId":"id-1","labels":{"instance":"node-a"},"startsAt":"2026-09-27T00:00:00Z"},
			{"labels":{"instance":"node-b"},"startsAt":"2026-09-27T00:00:00Z"}
		]
	}`)})
	if err != nil || len(events) != 1 || events[0].Type != "alerts.batch" {
		t.Fatalf("mixed delivery discarded: events=%+v err=%v", events, err)
	}
	alerts, err := plugin.AlertNormalizer.NormalizeAlert(events[0].Payload)
	if err != nil || len(alerts) != 2 || alerts[0].Resource != "node-a" || alerts[1].Resource != "node-b" {
		t.Fatalf("normalizer did not preserve item order: alerts=%+v err=%v", alerts, err)
	}
}
