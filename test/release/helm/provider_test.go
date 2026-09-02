package helm

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func prepareHelmProviderAndRuntime(t *testing.T, recorder *evidence, client *http.Client, base, origin, namespace, release, providerURL string) {
	t.Helper()
	var runtime struct {
		Plinth struct {
			RowVersion int64 `json:"rowVersion"`
		} `json:"plinth"`
	}
	helmJSONRequest(t, client, http.MethodGet, base+"/api/v1/runtime", origin, nil, http.StatusOK, &runtime)
	var prepared struct {
		RegistrationTokenHandle string `json:"registrationTokenHandle"`
	}
	helmJSONRequest(t, client, http.MethodPost, base+"/api/v1/runtime-slots/plinth/registration/prepare", origin, map[string]any{"clientCommandId": "t33-plinth-prepare", "expectedRowVersion": runtime.Plinth.RowVersion}, http.StatusOK, &prepared)
	var token struct {
		Slot              string `json:"slot"`
		Generation        int64  `json:"generation"`
		RegistrationToken string `json:"registrationToken"`
	}
	helmJSONRequest(t, client, http.MethodPost, base+"/api/v1/runtime-slots/registration-token/reveal", origin, map[string]any{"registrationTokenHandle": prepared.RegistrationTokenHandle}, http.StatusOK, &token)
	registration, err := json.Marshal(map[string]any{"slot": token.Slot, "generation": token.Generation, "token": token.RegistrationToken})
	if err != nil {
		t.Fatal(err)
	}
	recorder.run("register-plinth", nil, bytes.NewReader(append(registration, '\n')), 0, "kubectl", "--namespace", namespace, "exec", "-i", "deployment/"+release+"-plinth", "--container=plinth", "--", "/plinth", "register", "--config", "/etc/quoin/component.yaml")
	deadline := time.Now().Add(120 * time.Second)
	registered := false
	for time.Now().Before(deadline) {
		var current struct {
			Plinth struct {
				State     string `json:"state"`
				Connected bool   `json:"connected"`
			} `json:"plinth"`
		}
		helmJSONRequest(t, client, http.MethodGet, base+"/api/v1/runtime", origin, nil, http.StatusOK, &current)
		if current.Plinth.State == "registered" && current.Plinth.Connected {
			registered = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !registered {
		t.Fatal("Helm Plinth did not become registered and connected")
	}
	name := "t33-provider"
	helmJSONRequest(t, client, http.MethodPost, base+"/api/v1/connections", origin, map[string]any{"clientCommandId": "t33-provider-create", "name": name, "connection": map[string]any{"type": "model_provider", "baseUrl": providerURL, "chatModelId": "fixture-chat-1", "embeddingModelId": "fixture-embed-1", "contextBudgetTokens": 8192, "maxOutputTokens": 1024, "apiKey": "fixture-api-key-2026"}}, http.StatusCreated, nil)
	helmJSONRequest(t, client, http.MethodPost, base+"/api/v1/connections/"+name+"/probe", origin, map[string]any{"clientCommandId": "t33-provider-probe"}, http.StatusAccepted, nil)
	var resultID string
	deadline = time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		var results struct {
			Items []struct {
				ID      string `json:"id"`
				Outcome string `json:"outcome"`
			} `json:"items"`
		}
		helmJSONRequest(t, client, http.MethodGet, base+"/api/v1/connections/"+name+"/probe-results", origin, nil, http.StatusOK, &results)
		for _, item := range results.Items {
			if item.Outcome == "passed" {
				resultID = item.ID
			}
		}
		if resultID != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if resultID == "" {
		t.Fatal("Helm provider did not qualify")
	}
	var detail struct {
		RowVersion int64 `json:"rowVersion"`
	}
	helmJSONRequest(t, client, http.MethodGet, base+"/api/v1/connections/"+name, origin, nil, http.StatusOK, &detail)
	helmJSONRequest(t, client, http.MethodPost, base+"/api/v1/connections/"+name+"/enable", origin, map[string]any{"clientCommandId": "t33-provider-enable", "expectedRowVersion": detail.RowVersion, "qualifiedProbeResultId": resultID}, http.StatusOK, nil)
}

func helmJSONRequest(t *testing.T, client *http.Client, method, url, origin string, value any, expected int, destination any) {
	t.Helper()
	var body io.Reader
	if value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = strings.TrimPrefix(origin, "https://")
	request.Header.Set("Origin", origin)
	if value != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(response.Body)
	if response.StatusCode != expected {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, url, response.StatusCode, expected, responseBody)
	}
	if destination != nil && len(responseBody) != 0 {
		if err := json.Unmarshal(responseBody, destination); err != nil {
			t.Fatal(err)
		}
	}
}

type helmModelProvider struct {
	cmd  *exec.Cmd
	file *os.File
}

func startHelmModelProvider(t *testing.T, binary, evidenceDir string) *helmModelProvider {
	t.Helper()
	logPath := filepath.Join(evidenceDir, "model-provider.log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-address", "0.0.0.0:18443")
	cmd.Stdout, cmd.Stderr = file, file
	if err := cmd.Start(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, requestErr := http.Get("http://127.0.0.1:18443/v1/models")
		if requestErr == nil {
			_ = response.Body.Close()
			return &helmModelProvider{cmd: cmd, file: file}
		}
		time.Sleep(250 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	_ = file.Close()
	t.Fatal("model provider fixture did not become ready")
	return nil
}

func (provider *helmModelProvider) Stop() {
	if provider == nil {
		return
	}
	if provider.cmd != nil && provider.cmd.Process != nil {
		_ = provider.cmd.Process.Kill()
		_ = provider.cmd.Wait()
	}
	if provider.file != nil {
		_ = provider.file.Close()
	}
}

func assertHelmRuntimeRevoked(t *testing.T, client *http.Client, base, origin, slot string) {
	t.Helper()
	var runtime struct {
		Plinth struct {
			State             string `json:"state"`
			CurrentGeneration int64  `json:"currentGeneration"`
		} `json:"plinth"`
	}
	helmJSONRequest(t, client, http.MethodGet, base+"/api/v1/runtime", origin, nil, http.StatusOK, &runtime)
	if slot != "plinth" || runtime.Plinth.State != "revoked" || runtime.Plinth.CurrentGeneration != 0 {
		t.Fatalf("runtime after restore=%+v, want plinth revoked generation 0", runtime)
	}
}

func assertHelmConnectionRevalidation(t *testing.T, client *http.Client, base, origin, name string) {
	t.Helper()
	var detail struct {
		Enabled              bool `json:"enabled"`
		RevalidationRequired bool `json:"revalidationRequired"`
	}
	helmJSONRequest(t, client, http.MethodGet, base+"/api/v1/connections/"+name, origin, nil, http.StatusOK, &detail)
	if detail.Enabled || !detail.RevalidationRequired {
		t.Fatalf("connection after restore=%+v, want disabled and revalidationRequired", detail)
	}
}
