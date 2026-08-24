package browser

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestUnexpectedChildExitFreesSlotAndReportsCrash(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "short-lived-browser")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 0.05\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{StateDirectory: root, Capacity: 1, ChromiumBinary: binary, XvfbBinary: binary, X0VNCBinary: binary})
	if err != nil {
		t.Fatal(err)
	}
	crashed := make(chan int64, 1)
	manager.OnCrash = func(id int64) { crashed <- id }
	if _, err = manager.Start(context.Background(), 1, "https://example.test"); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-crashed:
		if id != 1 {
			t.Fatalf("unexpected crashed operation %d", id)
		}
	case <-time.After(time.Second):
		t.Fatal("process exit did not free operation or report browser crash")
	}
	if _, err = manager.Start(context.Background(), 2, "https://example.test"); err != nil {
		t.Fatalf("crashed operation retained capacity: %v", err)
	}
	if err = manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestChromiumCommandUsesDisposableProfileEnvironment(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	command := chromiumCommand(context.Background(), "chromium", profile, "https://example.test/login")
	if !slices.Contains(command.Args, "--no-sandbox") {
		t.Fatalf("Chromium command does not use the required container sandbox fallback: %q", command.Args)
	}
	for _, wanted := range []string{
		"--user-data-dir=" + profile,
		"HOME=" + profile,
		"XDG_CONFIG_HOME=" + profile,
		"XDG_CACHE_HOME=" + filepath.Join(profile, "cache"),
	} {
		if !slices.Contains(command.Args, wanted) && !slices.Contains(command.Env, wanted) {
			t.Fatalf("Chromium command missing %q: args=%q env=%q", wanted, command.Args, command.Env)
		}
	}
}
