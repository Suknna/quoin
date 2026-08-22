// Package attachments hosts the T14 ticket acceptance run; this file
// carries the evidence/command/HTTP helpers shared by the acceptance test
// (mirroring the T13 harness: one compose stack, streaming multipart
// uploads and the ui-message-stream drain).
package attachments

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	projectName = "quoin"
	quoinPort   = 18080
)

type ticketEvidence struct {
	dir       string
	commands  []map[string]any
	artifacts []map[string]any
	env       []string
	stateRoot string
}

func (evidence *ticketEvidence) run(t *testing.T, name string, stdin io.Reader, command string, arguments ...string) string {
	t.Helper()
	started := time.Now()
	cmd := exec.Command(command, arguments...)
	cmd.Env = evidence.env
	cmd.Dir = repoRoot(t)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	exitCode := 0
	if err := cmd.Run(); err != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	logPath := filepath.Join(evidence.dir, name+".log")
	os.WriteFile(logPath, combined.Bytes(), 0o644)
	evidence.commands = append(evidence.commands, map[string]any{"name": name, "exitCode": exitCode, "duration": time.Since(started).Round(time.Millisecond).String()})
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": logPath, "sha256": sha256Hex(combined.Bytes()), "bytes": combined.Len()})
	if exitCode != 0 {
		t.Fatalf("%s: exit=%d output:\n%s", name, exitCode, combined.String())
	}
	return combined.String()
}

func (evidence *ticketEvidence) logCommand(t *testing.T, name string, cmd *exec.Cmd) {
	t.Helper()
	started := time.Now()
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	exitCode := 0
	if err := cmd.Run(); err != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	logPath := filepath.Join(evidence.dir, name+".log")
	os.WriteFile(logPath, combined.Bytes(), 0o644)
	evidence.commands = append(evidence.commands, map[string]any{"name": name, "exitCode": exitCode, "duration": time.Since(started).Round(time.Millisecond).String()})
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": logPath, "sha256": sha256Hex(combined.Bytes()), "bytes": combined.Len()})
	if exitCode != 0 {
		t.Fatalf("%s: exit=%d output:\n%s", name, exitCode, combined.String())
	}
}

func (evidence *ticketEvidence) note(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(evidence.dir, name)
	os.WriteFile(path, []byte(content), 0o644)
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": path, "sha256": sha256Hex([]byte(content)), "bytes": len(content)})
}

func scanForSecrets(t *testing.T, evidenceDir string, secrets ...string) {
	t.Helper()
	filepath.Walk(evidenceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, _ := os.ReadFile(path)
		for _, secret := range secrets {
			if secret != "" && bytes.Contains(data, []byte(secret)) {
				t.Fatalf("secret material leaked into %s", path)
			}
		}
		return nil
	})
}

func cookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func httpPost(t *testing.T, client *http.Client, target, origin, body string) string {
	t.Helper()
	return httpPostExpect(t, client, target, origin, body, 0)
}

// httpPostExpect posts and returns the body; when want != 0 the status
// must match exactly (conflict/validation paths).
func httpPostExpect(t *testing.T, client *http.Client, target, origin, body string, want int) string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if want != 0 {
		if response.StatusCode != want {
			t.Fatalf("%s: status=%d want=%d body=%.800s", target, response.StatusCode, want, raw)
		}
		return string(raw)
	}
	if response.StatusCode >= 400 {
		t.Fatalf("%s: status=%d body=%.800s", target, response.StatusCode, raw)
	}
	return string(raw)
}

func httpPut(t *testing.T, client *http.Client, target, origin, body string) string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPut, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if response.StatusCode >= 400 {
		t.Fatalf("%s: status=%d body=%.800s", target, response.StatusCode, raw)
	}
	return string(raw)
}

func httpGet(t *testing.T, client *http.Client, target, origin string) string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if response.StatusCode >= 400 {
		t.Fatalf("%s: status=%d body=%.800s", target, response.StatusCode, raw)
	}
	return string(raw)
}

// uploadAttachment posts one streaming multipart staging upload and
// returns the raw body; want 0 accepts any 2xx, otherwise exact.
func uploadAttachment(t *testing.T, client *http.Client, base, origin, command, filename string, content []byte, want int) (string, int) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	field, _ := writer.CreateFormField("clientCommandId")
	field.Write([]byte(command))
	file, _ := writer.CreateFormFile("file", filename)
	file.Write(content)
	writer.Close()
	request, _ := http.NewRequest(http.MethodPost, base+"/api/v1/investigation-attachments", bytes.NewReader(buffer.Bytes()))
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("upload %s: %v", filename, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if want != 0 && response.StatusCode != want {
		t.Fatalf("upload %s: status=%d want=%d body=%.600s", filename, response.StatusCode, want, raw)
	}
	if want == 0 && response.StatusCode >= 400 {
		t.Fatalf("upload %s: status=%d body=%.600s", filename, response.StatusCode, raw)
	}
	return string(raw), response.StatusCode
}

func randomSecret(t *testing.T, length int) string {
	t.Helper()
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(content), 0o644)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found")
		}
		dir = parent
	}
}

func outputOf(t *testing.T, command string, arguments ...string) string {
	t.Helper()
	out, err := exec.Command(command, arguments...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("unavailable: %v", err)
	}
	return string(out)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}

// drainStream attaches one ui-message-stream turn to completion and
// returns the data frames (T13 owns the exact framing matrix; T14 proves
// the same protocol path carries attachment/tool turns end to end).
func drainStream(t *testing.T, client *http.Client, target, origin, body string) []string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	request.Header.Set("Accept", "text/event-stream")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("stream %s: %v", target, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		t.Fatalf("stream %s: status=%d body=%.800s", target, response.StatusCode, raw)
	}
	var frames []string
	reader := bytes.NewReader(nil)
	_ = reader
	buffer := make([]byte, 16*1024)
	var pending []byte
	for {
		read, readErr := response.Body.Read(buffer)
		if read > 0 {
			pending = append(pending, buffer[:read]...)
		}
		for {
			index := bytes.IndexByte(pending, '\n')
			if index < 0 {
				break
			}
			line := strings.TrimSuffix(string(pending[:index]), "\r")
			pending = pending[index+1:]
			if strings.HasPrefix(line, "data:") {
				frames = append(frames, strings.TrimPrefix(line, "data: "))
			}
		}
		if readErr != nil {
			break
		}
	}
	if len(frames) == 0 || frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("stream must terminate with [DONE]: %v", frames)
	}
	return frames
}
