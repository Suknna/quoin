package bootstrap

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Suknna/quoin/internal/contract"
)

const serviceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"

// PublishKubernetesSecret atomically replaces the data of a pre-created,
// fixed-name Secret using the bootstrap Job's short-lived ServiceAccount. The
// deployment helper never receives secret bytes: they travel only from the
// Quoin Job's private emptyDir to the Kubernetes API over its service account
// TLS channel.
func PublishKubernetesSecret(config contract.QuoinConfig, secretName string) error {
	if secretName == "" {
		return fmt.Errorf("Kubernetes Secret name is required")
	}
	return updateKubernetesSecret(secretName, filepath.Dir(config.RootKeyFile))
}

// kubernetesSecretData reads the complete bootstrap set without exposing it to
// callers outside the in-cluster bootstrap process.
func kubernetesSecretData(configPath string) (map[string]string, error) {
	files := []string{"root-key", "runtime-ca.pem", "runtime-ca.key", "runtime-tls.crt", "runtime-tls.key", "stele-service-token"}
	data := make(map[string]string, len(files))
	for _, name := range files {
		value, err := os.ReadFile(filepath.Join(configPath, name))
		if err != nil || len(value) == 0 {
			return nil, fmt.Errorf("read generated secret %s: %w", name, err)
		}
		data[name] = base64.StdEncoding.EncodeToString(value)
	}
	return data, nil
}

func kubernetesClient() (*http.Client, string, string, error) {
	ca, err := os.ReadFile(filepath.Join(serviceAccountDirectory, "ca.crt"))
	if err != nil {
		return nil, "", "", fmt.Errorf("read Kubernetes CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, "", "", fmt.Errorf("parse Kubernetes CA")
	}
	token, err := os.ReadFile(filepath.Join(serviceAccountDirectory, "token"))
	if err != nil {
		return nil, "", "", fmt.Errorf("read Kubernetes service-account token: %w", err)
	}
	namespace, err := os.ReadFile(filepath.Join(serviceAccountDirectory, "namespace"))
	if err != nil {
		return nil, "", "", fmt.Errorf("read Kubernetes namespace: %w", err)
	}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if host == "" || port == "" {
		return nil, "", "", fmt.Errorf("Kubernetes service endpoint is unavailable")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	return client, "https://" + host + ":" + port, strings.TrimSpace(string(token)) + "|" + strings.TrimSpace(string(namespace)), nil
}

func updateKubernetesSecret(secretName, directory string) error {
	client, endpoint, identity, err := kubernetesClient()
	if err != nil {
		return err
	}
	parts := strings.SplitN(identity, "|", 2)
	data, err := kubernetesSecretData(directory)
	if err != nil {
		return err
	}
	url := endpoint + "/api/v1/namespaces/" + parts[1] + "/secrets/" + secretName
	get, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	get.Header.Set("Authorization", "Bearer "+parts[0])
	response, err := client.Do(get)
	if err != nil {
		return fmt.Errorf("read Kubernetes Secret: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("read Kubernetes Secret: API returned %s", response.Status)
	}
	var current struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	err = json.NewDecoder(response.Body).Decode(&current)
	response.Body.Close()
	if err != nil || current.Metadata.ResourceVersion == "" {
		return fmt.Errorf("read Kubernetes Secret resource version: %w", err)
	}
	payload, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]string{"name": secretName, "resourceVersion": current.Metadata.ResourceVersion}, "data": data})
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+parts[0])
	request.Header.Set("Content-Type", "application/json")
	response, err = client.Do(request)
	if err != nil {
		return fmt.Errorf("update Kubernetes Secret: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("update Kubernetes Secret: API returned %s", response.Status)
	}
	return nil
}
