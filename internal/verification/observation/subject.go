package observation

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Suknna/quoin/internal/release/inputs"
)

// SubjectSchema is the stable kind of the browser-subject document the CI
// entrypoints freeze per cell before any browser runs.
const SubjectSchema = "quoin-ui-browser-subject-v1"

// BrandedResolutionSchema is the stable kind of the one-shot branded Chrome
// resolution frozen per qualification invocation
// (VERIFY-MATRIX-003: amd64-only, resolved at qualification start, digest
// frozen locally because the upstream publishes none).
const BrandedResolutionSchema = "quoin-branded-chrome-resolution-v1"

// BrowserSubject is the executable binding of one matrix cell: where the
// artifact came from, its verified digest and the observed build string.
// Every automation result and typed observation must bind back to exactly
// this digest (VERIFY-OBSERVATION-002).
type BrowserSubject struct {
	Schema         string `json:"schema"`
	CellID         string `json:"cell_id"`
	BrowserSubject string `json:"browser_subject"`
	Architecture   string `json:"architecture"`
	URL            string `json:"url"`
	SHA256         string `json:"sha256"` // bare hex, matches the lock format
	Bytes          int64  `json:"bytes"`
	Version        string `json:"version"`    // upstream-declared version when known
	Build          string `json:"build"`      // observed `<executable> --version` output
	ExecutablePath string `json:"executable"` // verified local executable
}

// ResolveSubject derives the artifact binding of one cell from the frozen
// authorities: playwright_chromium from the release input lock (per
// architecture), branded_chrome from the invocation's frozen resolution
// document. The caller downloads, verifies the digest and fills Build and
// ExecutablePath; this function only resolves the authority side.
func ResolveSubject(cellID string, brandedPath string) (BrowserSubject, error) {
	key, err := parseCellID(cellID)
	if err != nil {
		return BrowserSubject{}, err
	}
	subject := BrowserSubject{
		Schema:         SubjectSchema,
		CellID:         cellID,
		BrowserSubject: key.BrowserSubject,
		Architecture:   key.Architecture,
	}
	switch key.BrowserSubject {
	case SubjectPlaywrightChromium:
		lock, err := inputs.Load()
		if err != nil {
			return BrowserSubject{}, fmt.Errorf("release inputs: %w", err)
		}
		artifact, ok := lock.Playwright.Artifacts[key.Architecture]
		if !ok {
			return BrowserSubject{}, fmt.Errorf("no locked playwright artifact for %s", key.Architecture)
		}
		subject.URL = artifact.URL
		subject.SHA256 = artifact.SHA256
		subject.Bytes = artifact.Bytes
		subject.Version = lock.Playwright.ChromiumVersion
	case SubjectBrandedChrome:
		if key.Architecture != "linux/amd64" {
			return BrowserSubject{}, fmt.Errorf("branded chrome cell %s is outside its amd64-only vocabulary", cellID)
		}
		resolution, err := LoadBrandedResolution(brandedPath)
		if err != nil {
			return BrowserSubject{}, err
		}
		subject.URL = resolution.URL
		subject.SHA256 = resolution.SHA256
		subject.Bytes = resolution.Bytes
		subject.Version = resolution.Version
	default:
		return BrowserSubject{}, fmt.Errorf("unknown browser subject %q", key.BrowserSubject)
	}
	return subject, nil
}

// LoadSubject reads a frozen subject document (the setup phase output).
func LoadSubject(path string) (BrowserSubject, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return BrowserSubject{}, err
	}
	var subject BrowserSubject
	if err := json.Unmarshal(body, &subject); err != nil {
		return BrowserSubject{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if subject.Schema != SubjectSchema {
		return BrowserSubject{}, fmt.Errorf("%s carries schema %q, expected %q", path, subject.Schema, SubjectSchema)
	}
	if subject.ExecutablePath == "" || subject.Build == "" || subject.SHA256 == "" {
		return BrowserSubject{}, fmt.Errorf("%s is not a verified subject (executable/build/sha256 required)", path)
	}
	return subject, nil
}

// BrandedResolution is the frozen result of resolving the branded Chrome
// build once per invocation. Upstream is resolved (current stable), then
// this record freezes version/URL/digest so every branded cell of the
// invocation binds the same build.
type BrandedResolution struct {
	Schema     string `json:"schema"`
	ResolvedAt string `json:"resolved_at"`
	Channel    string `json:"channel"`
	Version    string `json:"version"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256"` // digest computed locally at freeze time
	Bytes      int64  `json:"bytes"`
}

// LoadBrandedResolution reads and validates a frozen branded-Chrome
// resolution document.
func LoadBrandedResolution(path string) (BrandedResolution, error) {
	if path == "" {
		return BrandedResolution{}, fmt.Errorf("branded chrome resolution path is empty: resolve and freeze the build before the first branded cell")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return BrandedResolution{}, err
	}
	var resolution BrandedResolution
	if err := json.Unmarshal(body, &resolution); err != nil {
		return BrandedResolution{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if resolution.Schema != BrandedResolutionSchema {
		return BrandedResolution{}, fmt.Errorf("%s carries schema %q, expected %q", path, resolution.Schema, BrandedResolutionSchema)
	}
	if resolution.Version == "" || resolution.URL == "" || !isBareHex64(resolution.SHA256) {
		return BrandedResolution{}, fmt.Errorf("%s is not a complete frozen resolution (version/url/sha256 required)", path)
	}
	return resolution, nil
}

func isBareHex64(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}
