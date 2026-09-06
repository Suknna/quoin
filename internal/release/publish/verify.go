package main

import (
	"crypto/sha256"
	"strings"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Suknna/quoin/internal/release/manifest"
	"github.com/Suknna/quoin/internal/release/offline"
	"github.com/Suknna/quoin/internal/release/signing"
	"github.com/Suknna/quoin/internal/release/subjects"
	"github.com/Suknna/quoin/internal/release/supplychain"
)

// gateCheck is one publish-gate result with a stable code and a recovery
// hint (OPS-VERIFY-003 structured checklist shape).
type gateCheck struct {
	Code     string `json:"code"`
	Subject  string `json:"subject"`
	Result   string `json:"result"`
	Error    string `json:"error,omitempty"`
	Recovery string `json:"recovery,omitempty"`
}

type gate struct {
	checks []gateCheck
	failed bool
}

func (gate *gate) pass(code, subject string) {
	gate.checks = append(gate.checks, gateCheck{Code: code, Subject: subject, Result: "passed"})
}

func (gate *gate) fail(code, subject string, err error, recovery string) {
	gate.failed = true
	gate.checks = append(gate.checks, gateCheck{Code: code, Subject: subject, Result: "failed", Error: err.Error(), Recovery: recovery})
}

// verifyMode is the publish gate over an assembled, signed release
// directory. It reads only; the report goes to -report outside the
// release directory, and any failed check exits non-zero.
func verifyMode(arguments []string) error {
	flags := map[string]string{}
	parser := &flagParser{arguments: arguments, booleans: map[string]bool{"-registry-insecure": true}}
	for {
		flag, value, hasValue, err := parser.next()
		if err != nil {
			return err
		}
		if !hasValue && flag == "" {
			break
		}
		switch flag {
		case "-registry-insecure":
			// Boolean presence flag; read through boolFlag below.
		case "-release", "-contracts", "-trust-root", "-identity", "-issuer", "-import-registry", "-report", "-allow-delegated":
			flags[flag] = value
		default:
			return fmt.Errorf("unknown verify argument %q", flag)
		}
	}
	for _, required := range []string{"-release", "-contracts", "-trust-root", "-identity", "-issuer"} {
		if flags[required] == "" {
			return fmt.Errorf("%s is required", required)
		}
	}
	insecure := parser.boolFlag("-registry-insecure")
	releaseDir := flags["-release"]
	reportPath := flags["-report"]
	if reportPath == "" {
		reportPath = "publish-gate-report.json"
	}

	result := &gate{}
	manifestBytes, document, inventory := loadRelease(result, releaseDir)
	if result.failed {
		return finishGate(result, reportPath)
	}
	rootPEM, err := os.ReadFile(flags["-trust-root"])
	if err != nil {
		return err
	}
	trust := supplychain.Trust{RootPEM: rootPEM, IdentityRegexp: flags["-identity"], Issuer: flags["-issuer"]}

	// One-way contract regeneration equality: recomputing the contract
	// facts from the frozen authority must reproduce the manifest bytes.
	recomputed, err := manifest.Contracts(flags["-contracts"])
	if err != nil {
		result.fail("manifest.contracts", "frozen contracts", err, "the checkout contracts directory is the only authority")
		return finishGate(result, reportPath)
	}
	if *recomputed != document.Contracts {
		result.fail("manifest.contracts", "contracts block", fmt.Errorf("recomputed contract facts differ from the manifest"), "rebuild the manifest from the frozen contracts")
	} else {
		result.pass("manifest.contracts", "one-way regeneration equal")
	}

	// The evidence bindings: every category re-verifies and its bundle
	// digest equals the recorded evidence_sha256 (VERIFY-EVIDENCE-004).
	evidence := map[string]manifest.EvidenceInput{}
	for _, category := range manifest.Categories {
		input := manifest.EvidenceInput{}
		var err error
		if input.Bundle, err = os.ReadFile(filepath.Join(releaseDir, "verification", category+".bundle.json")); err != nil {
			result.fail("evidence.bundle", category, err, "finalize must stage all seven categorized bundles")
			continue
		}
		if payload, readErr := os.ReadFile(filepath.Join(releaseDir, "verification", category+".payload")); readErr == nil {
			input.Payload = payload
		}
		if companion, readErr := os.ReadFile(filepath.Join(releaseDir, "verification", category+".companion")); readErr == nil {
			input.QualificationManifest = companion
		}
		evidence[category] = input
		sum := sha256.Sum256(input.Bundle)
		if document.Validation[category].EvidenceSHA256 != hex.EncodeToString(sum[:]) {
			result.fail("evidence.binding", category, fmt.Errorf("bundle digest differs from validation.%s.evidence_sha256", category), "the manifest must be rebuilt against the staged evidence")
			continue
		}
		result.pass("evidence.binding", category)
	}
	if !result.failed {
		inventoryBytes, err := os.ReadFile(filepath.Join(releaseDir, "verification", "subjects-inventory.json"))
		if err != nil {
			result.fail("evidence.subject", "subjects-inventory.json", err, "finalize stages the inventory beside the bundles")
		} else {
			subjectSHA := sha256.Sum256(inventoryBytes)
			sourceSubject, sourceErr := manifest.ResolveSourceSubject(repoRoot())
			if sourceErr != nil {
				sourceSubject = nil
			}
			delegated := map[string]bool{}
			for _, category := range strings.Split(flags["-allow-delegated"], ",") {
				if category != "" {
					delegated[category] = true
				}
			}
			validation, err := manifest.VerifyEvidenceWithDelegation(evidence, hex.EncodeToString(subjectSHA[:]), inventory, sourceSubject, delegated, trust)
			if err != nil || len(validation) != len(manifest.Categories) {
				errorMessage := fmt.Errorf("categorized evidence failed re-verification")
				if err != nil {
					errorMessage = err
				}
				result.fail("evidence.subject", "categorized bundles", errorMessage, "only signed PASSED bundles binding this subject inventory may close a release")
			} else {
				result.pass("evidence.subject", "subject-bound and PASSED")
			}
		}
	}

	// The staged assets equal their pinned digests.
	names, _ := subjects.Names(document.ReleaseVersion)
	for asset, digest := range map[string]string{
		"assets/" + names.ChartTgz:                                   document.Helm.TgzSHA256,
		"assets/" + names.Compose:                                    document.Compose.BundleSHA256,
		document.DeploymentHelper.Artifacts["linux/amd64"].AssetName: document.DeploymentHelper.Artifacts["linux/amd64"].SHA256,
		document.DeploymentHelper.Artifacts["linux/arm64"].AssetName: document.DeploymentHelper.Artifacts["linux/arm64"].SHA256,
	} {
		body, err := os.ReadFile(filepath.Join(releaseDir, filepath.FromSlash(asset)))
		if err != nil {
			result.fail("assets.digest", asset, err, "finalize stages the measured assets")
			continue
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != digest {
			result.fail("assets.digest", asset, fmt.Errorf("sha256 drift"), "republish from the measured subjects")
			continue
		}
		result.pass("assets.digest", asset)
	}

	// The Sigstore closure: exactly one external bundle per signed asset
	// with the right subject digest and identity.
	archivePath := filepath.Join(releaseDir, document.OfflineAssetName())
	archiveSHA := fileSHA(archivePath)
	manifestSHA := sha256.Sum256(manifestBytes)
	closure, err := signing.VerifyClosure(signing.Closure{
		Inventory: inventory, ManifestSHA: hex.EncodeToString(manifestSHA[:]), ArchiveSHA: archiveSHA,
		BundlesDir: filepath.Join(releaseDir, "bundles"), Trust: trust,
	})
	if err != nil {
		result.fail("sigstore.closure", "18 bundles", err, "every signed asset needs exactly one external bundle with the matching subject digest")
	} else {
		result.pass("sigstore.closure", fmt.Sprintf("%d bundle-to-asset bindings verified", len(closure.Checks)))
	}

	// The offline archive: inner manifest bit-equality, inner asset
	// digests, verification materials, content-addressed layouts and no
	// signature sidecar inside (no hash self-reference).
	archiveReport, err := offline.Verify(archivePath, offline.Expected{
		Manifest:      manifestBytes,
		ChartName:     names.ChartTgz,
		ChartSHA256:   document.Helm.TgzSHA256,
		ComposeName:   names.Compose,
		ComposeSHA256: document.Compose.BundleSHA256,
		HelperNames: map[string]string{
			document.DeploymentHelper.Artifacts["linux/amd64"].AssetName: document.DeploymentHelper.Artifacts["linux/amd64"].SHA256,
			document.DeploymentHelper.Artifacts["linux/arm64"].AssetName: document.DeploymentHelper.Artifacts["linux/arm64"].SHA256,
		},
		Verification: appendVerificationMaterials(mustRead(filepath.Join(releaseDir, "verification", "subjects-inventory.json")), evidence),
		IndexDigests: indexDigestsOf(document),
	}, offline.ExecRunner{})
	if err != nil || archiveReport == nil {
		if archiveReport != nil {
			for _, check := range archiveReport.Checks {
				if check.Result == "failed" {
					result.fail("offline."+check.Code, check.Subject, fmt.Errorf("%s", check.Error), "the archive must be rebuilt from the published manifest")
				}
			}
		}
		if archiveReport == nil || len(archiveReport.Checks) == 0 {
			result.fail("offline.archive", document.OfflineAssetName(), err, "zstd and tar must unpack the archive")
		}
	} else {
		result.pass("offline.archive", fmt.Sprintf("%d inner checks passed", len(archiveReport.Checks)))
	}

	// The offline import with digest readback into a fresh registry
	// (OPS-VERIFY-003, OPS-OFFLINE-002).
	if flags["-import-registry"] != "" && !result.failed && archiveReport != nil {
		importRegistry := flags["-import-registry"]
		runner := offline.ExecRunner{}
		for _, component := range subjects.Components {
			layoutDir := filepath.Join(archiveReport.Extracted, "images", component)
			target := importRegistry + "/" + component
			if err := offline.ImportLayout(runner, layoutDir, target, insecure); err != nil {
				result.fail("offline.import", component, err, "the layout must copy into the target registry preserving digests")
				continue
			}
			expected := document.Images[component]
			digest, platforms, err := offline.ReadBackIndex(runner, target+"@"+expected.IndexDigest, insecure)
			if err != nil {
				result.fail("offline.import-readback", component, err, "digest readback must answer from the target registry")
				continue
			}
			if digest != expected.IndexDigest {
				result.fail("offline.import-readback", component, fmt.Errorf("readback digest %s want %s", digest, expected.IndexDigest), "import must preserve the index digest")
				continue
			}
			for platform, want := range expected.Platforms {
				if platforms[platform] != want {
					result.fail("offline.import-readback", component+" "+platform, fmt.Errorf("readback platform digest %s want %s", platforms[platform], want), "import must preserve per-platform manifests")
				}
			}
			result.pass("offline.import-readback", component)
		}
	}

	return finishGate(result, reportPath)
}

// loadRelease reads and schema-validates the published manifest, the
// staged inventory and their equality.
func loadRelease(result *gate, releaseDir string) ([]byte, *manifest.Document, *subjects.Inventory) {
	manifestBytes, err := os.ReadFile(filepath.Join(releaseDir, "release-manifest.json"))
	if err != nil {
		result.fail("manifest.load", "release-manifest.json", err, "finalize writes the manifest at the release root")
		return nil, nil, nil
	}
	document, err := manifest.ParseValidate(manifestBytes)
	if err != nil {
		result.fail("manifest.schema", "release-manifest.json", err, "the frozen release-manifest schema is the only shape authority")
		return nil, nil, nil
	}
	result.pass("manifest.schema", "frozen schema validation")
	if err := document.NoMutableReferences(); err != nil {
		result.fail("manifest.no-latest", "manifest bytes", err, "digests only")
	} else {
		result.pass("manifest.no-latest", "no mutable tag reference")
	}
	inventoryBytes, err := os.ReadFile(filepath.Join(releaseDir, "verification", "subjects-inventory.json"))
	if err != nil {
		result.fail("manifest.inventory", "subjects-inventory.json", err, "the staged inventory is the manifest's subject")
		return manifestBytes, document, nil
	}
	inventory, err := subjects.Parse(inventoryBytes)
	if err != nil {
		result.fail("manifest.inventory", "subjects-inventory.json", err, "the closed inventory shape is mandatory")
		return manifestBytes, document, nil
	}
	if err := document.EqualToInventory(inventory); err != nil {
		result.fail("manifest.inventory-equality", "manifest vs inventory", err, "the manifest must be the mechanical projection of the inventory")
		return manifestBytes, document, inventory
	}
	result.pass("manifest.inventory-equality", "every digest and asset name equal")
	return manifestBytes, document, inventory
}

func indexDigestsOf(document *manifest.Document) map[string]string {
	digests := map[string]string{}
	for component, image := range document.Images {
		digests[component] = image.IndexDigest
	}
	return digests
}

func fileSHA(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func mustRead(path string) []byte {
	body, _ := os.ReadFile(path)
	return body
}

func finishGate(result *gate, reportPath string) error {
	document := map[string]any{
		"stage":  "verify",
		"checks": result.checks,
		"result": "passed",
	}
	if result.failed {
		document["result"] = "failed"
	}
	if err := writeReport(reportPath, document); err != nil {
		return err
	}
	fmt.Printf("publish gate: %s (%d checks, report %s)\n", document["result"], len(result.checks), reportPath)
	if result.failed {
		return fmt.Errorf("publish gate failed")
	}
	return nil
}
