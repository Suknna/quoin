package config

import (
	"path/filepath"
	"testing"
)

func TestStateDirectoryForKeepsBackendsSeparate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	compose, err := StateDirectoryFor("compose")
	if err != nil {
		t.Fatal(err)
	}
	helm, err := StateDirectoryFor("helm")
	if err != nil {
		t.Fatal(err)
	}
	if compose != filepath.Join("/state", "quoin", "compose") {
		t.Fatalf("compose state directory = %q", compose)
	}
	if helm != filepath.Join("/state", "quoin", "helm") {
		t.Fatalf("helm state directory = %q", helm)
	}
	if helm == compose {
		t.Fatal("helm and compose must not share install retry state")
	}
}
