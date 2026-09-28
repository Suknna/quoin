package app

import "testing"

func TestPublicReceiverURLForKindUsesDeploymentEndpoint(t *testing.T) {
	got, err := publicReceiverURLForKind("https://alerts.example.com/base/stele/webhook/alertmanager", "synthetic")
	if err != nil || got != "https://alerts.example.com/base/stele/webhook/synthetic" {
		t.Fatalf("receiver URL=%q err=%v", got, err)
	}
	if _, err := publicReceiverURLForKind("http://internal/stele/webhook/alertmanager", "synthetic"); err == nil {
		t.Fatal("unsafe deployment URL accepted")
	}
}
