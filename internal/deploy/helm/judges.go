package helm

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	sharedops "github.com/Suknna/quoin/internal/ops"
)

// The three judges below are the Kubernetes projection of the frozen
// operational contracts: readiness (OPS-HEALTH), the embedded metrics catalog
// and OPS-LOG-001. The shared exported contracts in internal/ops remain the
// single authority; these functions only perform the mechanical comparison.

func allowedNotReadyReasons(component string) map[string]bool {
	return map[string]bool{"runtime_unregistered": true, "dependency_unavailable": true, "starting": true}
}

func judgeReadiness(component, release string, readiness sharedops.Readiness) *verifyError {
	if readiness.Component != component {
		return verifyFail("readiness_identity_mismatch", "%s reported component %q", component, readiness.Component)
	}
	if readiness.Release != release {
		return verifyFail("release_mismatch", "%s reports release %q but the deployment is %q", component, readiness.Release, release)
	}
	if readiness.Reason == sharedops.Ready {
		if !readiness.AcceptingWork || readiness.Mode != "normal" {
			return verifyFail("readiness_incoherent", "%s is ready but mode=%s acceptingWork=%t", component, readiness.Mode, readiness.AcceptingWork)
		}
		return nil
	}
	if component == "quoin" {
		return verifyFail("quoin_not_ready", "quoin reason=%s mode=%s", readiness.Reason, readiness.Mode)
	}
	if !allowedNotReadyReasons(component)[string(readiness.Reason)] {
		return verifyFail("unexpected_not_ready", "%s reason=%s", component, readiness.Reason)
	}
	return nil
}

type metricsExpectation struct {
	labels    map[string]map[string]bool
	isLabeled bool
}

func judgeMetrics(component, exposition string) *verifyError {
	expectedFamilies, err := sharedops.CatalogFamiliesFor(component)
	if err != nil {
		return verifyFail("metrics_catalog_unavailable", "%s", err)
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
		line = strings.TrimSpace(line)
		if line == "" {
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
				return verifyFail("metrics_family_outside_catalog", "%s exports %q outside the frozen catalog", component, name)
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
			return verifyFail("metrics_help_mismatch", "%s %s HELP %q does not match the catalog %q", component, name, help, helps[name])
		}
		if observedType, ok := observedTypes[name]; ok {
			if observedType == "counter" && histograms[name] {
				return verifyFail("metrics_type_mismatch", "%s %s is exposed as %s but the catalog declares a histogram", component, name, observedType)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return verifyFail("metrics_family_missing", "%s does not preinitialize %v", component, missing)
	}
	for name, expectation := range expected {
		if !expectation.isLabeled {
			continue
		}
		observedLabels, ok := observed[name]
		if !ok {
			return verifyFail("metrics_labels_missing", "%s exports %s without its closed label set", component, name)
		}
		for label := range observedLabels {
			if label == "le" {
				continue
			}
			if _, known := expectation.labels[label]; !known {
				return verifyFail("metrics_label_outside_catalog", "%s %s exports label %q outside the catalog", component, name, label)
			}
		}
		for label := range expectation.labels {
			for _, value := range observedLabels[label] {
				if !expectation.labels[label][value] {
					return verifyFail("metrics_label_value_outside_catalog", "%s %s exports %s=%q outside the closed set", component, name, label, value)
				}
			}
			for value := range expectation.labels[label] {
				if !contains(observedLabels[label], value) {
					return verifyFail("metrics_label_value_missing", "%s %s never exports %s=%q", component, name, label, value)
				}
			}
		}
	}
	return nil
}

func judgeLogs(component, logs, release string) *verifyError {
	for _, raw := range strings.Split(strings.TrimSpace(logs), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return verifyFail("logs_not_json", "%s emitted a non-JSON log line: %.200s", component, line)
		}
		for _, field := range []string{"ts", "level", "component", "release", "code", "msg"} {
			if _, ok := entry[field]; !ok {
				return verifyFail("logs_missing_field", "%s log line misses field %q", component, field)
			}
		}
		if entry["component"] != component || entry["release"] != release {
			return verifyFail("logs_identity_mismatch", "%s log identity mismatch in %.200s", component, line)
		}
	}
	return nil
}

// firstJSONField decodes the first JSON object line of a verifier log.
func firstJSONField(body string, target any) error {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") {
			return json.Unmarshal([]byte(line), target)
		}
	}
	return fmt.Errorf("no JSON line in verifier output")
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
	if current.Len() > 0 {
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

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
