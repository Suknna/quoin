package main

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Suknna/quoin/internal/release/subjects"
	"github.com/Suknna/quoin/internal/release/supplychain"
)

// verifyMode runs the offline pre-qualification gate over an existing
// inventory: every subject's Sigstore bundle (subject digest equality,
// certificate identity/issuer, chain anchor) and every image index's SBOM and
// SLSA provenance attestations.
func verifyMode(arguments []string) error {
	var inventoryPath, bundlesDir, trustRootPath, identity, issuer string
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		value := func() (string, error) {
			index++
			if index >= len(arguments) {
				return "", fmt.Errorf("%s needs a value", argument)
			}
			return arguments[index], nil
		}
		var err error
		switch argument {
		case "-inventory":
			inventoryPath, err = value()
		case "-bundles":
			bundlesDir, err = value()
		case "-trust-root":
			trustRootPath, err = value()
		case "-identity":
			identity, err = value()
		case "-issuer":
			issuer, err = value()
		default:
			err = fmt.Errorf("unknown verify argument %q", argument)
		}
		if err != nil {
			return err
		}
	}
	if inventoryPath == "" || bundlesDir == "" || trustRootPath == "" || identity == "" || issuer == "" {
		return fmt.Errorf("-inventory, -bundles, -trust-root, -identity and -issuer are required")
	}
	rawInventory, err := os.ReadFile(inventoryPath)
	if err != nil {
		return err
	}
	inventory, err := subjects.Parse(rawInventory)
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	rootPEM, err := os.ReadFile(trustRootPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(rootPEM)) == "" {
		return fmt.Errorf("trust root is empty")
	}
	if !strings.Contains(string(rootPEM), "BEGIN CERTIFICATE") {
		// A raw DER file is tolerated by wrapping it in PEM.
		rootPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootPEM})
	}
	trust := supplychain.Trust{RootPEM: rootPEM, IdentityRegexp: identity, Issuer: issuer}

	type check struct {
		Bundle   string `json:"bundle"`
		Subject  string `json:"subject"`
		Identity string `json:"identity,omitempty"`
		Issuer   string `json:"issuer,omitempty"`
	}
	report := map[string]any{"release_version": inventory.ReleaseVersion, "bundles": []check{}, "attestations": nil}

	verifyBlob := func(bundleKey, bundleName, subjectDigest string) error {
		bundleJSON, err := os.ReadFile(filepath.Join(bundlesDir, bundleName))
		if err != nil {
			return fmt.Errorf("bundle %s: %w", bundleKey, err)
		}
		var payloadCompanion []byte
		if raw, err := os.ReadFile(filepath.Join(bundlesDir, bundleName+".payload")); err == nil {
			payloadCompanion = raw
		}
		result, err := supplychain.VerifyBundleWithPayload(bundleJSON, payloadCompanion, subjectDigest, trust)
		if err != nil {
			return fmt.Errorf("bundle %s: %w", bundleKey, err)
		}
		report["bundles"] = append(report["bundles"].([]check), check{
			Bundle: bundleName, Subject: subjectDigest, Identity: result.Identity, Issuer: result.Issuer,
		})
		return nil
	}
	for _, component := range subjects.Components {
		image := inventory.Images[component]
		if err := verifyBlob("image_indexes/"+component, inventory.Bundles["image_indexes/"+component], image.IndexDigest); err != nil {
			return err
		}
		for _, platform := range subjects.Platforms {
			key := "image_manifests/" + component + "/" + platform
			if err := verifyBlob(key, inventory.Bundles[key], image.Platforms[platform]); err != nil {
				return err
			}
		}
	}
	if err := verifyBlob("helm_oci", inventory.Bundles["helm_oci"], inventory.Chart.OCIDigest); err != nil {
		return err
	}
	if err := verifyBlob("compose", inventory.Bundles["compose"], "sha256:"+inventory.Compose.SHA256); err != nil {
		return err
	}
	for _, platform := range subjects.Platforms {
		key := "deployment_helper/" + platform
		if err := verifyBlob(key, inventory.Bundles[key], "sha256:"+inventory.Helpers[platform].SHA256); err != nil {
			return err
		}
	}

	host := registryHostOf(inventory.Images["quoin"].Repository)
	reader := supplychain.RegistryReader{Host: host}
	attestationResults := map[string][]supplychain.AttestationSubjects{}
	for _, component := range subjects.Components {
		image := inventory.Images[component]
		repository := strings.TrimPrefix(image.Repository, host+"/")
		results, err := reader.VerifyImageAttestations(repository, image.Platforms)
		if err != nil {
			return fmt.Errorf("%s attestations: %w", component, err)
		}
		attestationResults[component] = results
	}
	report["attestations"] = attestationResults
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

// registryHostOf splits "host/namespace/component" back to the registry host.
func registryHostOf(repository string) string {
	first := repository
	if index := strings.Index(repository, "/"); index > 0 {
		first = repository[:index]
	}
	if strings.Contains(first, ":") || strings.Contains(first, ".") {
		return first
	}
	return "docker.io"
}
