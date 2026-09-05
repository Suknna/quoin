package suites

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/verification/environments"
	"github.com/Suknna/quoin/internal/verification/faults"
	"github.com/Suknna/quoin/internal/verification/fixtures"
)

// monitoringImages are the official monitoring stack members the
// qualification resolves and freezes by digest (VERIFY-EXTERNAL-001).
var monitoringImages = map[string]string{
	"prometheus":   "prom/prometheus:v3.7.2",
	"alertmanager": "prom/alertmanager:v0.28.1",
	"thanos":       "thanosio/thanos:v0.39.2",
}

// monitoringLegs records the per-cell facts of the monitoring-stack
// suite.
type monitoringLegs struct {
	Cell                        string            `json:"cell"`
	PrometheusDigest            string            `json:"prometheusDigest"`
	AlertmanagerDigest          string            `json:"alertmanagerDigest"`
	ThanosDigest                string            `json:"thanosDigest"`
	PrometheusQueryHappyPath    bool              `json:"prometheusQueryHappyPath"`
	ThanosQuerySemantics        bool              `json:"thanosQuerySemantics"`
	AlertmanagerWebhookDelivery bool              `json:"alertmanagerWebhookDelivery"`
	SteleIngestAndProjection    bool              `json:"steleIngestAndProjection"`
	ProtocolErrorFixtures       map[string]string `json:"protocolErrorFixtures"`
	ProtocolFixturesAllObserved bool              `json:"protocolFixturesAllObserved"`
	TCPFaultPrimitives          map[string]string `json:"tcpFaultPrimitives"`
	TCPFaultsAllDeterministic   bool              `json:"tcpFaultsAllDeterministic"`
	Detail                      map[string]string `json:"detail"`
}

// RunMonitoringPhase executes the integration.monitoring-stack cell:
// official digest-pinned Prometheus, Alertmanager and Thanos happy
// paths, the deterministic protocol error corpus, the Toxiproxy TCP
// vocabulary and the Stele ingest projection on the live deployment.
func RunMonitoringPhase(request DeploymentRequest, stack *Stack, adminPassword string) error {
	switch request.Phase {
	case PhaseSetup:
		// Resolve and freeze the official digests first; the containers
		// run pinned to exactly these references.
		legs := monitoringLegs{Cell: request.Cell, Detail: map[string]string{}, ProtocolErrorFixtures: map[string]string{}, TCPFaultPrimitives: map[string]string{}}
		for name, tag := range monitoringImages {
			image, err := environments.ResolveImageDigest(execRunner{}, tag)
			if err != nil {
				return fmt.Errorf("resolve %s: %w", name, err)
			}
			switch name {
			case "prometheus":
				legs.PrometheusDigest = image.Reference
			case "alertmanager":
				legs.AlertmanagerDigest = image.Reference
			case "thanos":
				legs.ThanosDigest = image.Reference
			}
		}
		if err := request.storeJSON("monitoring-images.json", legs); err != nil {
			return err
		}
		request.logf("resolved official digests: prometheus=%s alertmanager=%s thanos=%s", legs.PrometheusDigest, legs.AlertmanagerDigest, legs.ThanosDigest)
		return nil
	case PhaseAction:
		var legs monitoringLegs
		if err := request.loadJSON("monitoring-images.json", &legs); err != nil {
			return err
		}
		legs = driveMonitoringLegs(request, stack, adminPassword, legs)
		if storeErr := request.storeJSON("monitoring-"+request.Cell+".json", legs); storeErr != nil {
			return storeErr
		}
		return nil
	case PhaseAssert:
		var legs monitoringLegs
		if err := request.loadJSON("monitoring-"+request.Cell+".json", &legs); err != nil {
			return fmt.Errorf("monitoring observations missing: %w", err)
		}
		passed := legs.PrometheusQueryHappyPath && legs.ThanosQuerySemantics && legs.AlertmanagerWebhookDelivery &&
			legs.SteleIngestAndProjection && legs.ProtocolFixturesAllObserved && legs.TCPFaultsAllDeterministic
		facts := map[string]any{
			"prometheus-query-happy-path":            boolWord(legs.PrometheusQueryHappyPath),
			"thanos-prometheus-api-query-semantics":  boolWord(legs.ThanosQuerySemantics),
			"alertmanager-webhook-delivery":          boolWord(legs.AlertmanagerWebhookDelivery),
			"stele-ingest-and-occurrence-projection": boolWord(legs.SteleIngestAndProjection),
			"deterministic-protocol-error-fixtures":  boolWord(legs.ProtocolFixturesAllObserved),
			"tcp-fault-primitives":                   boolWord(legs.TCPFaultsAllDeterministic),
		}
		checks := make([]map[string]string, 0, len(facts))
		for id, actual := range facts {
			state := "failed"
			if actual == "passed" {
				state = "passed"
			}
			checks = append(checks, map[string]string{"name": id, "result": state})
		}
		if err := request.writeFacts(facts, checks); err != nil {
			return err
		}
		if !passed {
			return fmt.Errorf("monitoring cell assertions failed: %+v", legs)
		}
		return nil
	case PhaseTeardown:
		name := faultRigName(request, "mon")
		removeContainers(name + "-prometheus")
		removeContainers(name + "-alertmanager")
		removeContainers(name + "-thanos")
		// The --rm containers disappear asynchronously; retry the
		// network removal until they release it.
		for attempt := 0; attempt < 20; attempt++ {
			if runDocker("network", "rm", name) == nil {
				return nil
			}
			time.Sleep(500 * time.Millisecond)
		}
		return nil
	}
	return fmt.Errorf("unknown phase %q", request.Phase)
}

func driveMonitoringLegs(request DeploymentRequest, stack *Stack, adminPassword string, legs monitoringLegs) monitoringLegs {
	network := faultRigName(request, "mon")
	_ = exec.Command("docker", "network", "create", network).Run()

	// Prometheus scraping itself on its frozen happy path.
	promConfig := filepath.Join(request.Workdir, "prometheus.yml")
	_ = os.WriteFile(promConfig, []byte("global:\n  scrape_interval: 1s\nscrape_configs:\n  - job_name: prometheus\n    static_configs:\n      - targets: ['localhost:9090']\n"), 0o644)
	promName := network + "-prometheus"
	removeContainers(promName)
	if output, err := exec.Command("docker", "run", "-d", "--rm", "--name", promName, "--network", network,
		"-v", promConfig+":/etc/prometheus/prometheus.yml:ro", legs.PrometheusDigest).CombinedOutput(); err != nil {
		legs.Detail["prometheus-run"] = firstLine(string(output))
		return legs
	}
	legs.PrometheusQueryHappyPath = queryPrometheus(network, promName, "/api/v1/query?query=up", legs.Detail)

	// Thanos Query exposes the same PromQL API semantics; a real query
	// against it proves the API surface and error classes.
	thanosName := network + "-thanos"
	removeContainers(thanosName)
	// Thanos Query serves the PromQL API surface with no store bound;
	// the semantics assertions run against that real API.
	if output, err := exec.Command("docker", "run", "-d", "--rm", "--name", thanosName, "--network", network,
		legs.ThanosDigest, "query", "--http-address=0.0.0.0:9090").CombinedOutput(); err != nil {
		legs.Detail["thanos-run"] = firstLine(string(output))
	} else {
		legs.ThanosQuerySemantics = queryPrometheus(network, thanosName, "/api/v1/query?query=up", legs.Detail) &&
			queryPrometheusError(network, thanosName, legs.Detail)
	}

	// Alertmanager delivering one webhook to a real receiver.
	legs.AlertmanagerWebhookDelivery = driveAlertmanagerWebhook(request, network, legs.AlertmanagerDigest, legs.Detail)

	// The Stele ingest projection on the live deployment.
	if session, err := stack.Login("admin", adminPassword); err == nil {
		legs.SteleIngestAndProjection = driveSteleDelivery(session, stack, legs.Detail)
	} else {
		legs.Detail["stele-login"] = err.Error()
	}

	// The deterministic protocol error corpus through a real client.
	corpus, err := fixtures.LoadCorpus(filepath.Join(request.RepoRoot, fixtures.CorpusDirDefault))
	if err != nil {
		legs.Detail["corpus"] = err.Error()
		return legs
	}
	replay := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, r *http.Request) {
		var fault fixtures.ProtocolFault
		_ = json.NewDecoder(r.Body).Decode(&fault)
		fault.Serve(writer, r)
	}))
	defer replay.Close()
	legs.ProtocolFixturesAllObserved = true
	for _, fault := range corpus {
		probeURL := fmt.Sprintf("%s/probe?f=%s", replay.URL, fault.ID)
		request, _ := http.NewRequest(http.MethodPost, probeURL, strings.NewReader(mustJSON(fault)))
		_ = request
		// Drive each fault through a dedicated replay endpoint so the
		// replay server serves exactly the requested fixture.
		observation := observeViaReplay(replay.URL, fault)
		legs.ProtocolErrorFixtures[fault.ID] = observation.ClientClass
		if observation.ClientClass != fault.Behavior+"_observed" {
			legs.ProtocolFixturesAllObserved = false
		}
	}

	// The closed TCP fault vocabulary against the in-network rig.
	arch, archErr := request.ServerArch()
	if archErr != nil {
		legs.Detail["tcp-arch"] = archErr.Error()
		return legs
	}
	client := filepath.Join(request.Workdir, "faultclient")
	if err := faults.BuildFaultclient(client, arch, request.RepoRoot); err != nil {
		legs.Detail["tcp-build"] = err.Error()
		return legs
	}
	rig, err := faults.StartNetworkRig(faultRigName(request, "tcpmon"), client, "alpine:3.20",
		filepath.Join(request.Workdir, "tcpmon"), 28574)
	if err != nil {
		legs.Detail["tcp-rig"] = err.Error()
		return legs
	}
	defer rig.Stop()
	legs.TCPFaultsAllDeterministic = true
	for _, fault := range faults.TCPFaults {
		observation, err := rig.ObserveTCPFault(fault)
		if err != nil {
			legs.Detail["tcp-"+fault] = err.Error()
			legs.TCPFaultsAllDeterministic = false
			continue
		}
		legs.TCPFaultPrimitives[fault] = observation.ClientClass
		if observation.ClientClass != "fault_deterministic_"+fault {
			legs.TCPFaultsAllDeterministic = false
		}
	}
	return legs
}

// queryViaHelper drives one HTTP request against a monitoring member
// through a same-network alpine helper container (the official
// Prometheus and Thanos images are distroless and carry no shell).
func queryViaHelper(network, target, path string) (string, int) {
	output, code, _ := runDockerCapture("run", "--rm", "--network", network, "alpine:3.20",
		"wget", "-qO-", "-T", "20", "http://"+target+":9090"+path)
	return output, code
}

// queryPrometheus proves the happy path against one Prometheus-shaped
// API member: a 200 with a well-formed vector result.
func queryPrometheus(network, container, path string, detail map[string]string) bool {
	// The API member needs a few seconds after container start before
	// its listener binds; retry the exchange inside that window.
	var output string
	code := 1
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		output, code = queryViaHelper(network, container, path)
		if code == 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if code != 0 {
		detail["query-"+container] = fmt.Sprintf("exit=%d %s", code, firstLine(output))
		return false
	}
	var document struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []any  `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(output), &document) != nil || document.Status != "success" {
		detail["query-"+container] = firstLine(output)
		return false
	}
	return true
}

// queryPrometheusError proves the API error semantics: a malformed
// query must answer a structured error, never a 2xx.
func queryPrometheusError(network, container string, detail map[string]string) bool {
	// The malformed-query semantics only count once the member answers
	// at all, so wait for the healthy query first.
	if !queryPrometheus(network, container, "/api/v1/query?query=up", detail) {
		return false
	}
	output, code := queryViaHelper(network, container, "/api/v1/query?query=up%7B")
	if strings.Contains(output, `"status":"error"`) || code != 0 {
		return true
	}
	detail["query-error-"+container] = firstLine(output)
	return false
}

// driveAlertmanagerWebhook proves real webhook delivery: Alertmanager
// forwards a posted alert to a real HTTP receiver.
func driveAlertmanagerWebhook(request DeploymentRequest, network, digest string, detail map[string]string) bool {
	catcher := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		// Alertmanager retries on failure; a second delivery must never
		// block the handler while the first payload is still queued.
		select {
		case catcher <- string(body):
		default:
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	// The receiver must be reachable from the container: route it
	// through the host gateway alias Docker provides.
	receiver := strings.Replace(server.URL, "127.0.0.1", "host.docker.internal", 1)
	config := filepath.Join(request.Workdir, "alertmanager.yml")
	if writeErr := os.WriteFile(config, []byte(fmt.Sprintf("route:\n  group_by: [alertname]\n  group_interval: 1s\n  receiver: t40\nreceivers:\n  - name: t40\n    webhook_configs:\n      - url: %q\n", receiver)), 0o644); writeErr != nil {
		detail["alertmanager-config"] = writeErr.Error()
		return false
	}
	name := network + "-alertmanager"
	removeContainers(name)
	output, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--network", network,
		"--add-host", "host.docker.internal:host-gateway",
		"-v", config+":/etc/alertmanager/alertmanager.yml:ro", digest).CombinedOutput()
	if err != nil {
		detail["alertmanager-run"] = firstLine(string(output))
		return false
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ready, _, err := runDockerCapture("exec", name, "wget", "-qO-", "-T", "5", "http://127.0.0.1:9093/-/ready"); err == nil && strings.Contains(ready, "OK") {
			break
		}
		time.Sleep(time.Second)
	}
	alert := `[{"status":"firing","labels":{"alertname":"T40MonitoringCell","severity":"warning"},"startsAt":"` + time.Now().UTC().Format(time.RFC3339) + `","endsAt":"` + time.Now().UTC().Add(time.Hour).Format(time.RFC3339) + `"}]`
	postOutput, code, err := runDockerCapture("exec", name, "wget", "-qO-", "--post-data", alert, "--header", "Content-Type: application/json", "-T", "10", "http://127.0.0.1:9093/api/v2/alerts")
	detail["alertmanager-post"] = fmt.Sprintf("exit=%d out=%.120s", code, firstLine(postOutput))
	if err != nil || code != 0 {
		return false
	}
	select {
	case body := <-catcher:
		detail["alertmanager-webhook"] = firstLine(body)
		return strings.Contains(body, "T40MonitoringCell")
	case <-time.After(45 * time.Second):
		if logs, _, logErr := runDockerCapture("logs", "--tail", "20", name); logErr == nil {
			detail["alertmanager-webhook"] = "not delivered; logs: " + lastWords(logs, 6)
		} else {
			detail["alertmanager-webhook"] = "not delivered"
		}
		return false
	}
}

// observeViaReplay drives one corpus fault through the replay server
// with a dedicated endpoint per fixture.
func observeViaReplay(base string, fault fixtures.ProtocolFault) fixtures.FaultObservation {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, r *http.Request) {
		fault.Serve(writer, r)
	}))
	defer server.Close()
	return fixtures.Observe(&http.Client{}, server.URL+"/probe", fault, 3*time.Second)
}

func mustJSON(value any) string {
	body, _ := json.Marshal(value)
	return string(body)
}

// execRunner adapts exec.Command to the environments probe interface.
type execRunner struct{}

func (execRunner) Output(name string, arguments ...string) (string, error) {
	body, err := exec.Command(name, arguments...).CombinedOutput()
	return string(body), err
}

// lastWords returns the final words of a log blob for detail records.
func lastWords(text string, count int) string {
	fields := strings.Fields(text)
	if len(fields) > count {
		fields = fields[len(fields)-count:]
	}
	return strings.Join(fields, " ")
}
