// Package compose hosts the T01 ticket acceptance run. This file carries the
// shared evidence-recording and HTTP fixtures used by ticket01_test.go.
package compose_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newCookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func jsonHeaders(origin string) map[string]string {
	return map[string]string{"Content-Type": "application/json", "Origin": origin}
}

func clientHasSession(t *testing.T, client *http.Client, base string) bool {
	t.Helper()
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range client.Jar.Cookies(parsed) {
		if cookie.Name == "__Host-quoin-session" && cookie.Value != "" {
			return true
		}
	}
	return false
}

func doRequest(t *testing.T, client *http.Client, method, target, body string, headers map[string]string, wantStatus int, _ string) string {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != wantStatus {
		t.Fatalf("%s %s: status=%d want=%d body=%.500s", method, target, response.StatusCode, wantStatus, raw)
	}
	return string(raw)
}

func doStatus(t *testing.T, client *http.Client, method, target, body string, headers map[string]string, wantStatus int) string {
	t.Helper()
	return doRequest(t, client, method, target, body, headers, wantStatus, "")
}

func randomPassword(t *testing.T) string {
	t.Helper()
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatal(err)
	}
	return "quoin-" + base64.RawURLEncoding.EncodeToString(buffer)
}

func requireCommand(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not available: %v", name, err)
	}
}

func outputOf(t *testing.T, command string, arguments ...string) string {
	t.Helper()
	cmd := exec.Command(command, arguments...)
	cmd.Dir = repoRoot(t)
	result, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("unavailable: %v", err)
	}
	return string(result)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found")
		}
		dir = parent
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n"
}

func sha256Bytes(data []byte) string {
	return sha256String(string(data))
}

func sha256String(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}
