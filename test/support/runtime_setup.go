// Package support contains real deployment setup operations shared by black-box
// tests. It deliberately talks only to public HTTP and Compose CLI surfaces.
package support

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// PrepareProviderAndRuntime registers Plinth and enables a model-provider
// connection after a real probe. gateway is the host address reachable from
// the Compose network; providerURL is its OpenAI-compatible fixture endpoint.
func PrepareProviderAndRuntime(t testing.TB, client *http.Client, base, origin, composeFile, project, providerURL, namePrefix string) {
	t.Helper()
	_ = composeFile
	_ = project
	// Plinth connects automatically through its deployment CA-signed mTLS
	// client identity (ADR-0009); there is no registration step to drive.
	name := namePrefix + "-provider"
	post(t, client, base+"/api/v1/connections", origin, fmt.Sprintf(`{"clientCommandId":%q,"name":%q,"connection":{"type":"model_provider","baseUrl":%q,"chatModelId":"fixture-chat-1","embeddingModelId":"fixture-embed-1","contextBudgetTokens":8192,"maxOutputTokens":1024,"apiKey":"fixture-api-key-2026"}}`, namePrefix+"-create", name, providerURL))
	post(t, client, base+"/api/v1/connections/"+name+"/probe", origin, fmt.Sprintf(`{"clientCommandId":%q}`, namePrefix+"-probe"))
	var resultID string
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		var results struct {
			Items []struct {
				ID      string `json:"id"`
				Outcome string `json:"outcome"`
			} `json:"items"`
		}
		mustJSON(t, get(t, client, base+"/api/v1/connections/"+name+"/probe-results", origin), &results)
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
		t.Fatal("provider did not qualify")
	}
	var detail struct {
		RowVersion int64 `json:"rowVersion"`
	}
	mustJSON(t, get(t, client, base+"/api/v1/connections/"+name, origin), &detail)
	post(t, client, base+"/api/v1/connections/"+name+"/enable", origin, fmt.Sprintf(`{"clientCommandId":%q,"expectedRowVersion":%d,"qualifiedProbeResultId":%q}`, namePrefix+"-enable", detail.RowVersion, resultID))
}

// CreateAlertSource returns the one-time bearer only for caller-side delivery tests.
func CreateAlertSource(t testing.TB, client *http.Client, base, origin, key, commandID string) string {
	return createAlertSource(t, client, base, origin, "", key, commandID)
}

// CreateAlertSourceWithHost is the same public-path fixture operation for a
// deployment whose public origin is represented through an HTTP Host override
// (for example a Kubernetes port-forward).
func CreateAlertSourceWithHost(t testing.TB, client *http.Client, base, origin, host, key, commandID string) string {
	return createAlertSource(t, client, base, origin, host, key, commandID)
}

func createAlertSource(t testing.TB, client *http.Client, base, origin, host, key, commandID string) string {
	t.Helper()
	var created struct {
		RevealHandle string `json:"revealHandle"`
	}
	create := post
	if host != "" {
		create = func(t testing.TB, client *http.Client, url, origin, body string) string {
			return postWithHost(t, client, url, origin, host, body)
		}
	}
	mustJSON(t, create(t, client, base+"/api/v1/alert-sources", origin, fmt.Sprintf(`{"key":%q,"protocol":"alertmanager","clientCommandId":%q}`, key, commandID)), &created)
	var revealed struct {
		BearerToken string `json:"bearerToken"`
	}
	mustJSON(t, create(t, client, base+"/api/v1/alert-sources/credentials/reveal", origin, fmt.Sprintf(`{"revealHandle":%q}`, created.RevealHandle)), &revealed)
	if revealed.BearerToken == "" {
		t.Fatal("alert bearer missing")
	}
	return revealed.BearerToken
}
func get(t testing.TB, client *http.Client, url, origin string) string {
	t.Helper()
	r, e := http.NewRequest(http.MethodGet, url, nil)
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Origin", origin)
	return response(t, client, r)
}
func post(t testing.TB, client *http.Client, url, origin, body string) string {
	t.Helper()
	r, e := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", origin)
	return response(t, client, r)
}
func postWithHost(t testing.TB, client *http.Client, url, origin, host, body string) string {
	t.Helper()
	r, e := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	r.Host = host
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", origin)
	return response(t, client, r)
}

func response(t testing.TB, client *http.Client, r *http.Request) string {
	t.Helper()
	q, e := client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer q.Body.Close()
	b, _ := io.ReadAll(q.Body)
	if q.StatusCode < 200 || q.StatusCode >= 300 {
		t.Fatalf("%s %s status=%d: %s", r.Method, r.URL, q.StatusCode, b)
	}
	return string(b)
}
func mustJSON(t testing.TB, value string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(value), target); err != nil {
		t.Fatalf("JSON: %v: %s", err, value)
	}
}
func randomID(t testing.TB) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
