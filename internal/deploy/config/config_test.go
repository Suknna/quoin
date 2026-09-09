package config

import (
	"path/filepath"
	"testing"
)

func TestStateDirectoryForAcceptsOnlyCompose(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	compose, err := StateDirectoryFor("compose")
	if err != nil {
		t.Fatal(err)
	}
	if compose != filepath.Join("/state", "quoin", "compose") {
		t.Fatalf("compose state directory = %q", compose)
	}
	if _, err := StateDirectoryFor("kubernetes"); err == nil {
		t.Fatal("native Kubernetes deployments must not receive helper retry state")
	}
}
