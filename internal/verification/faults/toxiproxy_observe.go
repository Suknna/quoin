package faults

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// NetworkRig is the closed docker network the TCP fault primitives
// execute on: a paced upstream (faultclient upstream), the digest-pinned
// Toxiproxy container and an observer container whose faultclient
// exchanges see unmodified TCP semantics — host port-forwarding would
// flatten RST to EOF and distort pacing, so every observation runs
// inside the rig (fault.network cells, VERIFY-FAULT-001).
type NetworkRig struct {
	Name        string // unique network name
	ClientPath  string // host path of the linux faultclient binary
	Image       string // base image carrying sh
	Workdir     string // host directory receiving exchange reports
	ProxyAPI    int    // host-side 127.0.0.1 port for the Toxiproxy API
	proxy       *Toxiproxy
	proxyListen int // in-network proxy listener port
}

// StartNetworkRig creates the network and its three containers. names
// derive from the unique rig name, so cleanup owns exactly what it
// created.
func StartNetworkRig(name, clientPath, image, workdir string, apiPort int) (*NetworkRig, error) {
	rig := &NetworkRig{
		Name: name, ClientPath: clientPath, Image: image, Workdir: workdir,
		ProxyAPI: apiPort, proxyListen: 8666,
	}
	rig.Stop()
	if output, err := capture("docker", "network", "create", name); err != nil {
		return nil, fmt.Errorf("network create: %v: %s", err, output)
	}
	if output, err := capture("docker", "run", "-d", "--rm", "--name", name+"-upstream",
		"--network", name, "-v", clientPath+":/faultclient:ro", image, "/faultclient", "upstream", "--address", ":19090"); err != nil {
		rig.Stop()
		return nil, fmt.Errorf("upstream run: %v: %s", err, output)
	}
	if output, err := capture("docker", "run", "-d", "--rm", "--name", name+"-toxiproxy",
		"--network", name, "-p", fmt.Sprintf("127.0.0.1:%d:8474", apiPort), ToxiproxyImageTag); err != nil {
		rig.Stop()
		return nil, fmt.Errorf("toxiproxy run: %v: %s", err, output)
	}
	if output, err := capture("docker", "run", "-d", "--rm", "--name", name+"-observer",
		"--network", name, "-v", clientPath+":/faultclient:ro", "-v", workdir+":/work",
		"--entrypoint", "sh", image, "-c", "sleep 3600"); err != nil {
		rig.Stop()
		return nil, fmt.Errorf("observer run: %v: %s", err, output)
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		rig.Stop()
		return nil, err
	}
	rig.proxy = &Toxiproxy{
		container: name + "-toxiproxy",
		api:       LoopbackHost() + ":" + strconv.Itoa(apiPort),
		proxyPort: rig.proxyListen,
		upstream:  name + "-upstream:19090",
	}
	if err := rig.proxy.awaitAPI(30 * time.Second); err != nil {
		rig.Stop()
		return nil, err
	}
	if err := rig.proxy.ResetProxy(); err != nil {
		rig.Stop()
		return nil, err
	}
	return rig, nil
}

// Proxy exposes the rig's Toxiproxy controller.
func (rig *NetworkRig) Proxy() *Toxiproxy { return rig.proxy }

// exchangeReport mirrors the faultclient exchange JSON.
type exchangeReport struct {
	Received  int    `json:"received"`
	ElapsedMS int64  `json:"elapsedMs"`
	ErrorText string `json:"errorText"`
	Reset     bool   `json:"reset"`
	Eof       bool   `json:"eof"`
}

// exchangeInNetwork drives one observer exchange and reads its report.
func (rig *NetworkRig) exchangeInNetwork(tag string) (exchangeReport, error) {
	report := filepath.Join(rig.Workdir, "exchange-"+tag+".json")
	_ = os.Remove(report)
	target := fmt.Sprintf("%s-toxiproxy:%d", rig.Name, rig.proxyListen)
	output, err := capture("docker", "exec", rig.Name+"-observer",
		"/faultclient", "exchange", "--address", target, "--report", "/work/exchange-"+tag+".json")
	if err != nil {
		return exchangeReport{}, fmt.Errorf("observer exchange %s: %v: %s", tag, err, output)
	}
	body, err := os.ReadFile(report)
	if err != nil {
		return exchangeReport{}, err
	}
	var parsed exchangeReport
	if err := json.Unmarshal(body, &parsed); err != nil {
		return exchangeReport{}, err
	}
	return parsed, nil
}

// TCPObservation records what the in-network client observed under one
// fault, plus the pre-fault baseline of the same exchange.
type TCPObservation struct {
	Fault           string `json:"fault"`
	BaselineBytes   int    `json:"baselineBytes"`
	BaselineElapsed string `json:"baselineElapsed"`
	ReceivedBytes   int    `json:"receivedBytes"`
	TotalBytes      int    `json:"totalBytes"`
	Elapsed         string `json:"elapsed"`
	TransportError  string `json:"transportError,omitempty"`
	ConnectionReset bool   `json:"connectionReset"`
	StreamEOF       bool   `json:"streamEof"`
	ClientClass     string `json:"clientClass"`
}

// totalBody is the frozen exchange body size served by the faultclient
// upstream (64 KiB in 4 KiB chunks paced at 40ms).
const totalBody = 4096 * 16

// ObserveTCPFault runs the baseline exchange, applies the fault's
// toxic, runs the faulted exchange and classifies the client-side
// facts. The class vocabulary is closed: fault_deterministic_<fault>
// when the transport behaved exactly as the toxic dictates, and
// unexpected when anything else happened.
func (rig *NetworkRig) ObserveTCPFault(fault string) (TCPObservation, error) {
	if err := rig.proxy.ResetProxy(); err != nil {
		return TCPObservation{}, err
	}
	time.Sleep(200 * time.Millisecond)
	baseline, err := rig.exchangeInNetwork("baseline")
	if err != nil {
		return TCPObservation{}, err
	}
	if err := rig.proxy.ApplyToxic(fault); err != nil {
		return TCPObservation{}, err
	}
	time.Sleep(300 * time.Millisecond) // let the toxic take effect on new connections
	faulted, err := rig.exchangeInNetwork("faulted")
	if err != nil {
		return TCPObservation{}, err
	}
	observation := TCPObservation{
		Fault:           fault,
		BaselineBytes:   baseline.Received,
		BaselineElapsed: (time.Duration(baseline.ElapsedMS) * time.Millisecond).String(),
		ReceivedBytes:   faulted.Received,
		TotalBytes:      totalBody,
		Elapsed:         (time.Duration(faulted.ElapsedMS) * time.Millisecond).String(),
		TransportError:  faulted.ErrorText,
		ConnectionReset: faulted.Reset,
		StreamEOF:       faulted.Eof,
	}
	observation.ClientClass = classifyTCP(fault, observation, baseline, faulted)
	if err := rig.proxy.RemoveToxic(fault); err != nil {
		return observation, err
	}
	return observation, nil
}

// classifyTCP maps raw transport facts to the closed class per fault.
// Thresholds leave headroom around the frozen toxic magnitudes so
// scheduling noise cannot flip a class.
func classifyTCP(fault string, observation TCPObservation, baseline, faulted exchangeReport) string {
	complete := faulted.Received == totalBody && faulted.Eof
	baselineDuration := time.Duration(baseline.ElapsedMS) * time.Millisecond
	faultedDuration := time.Duration(faulted.ElapsedMS) * time.Millisecond
	switch fault {
	case "latency":
		// The toxic adds a fixed 400ms to the exchange; compare against
		// this rig's own baseline so host pacing cannot flip the class.
		if complete && faultedDuration >= baselineDuration+300*time.Millisecond {
			return "fault_deterministic_latency"
		}
	case "timeout":
		// Data transmission stops after the timeout window: the client
		// sees the stream end without any body past the window.
		if faulted.Eof && faulted.Received == 0 && faultedDuration >= 200*time.Millisecond {
			return "fault_deterministic_timeout"
		}
	case "reset_peer":
		if faulted.Reset {
			return "fault_deterministic_reset_peer"
		}
	case "bandwidth":
		if complete && faultedDuration >= baselineDuration+1500*time.Millisecond {
			return "fault_deterministic_bandwidth"
		}
	case "limit_data":
		if faulted.Eof && faulted.Received == 2048 {
			return "fault_deterministic_limit_data"
		}
	}
	return "unexpected"
}

// RoutesRestored proves the proxy returns to a clean route after the
// toxic is removed: a fresh baseline exchange completes in the baseline
// envelope again.
func (rig *NetworkRig) RoutesRestored() bool {
	if err := rig.proxy.ResetProxy(); err != nil {
		return false
	}
	time.Sleep(200 * time.Millisecond)
	fresh, err := rig.exchangeInNetwork("restored")
	return err == nil && fresh.Received == totalBody && fresh.Eof
}

// Stop removes the rig's three containers and the network. The
// containers are --rm, so their disappearance is asynchronous; the
// network removal retries briefly until they have fully gone.
func (rig *NetworkRig) Stop() {
	for _, suffix := range []string{"-observer", "-toxiproxy", "-upstream"} {
		removeContainer(rig.Name + suffix)
	}
	for attempt := 0; attempt < 20; attempt++ {
		if err := exec.Command("docker", "network", "rm", rig.Name).Run(); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Removed proves every owned container and the network are gone. The
// docker CLI's casing of its "no such object" / "network not found"
// answers has shifted between versions, so the proof matches the
// wording case-insensitively.
func (rig *NetworkRig) Removed() bool {
	for _, suffix := range []string{"-observer", "-toxiproxy", "-upstream"} {
		output, err := capture("docker", "inspect", rig.Name+suffix, "--format", "{{.Name}}")
		if err == nil || !strings.Contains(strings.ToLower(output), "no such object") {
			return false
		}
	}
	output, err := capture("docker", "network", "inspect", rig.Name, "--format", "{{.Name}}")
	lowered := strings.ToLower(output)
	return err != nil && (strings.Contains(lowered, "not found") || strings.Contains(lowered, "no such network"))
}

// BuildFaultclient cross-builds the in-network exchange actor for the
// docker server architecture.
func BuildFaultclient(outputPath, goarch, repoRoot string) error {
	command := exec.Command("go", "build", "-o", outputPath, "./internal/verification/faults/faultclient")
	command.Dir = repoRoot
	command.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
	var combined strings.Builder
	command.Stdout = &combined
	command.Stderr = &combined
	if err := command.Run(); err != nil {
		return fmt.Errorf("faultclient build: %v: %s", err, combined.String())
	}
	return nil
}
