// Package inputs loads the frozen release input locks from the one-way
// embedded contract projections and derives the machine-checked build inputs:
// digest-pinned base images, per-architecture apt version pins and the locked
// Playwright/Chromium browser artifacts (OPS-IMAGE-002/003/006,
// OPS-SUPPLY-002). The lock files are frozen contracts; this package is the
// only reader production build code needs and never mutates them.
package inputs

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	"gopkg.in/yaml.v3"
)

// Architectures is the closed platform set of every release.
var Architectures = []string{"linux/amd64", "linux/arm64"}

// Components is the closed component set of every release.
var Components = []string{"lintel", "plinth", "quoin", "stele"}

// BaseImage is one digest-locked multi-platform base.
type BaseImage struct {
	Repository  string            `yaml:"repository"`
	SourceTag   string            `yaml:"source_tag"`
	IndexDigest string            `yaml:"index_digest"`
	Platforms   map[string]string `yaml:"platforms"`
}

// SourceFile is a locked upstream source path and its SHA-256.
type SourceFile struct {
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256"`
}

// BrowserArtifact is one locked per-architecture Chromium download.
type BrowserArtifact struct {
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
	Bytes  int64  `yaml:"bytes"`
}

// PlaywrightLock carries the frozen Playwright tag, its two locked upstream
// sources and the per-architecture browser artifacts.
type PlaywrightLock struct {
	Version          string                     `yaml:"version"`
	SourceTag        string                     `yaml:"source_tag"`
	BrowsersJSON     SourceFile                 `yaml:"browsers_json"`
	RegistrySource   SourceFile                 `yaml:"registry_source"`
	ChromiumRevision string                     `yaml:"chromium_revision"`
	ChromiumVersion  string                     `yaml:"chromium_version"`
	Artifacts        map[string]BrowserArtifact `yaml:"artifacts"`
}

// LintelPackage is one locked lintel runtime package with full per-architecture
// Debian versions.
type LintelPackage struct {
	Name     string            `yaml:"name"`
	Versions map[string]string `yaml:"versions"`
}

// LintelRuntime is the frozen lintel browser runtime package set.
type LintelRuntime struct {
	PlaywrightNativeDeps SourceFile `yaml:"playwright_native_deps"`
	PackageSource        struct {
		BaseImage         string `yaml:"base_image"`
		InstallRecommends bool   `yaml:"install_recommends"`
	} `yaml:"package_source"`
	Executables []string        `yaml:"executables"`
	Packages    []LintelPackage `yaml:"packages"`
}

// PlinthTools is the frozen plinth worker tool catalog. Only the package
// lock is consumed by image builds; the bash tool contract itself belongs to
// the Plinth runtime.
type PlinthTools struct {
	Owner     string `yaml:"owner"`
	BaseImage string `yaml:"base_image"`
	Packages  []struct {
		Name     string            `yaml:"name"`
		Versions map[string]string `yaml:"versions"`
	} `yaml:"packages"`
}

type lockDocument struct {
	ContractVersion int                  `yaml:"contract_version"`
	Architectures   []string             `yaml:"architectures"`
	BaseImages      map[string]BaseImage `yaml:"base_images"`
	ComponentBases  map[string]string    `yaml:"component_bases"`
	Playwright      PlaywrightLock       `yaml:"playwright"`
	LintelRuntime   LintelRuntime        `yaml:"lintel_runtime"`
}

// Lock is the parsed frozen release input authority.
type Lock struct {
	ContractVersion int
	BaseImages      map[string]BaseImage
	ComponentBases  map[string]string
	Playwright      PlaywrightLock
	LintelRuntime   LintelRuntime
	PlinthTools     PlinthTools
}

// Load decodes both frozen locks with strict unknown-field rejection and
// checks the closed architecture/component vocabulary.
func Load() (Lock, error) {
	var document lockDocument
	if err := decodeStrict(gen.ReleaseInputsYAML, &document); err != nil {
		return Lock{}, fmt.Errorf("release inputs lock: %w", err)
	}
	if document.ContractVersion != 1 {
		return Lock{}, fmt.Errorf("unsupported release inputs contract_version %d", document.ContractVersion)
	}
	if len(document.Architectures) != 2 || document.Architectures[0] != Architectures[0] || document.Architectures[1] != Architectures[1] {
		return Lock{}, fmt.Errorf("release inputs architectures %v are not the frozen pair %v", document.Architectures, Architectures)
	}
	var tools PlinthTools
	// The worker tool catalog carries bash/landlock/forbidden-action sections
	// owned by the Plinth runtime contract; the image build consumes only the
	// package lock, so the catalog decodes leniently here while the contract
	// gate keeps validating the full document against its frozen schema.
	if err := yaml.Unmarshal(gen.PlinthWorkerToolsYAML, &tools); err != nil {
		return Lock{}, fmt.Errorf("plinth worker tools catalog: %w", err)
	}
	if len(tools.Packages) == 0 {
		return Lock{}, fmt.Errorf("plinth worker tools catalog has no packages")
	}
	for _, component := range Components {
		if document.ComponentBases[component] == "" {
			return Lock{}, fmt.Errorf("component %s has no locked base image", component)
		}
		if _, ok := document.BaseImages[document.ComponentBases[component]]; !ok {
			return Lock{}, fmt.Errorf("component %s references unknown base %q", component, document.ComponentBases[component])
		}
	}
	for _, base := range document.BaseImages {
		if err := checkDigest(base.IndexDigest); err != nil {
			return Lock{}, fmt.Errorf("base %s index digest: %w", base.Repository, err)
		}
		for platform, digest := range base.Platforms {
			if platform != "linux/amd64" && platform != "linux/arm64" {
				return Lock{}, fmt.Errorf("base %s locks unknown platform %q", base.Repository, platform)
			}
			if err := checkDigest(digest); err != nil {
				return Lock{}, fmt.Errorf("base %s platform %s digest: %w", base.Repository, platform, err)
			}
		}
	}
	for _, platform := range Architectures {
		if _, ok := document.Playwright.Artifacts[platform]; !ok {
			return Lock{}, fmt.Errorf("playwright artifact for %s is missing", platform)
		}
	}
	return Lock{
		ContractVersion: document.ContractVersion,
		BaseImages:      document.BaseImages,
		ComponentBases:  document.ComponentBases,
		Playwright:      document.Playwright,
		LintelRuntime:   document.LintelRuntime,
		PlinthTools:     tools,
	}, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	return decoder.Decode(target)
}

func checkDigest(digest string) error {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != 7+64 {
		return fmt.Errorf("malformed sha256 digest %q", digest)
	}
	return nil
}

// Base returns the digest-locked base of one component.
func (lock Lock) Base(component string) (BaseImage, error) {
	key, ok := lock.ComponentBases[component]
	if !ok {
		return BaseImage{}, fmt.Errorf("unknown component %q", component)
	}
	base, ok := lock.BaseImages[key]
	if !ok {
		return BaseImage{}, fmt.Errorf("component %q base %q missing", component, key)
	}
	return base, nil
}

// BuildArgs returns the docker build arguments that pin one component's base
// image to the locked digests. The Dockerfile ARG defaults are the same
// authority; explicit arguments keep the pinned pull identical on mirrored
// networks.
func (lock Lock) BuildArgs(component string) ([]string, error) {
	base, err := lock.Base(component)
	if err != nil {
		return nil, err
	}
	switch {
	case strings.Contains(base.Repository, "distroless"):
		return []string{"DISTROLESS_REPO=" + base.Repository, "DISTROLESS_INDEX_DIGEST=" + base.IndexDigest}, nil
	case strings.Contains(base.Repository, "debian"):
		return []string{"DEBIAN_REPO=" + base.Repository, "DEBIAN_INDEX_DIGEST=" + base.IndexDigest}, nil
	default:
		return nil, fmt.Errorf("component %s base %s has no Dockerfile ARG mapping", component, base.Repository)
	}
}

// LintelAPTSpecs returns the per-architecture name=version apt arguments for
// the lintel browser runtime packages (OPS-IMAGE-006: digest-pinned base,
// --no-install-recommends and per-package name=version installs).
func (lock Lock) LintelAPTSpecs(arch string) ([]string, error) {
	return lock.aptSpecs("lintel", arch)
}

// PlinthAPTSpecs returns the per-architecture name=version apt arguments for
// the plinth worker tools.
func (lock Lock) PlinthAPTSpecs(arch string) ([]string, error) {
	return lock.aptSpecs("plinth", arch)
}

// aptSpecs projects one component's locked packages into sorted name=version
// arguments for one architecture.
func (lock Lock) aptSpecs(component, arch string) ([]string, error) {
	specs, err := lock.DebianSpecs(component)
	if err != nil {
		return nil, err
	}
	perArch, ok := specs["linux/"+arch]
	if !ok {
		return nil, fmt.Errorf("component %s has no linux/%s package set", component, arch)
	}
	arguments := make([]string, 0, len(perArch))
	for _, name := range sortedNames(perArch) {
		arguments = append(arguments, name+"="+perArch[name])
	}
	return arguments, nil
}

func sortedNames(mapping map[string]string) []string {
	names := make([]string, 0, len(mapping))
	for name := range mapping {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DebianSpecs returns the per-architecture name=version apt pin map of one
// Debian-based component; the image mirror checks and the builder consume the
// same projection.
func (lock Lock) DebianSpecs(component string) (map[string]map[string]string, error) {
	var packages map[string]map[string]string
	switch component {
	case "plinth":
		packages = make(map[string]map[string]string, len(lock.PlinthTools.Packages))
		for _, entry := range lock.PlinthTools.Packages {
			packages[entry.Name] = entry.Versions
		}
	case "lintel":
		packages = make(map[string]map[string]string, len(lock.LintelRuntime.Packages))
		for _, entry := range lock.LintelRuntime.Packages {
			packages[entry.Name] = entry.Versions
		}
	default:
		return nil, fmt.Errorf("component %q has no Debian package lock", component)
	}
	specs := map[string]map[string]string{"linux/amd64": {}, "linux/arm64": {}}
	for name, versions := range packages {
		for platform := range specs {
			version, ok := versions[platform]
			if !ok || version == "" {
				return nil, fmt.Errorf("%s package %s has no %s version", component, name, platform)
			}
			specs[platform][name] = version
		}
	}
	if len(packages) == 0 {
		return nil, fmt.Errorf("component %q has an empty package lock", component)
	}
	return specs, nil
}

// ChromiumBuildArgs returns the docker build arguments that pin the lintel
// Chromium download to the locked Playwright artifacts (OPS-IMAGE-002).
func (lock Lock) ChromiumBuildArgs() []string {
	arguments := []string{}
	for arch, argument := range map[string][2]string{
		"amd64": {"CHROMIUM_AMD64_URL", "CHROMIUM_AMD64_SHA256"},
		"arm64": {"CHROMIUM_ARM64_URL", "CHROMIUM_ARM64_SHA256"},
	} {
		artifact := lock.Playwright.Artifacts["linux/"+arch]
		arguments = append(arguments,
			argument[0]+"="+artifact.URL,
			argument[1]+"="+artifact.SHA256,
		)
	}
	sort.Strings(arguments)
	return arguments
}
