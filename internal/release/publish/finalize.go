package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/release/manifest"
	"github.com/Suknna/quoin/internal/release/offline"
	"github.com/Suknna/quoin/internal/release/subjects"
	"github.com/Suknna/quoin/internal/release/supplychain"
)

// finalizeMode derives the manifest, stages the release directory and
// packs the offline archive.
func finalizeMode(arguments []string) error {
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
		case "-inventory", "-evidence", "-contracts", "-assets", "-bundles", "-trust-root", "-identity", "-issuer", "-out", "-work", "-allow-delegated":
			flags[flag] = value
		default:
			return fmt.Errorf("unknown finalize argument %q", flag)
		}
	}
	for _, required := range []string{"-inventory", "-evidence", "-contracts", "-assets", "-bundles", "-trust-root", "-identity", "-issuer", "-out"} {
		if flags[required] == "" {
			return fmt.Errorf("%s is required", required)
		}
	}
	var delegated []string
	if allowance := flags["-allow-delegated"]; allowance != "" {
		// Local acceptance mechanism proof only: the named categories carry
		// signed delegation reports; the production workflow never sets
		// this flag and its categories demand passing Test Results.
		delegated = strings.Split(allowance, ",")
	}
	insecure := parser.boolFlag("-registry-insecure")

	inventoryBytes, err := os.ReadFile(flags["-inventory"])
	if err != nil {
		return err
	}
	inventory, err := subjects.Parse(inventoryBytes)
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	rootPEM, err := os.ReadFile(flags["-trust-root"])
	if err != nil {
		return err
	}
	trust := supplychain.Trust{RootPEM: rootPEM, IdentityRegexp: flags["-identity"], Issuer: flags["-issuer"]}

	evidence, err := readEvidenceDir(flags["-evidence"])
	if err != nil {
		return err
	}
	document, err := manifest.Build(manifest.Inputs{
		Inventory: inventory, InventoryBytes: inventoryBytes,
		Evidence: evidence, Trust: trust,
		ContractsDir: flags["-contracts"],
		RepoRoot:     repoRoot(),
		GeneratedAt:  time.Now().UTC(),

		AllowDelegatedCategories: delegated,
	})
	if err != nil {
		return fmt.Errorf("build manifest: %w", err)
	}
	manifestBytes, err := document.Marshal()
	if err != nil {
		return err
	}
	if _, err := manifest.ParseValidate(manifestBytes); err != nil {
		return fmt.Errorf("generated manifest fails its frozen schema: %w", err)
	}
	if err := document.NoMutableReferences(); err != nil {
		return err
	}
	releaseDir := flags["-out"]
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(releaseDir, "release-manifest.json"), manifestBytes, 0o644); err != nil {
		return err
	}

	// Stage the measured assets and verify every digest while copying.
	names, _ := subjects.Names(inventory.ReleaseVersion)
	staged, err := stageAssets(flags["-assets"], releaseDir, inventory, names)
	if err != nil {
		return err
	}
	// Stage the sixteen subject bundles the T39 sign job produced.
	bundlesDir := filepath.Join(releaseDir, "bundles")
	if err := copyDir(flags["-bundles"], bundlesDir, ".sigstore.json"); err != nil {
		return err
	}
	// Stage the verification materials: the subject inventory and the
	// seven categorized evidence bundles.
	verificationDir := filepath.Join(releaseDir, "verification")
	if err := os.MkdirAll(verificationDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(verificationDir, "subjects-inventory.json"), inventoryBytes, 0o644); err != nil {
		return err
	}
	for category, input := range evidence {
		if err := os.WriteFile(filepath.Join(verificationDir, category+".bundle.json"), input.Bundle, 0o644); err != nil {
			return err
		}
		if input.Payload != nil {
			if err := os.WriteFile(filepath.Join(verificationDir, category+".payload"), input.Payload, 0o644); err != nil {
				return err
			}
		}
		if input.QualificationManifest != nil {
			if err := os.WriteFile(filepath.Join(verificationDir, category+".companion"), input.QualificationManifest, 0o644); err != nil {
				return err
			}
		}
	}

	// Pull the four component layouts and pack the offline archive.
	runner := offline.ExecRunner{}
	work := flags["-work"]
	if work == "" {
		work = filepath.Join(os.TempDir(), "quoin-finalize")
	}
	layouts := map[string]string{}
	for _, component := range subjects.Components {
		image := inventory.Images[component]
		layoutDir := filepath.Join(work, "images", component)
		if err := os.RemoveAll(layoutDir); err != nil {
			return err
		}
		if err := offline.PullLayout(runner, image.Repository+"@"+image.IndexDigest, layoutDir, insecure); err != nil {
			return fmt.Errorf("%s layout: %w", component, err)
		}
		layouts[component] = layoutDir
	}
	helpers := map[string][]byte{}
	for _, platform := range subjects.Platforms {
		body, err := os.ReadFile(filepath.Join(releaseDir, inventory.Helpers[platform].AssetName))
		if err != nil {
			return err
		}
		helpers[inventory.Helpers[platform].AssetName] = body
	}
	chartBody, err := os.ReadFile(filepath.Join(releaseDir, "assets", names.ChartTgz))
	if err != nil {
		return err
	}
	composeBody, err := os.ReadFile(filepath.Join(releaseDir, "assets", names.Compose))
	if err != nil {
		return err
	}
	archive, err := offline.Build(filepath.Join(releaseDir, "offline-archive"), offline.Contents{
		Manifest:     manifestBytes,
		ChartName:    names.ChartTgz,
		Chart:        chartBody,
		ComposeName:  names.Compose,
		Compose:      composeBody,
		Helpers:      helpers,
		Verification: appendVerificationMaterials(inventoryBytes, evidence),
		ImageLayouts: layouts,
	}, runner)
	if err != nil {
		return fmt.Errorf("build offline archive: %w", err)
	}
	if err := os.Rename(archive.Path, filepath.Join(releaseDir, document.OfflineAssetName())); err != nil {
		return err
	}
	manifestSHA := sha256.Sum256(manifestBytes)
	return writeReport(filepath.Join(releaseDir, "finalize-report.json"), map[string]any{
		"stage":          "finalize",
		"release":        inventory.ReleaseVersion,
		"manifestSHA256": hex.EncodeToString(manifestSHA[:]),
		"archive":        map[string]any{"asset": document.OfflineAssetName(), "sha256": archive.SHA256, "bytes": archive.Bytes},
		"stagedAssets":   staged,
		"evidence":       document.Validation,
	})
}

// readEvidenceDir loads the closed category set from one directory; a
// <category>.payload companion beside each bundle carries the signed
// statement bytes of messageSignature bundles.
func readEvidenceDir(dir string) (map[string]manifest.EvidenceInput, error) {
	evidence := map[string]manifest.EvidenceInput{}
	for _, category := range manifest.Categories {
		bundle, err := os.ReadFile(filepath.Join(dir, category+".bundle.json"))
		if err != nil {
			return nil, fmt.Errorf("evidence %s: %w", category, err)
		}
		input := manifest.EvidenceInput{Bundle: bundle}
		if payload, err := os.ReadFile(filepath.Join(dir, category+".payload")); err == nil {
			input.Payload = payload
		}
		if companion, err := os.ReadFile(filepath.Join(dir, category+".companion")); err == nil {
			input.QualificationManifest = companion
		}
		evidence[category] = input
	}
	return evidence, nil
}

// stageAssets copies the chart tgz, the compose bundle and both helpers
// into the release directory layout, asserting each recorded SHA-256
// against the copied bytes.
func stageAssets(assetsDir, releaseDir string, inventory *subjects.Inventory, names subjects.AssetNames) (map[string]string, error) {
	staged := map[string]string{}
	copyChecked := func(source, destination, wantSHA string) error {
		body, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != wantSHA {
			return fmt.Errorf("%s sha256 %s want %s", filepath.Base(source), hex.EncodeToString(sum[:]), wantSHA)
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(destination, body, 0o755); err != nil {
			return err
		}
		staged[filepath.Base(source)] = wantSHA
		return nil
	}
	if err := copyChecked(filepath.Join(assetsDir, "chart", names.ChartTgz), filepath.Join(releaseDir, "assets", names.ChartTgz), inventory.Chart.TgzSHA256); err != nil {
		return nil, err
	}
	if err := copyChecked(filepath.Join(assetsDir, names.Compose), filepath.Join(releaseDir, "assets", names.Compose), inventory.Compose.SHA256); err != nil {
		return nil, err
	}
	for _, platform := range subjects.Platforms {
		helper := inventory.Helpers[platform]
		if err := copyChecked(filepath.Join(assetsDir, helper.AssetName), filepath.Join(releaseDir, helper.AssetName), helper.SHA256); err != nil {
			return nil, err
		}
	}
	return staged, nil
}

// appendVerificationMaterials flattens the archive's verification payload.
func appendVerificationMaterials(inventoryBytes []byte, evidence map[string]manifest.EvidenceInput) map[string][]byte {
	materials := map[string][]byte{"subjects-inventory.json": inventoryBytes}
	for category, input := range evidence {
		materials[category+".bundle.json"] = input.Bundle
		if input.Payload != nil {
			materials[category+".payload"] = input.Payload
		}
		if input.QualificationManifest != nil {
			materials[category+".companion"] = input.QualificationManifest
		}
	}
	return materials
}

// copyDir copies every regular file of one directory into another,
// preserving names; the release closure copies the signed bundles and
// their payload companions verbatim.
func copyDir(source, destination, _ string) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), body, 0o644); err != nil {
			return err
		}
	}
	return nil
}
