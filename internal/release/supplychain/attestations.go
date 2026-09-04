package supplychain

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Suknna/quoin/internal/release/subjects"
)

// RegistryReader reads raw OCI distribution manifests from one registry.
type RegistryReader struct {
	Host   string
	Client *http.Client
	// Username and Password authenticate reads from private registries
	// (exchanged for a scoped bearer token where the registry requires it;
	// plain basic auth is the fallback). Empty credentials read anonymously.
	Username string
	Password string

	tokens map[string]string
}

func (reader RegistryReader) client() *http.Client {
	if reader.Client != nil {
		return reader.Client
	}
	return http.DefaultClient
}

// scheme picks the registry transport: loopback test registries are plain
// HTTP; real registries are always HTTPS.
func (reader RegistryReader) scheme() string {
	host := reader.Host
	if strings.HasPrefix(host, "127.0.0.1") || strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "[::1]") {
		return "http"
	}
	return "https"
}

// manifestAccept is the closed media-type set the gate accepts from the
// registry: image indexes, image manifests and their attestation variants.
const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.attestation.manifest.v1+json"

func (reader RegistryReader) fetch(repository, reference string) ([]byte, string, error) {
	url := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", reader.scheme(), reader.Host, repository, reference)
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", manifestAccept)
	response, err := reader.client().Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<24))
	if err != nil {
		return nil, "", err
	}
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("registry %s:%s: status %d", repository, reference, response.StatusCode)
	}
	return body, response.Header.Get("Content-Type"), nil
}

// TagDigest reads the pushed manifest digest of one tag reference.
func (reader RegistryReader) TagDigest(repository, tag string) (string, error) {
	url := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", reader.scheme(), reader.Host, repository, tag)
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", manifestAccept)
	response, err := reader.client().Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	io.Copy(io.Discard, response.Body)
	digest := response.Header.Get("Docker-Content-Digest")
	if response.StatusCode != http.StatusOK || digest == "" {
		return "", fmt.Errorf("registry tag %s/%s:%s unreadable: status=%d", reader.Host, repository, tag, response.StatusCode)
	}
	return digest, nil
}

// FetchIndex returns the raw index/manifest bytes of one reference.
func (reader RegistryReader) FetchIndex(repository, reference string) ([]byte, error) {
	body, _, err := reader.fetch(repository, reference)
	return body, err
}

// indexDocument is the OCI image index manifest list.
type indexDocument struct {
	MediaType string `json:"mediaType"`
	Manifests []struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Annotations map[string]string `json:"annotations"`
		Platform    *struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
	} `json:"manifests"`
}

// AttestationSubjects is the per-platform attestation verification outcome.
type AttestationSubjects struct {
	Platform       string   `json:"platform"`
	ManifestDigest string   `json:"manifest_digest"`
	SBOM           bool     `json:"sbom"`
	Provenance     bool     `json:"provenance"`
	Subjects       []string `json:"attestation_subject_digests"`
}

const (
	sbomPredicateType       = "https://spdx.dev/Document"
	provenancePredicateType = "https://slsa.dev/provenance/v1"
)

// VerifyImageAttestations proves that the pushed index for every expected
// platform carries BuildKit SPDX SBOM and SLSA provenance v1 attestations
// whose in-toto subjects equal that platform's image manifest digest exactly
// (OPS-SUPPLY-001/002).
func (reader RegistryReader) VerifyImageAttestations(repository string, expectedPlatforms map[string]string) ([]AttestationSubjects, error) {
	indexBody, err := reader.FetchIndex(repository, "index")
	if err != nil {
		return nil, err
	}
	var index indexDocument
	if err := json.Unmarshal(indexBody, &index); err != nil {
		return nil, fmt.Errorf("index %s: %w", repository, err)
	}
	results := make([]AttestationSubjects, 0, len(expectedPlatforms))
	for _, platform := range subjects.Platforms {
		manifestDigest, ok := expectedPlatforms[platform]
		if !ok {
			return nil, fmt.Errorf("platform %s missing from expectations", platform)
		}
		result := AttestationSubjects{Platform: platform, ManifestDigest: manifestDigest}
		for _, descriptor := range index.Manifests {
			if descriptor.Digest != manifestDigest {
				continue
			}
			if descriptor.Platform == nil || descriptor.Platform.OS != "linux" {
				return nil, fmt.Errorf("platform manifest %s has no linux platform descriptor", manifestDigest)
			}
			arch := descriptor.Platform.Architecture
			if "linux/"+arch != platform {
				return nil, fmt.Errorf("manifest %s declares %s, expected %s", manifestDigest, arch, platform)
			}
		}
		for _, descriptor := range index.Manifests {
			if referenceDigest := descriptor.Annotations["vnd.docker.reference.digest"]; referenceDigest != manifestDigest {
				continue
			}
			attestation, err := reader.verifyAttestationManifest(repository, descriptor.Digest, manifestDigest, &result)
			if err != nil {
				return nil, err
			}
			result.Subjects = append(result.Subjects, attestation)
		}
		if !result.SBOM || !result.Provenance {
			return nil, fmt.Errorf("platform %s lacks attestations (sbom=%v provenance=%v)", platform, result.SBOM, result.Provenance)
		}
		results = append(results, result)
	}
	return results, nil
}

// verifyAttestationManifest fetches one attestation manifest and verifies
// every in-toto statement layer it carries (BuildKit stores the plain
// statements, one layer per predicate type, with an OCI subject field).
func (reader RegistryReader) verifyAttestationManifest(repository, attestationDigest, subjectDigest string, result *AttestationSubjects) (string, error) {
	body, _, err := reader.fetch(repository, attestationDigest)
	if err != nil {
		return "", err
	}
	var manifest struct {
		ArtifactType string `json:"artifactType"`
		Layers       []struct {
			MediaType   string            `json:"mediaType"`
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"layers"`
		Subject *struct {
			Digest string `json:"digest"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return "", fmt.Errorf("attestation manifest %s: %w", attestationDigest, err)
	}
	if !strings.Contains(manifest.ArtifactType, "attestation") || len(manifest.Layers) == 0 {
		return "", fmt.Errorf("manifest %s is not an attestation manifest", attestationDigest)
	}
	if manifest.Subject != nil && manifest.Subject.Digest != subjectDigest {
		return "", fmt.Errorf("attestation manifest %s subjects %s, want %s", attestationDigest, manifest.Subject.Digest, subjectDigest)
	}
	seen := []string{}
	for _, layer := range manifest.Layers {
		layerBody, err := reader.fetchBlob(repository, layer.Digest)
		if err != nil {
			return "", err
		}
		var document statement
		if err := json.Unmarshal(layerBody, &document); err != nil {
			return "", fmt.Errorf("attestation layer %s: %w", layer.Digest, err)
		}
		if document.PredicateType == "" {
			return "", fmt.Errorf("attestation layer %s has no predicate type", layer.Digest)
		}
		switch document.PredicateType {
		case sbomPredicateType:
			result.SBOM = true
		case provenancePredicateType:
			result.Provenance = true
		default:
			return "", fmt.Errorf("attestation %s has unknown predicate type %q", attestationDigest, document.PredicateType)
		}
		if len(document.Subject) == 0 {
			return "", fmt.Errorf("attestation %s has no subject", attestationDigest)
		}
		for _, subject := range document.Subject {
			digest := subject.Digest["sha256"]
			if !strings.HasPrefix(digest, "sha256:") {
				digest = "sha256:" + digest
			}
			if digest != subjectDigest {
				return "", fmt.Errorf("attestation %s subject %s does not equal image manifest %s", attestationDigest, digest, subjectDigest)
			}
		}
		seen = append(seen, document.PredicateType+"@"+subjectDigest)
	}
	return strings.Join(seen, ","), nil
}

func (reader RegistryReader) fetchBlob(repository, digest string) ([]byte, error) {
	url := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", reader.scheme(), reader.Host, repository, digest)
	response, err := reader.client().Get(url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry blob %s/%s: status %d", repository, digest, response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 1<<26))
}

// authorize attaches credentials to one registry request, exchanging them
// for a repository-scoped bearer token the first time a repository is read
// (the GHCR pattern) and falling back to direct basic auth.
func (reader RegistryReader) authorize(request *http.Request, repository string) {
	if reader.Username == "" && reader.Password == "" {
		return
	}
	if reader.tokens == nil {
		reader.tokens = map[string]string{}
	}
	if token, ok := reader.tokens[repository]; ok {
		request.Header.Set("Authorization", "Bearer "+token)
		return
	}
	tokenURL := fmt.Sprintf("%s://%s/token?service=%s&scope=repository:%s:pull",
		reader.scheme(), reader.Host, reader.Host, repository)
	tokenRequest, err := http.NewRequest(http.MethodGet, tokenURL, nil)
	if err == nil {
		tokenRequest.SetBasicAuth(reader.Username, reader.Password)
		if response, err := reader.client().Do(tokenRequest); err == nil {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				var document struct {
					Token string `json:"token"`
				}
				if json.Unmarshal(body, &document) == nil && document.Token != "" {
					reader.tokens[repository] = document.Token
					request.Header.Set("Authorization", "Bearer "+document.Token)
					return
				}
			}
		}
	}
	request.SetBasicAuth(reader.Username, reader.Password)
}
