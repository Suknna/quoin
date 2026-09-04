package inputs

import (
	"strings"
	"testing"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

func TestLoadDecodesFrozenLock(t *testing.T) {
	lock, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if lock.ContractVersion != 1 {
		t.Fatalf("contract version %d", lock.ContractVersion)
	}
	for _, component := range Components {
		base, err := lock.Base(component)
		if err != nil {
			t.Fatalf("%s: %v", component, err)
		}
		if base.IndexDigest == "" || len(base.Platforms) != 2 {
			t.Fatalf("%s base incomplete: %+v", component, base)
		}
		for _, platform := range Architectures {
			if base.Platforms[platform] == "" {
				t.Fatalf("%s base missing %s platform digest", component, platform)
			}
		}
	}
	if len(lock.Playwright.Artifacts) != 2 {
		t.Fatalf("playwright artifacts %v", lock.Playwright.Artifacts)
	}
	if lock.Playwright.ChromiumRevision == "" || lock.Playwright.BrowsersJSON.SHA256 == "" {
		t.Fatalf("playwright source locks incomplete: %+v", lock.Playwright)
	}
}

func TestAPTSpeDerivePerArchitecture(t *testing.T) {
	lock, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		lintel, err := lock.LintelAPTSpecs(arch)
		if err != nil {
			t.Fatal(err)
		}
		if len(lintel) != len(lock.LintelRuntime.Packages) {
			t.Fatalf("lintel %s spec %v misses packages", arch, lintel)
		}
		for _, spec := range lintel {
			if !strings.Contains(spec, "=") {
				t.Fatalf("unpinned lintel package %q", spec)
			}
		}
		plinth, err := lock.PlinthAPTSpecs(arch)
		if err != nil {
			t.Fatal(err)
		}
		if len(plinth) != len(lock.PlinthTools.Packages) {
			t.Fatalf("plinth %s spec %v misses tools", arch, plinth)
		}
	}
	if _, err := lock.LintelAPTSpecs("riscv64"); err == nil {
		t.Fatal("unknown architecture must fail")
	}
}

func TestBuildArgsPinLockedDigests(t *testing.T) {
	lock, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		args, err := lock.BuildArgs(component)
		if err != nil {
			t.Fatalf("%s: %v", component, err)
		}
		base, _ := lock.Base(component)
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, base.IndexDigest) {
			t.Fatalf("%s build args %v do not pin index digest %s", component, args, base.IndexDigest)
		}
	}
	if _, err := lock.BuildArgs("unknown"); err == nil {
		t.Fatal("unknown component must fail")
	}
}

func TestStrictDecodingRejectsUnknownFields(t *testing.T) {
	tampered := strings.Replace(string(gen.ReleaseInputsYAML), "contract_version: 1", "extra_field: x\ncontract_version: 1", 1)
	var document lockDocument
	if err := decodeStrict([]byte(tampered), &document); err == nil {
		t.Fatal("unknown release-inputs field must fail strict decode")
	}
}
