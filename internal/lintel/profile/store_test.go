package profile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallReplacesUnacknowledgedGeneration(t *testing.T) {
	store := NewStore(t.TempDir())
	manifest := Manifest{IdentityID: 42, Generation: 7, IdentityRevision: 11, ChromiumRevision: "Chromium 140"}
	for _, body := range []string{"first", "second"} {
		source := filepath.Join(t.TempDir(), "temporary-profile-"+body)
		if err := os.MkdirAll(source, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "Preferences"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Install(source, manifest); err != nil {
			t.Fatal(err)
		}
	}
	path, err := store.GenerationPath(42, 7)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(path, "Preferences"))
	if err != nil || string(body) != "second" {
		t.Fatalf("orphan generation was not deterministically replaced: %q err=%v", body, err)
	}
}

func TestInstallKeysProfileByGenerationNotDatabaseLocator(t *testing.T) {
	store := NewStore(t.TempDir())
	source := filepath.Join(t.TempDir(), "temporary-profile")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "Preferences"), []byte("profile"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{IdentityID: 42, Generation: 7, IdentityRevision: 11, ChromiumRevision: "Chromium 140"}
	digest, err := store.Install(source, manifest)
	if err != nil {
		t.Fatal(err)
	}
	loaded, loadedDigest, err := store.Inspect(42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != manifest || !bytes.Equal(digest, loadedDigest) {
		t.Fatalf("manifest/digest mismatch: %#v", loaded)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source should be moved, err=%v", err)
	}
}
