package main

// Registry and source-repository helpers shared by the build stages.

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/Suknna/quoin/internal/release/supplychain"
)

func outputOf(argv ...string) string {
	output, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// registryTagDigest reads the pushed manifest digest of one tag via the
// shared registry reader (scheme-aware for local and remote registries).
func registryTagDigest(host, repository, tag string) (string, error) {
	return supplychain.RegistryReader{Host: host}.TagDigest(repository, tag)
}

// platformManifestDigest reads one pushed per-platform index and extracts the
// real platform manifest digest plus the attached attestation manifests.
func platformManifestDigest(host, repository, indexDigest string) (string, []string, error) {
	reader := supplychain.RegistryReader{Host: host}
	body, err := reader.FetchIndex(repository, indexDigest)
	if err != nil {
		return "", nil, err
	}
	var index struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
			Platform    *struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &index); err != nil {
		return "", nil, err
	}
	platformDigest := ""
	attestations := []string{}
	for _, descriptor := range index.Manifests {
		if descriptor.Platform != nil && descriptor.Platform.OS == "linux" && descriptor.Platform.Architecture != "unknown" {
			if platformDigest != "" {
				return "", nil, fmt.Errorf("%s index carries two platform manifests", repository)
			}
			platformDigest = descriptor.Digest
			continue
		}
		if descriptor.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
			attestations = append(attestations, descriptor.Digest)
		}
	}
	if platformDigest == "" {
		return "", nil, fmt.Errorf("%s index %s carries no platform manifest", repository, indexDigest)
	}
	sort.Strings(attestations)
	return platformDigest, attestations, nil
}

// splitRegistry divides a full repository reference (host/namespace/component)
// into the registry host and the registry-side repository path.
func splitRegistry(reference string) (host, repository string) {
	index := strings.Index(reference, "/")
	return reference[:index], reference[index+1:]
}
