package ops_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/ops"
)

// contractRoot resolves the repository's frozen contract directory from the
// package directory, so the fixture reads the same files ci/verify-contracts
// owns rather than a copied fixture.
func contractRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "..", "..", "docs", "specs", "quoin-v1", "contracts")
	if _, err := os.Stat(filepath.Join(root, "openapi.yaml")); err != nil {
		t.Fatalf("frozen contracts not reachable at %s: %v", root, err)
	}
	return root
}

// TestMetricsLabelProjection is the OPS-METRIC-001/OPS-VERIFY-004 fixture: the
// runtime label-value projection (enumLabelValues in internal/ops) must equal
// the set produced from the real machine sources the catalog references, and
// every catalog label set must resolve. A drift in openapi.yaml, runtime.proto
// or sql/schema.sql must fail here instead of silently diverging from
// contracts/metrics.yaml.
func TestMetricsLabelProjection(t *testing.T) {
	root := contractRoot(t)

	tags, methods := openapiRouteGroupsAndMethods(t, filepath.Join(root, "openapi.yaml"))
	assertEqualSet(t, "openapi_route_group", tags, ops.ProjectedLabelValues(t, "openapi_route_group"))
	assertEqualSet(t, "openapi_method", methods, ops.ProjectedLabelValues(t, "openapi_method"))

	proto := parseRuntimeProto(t, filepath.Join(root, "runtime.proto"))
	assertEqualSet(t, "runtime_slot", proto.enums["RuntimeSlot"], ops.ProjectedLabelValues(t, "runtime_slot"))
	assertEqualSet(t, "attempt_type", proto.enums["AttemptType"], ops.ProjectedLabelValues(t, "attempt_type"))
	assertEqualSet(t, "attempt_termination_reason", proto.enums["TerminationReason"], ops.ProjectedLabelValues(t, "attempt_termination_reason"))
	assertEqualSet(t, "delivery_status", proto.enums["DeliveryStatus"], ops.ProjectedLabelValues(t, "delivery_status"))
	assertEqualSet(t, "rpc_group", proto.services, ops.ProjectedLabelValues(t, "rpc_group"))

	schema, err := os.ReadFile(filepath.Join(root, "sql", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	assertEqualSet(t, "maintenance_reason", snakeCaseAll(sqlCheckValues(t, schema, "reason", "maintenance_state")), ops.ProjectedLabelValues(t, "maintenance_reason"))
	assertEqualSet(t, "model_operation", sqlCheckValues(t, schema, "operation"), ops.ProjectedLabelValues(t, "model_operation"))
	assertEqualSet(t, "model_call_status", sqlCheckValues(t, schema, "status", "model_calls"), ops.ProjectedLabelValues(t, "model_call_status"))
	assertEqualSet(t, "tool_execution_mode", sqlCheckValues(t, schema, "execution_mode"), ops.ProjectedLabelValues(t, "tool_execution_mode"))
	assertEqualSet(t, "tool_call_status", sqlCheckValues(t, schema, "status", "tool_calls"), ops.ProjectedLabelValues(t, "tool_call_status"))

	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		if _, err := ops.CatalogFamiliesFor(component); err != nil {
			t.Fatalf("catalog projection for %s: %v", component, err)
		}
	}
	if !strings.Contains(string(gen.MetricsYAML), "storage_operation") {
		t.Fatal("embedded metrics catalog lost the storage_operation label set")
	}
}

func openapiRouteGroupsAndMethods(t *testing.T, path string) ([]string, []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]bool{}
	tagBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		if line == "tags:" {
			tagBlock = true
			continue
		}
		if tagBlock {
			if !strings.HasPrefix(line, "  ") || strings.TrimSpace(line) == "" {
				tagBlock = false
				continue
			}
			if strings.Contains(line, "name:") {
				value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
				groups[strings.TrimSpace(strings.TrimPrefix(value, "name:"))] = true
			}
		}
	}
	methods := map[string]bool{}
	known := map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true}
	inPaths := false
	for _, line := range strings.Split(string(data), "\n") {
		if line == "paths:" {
			inPaths = true
			continue
		}
		if inPaths && !strings.HasPrefix(line, " ") && line != "" {
			inPaths = false
			continue
		}
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasSuffix(trimmed, ":") && known[strings.TrimSuffix(trimmed, ":")] {
			methods[strings.TrimSuffix(trimmed, ":")] = true
		}
	}
	return sortedKeys(groups), sortedKeys(methods)
}

type protoIndex struct {
	enums    map[string][]string
	services []string
}

func parseRuntimeProto(t *testing.T, path string) protoIndex {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	index := protoIndex{enums: map[string][]string{}}
	servicePattern := regexp.MustCompile(`(?m)^service ([A-Za-z0-9_]+)`)
	enumName := ""
	prefix := ""
	for _, line := range strings.Split(string(data), "\n") {
		if match := servicePattern.FindStringSubmatch(line); match != nil {
			index.services = append(index.services, snakeCase(match[1]))
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "enum ") {
			enumName = strings.TrimSuffix(strings.TrimPrefix(trimmed, "enum "), " {")
			// Proto enum values carry the SCREAMING_SNAKE enum name as their
			// shared prefix (RUNTIME_SLOT_PLINTH), not the CamelCase name.
			prefix = strings.ToUpper(snakeCase(enumName))
			index.enums[enumName] = nil
			continue
		}
		if enumName != "" {
			if strings.HasPrefix(trimmed, "}") {
				enumName = ""
				continue
			}
			value := regexp.MustCompile(`^([A-Z0-9_]+)\s*=\s*\d+;`).FindStringSubmatch(trimmed)
			if value == nil {
				continue
			}
			if value[1] == prefix+"_UNSPECIFIED" {
				continue
			}
			index.enums[enumName] = append(index.enums[enumName], strings.ToLower(strings.TrimPrefix(value[1], prefix+"_")))
		}
	}
	sort.Strings(index.services)
	for name := range index.enums {
		sort.Strings(index.enums[name])
	}
	return index
}

// sqlCheckValues extracts the CHECK (column IN (...)) closed set. When table
// is given the search stops inside that CREATE TABLE block, which is required
// for columns like status that appear in several tables.
func sqlCheckValues(t *testing.T, schema []byte, column string, table ...string) []string {
	t.Helper()
	text := string(schema)
	if len(table) > 0 {
		start := strings.Index(text, "CREATE TABLE "+table[0]+" (")
		if start < 0 {
			t.Fatalf("table %s not found", table[0])
		}
		end := strings.Index(text[start:], "\n) STRICT")
		if end < 0 {
			t.Fatalf("table %s has no STRICT terminator", table[0])
		}
		text = text[start : start+end]
	}
	pattern := regexp.MustCompile(regexp.QuoteMeta(column) + `\s+TEXT[^,]*?CHECK \([^)]* IN \(([^)]*)\)`)
	match := pattern.FindStringSubmatch(text)
	if match == nil {
		t.Fatalf("no CHECK IN set found for column %q", column)
	}
	var values []string
	for _, raw := range strings.Split(match[1], ",") {
		raw = strings.TrimSpace(raw)
		raw = strings.Trim(raw, "'")
		if raw != "" {
			values = append(values, raw)
		}
	}
	sort.Strings(values)
	return values
}

func snakeCase(value string) string {
	var builder strings.Builder
	for index, r := range value {
		if r >= 'A' && r <= 'Z' {
			if index > 0 {
				builder.WriteByte('_')
			}
			builder.WriteRune(r - 'A' + 'a')
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func snakeCaseAll(values []string) []string {
	projected := make([]string, 0, len(values))
	for _, value := range values {
		projected = append(projected, snakeCase(value))
	}
	sort.Strings(projected)
	return projected
}

func assertEqualSet(t *testing.T, name string, machine, projected []string) {
	t.Helper()
	if strings.Join(machine, "\x00") == strings.Join(projected, "\x00") {
		return
	}
	t.Fatalf("label set %q diverges from its machine source.\nreal source: %v\nprojection: %v", name, machine, projected)
}

func sortedKeys(set map[string]bool) []string {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}
