// Package faults owns the closed network and storage fault primitives
// of Release Qualification (VERIFY-FAULT-001/002): the digest-pinned
// Toxiproxy TCP vocabulary (latency, timeout, reset_peer, bandwidth,
// limit_data) and the quoin-faultfs storage errno vocabulary mounted
// through a privileged native container. Each primitive is observed
// through real client traffic on the host; teardown removes the owned
// containers and proves removal, because cleanup failure fails the
// ticket even when behavior assertions pass.
package faults

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// ToxiproxyImageTag is the frozen Toxiproxy version; the qualification
// resolves and records the exact repository digest at start.
const ToxiproxyImageTag = "ghcr.io/shopify/toxiproxy:2.12.0"

// TCPFaults is the closed TCP fault vocabulary of the frozen catalog
// (fault.network cells); the order is the catalog's cell order.
var TCPFaults = []string{"latency", "timeout", "reset_peer", "bandwidth", "limit_data"}

// StorageFaults maps each fault.storage cell fault to its operation and
// the Linux errno the witness must observe.
var StorageFaults = map[string]struct {
	Operation string
	Errno     int
}{
	"enospc":     {Operation: "write", Errno: 28},
	"edquot":     {Operation: "write", Errno: 122},
	"erofs":      {Operation: "write", Errno: 30},
	"fsync-eio":  {Operation: "fsync", Errno: 5},
	"rename-eio": {Operation: "rename", Errno: 5},
}

// StoragePaths are the catalog's path scopes inside the faultfs mount.
var StoragePaths = []string{"sqlite", "artifact-staging", "backup-output"}

// LoopbackHost is the address service APIs published on the
// qualification host's loopback are reached at. A qualification cell
// running inside a container against the host docker daemon reaches
// them through QUOIN_LOOPBACK_HOST (typically host.docker.internal);
// everything the docker daemon itself resolves keeps 127.0.0.1
// references.
func LoopbackHost() string {
	if host := osGetenv("QUOIN_LOOPBACK_HOST"); host != "" {
		return host
	}
	return "127.0.0.1"
}

// Toxiproxy drives one digest-pinned Toxiproxy container over its REST
// API. The API address is reachable from the host; proxied listeners
// are reachable from both host and sibling containers.
type Toxiproxy struct {
	container string
	api       string // host-side API endpoint
	proxyPort int    // host-side port of the single reused proxy listener
	upstream  string
}

// awaitAPI polls the Toxiproxy REST endpoint until it answers.
func (proxy *Toxiproxy) awaitAPI(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response, err := http.Get("http://" + proxy.api + "/version")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("toxiproxy API %s did not become ready", proxy.api)
}

// Version freezes the observed server version string for evidence.
func (proxy *Toxiproxy) Version() string {
	response, err := http.Get("http://" + proxy.api + "/version")
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 256))
	return strings.TrimSpace(string(body))
}

// toxicPayload is the API body of one toxic; the closed vocabulary maps
// to the exact attributes Toxiproxy owns (VERIFY-FAULT-001: TCP
// primitives map to digest-pinned Toxiproxy latency/timeout/reset_peer/
// bandwidth/limit_data and nothing else).
type toxicPayload struct {
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Stream     string         `json:"stream"`
	Toxicity   float64        `json:"toxicity"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// toxicFor names the deterministic toxic of one fault: the magnitudes
// are frozen so observations compare against stable thresholds.
func toxicFor(fault string) toxicPayload {
	switch fault {
	case "latency":
		return toxicPayload{Name: "t40-" + fault, Type: "latency", Stream: "downstream", Toxicity: 1.0,
			// 600ms leaves headroom over the +300ms classification
			// threshold after the kernel's coalescing absorbs part of
			// the per-event delay.
			Attributes: map[string]any{"latency": 600}}
	case "timeout":
		return toxicPayload{Name: "t40-" + fault, Type: "timeout", Stream: "downstream", Toxicity: 1.0,
			Attributes: map[string]any{"timeout": 250}}
	case "reset_peer":
		return toxicPayload{Name: "t40-" + fault, Type: "reset_peer", Stream: "downstream", Toxicity: 1.0}
	case "bandwidth":
		return toxicPayload{Name: "t40-" + fault, Type: "bandwidth", Stream: "downstream", Toxicity: 1.0,
			Attributes: map[string]any{"rate": 16}} // 16 KB/s over a 64 KiB body
	case "limit_data":
		return toxicPayload{Name: "t40-" + fault, Type: "limit_data", Stream: "downstream", Toxicity: 1.0,
			Attributes: map[string]any{"bytes": 2048}}
	}
	return toxicPayload{}
}

// ResetProxy (re)creates the single qualification proxy with no toxics,
// returning the connection to a clean route. Any previous instance is
// deleted first so the proxy always starts from a known clean state.
func (proxy *Toxiproxy) ResetProxy() error {
	_ = proxy.deleteProxy()
	body, _ := json.Marshal(map[string]any{
		"name": "t40-proxy", "listen": fmt.Sprintf("0.0.0.0:%d", proxy.proxyPort), "upstream": proxy.upstream,
	})
	request, _ := http.NewRequest(http.MethodPost, "http://"+proxy.api+"/proxies", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		payload, _ := io.ReadAll(response.Body)
		return fmt.Errorf("proxy create answered %d: %s", response.StatusCode, payload)
	}
	return nil
}

func (proxy *Toxiproxy) deleteProxy() error {
	request, _ := http.NewRequest(http.MethodDelete, "http://"+proxy.api+"/proxies/t40-proxy", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	response.Body.Close()
	return nil
}

// ApplyToxic installs one fault's toxic on the proxy.
func (proxy *Toxiproxy) ApplyToxic(fault string) error {
	body, _ := json.Marshal(toxicFor(fault))
	request, _ := http.NewRequest(http.MethodPost, "http://"+proxy.api+"/proxies/t40-proxy/toxics", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		payload, _ := io.ReadAll(response.Body)
		return fmt.Errorf("toxic %s rejected: %s", fault, payload)
	}
	return nil
}

// RemoveToxic uninstalls the fault's toxic.
func (proxy *Toxiproxy) RemoveToxic(fault string) error {
	request, _ := http.NewRequest(http.MethodDelete, "http://"+proxy.api+"/proxies/t40-proxy/toxics/t40-"+fault, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	response.Body.Close()
	return nil
}

// Address is the host-side proxy endpoint.
func (proxy *Toxiproxy) Address() string {
	return fmt.Sprintf("127.0.0.1:%d", proxy.proxyPort)
}

// Stop removes the container; the caller proves removal afterwards.
func (proxy *Toxiproxy) Stop() {
	removeContainer(proxy.container)
}

// ContainerRemoved proves the owned container is gone.
func (proxy *Toxiproxy) ContainerRemoved() bool {
	output, err := capture("docker", "inspect", proxy.container, "--format", "{{.Name}}")
	return err != nil && strings.Contains(strings.ToLower(output), "no such object")
}

func removeContainer(name string) {
	_ = exec.Command("docker", "rm", "-f", name).Run()
}

func capture(name string, arguments ...string) (string, error) {
	var combined bytes.Buffer
	command := exec.Command(name, arguments...)
	command.Stdout = &combined
	command.Stderr = &combined
	err := command.Run()
	return combined.String(), err
}
