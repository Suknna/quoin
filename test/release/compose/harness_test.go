package release_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// evidence records every command, artifact and observation the T30
// acceptance produces, mirroring the T01 recorder contract.
type evidence struct {
	t          *testing.T
	dir        string
	commands   []commandRecord
	artifacts  []artifactRecord
	gitCommit  string
	dirtyState string
	toolInfo   map[string]string
	startedAt  time.Time
}

type commandRecord struct {
	Name     string   `json:"name"`
	Args     []string `json:"args"`
	ExitCode int      `json:"exitCode"`
	Duration string   `json:"duration"`
}

type artifactRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

func newEvidence(t *testing.T, dir string) *evidence {
	t.Helper()
	recorder := &evidence{t: t, dir: dir, startedAt: time.Now().UTC(), toolInfo: map[string]string{}}
	recorder.gitCommit = strings.TrimSpace(recorder.output("git", "rev-parse", "HEAD"))
	status, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	recorder.dirtyState = sha256Hex(status)
	for name, argv := range map[string][]string{
		"go":      {"go", "version"},
		"docker":  {"docker", "version", "--format", "{{.Server.Version}}"},
		"compose": {"docker", "compose", "version"},
		"buildx":  {"docker", "buildx", "version"},
	} {
		recorder.toolInfo[name] = strings.TrimSpace(recorder.output(argv...))
	}
	recorder.note("environment.json", mustJSON(t, map[string]any{
		"gitCommit": recorder.gitCommit, "dirtyStateDigest": recorder.dirtyState, "tools": recorder.toolInfo,
	}))
	return recorder
}

func (recorder *evidence) output(argv ...string) string {
	recorder.t.Helper()
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		recorder.t.Fatalf("%s: %v", strings.Join(argv, " "), err)
	}
	return string(out)
}

// run executes a command in the repository root, records its exit code and
// combined output, and returns the output. wantExit -1 accepts any code.
func (recorder *evidence) run(name string, env []string, stdin io.Reader, wantExit int, argv ...string) string {
	recorder.t.Helper()
	logPath := filepath.Join(recorder.dir, name+".log")
	started := time.Now()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = repoRoot(recorder.t)
	if env != nil {
		command.Env = env
	}
	command.Stdin = stdin
	var combined bytes.Buffer
	command.Stdout = &combined
	command.Stderr = &combined
	_ = command.Run()
	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	_ = os.WriteFile(logPath, combined.Bytes(), 0o644)
	recorder.commands = append(recorder.commands, commandRecord{Name: name, Args: append([]string(nil), argv...), ExitCode: exitCode, Duration: time.Since(started).Round(time.Millisecond).String()})
	recorder.artifacts = append(recorder.artifacts, artifactRecord{Path: logPath, SHA256: sha256Hex(combined.Bytes()), Bytes: combined.Len()})
	if wantExit >= 0 && exitCode != wantExit {
		recorder.t.Fatalf("%s: exit=%d want=%d output:\n%s", name, exitCode, wantExit, combined.String())
	}
	return combined.String()
}

func (recorder *evidence) note(name, content string) {
	recorder.t.Helper()
	path := filepath.Join(recorder.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		recorder.t.Fatal(err)
	}
	recorder.artifacts = append(recorder.artifacts, artifactRecord{Path: path, SHA256: sha256Hex([]byte(content)), Bytes: len(content)})
}

func (recorder *evidence) observe(name string, value any) {
	recorder.t.Helper()
	recorder.note(name, mustJSON(recorder.t, value))
}

// composeEnv builds the environment for one isolated helper invocation.
func composeEnv(workRoot, project string) []string {
	return append(os.Environ(),
		"XDG_STATE_HOME="+filepath.Join(workRoot, project, "state"),
		"QUOIN_COMPOSE_PROJECT="+project,
		"QUOIN_DEPLOY_SCRIPTED=1",
		"DOCKER_CLI_HINTS=false",
	)
}

// loginAsAdmin proves a real same-origin login round-trip against the
// published public port; it returns whether the login succeeded.
func loginAsAdmin(t *testing.T, base, origin, username, password string) bool {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequest(http.MethodPost, base+"/api/v1/auth/login", strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode == http.StatusOK && strings.Contains(string(body), `"username":`)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sha256File(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return sha256Hex(data), nil
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func randomPassword(t *testing.T) string {
	t.Helper()
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatal(err)
	}
	return "t30-" + base64.RawURLEncoding.EncodeToString(buffer)
}
