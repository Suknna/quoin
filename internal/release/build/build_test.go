package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParsePlatformClosedVocabulary(t *testing.T) {
	entry, err := parsePlatform("linux/arm64=emulated")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Platform != "linux/arm64" || entry.Mode != "emulated" || entry.Arch != "arm64" {
		t.Fatalf("parsed %+v", entry)
	}
	for _, bad := range []string{"linux/riscv64=native", "linux/amd64=qemu", "amd64", "linux/amd64"} {
		if _, err := parsePlatform(bad); err == nil {
			t.Fatalf("%q must fail", bad)
		}
	}
}

func TestAssertNoLatest(t *testing.T) {
	for _, ok := range []string{
		"image: 127.0.0.1:5000/quoin@sha256:" + "ab32",
		"registry:5000/quoin", "quoin:1.2.3",
	} {
		if err := assertNoLatest(ok); err != nil {
			t.Fatalf("%q wrongly rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"image: repo:latest", "tag: latest", "- latest", "latest:", "\"latest\""} {
		if err := assertNoLatest(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

func TestWriteComposeBundleDeterministic(t *testing.T) {
	entries := map[string][]byte{
		"compose.yaml": []byte("a: 1\n"),
		"quoin-deploy": []byte("#!/bin/sh\n"),
	}
	first := filepath.Join(t.TempDir(), "a.tar.gz")
	second := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := writeComposeBundle(first, entries); err != nil {
		t.Fatal(err)
	}
	if err := writeComposeBundle(second, entries); err != nil {
		t.Fatal(err)
	}
	firstData, _ := os.ReadFile(first)
	secondData, _ := os.ReadFile(second)
	if string(firstData) != string(secondData) {
		t.Fatal("compose bundle is not deterministic")
	}
}

func TestRegistryHostOf(t *testing.T) {
	for _, entry := range []struct{ repository, host string }{
		{"127.0.0.1:5099/t39/quoin", "127.0.0.1:5099"},
		{"ghcr.io/suknna/quoin", "ghcr.io"},
		{"registry.example.com:5000/ns/quoin", "registry.example.com:5000"},
	} {
		if got := registryHostOf(entry.repository); got != entry.host {
			t.Fatalf("registryHostOf(%q) = %q want %q", entry.repository, got, entry.host)
		}
	}
}
