package helm

// Digest-faithful OCI chart fetch. Helm 3 cannot resolve an OCI chart
// reference by digest alone (helm/helm#10619; digest pulls land in helm
// 4), but OPS-RELEASE-003 requires installing exactly the digest-pinned
// chart bytes. This fetcher is the bridge: it reads the chart manifest
// by digest through the registry v2 API, verifies the digest, extracts
// the one chart tgz layer to the state directory, and hands helm the
// verified local file — helm never resolves a mutable tag, and the
// digest remains the sole chart authority.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// chartFetcher reads one digest-pinned chart from an OCI registry.
type chartFetcher struct {
	Host       string
	Repository string
	Client     *http.Client
	// username/password ride the registry's Bearer challenge (the token
	// exchange carries them only to the challenge's own realm); empty
	// credentials use the anonymous token flow public registries offer.
	username string
	password string
}

func (fetcher *chartFetcher) scheme() string {
	if isLoopbackHost(fetcher.Host) {
		return "http"
	}
	return "https"
}

// fetchedChart is the verified local chart artifact.
type fetchedChart struct {
	Path   string // the extracted .tgz (chart layer bytes, digest-verified)
	Digest string // the pinned manifest digest, re-verified on read
}

// fetchChartByDigest pulls the chart at the exact digest and writes the
// chart layer to destination (a .tgz path). The manifest digest is
// verified against the bytes actually served before the layer is
// accepted, and the layer's own declared digest is verified too.
func fetchChartByDigest(host, repository, digest, destination string) (*fetchedChart, error) {
	fetcher := &chartFetcher{
		Host: host, Repository: repository,
		Client:   &http.Client{Timeout: 60 * time.Second},
		username: os.Getenv("QUOIN_CHART_REGISTRY_USER"),
		password: os.Getenv("QUOIN_CHART_REGISTRY_PASSWORD"),
	}
	manifest, err := fetcher.fetchJSON("manifests", digest,
		"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.cncf.helm.config.v1+json")
	if err != nil {
		return nil, fmt.Errorf("chart manifest %s@%s: %w", repository, digest, err)
	}
	sum := sha256.Sum256(manifest)
	if hex.EncodeToString(sum[:]) != strings.TrimPrefix(digest, "sha256:") {
		return nil, fmt.Errorf("chart manifest digest mismatch: served bytes do not hash to %s", digest)
	}
	var shape struct {
		MediaType string `json:"mediaType"`
		Config    struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifest, &shape); err != nil {
		return nil, fmt.Errorf("chart manifest shape: %w", err)
	}
	// A helm chart artifact is a config of type helm.config plus exactly
	// one tgz layer; tolerate the plain image manifest form too and pick
	// the single tgz layer either way.
	const chartLayer = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	var layer string
	for _, candidate := range shape.Layers {
		if candidate.MediaType == chartLayer || strings.HasSuffix(candidate.MediaType, "tar+gzip") {
			if layer != "" {
				return nil, fmt.Errorf("chart manifest carries multiple chart layers")
			}
			layer = candidate.Digest
		}
	}
	if layer == "" {
		if len(shape.Layers) == 1 {
			layer = shape.Layers[0].Digest
		} else {
			return nil, fmt.Errorf("chart manifest has no identifiable chart layer (%d layers)", len(shape.Layers))
		}
	}
	body, err := fetcher.fetchBytes("blobs", layer)
	if err != nil {
		return nil, fmt.Errorf("chart layer %s: %w", layer, err)
	}
	layerSum := sha256.Sum256(body)
	if "sha256:"+hex.EncodeToString(layerSum[:]) != layer {
		return nil, fmt.Errorf("chart layer digest mismatch: served bytes do not hash to %s", layer)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(destination, body, 0o600); err != nil {
		return nil, err
	}
	return &fetchedChart{Path: destination, Digest: digest}, nil
}

// fetchJSON reads one registry object expecting JSON manifest content.
func (fetcher *chartFetcher) fetchJSON(kind, reference, accept string) ([]byte, error) {
	body, err := fetcher.fetch(kind, reference, accept)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (fetcher *chartFetcher) fetchBytes(kind, reference string) ([]byte, error) {
	return fetcher.fetch(kind, reference, "application/octet-stream")
}

func (fetcher *chartFetcher) fetch(kind, reference, accept string) ([]byte, error) {
	url := fmt.Sprintf("%s://%s/v2/%s/%s/%s", fetcher.scheme(), fetcher.Host, fetcher.Repository, kind, reference)
	body, unauthorized, err := fetcher.attempt(url, accept, "")
	if err != nil {
		return nil, err
	}
	if body != nil {
		return body, nil
	}
	// Private registries answer 401 with a distribution-spec Bearer
	// challenge; one token exchange (basic credentials ride it when
	// configured) then authorizes the real request.
	token, tokenErr := fetcher.exchangeToken(unauthorized)
	if tokenErr != nil {
		return nil, tokenErr
	}
	body, _, err = fetcher.attempt(url, accept, token)
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, fmt.Errorf("registry %s: still unauthorized after token exchange", url)
	}
	return body, nil
}

// attempt issues one request and fully reads a 200; an empty body with
// a non-nil challenge means the caller must exchange a token first.
func (fetcher *chartFetcher) attempt(url, accept, bearer string) ([]byte, string, error) {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", accept)
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	} else if fetcher.username != "" {
		request.SetBasicAuth(fetcher.username, fetcher.password)
	}
	response, err := fetcher.Client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized && bearer == "" {
		return nil, response.Header.Get("WWW-Authenticate"), nil
	}
	if response.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, "", fmt.Errorf("registry %s: status %d: %s", url, response.StatusCode, strings.TrimSpace(string(preview)))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, "", err
	}
	return body, "", nil
}

// exchangeToken performs the distribution-spec Bearer token exchange
// the WWW-Authenticate challenge describes.
func (fetcher *chartFetcher) exchangeToken(challenge string) (string, error) {
	if !strings.HasPrefix(challenge, "Bearer ") {
		return "", fmt.Errorf("unsupported registry challenge %q", challenge)
	}
	var realm, service, scope string
	for _, field := range strings.Split(strings.TrimPrefix(challenge, "Bearer "), ",") {
		key, value, found := strings.Cut(strings.TrimSpace(field), "=")
		if !found {
			continue
		}
		value = strings.Trim(value, `"`)
		switch key {
		case "realm":
			realm = value
		case "service":
			service = value
		case "scope":
			scope = value
		}
	}
	if realm == "" {
		return "", fmt.Errorf("registry challenge carries no realm")
	}
	tokenURL := realm + "?scope=" + url.QueryEscape(scope)
	if service != "" {
		tokenURL += "&service=" + url.QueryEscape(service)
	}
	request, err := http.NewRequest(http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}
	if fetcher.username != "" {
		request.SetBasicAuth(fetcher.username, fetcher.password)
	}
	response, err := fetcher.Client.Do(request)
	if err != nil {
		return "", fmt.Errorf("registry token exchange: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token exchange: status %d", response.StatusCode)
	}
	var tokenDocument struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokenDocument); err != nil {
		return "", fmt.Errorf("registry token payload: %w", err)
	}
	token := tokenDocument.Token
	if token == "" {
		token = tokenDocument.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("registry token payload carries no token")
	}
	return token, nil
}

// isLoopbackHost reports the plain-HTTP registry topology (loopback
// qualification and offline-import registries).
func isLoopbackHost(host string) bool {
	return strings.HasPrefix(host, "127.0.0.1") || strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "[::1]")
}
