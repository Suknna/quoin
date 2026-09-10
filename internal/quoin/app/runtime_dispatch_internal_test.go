package app

import (
	"encoding/json"
	"testing"
)

// The supervisor ResultProposal reaches this parser after a real HTTP probe.
// Keep both Prometheus-compatible schema kinds explicit so a new producer
// cannot be accepted under the wrong connection identity.
func TestParseTypedChildAcceptsPrometheusProbeResult(t *testing.T) {
	detail := json.RawMessage(`{"kind":"prometheus","query":"vector(1)","responseType":"vector","sampleCount":1,"sampleValue":"1"}`)
	child, err := parseTypedChild("connection_probe_prometheus_v1", detail)
	if err != nil {
		t.Fatal(err)
	}
	if child.Thanos == nil || child.Thanos.Query != "vector(1)" || child.Thanos.ResponseType != "vector" || child.Thanos.SampleCount != 1 || child.Thanos.SampleValue != "1" {
		t.Fatalf("Prometheus typed child did not preserve the validated probe: %+v", child)
	}
}

func TestParseTypedChildRejectsCrossTypeMetricsResult(t *testing.T) {
	detail := json.RawMessage(`{"kind":"thanos","query":"vector(1)","responseType":"vector","sampleCount":1,"sampleValue":"1"}`)
	if _, err := parseTypedChild("connection_probe_prometheus_v1", detail); err == nil {
		t.Fatal("Prometheus schema kind must reject a Thanos detail identity")
	}
}
