package environments

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	responses map[string]string
	errs      map[string]error
}

func (runner fakeRunner) Output(name string, arguments ...string) (string, error) {
	key := name + " " + strings.Join(arguments, " ")
	if err, present := runner.errs[key]; present {
		return "", err
	}
	if response, present := runner.responses[key]; present {
		return response, nil
	}
	return "", errors.New("unexpected probe: " + key)
}

func TestResolveNativeReportsComposeWithNativeExecution(t *testing.T) {
	runner := fakeRunner{responses: map[string]string{
		"docker version --format {{.Server.Os}}/{{.Server.Arch}} {{.Server.Version}}": "linux/arm64 29.4.0",
		"docker version --format {{.Client.Os}}/{{.Client.Arch}}":                     "linux/arm64",
	}}
	native := ResolveNative(runner)
	if native.Backend != "compose" || native.Architecture != "linux/arm64" {
		t.Fatalf("backend/architecture: %+v", native)
	}
	if !native.NativeExecution || native.Emulated {
		t.Fatalf("equal host/server platforms must be native: %+v", native)
	}
	if native.ServerVersion != "29.4.0" {
		t.Fatalf("server version not frozen: %+v", native)
	}
}

func TestResolveNativeMarksEmulatedServerNonNative(t *testing.T) {
	runner := fakeRunner{responses: map[string]string{
		"docker version --format {{.Server.Os}}/{{.Server.Arch}} {{.Server.Version}}": "linux/amd64 29.4.0",
		"docker version --format {{.Client.Os}}/{{.Client.Arch}}":                     "linux/arm64",
	}}
	native := ResolveNative(runner)
	if native.NativeExecution || !native.Emulated {
		t.Fatalf("a foreign-architecture server must never satisfy a native cell: %+v", native)
	}
}

func TestResolveNativeWithoutDockerReportsNoBackend(t *testing.T) {
	native := ResolveNative(fakeRunner{errs: map[string]error{
		"docker version --format {{.Server.Os}}/{{.Server.Arch}} {{.Server.Version}}": errors.New("no docker"),
	}})
	if native.Backend != "" || native.Architecture != "" {
		t.Fatalf("missing docker must not report a backend: %+v", native)
	}
}

func TestSelectMaintainedWindowPicksThreeLatestMinors(t *testing.T) {
	published := map[string]bool{
		"v1.31.9": true, "v1.31.4": true,
		"v1.32.5": true, "v1.32.1": true,
		"v1.33.2": true, "v1.33.0": true,
		"v1.34.1": true, "v1.34.0": true, "v1.34.3": true,
		"v1.29.8":       true, // EOL minor: outside the window
		"not-a-version": true,
	}
	window := SelectMaintainedWindow(published, time.Now())
	if len(window) != 3 {
		t.Fatalf("window length %d, want 3: %+v", len(window), window)
	}
	want := []string{"v1.34.3", "v1.33.2", "v1.32.5"}
	for index, cell := range window {
		if cell.Selector != strings.Replace("maintained_minor_INDEX.latest_patch", "INDEX", string(rune('0'+index)), 1) {
			t.Fatalf("selector %q out of order", cell.Selector)
		}
		if cell.Version != want[index] {
			t.Fatalf("cell %d = %s, want %s", index, cell.Version, want[index])
		}
	}
}

func TestResolveKubernetesWindowFailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/release/stable.txt":
			_, _ = writer.Write([]byte("v1.34.3\n"))
		case "/release/stable-1.34.txt":
			_, _ = writer.Write([]byte("v1.34.3\n"))
		case "/release/stable-1.33.txt":
			_, _ = writer.Write([]byte("v1.33.2\n"))
		case "/release/stable-1.32.txt":
			_, _ = writer.Write([]byte("v1.32.5\n"))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	// The resolver must read the official endpoints; rerouting it at the
	// transport level proves the selection logic end to end offline.
	original := stableEndpoint
	defer func() { stableEndpoint = original }()
	stableEndpoint = server.URL + "/release/stable.txt"
	originalMinor := minorEndpointFmt
	defer func() { minorEndpointFmt = originalMinor }()
	minorEndpointFmt = server.URL + "/release/stable-1.%d.txt"

	window, err := ResolveKubernetesWindow(context.Background(), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(window.Cells) != 3 || window.Cells[0].Version != "v1.34.3" || window.Cells[2].Version != "v1.32.5" {
		t.Fatalf("window: %+v", window.Cells)
	}
	if window.ResolvedAt == "" || window.Source == "" {
		t.Fatalf("resolution must freeze provenance: %+v", window)
	}

	// A truncated window must fail closed rather than freeze a partial one.
	stableEndpoint = server.URL + "/release/missing.txt"
	if _, err := ResolveKubernetesWindow(context.Background(), server.Client()); err == nil {
		t.Fatal("unreachable stable endpoint must fail closed")
	}
}

func TestResolveImageDigestPrefersDigestReference(t *testing.T) {
	runner := fakeRunner{responses: map[string]string{
		"docker pull ghcr.io/shopify/toxiproxy:2.12.0":                                         "",
		"docker image inspect ghcr.io/shopify/toxiproxy:2.12.0 --format {{json .RepoDigests}}": `["ghcr.io/shopify/toxiproxy@sha256:other","ghcr.io/shopify/toxiproxy@sha256:first"]`,
	}}
	image, err := ResolveImageDigest(runner, "ghcr.io/shopify/toxiproxy:2.12.0")
	if err != nil {
		t.Fatal(err)
	}
	if image.Digest != "ghcr.io/shopify/toxiproxy@sha256:first" || image.Reference == "" {
		t.Fatalf("digest not deterministically frozen: %+v", image)
	}
}
