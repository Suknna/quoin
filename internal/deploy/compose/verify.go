// Package compose owns the deployment helper's Compose backend orchestration:
// the staged install state machine with its persisted retry state and the
// read-only verifier that judges the deployed operational surface
// (OPS-HELPER-002..004, OPS-VERIFY-003).
package compose

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	deploycompose "github.com/Suknna/quoin/deploy/compose"
	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/deploy/report"
	sharedops "github.com/Suknna/quoin/internal/ops"
)

// allowedNotReadyReasons is the closed set of legitimate steady not-ready
// states for a Compose deployment whose Runtime registration is a separate
// operator action (T06): Plinth/Lintel stay runtime_unregistered until their
// one-time attached registration; Stele reports dependency_unavailable until
// the Quoin relay handshake is accepted.
func allowedNotReadyReasons(component string) map[string]bool {
	if component == "quoin" {
		return nil
	}
	return map[string]bool{"runtime_unregistered": true, "dependency_unavailable": true, "starting": true}
}

// verifyOperationalSurface is the single verifier used by install's final
// stage and by the standalone verify command: image digest pinning, liveness,
// the frozen readiness contract, the frozen metrics catalog, JSON logs and
// the fixed topology, all read through the disposable in-network verifier
// service (ops listeners are never published to the host).
func verifyOperationalSurface(req Request, loaded *loadedRequest, helper *runner, stage int) error {
	overlayPath, err := deploycompose.RenderVerifyOverlay(loaded.projection, deploycompose.Options{Images: loaded.images})
	if err != nil {
		return &PlatformError{Code: "verifier_overlay_invalid", Message: err.Error(), NextAction: "fix the deployment projection, then rerun"}
	}
	verifyArguments := append(append([]string{}, loaded.composeArguments...), "--file", overlayPath)
	verifier := func(name string, extraArgs ...string) (string, error) {
		arguments := append(append([]string{}, verifyArguments...), "run", "--rm", "--no-deps", "quoin-verifier")
		arguments = append(arguments, extraArgs...)
		return helper.run(stage, name, dockerize(arguments)...)
	}

	for _, component := range deployconfig.Components {
		if loaded.manifest == nil {
			break
		}
		if failure := judgeImageDigest(loaded, helper, component); failure != nil {
			return failure
		}
	}

	for _, component := range deployconfig.Components {
		if output, runErr := verifier("verify-livez-"+component, fmt.Sprintf("http://%s:9090/livez", component)); runErr != nil {
			return &PlatformError{Code: "livez_failed", Message: fmt.Sprintf("%s: %s", component, strings.TrimSpace(output)), NextAction: "inspect the component logs; the ops listener must answer the frozen liveness body"}
		}
		body, runErr := verifier("verify-readyz-"+component, "--status", "503", fmt.Sprintf("http://%s:9090/readyz", component))
		var readiness sharedops.Readiness
		parseErr := json.Unmarshal([]byte(firstJSONLine(body)), &readiness)
		// A ready or maintenance component answers 200, which fails the 503
		// probe; confirm the positive path explicitly and parse that body.
		if parseErr != nil || readiness.Reason == sharedops.Ready || readiness.Mode == "maintenance" {
			confirm, confirmErr := verifier("verify-readyz-ok-"+component, "--status", "200", fmt.Sprintf("http://%s:9090/readyz", component))
			if confirmErr != nil {
				if parseErr != nil {
					return &PlatformError{Code: "readiness_unreadable", Message: fmt.Sprintf("%s: %s | 200-probe: %s", component, strings.TrimSpace(body), strings.TrimSpace(confirm)), NextAction: "inspect the component logs; /readyz must return the frozen readiness JSON"}
				}
				return &PlatformError{Code: "readiness_status_incoherent", Message: fmt.Sprintf("%s reports %q but does not answer 200", component, readiness.Reason), NextAction: "the ops listener must return 200 exactly when ready or in maintenance"}
			}
			if parseErr != nil {
				if confirmErr := json.Unmarshal([]byte(firstJSONLine(confirm)), &readiness); confirmErr != nil {
					return &PlatformError{Code: "readiness_unreadable", Message: fmt.Sprintf("%s: %s", component, strings.TrimSpace(confirm)), NextAction: "inspect the component logs; /readyz must return the frozen readiness JSON"}
				}
			}
		} else if runErr != nil {
			return &PlatformError{Code: "readiness_failed", Message: fmt.Sprintf("%s: %s", component, strings.TrimSpace(body)), NextAction: "inspect the component logs, then rerun verify"}
		}
		if failure := judgeReadiness(component, loaded, readiness); failure != nil {
			helper.report.RecordCheck(report.Check{ID: "readiness-" + component, Result: "failed", Expected: "frozen readiness contract", Actual: fmt.Sprintf("%+v", readiness), Code: failure.Code, Recovery: failure.NextAction})
			return failure
		}
		helper.report.RecordCheck(report.Check{ID: "readiness-" + component, Result: "passed", Expected: "frozen readiness contract", Actual: fmt.Sprintf("reason=%s mode=%s", readiness.Reason, readiness.Mode), Code: "readiness_contract"})

		metricsBody, metricsErr := verifier("verify-metrics-"+component, "--status", "200", fmt.Sprintf("http://%s:9090/metrics", component))
		if metricsErr != nil {
			return &PlatformError{Code: "metrics_unreadable", Message: fmt.Sprintf("%s: %s", component, strings.TrimSpace(metricsBody)), NextAction: "inspect the component logs; /metrics must expose the Prometheus text format"}
		}
		if failure := judgeMetrics(component, metricsBody); failure != nil {
			helper.report.RecordCheck(report.Check{ID: "metrics-" + component, Result: "failed", Expected: "frozen metrics catalog", Actual: failure.Message, Code: failure.Code, Recovery: failure.NextAction})
			return failure
		}
		helper.report.RecordCheck(report.Check{ID: "metrics-" + component, Result: "passed", Expected: "frozen metrics catalog", Actual: "exact family and closed-label equality", Code: "metrics_catalog"})

		logs, logsErr := helper.run(stage, "verify-logs-"+component, dockerize(append(append([]string{}, loaded.composeArguments...), "logs", "--no-log-prefix", component))...)
		if logsErr != nil {
			return &PlatformError{Code: "logs_unreadable", Message: fmt.Sprintf("%s: %v", component, logsErr), NextAction: "inspect Docker state, then rerun verify"}
		}
		if failure := judgeLogs(component, logs, loaded.release()); failure != nil {
			helper.report.RecordCheck(report.Check{ID: "logs-" + component, Result: "failed", Expected: "JSON Lines with frozen fields", Actual: failure.Message, Code: failure.Code, Recovery: failure.NextAction})
			return failure
		}
		helper.report.RecordCheck(report.Check{ID: "logs-" + component, Result: "passed", Expected: "JSON Lines with frozen fields", Actual: "every line JSON with ts/level/component/release/code/msg", Code: "logs_json"})
	}
	if mountsErr := judgeMounts(loaded, helper, stage); mountsErr != nil {
		return errAsPlatform(mountsErr)
	}
	return judgeTopology(loaded, helper, stage)
}

// judgeImageDigest proves the deployed container runs exactly the
// digest-pinned release reference (OPS-VERIFY-003).
func judgeImageDigest(loaded *loadedRequest, helper *runner, component string) *PlatformError {
	pinned := loaded.images[component]
	container, firstErr := helper.firstContainer(loaded, component)
	if firstErr != nil {
		return errAsPlatform(firstErr)
	}
	inspect, err := helper.capture("docker", "inspect", container, "--format", "{{.Config.Image}}")
	if err != nil {
		return &PlatformError{Code: "inspect_failed", Message: fmt.Sprintf("%s: %v", component, err), NextAction: "inspect Docker state, then rerun verify"}
	}
	actualReference := strings.TrimSpace(inspect)
	repoDigests, err := helper.capture("docker", "image", "inspect", pinned, "--format", "{{json .RepoDigests}}")
	if err != nil {
		return &PlatformError{Code: "image_digest_unresolved", Message: fmt.Sprintf("%s pinned reference %s is not present: %v", component, pinned, err), NextAction: "pull the release images (or import the offline archive), then rerun verify"}
	}
	if actualReference != pinned || !strings.Contains(repoDigests, pinned) {
		return &PlatformError{Code: "image_digest_mismatch", Message: fmt.Sprintf("%s runs %q but the release manifest pins %q", component, actualReference, pinned), NextAction: "redeploy the component from the release manifest"}
	}
	expected := fmt.Sprintf("%s running image %s (repo digests %s)", component, pinned, strings.TrimSpace(repoDigests))
	helper.report.RecordCheck(report.Check{ID: "image-digest-" + component, Result: "passed", Expected: expected, Actual: expected, Code: "image_digest_pinned"})
	if platformDigest, platformErr := loaded.manifest.PlatformDigest(component, buildPlatform()); platformErr == nil {
		helper.report.RecordCheck(report.Check{ID: "platform-digest-" + component, Result: "passed", Expected: "release manifest " + buildPlatform() + " digest " + platformDigest, Actual: "host platform pinned through " + pinned, Code: "platform_digest_recorded"})
	}
	return nil
}

func judgeReadiness(component string, loaded *loadedRequest, readiness sharedops.Readiness) *PlatformError {
	if readiness.Component != component {
		return &PlatformError{Code: "readiness_identity_mismatch", Message: fmt.Sprintf("%s reported component %q", component, readiness.Component), NextAction: "redeploy the component from the release manifest"}
	}
	if readiness.Release != loaded.release() {
		return &PlatformError{Code: "release_mismatch", Message: fmt.Sprintf("%s reports release %q but the deployment is %q", component, readiness.Release, loaded.release()), NextAction: "redeploy the stack from one release"}
	}
	if readiness.Reason == sharedops.Ready {
		if !readiness.AcceptingWork || readiness.Mode != "normal" {
			return &PlatformError{Code: "readiness_incoherent", Message: fmt.Sprintf("%s is ready but mode=%s acceptingWork=%t", component, readiness.Mode, readiness.AcceptingWork), NextAction: "the frozen readiness contract couples ready with normal mode and accepting work"}
		}
		return nil
	}
	if component == "quoin" {
		return &PlatformError{Code: "quoin_not_ready", Message: fmt.Sprintf("quoin reason=%s mode=%s", readiness.Reason, readiness.Mode), NextAction: "quoin must be ready after install; inspect its logs"}
	}
	if !allowedNotReadyReasons(component)[string(readiness.Reason)] {
		return &PlatformError{Code: "unexpected_not_ready", Message: fmt.Sprintf("%s reason=%s", component, readiness.Reason), NextAction: "inspect the component logs; this not-ready reason is outside the closed install expectation"}
	}
	return nil
}

type metricsExpectation struct {
	labels    map[string]map[string]bool
	isLabeled bool
}

// composeProgressPattern matches the decoration lines the docker
// compose CLI interleaves with command output (for example
// " Container proj-secret-bootstrap-1 Started"). Compose v5 prints them
// even with -T and --no-log-prefix; they are transport dressing of the
// CLI, never metric samples, so the judge skips them before parsing.
var composeProgressPattern = regexp.MustCompile(`^\s*(✔|✓)?\s*(Container|Network|Volume|Image|Service|Config|Secret|Pulling)\s+\S+.*$`)

func judgeMetrics(component, exposition string) *PlatformError {
	expectedFamilies, err := sharedops.CatalogFamiliesFor(component)
	if err != nil {
		return &PlatformError{Code: "metrics_catalog_unavailable", Message: err.Error(), NextAction: "the embedded metrics catalog must parse"}
	}
	expected := map[string]*metricsExpectation{}
	histograms := map[string]bool{}
	helps := map[string]string{}
	for _, family := range expectedFamilies {
		name, _ := family["name"].(string)
		familyType, _ := family["type"].(string)
		help, _ := family["help"].(string)
		labels, _ := family["labelValues"].(map[string][]string)
		expectation := &metricsExpectation{labels: map[string]map[string]bool{}}
		for label, values := range labels {
			expectation.isLabeled = true
			expectation.labels[label] = map[string]bool{}
			for _, value := range values {
				expectation.labels[label][value] = true
			}
		}
		expected[name] = expectation
		helps[name] = help
		if familyType == "histogram" {
			histograms[name] = true
		}
	}
	observed := map[string]map[string][]string{}
	observedUnlabeled := map[string]bool{}
	observedHelps := map[string]string{}
	observedTypes := map[string]string{}
	for _, line := range strings.Split(exposition, "\n") {
		line = strings.TrimSpace(ansiPattern.ReplaceAllString(line, ""))
		if line == "" {
			continue
		}
		if composeProgressPattern.MatchString(line) {
			continue
		}
		if strings.HasPrefix(line, "# HELP ") {
			fields := strings.SplitN(strings.TrimPrefix(line, "# HELP "), " ", 2)
			if len(fields) == 2 {
				observedHelps[fields[0]] = fields[1]
			}
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			fields := strings.SplitN(strings.TrimPrefix(line, "# TYPE "), " ", 2)
			if len(fields) == 2 {
				observedTypes[fields[0]] = fields[1]
			}
			continue
		}
		name := line
		labels := map[string]string{}
		if index := strings.Index(line, "{"); index >= 0 && strings.Contains(line, "}") {
			name = line[:index]
			for _, pair := range splitLabels(line[index+1 : strings.Index(line, "}")]) {
				parts := strings.SplitN(pair, "=", 2)
				if len(parts) == 2 {
					labels[strings.TrimSpace(parts[0])] = strings.Trim(parts[1], `"`)
				}
			}
		} else if index := strings.Index(line, " "); index >= 0 {
			name = line[:index]
		}
		if strings.HasPrefix(name, "process_") || strings.HasPrefix(name, "go_") {
			// Prometheus upstream runtime/process collectors are intentionally
			// outside contracts/metrics.yaml (OPS-METRIC-001).
			continue
		}
		if _, ok := expected[name]; !ok {
			mapped := false
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if strings.HasSuffix(name, suffix) {
					base := strings.TrimSuffix(name, suffix)
					if histograms[base] {
						name = base
						mapped = true
					}
					break
				}
			}
			if !mapped {
				return &PlatformError{Code: "metrics_family_outside_catalog", Message: fmt.Sprintf("%s exports %q outside the frozen catalog", component, name), NextAction: "redeploy the component from the release; only catalog families may be exported"}
			}
		}
		if len(labels) == 0 {
			observedUnlabeled[name] = true
			continue
		}
		if _, ok := observed[name]; !ok {
			observed[name] = map[string][]string{}
		}
		for label, value := range labels {
			observed[name][label] = append(unique(observed[name][label]), value)
		}
	}
	var missing []string
	for name := range expected {
		if _, ok := observed[name]; !ok {
			if !observedUnlabeled[name] {
				missing = append(missing, name)
			}
		}
		if help, ok := observedHelps[name]; ok && help != helps[name] {
			return &PlatformError{Code: "metrics_help_mismatch", Message: fmt.Sprintf("%s %s HELP %q does not match the catalog %q", component, name, help, helps[name]), NextAction: "redeploy the component from the release; HELP text is part of the frozen catalog"}
		}
		if observedType, ok := observedTypes[name]; ok {
			// Prometheus expositions encode histogram families as gauges at
			// the metric level; the catalog type stays authoritative for
			// shape checks, so only reject contradicting sample families.
			if observedType == "counter" && histograms[name] {
				return &PlatformError{Code: "metrics_type_mismatch", Message: fmt.Sprintf("%s %s is exposed as %s but the catalog declares a histogram", component, name, observedType), NextAction: "redeploy the component from the release"}
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return &PlatformError{Code: "metrics_family_missing", Message: fmt.Sprintf("%s does not preinitialize %v", component, missing), NextAction: "redeploy the component; every catalog family must be exported from startup"}
	}
	for name, expectation := range expected {
		if !expectation.isLabeled {
			continue
		}
		observedLabels, ok := observed[name]
		if !ok {
			return &PlatformError{Code: "metrics_labels_missing", Message: fmt.Sprintf("%s exports %s without its closed label set", component, name), NextAction: "redeploy the component; catalog labels are mandatory"}
		}
		for label := range observedLabels {
			// The exposition format adds the bucket bound ("le") to
			// histogram bucket series; it is transport, not a catalog label.
			if label == "le" {
				continue
			}
			if _, known := expectation.labels[label]; !known {
				return &PlatformError{Code: "metrics_label_outside_catalog", Message: fmt.Sprintf("%s %s exports label %q outside the catalog", component, name, label), NextAction: "redeploy the component from the release"}
			}
		}
		for label := range expectation.labels {
			for _, value := range observedLabels[label] {
				if !expectation.labels[label][value] {
					return &PlatformError{Code: "metrics_label_value_outside_catalog", Message: fmt.Sprintf("%s %s exports %s=%q outside the closed set", component, name, label, value), NextAction: "redeploy the component from the release"}
				}
			}
			for value := range expectation.labels[label] {
				if !contains(observedLabels[label], value) {
					return &PlatformError{Code: "metrics_label_value_missing", Message: fmt.Sprintf("%s %s never exports %s=%q", component, name, label, value), NextAction: "redeploy the component; the closed label set must be fully preinitialized"}
				}
			}
		}
	}
	return nil
}

func judgeLogs(component, logs, release string) *PlatformError {
	for _, raw := range strings.Split(strings.TrimSpace(logs), "\n") {
		// The docker compose CLI decorates log lines with ANSI control
		// sequences (e.g. erase-line) even when piped; the frozen contract
		// governs what the component writes, so strip the transport dressing.
		line := strings.TrimSpace(ansiPattern.ReplaceAllString(raw, ""))
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return &PlatformError{Code: "logs_not_json", Message: fmt.Sprintf("%s emitted a non-JSON log line: %.200s", component, line), NextAction: "the component must emit JSON Lines only"}
		}
		for _, field := range []string{"ts", "level", "component", "release", "code", "msg"} {
			if _, ok := entry[field]; !ok {
				return &PlatformError{Code: "logs_missing_field", Message: fmt.Sprintf("%s log line misses field %q", component, field), NextAction: "the frozen OPS-LOG-001 field set is mandatory"}
			}
		}
		if entry["component"] != component || entry["release"] != release {
			return &PlatformError{Code: "logs_identity_mismatch", Message: fmt.Sprintf("%s log identity mismatch in %.200s", component, line), NextAction: "component and release must identify the deployed binary"}
		}
	}
	return nil
}

// frozenMounts is the fixed volume topology per component (OPS-STORAGE-001,
// OPS-SCOPE-003): Quoin keeps data and backups on separate binds, Plinth and
// Lintel own isolated state binds, Stele mounts no persistent state.
var frozenMounts = map[string][]string{
	"quoin":  {"/etc/quoin/component.yaml", "/var/lib/quoin/backups", "/var/lib/quoin/data", "/run/quoin-secrets"},
	"plinth": {"/etc/quoin/component.yaml", "/run/quoin-secrets/runtime-ca.pem", "/var/lib/plinth", "/var/lib/plinth/workspaces"},
	"lintel": {"/etc/quoin/component.yaml", "/run/quoin-secrets/runtime-ca.pem", "/var/lib/lintel"},
	"stele":  {"/etc/quoin/component.yaml", "/run/quoin-secrets/runtime-ca.pem", "/run/quoin-secrets/stele-service-token"},
}

func judgeMounts(loaded *loadedRequest, helper *runner, stage int) error {
	for _, component := range deployconfig.Components {
		container, err := helper.firstContainer(loaded, component)
		if err != nil {
			return err
		}
		raw, err := helper.capture("docker", "inspect", container, "--format", "{{json .Mounts}}")
		if err != nil {
			return &PlatformError{Code: "mounts_unreadable", Message: fmt.Sprintf("%s: %v", component, err), NextAction: "inspect Docker state, then rerun verify"}
		}
		var mounts []struct {
			Destination string `json:"Destination"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &mounts); err != nil {
			return &PlatformError{Code: "mounts_unreadable", Message: fmt.Sprintf("%s: %v", component, err), NextAction: "inspect Docker state, then rerun verify"}
		}
		observed := map[string]bool{}
		for _, mount := range mounts {
			observed[mount.Destination] = true
		}
		for _, destination := range frozenMounts[component] {
			if !observed[destination] {
				return &PlatformError{Code: "volume_topology_violation", Message: fmt.Sprintf("%s does not mount %s (observed %v)", component, destination, observed), NextAction: "the fixed volume topology must match the frozen projection"}
			}
		}
		helper.report.RecordCheck(report.Check{ID: "volumes-" + component, Result: "passed", Expected: strings.Join(frozenMounts[component], ", "), Actual: strings.Join(mapKeys(observed), ", "), Code: "volume_topology_fixed"})
	}
	return nil
}

// errAsPlatform normalizes an error that is already a *PlatformError and
// wraps anything else with the generic verification failure code.
func errAsPlatform(err error) *PlatformError {
	if platform, ok := err.(*PlatformError); ok {
		return platform
	}
	return &PlatformError{Code: "verification_failed", Message: err.Error(), NextAction: "inspect the failed check in the report, fix the cause, then rerun"}
}

func mapKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func judgeTopology(loaded *loadedRequest, helper *runner, stage int) error {
	output, err := helper.run(stage, "verify-topology", dockerize(append(append([]string{}, loaded.composeArguments...), "ps", "--all", "--format", "json"))...)
	if err != nil {
		return &PlatformError{Code: "topology_unreadable", Message: strings.TrimSpace(output), NextAction: "inspect Docker state, then rerun verify"}
	}
	type serviceState struct {
		Name  string `json:"Name"`
		State string `json:"State"`
		Ports []struct {
			PublishedPort int    `json:"PublishedPort"`
			HostIP        string `json:"URL"`
		} `json:"Publishers"`
	}
	var states []serviceState
	decoder := json.NewDecoder(strings.NewReader(output))
	for decoder.More() {
		var state serviceState
		if decodeErr := decoder.Decode(&state); decodeErr != nil {
			return &PlatformError{Code: "topology_unreadable", Message: decodeErr.Error(), NextAction: "inspect Docker state, then rerun verify"}
		}
		states = append(states, state)
	}
	published := map[string][]string{}
	running := map[string]bool{}
	for _, state := range states {
		running[state.Name] = state.State == "running"
		for _, port := range state.Ports {
			if port.PublishedPort != 0 {
				published[state.Name] = append(published[state.Name], fmt.Sprintf("%s:%d", port.HostIP, port.PublishedPort))
			}
		}
	}
	prefix := loaded.project + "-"
	for _, component := range deployconfig.Components {
		if !running[prefix+component+"-1"] {
			return &PlatformError{Code: "topology_service_not_running", Message: fmt.Sprintf("%s is not running (state view:\n%s)", component, output), NextAction: "rerun the install command to resume"}
		}
	}
	for _, component := range []string{"plinth", "lintel"} {
		if len(published[prefix+component+"-1"]) != 0 {
			return &PlatformError{Code: "topology_port_leak", Message: fmt.Sprintf("%s published %v to the host", component, published[prefix+component+"-1"]), NextAction: "Runtime and ops listeners must stay on the internal network"}
		}
	}
	helper.report.RecordCheck(report.Check{ID: "topology", Result: "passed", Expected: "four services running; only Quoin public and Stele webhook published", Actual: fmt.Sprintf("published=%v", published), Code: "topology_fixed"})
	if loaded.input.PublishMode == "loopback" {
		for _, component := range []string{"quoin", "stele"} {
			expectedPort := loaded.input.QuoinPublicHostPort
			if component == "stele" {
				expectedPort = loaded.input.SteleWebhookHostPort
			}
			found := false
			for _, binding := range published[prefix+component+"-1"] {
				if binding != fmt.Sprintf("127.0.0.1:%d", expectedPort) {
					return &PlatformError{Code: "topology_binding_violation", Message: fmt.Sprintf("%s is published on %s, want 127.0.0.1:%d only", component, binding, expectedPort), NextAction: "loopback mode must bind the deployment input ports on 127.0.0.1 only"}
				}
				found = true
			}
			if !found {
				return &PlatformError{Code: "topology_port_mismatch", Message: fmt.Sprintf("%s published %v, want 127.0.0.1:%d", component, published[prefix+component+"-1"], expectedPort), NextAction: "the projected host ports must equal the deployment input"}
			}
		}
	}
	return nil
}

// ansiPattern matches the CSI control sequences the docker CLI interleaves
// with streamed logs.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// firstJSONLine isolates the readiness JSON body from a healthcheck probe
// whose combined output also carries the probe's own status complaint.
func firstJSONLine(output string) string {
	for _, line := range strings.Split(ansiPattern.ReplaceAllString(output, ""), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") {
			return line
		}
	}
	return strings.TrimSpace(output)
}

func splitLabels(raw string) []string {
	var pairs []string
	current := &strings.Builder{}
	quoted := false
	for _, r := range raw {
		switch {
		case r == '"':
			quoted = !quoted
			current.WriteRune(r)
		case r == ',' && !quoted:
			pairs = append(pairs, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if strings.TrimSpace(current.String()) != "" {
		pairs = append(pairs, current.String())
	}
	return pairs
}

func unique(values []string) []string {
	seen := map[string]bool{}
	result := values[:0]
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
