package subjects_test

import (
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/release/subjects"
)

// runArtifactAssertions inspects the produced artifacts without executing
// any foreign-architecture binary: the Compose bundle carries exactly the
// closed OPS-RELEASE-003 entry set with digest-pinned images and no mutable
// tags, and the helpers are static ELF binaries for their declared
// architectures.
func runArtifactAssertions(t *testing.T, recorder *evidence, inventory *subjects.Inventory, work string) map[string]map[string]any {
	t.Helper()
	assertions := map[string]map[string]any{}
	names, _ := subjects.Names(releaseVersion)
	bundlePath := work + "/" + names.Compose

	entries := strings.Split(strings.TrimSpace(recorder.run("compose-bundle-list", nil, 0,
		"tar", "-tzf", bundlePath)), "\n")
	closed := map[string]bool{
		"compose.yaml": true, "install-minimal.yaml": true,
		"schema/deployment-config.schema.json": true, "quoin-deploy": true,
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if !closed[entry] {
			t.Fatalf("compose bundle carries out-of-contract entry %q", entry)
		}
		seen[entry] = true
	}
	if len(seen) != len(closed) {
		t.Fatalf("compose bundle misses entries: %v", entries)
	}
	assertions["compose-closed-set"] = map[string]any{
		"expected": "exactly compose.yaml, install-minimal.yaml, schema/deployment-config.schema.json, quoin-deploy",
		"actual":   entries,
	}

	composeYAML, err := readComposeBundleEntry(t, bundlePath, "compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	imageLines := []string{}
	for _, component := range subjects.Components {
		image := inventory.Images[component]
		pinned := image.Repository + "@" + image.IndexDigest
		if !strings.Contains(string(composeYAML), pinned) {
			t.Fatalf("compose.yaml does not pin %s to %s", component, pinned)
		}
		imageLines = append(imageLines, pinned)
	}
	if strings.Contains(string(composeYAML), ":latest") {
		t.Fatal("compose.yaml references a mutable tag")
	}
	schemaEntry, err := readComposeBundleEntry(t, bundlePath, "schema/deployment-config.schema.json")
	if err != nil || len(schemaEntry) == 0 {
		t.Fatalf("schema entry unreadable: %v", err)
	}
	assertions["compose-digest-pinned"] = map[string]any{
		"expected": "all four services pinned to their index digests, schema included, no latest",
		"actual":   imageLines,
	}
	recorder.observe("compose-bundle-compose.yaml", string(composeYAML))

	for platform, elf := range map[string]string{
		"linux/amd64": "ELF 64-bit LSB executable, x86-64",
		"linux/arm64": "ELF 64-bit LSB executable, ARM aarch64",
	} {
		path := work + "/" + names.Helper[platform]
		fileType := recorder.run("helper-file-"+strings.TrimPrefix(platform, "linux/"), nil, 0, "file", "-b", path)
		if !strings.Contains(fileType, elf) {
			t.Fatalf("helper %s is %q, want %q", platform, fileType, elf)
		}
		if !strings.Contains(fileType, "statically linked") {
			t.Fatalf("helper %s is not static: %q", platform, fileType)
		}
		// No foreign-architecture execution: the arm64 helper is inspected
		// only. This is the QEMU-runtime-claim boundary.
	}
	assertions["helper-binaries"] = map[string]any{
		"expected": "static ELF binaries per declared architecture, inspected with file(1), never executed cross-architecture",
		"actual":   "both match their architecture and are statically linked",
	}
	return assertions
}
