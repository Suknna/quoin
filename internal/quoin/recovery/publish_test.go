package recovery

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPublishRestoresRollbackPointWhenDirectorySyncFails(t *testing.T) {
	data := t.TempDir()
	staging := t.TempDir()
	writeTree(t, data, "quoin.db", "old-db")
	writeTree(t, data, "artifacts/blob", "old-artifact")
	writeTree(t, staging, "quoin.db", "new-db")
	writeTree(t, staging, "artifacts/blob", "new-artifact")

	_, err := publishWithSync(data, staging, "", func(string) error { return errors.New("injected directory sync failure") })
	if err == nil {
		t.Fatal("publish succeeded despite sync failure")
	}
	assertFile(t, filepath.Join(data, "quoin.db"), "old-db")
	assertFile(t, filepath.Join(data, "artifacts", "blob"), "old-artifact")
}

func TestPublishPreservesRollbackWhenRollbackRenameFails(t *testing.T) {
	data := t.TempDir()
	staging := t.TempDir()
	writeTree(t, data, "quoin.db", "old-db")
	writeTree(t, data, "artifacts/blob", "old-artifact")
	writeTree(t, staging, "quoin.db", "new-db")
	writeTree(t, staging, "artifacts/blob", "new-artifact")

	originalRename := renamePath
	t.Cleanup(func() { renamePath = originalRename })
	renamePath = func(oldPath, newPath string) error {
		if oldPath == filepath.Join(data, ".restore-rollback-1", "quoin.db") && newPath == filepath.Join(data, "quoin.db") {
			return errors.New("injected restore database rename failure")
		}
		return originalRename(oldPath, newPath)
	}
	_, err := publishWithSync(data, staging, ".restore-rollback-1", func(string) error { return errors.New("injected directory sync failure") })
	if err == nil {
		t.Fatal("publish succeeded despite rollback rename failure")
	}
	assertFile(t, filepath.Join(data, ".restore-rollback-1", "quoin.db"), "old-db")
}

func TestPublishRetainsRollbackUntilFinalize(t *testing.T) {
	data := t.TempDir()
	staging := t.TempDir()
	writeTree(t, data, "quoin.db", "old-db")
	writeTree(t, data, "artifacts/blob", "old-artifact")
	writeTree(t, staging, "quoin.db", "new-db")
	writeTree(t, staging, "artifacts/blob", "new-artifact")

	var synced []string
	rollback, err := publishWithSync(data, staging, ".restore-rollback-1", func(directory string) error {
		synced = append(synced, directory)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{rollback, data}; !slices.Equal(synced, want) {
		t.Fatalf("synced directories=%q, want %q", synced, want)
	}
	assertFile(t, filepath.Join(rollback, "quoin.db"), "old-db")
	if err := Finalize(data, filepath.Base(rollback)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rollback); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback remains after finalize: %v", err)
	}
}

func writeTree(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
