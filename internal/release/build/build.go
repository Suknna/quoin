// Command build is the release-subject builder behind ci/build-release-subjects
// (T39). From one source checkout and one release version it builds, per
// native or explicitly-declared emulated platform:
//
//   - the four component images with BuildKit SPDX SBOM and SLSA provenance
//     attestations, pushed per platform by tag and merged into one OCI index;
//   - the Helm chart package and its OCI push;
//   - the digest-pinned Compose bundle;
//   - the two static quoin-deploy helpers.
//
// It writes the measured subject inventory the final Release manifest will
// reference (T42). It never signs; the CI workflow signs with cosign and the
// supplychain gate verifies.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/release/inputs"
	"github.com/Suknna/quoin/internal/release/subjects"
)

type platformMode struct {
	Platform string // linux/amd64 | linux/arm64
	Mode     string // native | emulated
	Arch     string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "build-release-subjects: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	registry  string // e.g. 127.0.0.1:5099/t39 or ghcr.io/suknna
	version   string
	chartOCI  string
	builder   string
	work      string
	out       string
	platforms []platformMode
	logs      string
	stage     string // all | images | assemble
}

func run(arguments []string) error {
	if len(arguments) > 0 && arguments[0] == "verify" {
		return verifyMode(arguments[1:])
	}
	options, err := parseArguments(arguments)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(options.logs, 0o755); err != nil {
		return err
	}
	lock, err := inputs.Load()
	if err != nil {
		return err
	}
	switch options.stage {
	case "images":
		return buildImagePlatforms(options, lock)
	case "assemble":
		inventory, err := assembleInventory(options, lock)
		if err != nil {
			return err
		}
		return writeInventory(options, inventory)
	case "all":
		if err := buildImagePlatforms(options, lock); err != nil {
			return err
		}
		inventory, err := assembleInventory(options, lock)
		if err != nil {
			return err
		}
		return writeInventory(options, inventory)
	default:
		return fmt.Errorf("unknown stage %q", options.stage)
	}
}

// writeInventory validates and persists the final subject inventory.
func writeInventory(options *options, inventory *subjects.Inventory) error {
	if err := inventory.Validate(); err != nil {
		return fmt.Errorf("inventory validation: %w", err)
	}
	data, err := inventory.Marshal()
	if err != nil {
		return err
	}
	if err := os.WriteFile(options.out, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("subjects inventory written to %s\n", options.out)
	return nil
}

// assembleInventory merges the per-platform build fragments into the full
// subject inventory and completes the chart, compose and helper subjects.
func assembleInventory(options *options, lock inputs.Lock) (*subjects.Inventory, error) {
	inventory := &subjects.Inventory{
		Schema:         subjects.Schema,
		ReleaseVersion: options.version,
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		Images:         map[string]subjects.ImageSubject{},
		Helpers:        map[string]subjects.BlobSubject{},
		Bundles:        bundleNameMap(),
		Browser: subjects.BrowserSubjects{
			PlaywrightVersion: lock.Playwright.Version,
			ChromiumRevision:  lock.Playwright.ChromiumRevision,
			Artifacts:         map[string]subjects.BlobSubject{},
		},
	}
	var err error
	inventory.SourceCommit, err = gitCommit()
	if err != nil {
		return nil, err
	}
	for platform, artifact := range lock.Playwright.Artifacts {
		inventory.Browser.Artifacts[platform] = subjects.BlobSubject{
			AssetName: filepath.Base(artifact.URL),
			SHA256:    artifact.SHA256,
		}
	}
	if err := mergeImageFragments(options, inventory); err != nil {
		return nil, err
	}
	if err := buildChart(options, inventory); err != nil {
		return nil, err
	}
	if err := buildComposeBundle(options, inventory); err != nil {
		return nil, err
	}
	if err := buildHelpers(options, inventory); err != nil {
		return nil, err
	}
	return inventory, nil
}

func parseArguments(arguments []string) (*options, error) {
	options := &options{platforms: []platformMode{}}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		value := func() (string, error) {
			index++
			if index >= len(arguments) {
				return "", fmt.Errorf("%s needs a value", argument)
			}
			return arguments[index], nil
		}
		switch argument {
		case "-registry":
			registry, err := value()
			if err != nil {
				return nil, err
			}
			options.registry = registry
		case "-version":
			version, err := value()
			if err != nil {
				return nil, err
			}
			options.version = version
		case "-chart-oci":
			chartOCI, err := value()
			if err != nil {
				return nil, err
			}
			options.chartOCI = chartOCI
		case "-builder":
			builder, err := value()
			if err != nil {
				return nil, err
			}
			options.builder = builder
		case "-work":
			work, err := value()
			if err != nil {
				return nil, err
			}
			options.work = work
		case "-out":
			out, err := value()
			if err != nil {
				return nil, err
			}
			options.out = out
		case "-logs":
			logs, err := value()
			if err != nil {
				return nil, err
			}
			options.logs = logs
		case "-platform":
			specification, err := value()
			if err != nil {
				return nil, err
			}
			entry, err := parsePlatform(specification)
			if err != nil {
				return nil, err
			}
			options.platforms = append(options.platforms, entry)
		case "-stage":
			stage, err := value()
			if err != nil {
				return nil, err
			}
			options.stage = stage
		default:
			return nil, fmt.Errorf("unknown argument %q", argument)
		}
	}
	if options.stage == "" {
		options.stage = "all"
	}
	if options.registry == "" || options.version == "" || options.work == "" {
		return nil, fmt.Errorf("-registry, -version and -work are required")
	}
	if options.stage != "images" && options.out == "" {
		return nil, fmt.Errorf("-out is required for stage %s", options.stage)
	}
	if options.stage != "assemble" && options.chartOCI == "" {
		return nil, fmt.Errorf("-chart-oci is required for stage %s", options.stage)
	}
	if options.stage != "assemble" && len(options.platforms) == 0 {
		return nil, fmt.Errorf("at least one -platform linux/<arch>=<native|emulated> is required")
	}
	if options.logs == "" {
		options.logs = filepath.Join(options.work, "logs")
	}
	if _, err := subjects.Names(options.version); err != nil {
		return nil, err
	}
	return options, nil
}

func parsePlatform(specification string) (platformMode, error) {
	parts := strings.Split(specification, "=")
	if len(parts) != 2 {
		return platformMode{}, fmt.Errorf("platform %q must be linux/<arch>=<native|emulated>", specification)
	}
	platform, mode := parts[0], parts[1]
	if platform != "linux/amd64" && platform != "linux/arm64" {
		return platformMode{}, fmt.Errorf("platform %q is outside the frozen pair", platform)
	}
	if mode != "native" && mode != "emulated" {
		return platformMode{}, fmt.Errorf("platform mode %q must be native or emulated", mode)
	}
	return platformMode{Platform: platform, Mode: mode, Arch: strings.TrimPrefix(platform, "linux/")}, nil
}

// command runs one external command, logging output and exit code.
func command(options *options, name string, argv ...string) (string, error) {
	started := time.Now()
	process := exec.Command(argv[0], argv[1:]...)
	if options.work != "" {
		process.Dir = repoRoot()
	}
	var output bytes.Buffer
	process.Stdout, process.Stderr = &output, &output
	err := process.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		code = -1
	}
	logPath := filepath.Join(options.logs, name+".log")
	if writeErr := os.WriteFile(logPath, output.Bytes(), 0o644); writeErr != nil {
		return "", writeErr
	}
	fmt.Printf("[%s] %s (exit %d, %.1fs)\n", name, strings.Join(argv, " "), code, time.Since(started).Seconds())
	if err != nil {
		return output.String(), fmt.Errorf("%s exited %d (log: %s)", argv[0], code, logPath)
	}
	return output.String(), nil
}

func repoRoot() string {
	directory, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "."
		}
		directory = parent
	}
}

func gitCommit() (string, error) {
	output, err := exec.Command("git", "-C", repoRoot(), "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git commit: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// imageFragment is the per-platform build output shared between the native
// matrix builds and the assemble stage.
type imageFragment struct {
	Platform string                   `json:"platform"`
	Mode     string                   `json:"mode"`
	Images   map[string]fragmentImage `json:"images"`
}

type fragmentImage struct {
	PlatformDigest string   `json:"platform_manifest_digest"`
	Attestations   []string `json:"attestation_manifests"`
}

// buildImagePlatforms builds and pushes every component for the declared
// platforms with SBOM/provenance attestations and writes one fragment file
// per platform under the work directory.
func buildImagePlatforms(options *options, lock inputs.Lock) error {
	goproxy := os.Getenv("QUOIN_IMAGE_GOPROXY")
	if goproxy == "" {
		if env := os.Getenv("GOPROXY"); env != "" {
			goproxy = env
		} else {
			goproxy = outputOf("go", "env", "GOPROXY")
		}
	}
	for _, platform := range options.platforms {
		fragment := imageFragment{
			Platform: platform.Platform,
			Mode:     platform.Mode,
			Images:   map[string]fragmentImage{},
		}
		for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
			repository := options.registry + "/" + component
			baseArgs, err := lock.BuildArgs(component)
			if err != nil {
				return err
			}
			arguments := []string{"docker", "buildx", "build", "--builder", options.builder,
				"--platform", platform.Platform,
				"--sbom=true", "--provenance=mode=min",
				"-f", "deploy/images/" + component + "/Dockerfile",
			}
			for _, argument := range baseArgs {
				arguments = append(arguments, "--build-arg", argument)
			}
			arguments = append(arguments, "--build-arg", "GOPROXY="+goproxy, "-t", repository+":"+platform.Arch, "--push", ".")
			if _, err := command(options, "build-"+component+"-"+platform.Arch, arguments...); err != nil {
				return fmt.Errorf("%s %s: %w", component, platform.Platform, err)
			}
			host, repository := splitRegistry(repository)
			indexDigest, err := registryTagDigest(host, repository, platform.Arch)
			if err != nil {
				return err
			}
			platformDigest, attestations, err := platformManifestDigest(host, repository, indexDigest)
			if err != nil {
				return err
			}
			fragment.Images[component] = fragmentImage{PlatformDigest: platformDigest, Attestations: attestations}
		}
		encoded, err := json.MarshalIndent(fragment, "", "  ")
		if err != nil {
			return err
		}
		path := filepath.Join(options.work, "images-"+platform.Arch+".json")
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			return err
		}
		fmt.Printf("platform fragment written to %s\n", path)
	}
	return nil
}

// mergeImageFragments reads both per-platform fragments, merges the pushed
// per-platform indexes into the component OCI indexes and fills the image
// subjects of the inventory.
func mergeImageFragments(options *options, inventory *subjects.Inventory) error {
	fragments := map[string]imageFragment{}
	for _, arch := range []string{"amd64", "arm64"} {
		raw, err := os.ReadFile(filepath.Join(options.work, "images-"+arch+".json"))
		if err != nil {
			return fmt.Errorf("platform fragment %s missing (run the %s build first): %w", arch, arch, err)
		}
		var fragment imageFragment
		if err := json.Unmarshal(raw, &fragment); err != nil {
			return err
		}
		if fragment.Platform != "linux/"+arch || (fragment.Mode != "native" && fragment.Mode != "emulated") {
			return fmt.Errorf("fragment %s declares %s/%s", arch, fragment.Platform, fragment.Mode)
		}
		fragments[fragment.Platform] = fragment
	}
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		repository := options.registry + "/" + component
		subject := subjects.ImageSubject{
			Repository:     repository,
			Platforms:      map[string]string{},
			BuildExecution: map[string]string{},
			Attestations:   map[string][]string{},
		}
		for _, platform := range subjects.Platforms {
			fragment, ok := fragments[platform]
			if !ok {
				return fmt.Errorf("%s: %s fragment missing", component, platform)
			}
			image, ok := fragment.Images[component]
			if !ok {
				return fmt.Errorf("%s: %s fragment has no %s image", component, platform, component)
			}
			subject.Platforms[platform] = image.PlatformDigest
			subject.BuildExecution[platform] = fragment.Mode
			subject.Attestations[platform] = image.Attestations
		}
		// Merge the per-platform indexes (attestations travel with them).
		if _, err := command(options, "index-"+component,
			"docker", "buildx", "imagetools", "create", "-t", repository+":index",
			repository+":amd64", repository+":arm64"); err != nil {
			return err
		}
		host, repository := splitRegistry(repository)
		digest, err := registryTagDigest(host, repository, "index")
		if err != nil {
			return err
		}
		subject.IndexDigest = digest
		inventory.Images[component] = subject
	}
	return nil
}
