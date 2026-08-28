package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bufbuild/protocompile"
	"github.com/dlclark/regexp2"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

type fixture struct {
	data   string
	schema string
}

func main() {
	root, err := findRoot()
	if err != nil {
		fatal(err)
	}
	checks := []struct {
		name string
		fn   func(string) error
	}{
		{"generated projections", verifyGenerated},
		{"JSON Schemas and fixtures", verifySchemas},
		{"OpenAPI", verifyOpenAPI},
		{"Proto", verifyProto},
		{"SQLite", verifySQLite},
	}
	for _, check := range checks {
		if err := check.fn(root); err != nil {
			fatal(fmt.Errorf("%s: %w", check.name, err))
		}
		fmt.Printf("PASS\t%s\n", check.name)
	}
}

func verifyGenerated(root string) error {
	return runGo(root, "run", "./ci/contracts/generate", "--check")
}

func verifySchemas(root string) error {
	schemaDir := filepath.Join(root, "docs/specs/quoin-v1/contracts/schemas")
	entries, err := filepath.Glob(filepath.Join(schemaDir, "*.schema.json"))
	if err != nil {
		return err
	}
	sort.Strings(entries)
	compiled := make(map[string]*jsonschema.Schema, len(entries))
	for _, path := range entries {
		compiler := jsonschema.NewCompiler()
		compiler.UseRegexpEngine(compileECMAScriptRegexp)
		compiler.AssertFormat()
		schema, err := compiler.Compile(path)
		if err != nil {
			return fmt.Errorf("compile %s: %w", filepath.Base(path), err)
		}
		compiled[filepath.Base(path)] = schema
	}
	fixtures := []fixture{
		{"contracts/metrics.yaml", "metrics.schema.json"},
		{"contracts/connection-probes.yaml", "connection-probes.schema.json"},
		{"contracts/plinth-worker-tools.yaml", "plinth-worker-tools.schema.json"},
		{"contracts/release-inputs.yaml", "release-inputs.schema.json"},
		{"contracts/verification-catalog.yaml", "verification-catalog.schema.json"},
		{"contracts/verification-result-profile.yaml", "verification-result-profile.schema.json"},
		{"contracts/examples/compose-install.yaml", "deployment-config.schema.json"},
		{"contracts/examples/helm-install.yaml", "deployment-config.schema.json"},
		{"contracts/examples/deployment-verification-request.json", "deployment-verification.schema.json"},
		{"contracts/examples/deployment-verification-report.json", "deployment-verification.schema.json"},
	}
	base := filepath.Join(root, "docs/specs/quoin-v1")
	for _, item := range fixtures {
		value, err := readDocument(filepath.Join(base, item.data))
		if err != nil {
			return err
		}
		if err := compiled[item.schema].Validate(value); err != nil {
			return fmt.Errorf("validate %s: %w", item.data, err)
		}
	}
	return verifyInspectionPromQLSchema(compiled["inspection-promql-execution.schema.json"])
}

func verifyInspectionPromQLSchema(schema *jsonschema.Schema) error {
	valid := []map[string]any{
		{
			"schemaKind": "inspection_promql_execution_v1", "attemptId": 1, "inspectionRunId": 2, "checkKey": "availability", "grantId": 3,
			"evidenceAt": "2026-08-28T00:00:00Z", "query": map[string]any{"mode": "instant", "expression": "up", "rangeSeconds": nil, "stepSeconds": nil},
		},
		{
			"schemaKind": "inspection_promql_execution_v1", "attemptId": 1, "inspectionRunId": 2, "checkKey": "latency", "grantId": 3,
			"evidenceAt": "2026-08-28T00:05:00Z", "query": map[string]any{"mode": "range", "expression": "up", "rangeSeconds": 300, "stepSeconds": 60},
		},
		{
			"schemaKind": "inspection_promql_result_v1", "attemptId": 1, "inspectionRunId": 2, "checkKey": "latency", "queryMode": "range", "outcome": "success", "observedAt": "2026-08-28T00:05:01Z",
			"executionWindow": map[string]any{"startAt": "2026-08-28T00:00:00Z", "endAt": "2026-08-28T00:05:00Z", "stepSeconds": 60}, "result": map[string]any{"resultType": "matrix", "result": []any{map[string]any{"metric": map[string]any{"job": "api"}, "values": []any{[]any{1724716800, "1"}}}}}, "warnings": []any{}, "errors": []any{}, "gapReason": nil,
		},
		{
			"schemaKind": "inspection_promql_result_v1", "attemptId": 1, "inspectionRunId": 2, "checkKey": "availability", "queryMode": "instant", "outcome": "success", "observedAt": "2026-08-28T00:00:00Z", "executionWindow": nil,
			"result": map[string]any{"resultType": "vector", "result": []any{map[string]any{"metric": map[string]any{"job": "api"}, "value": []any{1724716800, "1"}}}}, "warnings": []any{}, "errors": []any{}, "gapReason": nil,
		},
		{
			"schemaKind": "inspection_promql_result_v1", "attemptId": 1, "inspectionRunId": 2, "checkKey": "availability", "queryMode": "instant", "outcome": "gap", "observedAt": "2026-08-28T00:00:00Z", "executionWindow": nil,
			"result": nil, "warnings": []any{}, "errors": []any{"no data"}, "gapReason": "no_data",
		},
	}
	for _, value := range valid {
		if err := schema.Validate(value); err != nil {
			return fmt.Errorf("inspection PromQL valid fixture: %w", err)
		}
	}
	if err := verifyInspectionPromQLRangeWindow(valid[1], valid[2]); err != nil {
		return fmt.Errorf("inspection PromQL valid range result: %w", err)
	}
	for name, change := range map[string]func(map[string]any){
		"missing window": func(result map[string]any) { result["executionWindow"] = nil },
		"wrong end": func(result map[string]any) {
			result["executionWindow"] = map[string]any{"startAt": "2026-08-28T00:00:00Z", "endAt": "2026-08-28T00:04:00Z", "stepSeconds": 60}
		},
		"wrong start": func(result map[string]any) {
			result["executionWindow"] = map[string]any{"startAt": "2026-08-28T00:01:00Z", "endAt": "2026-08-28T00:05:00Z", "stepSeconds": 60}
		},
		"wrong step": func(result map[string]any) {
			result["executionWindow"] = map[string]any{"startAt": "2026-08-28T00:00:00Z", "endAt": "2026-08-28T00:05:00Z", "stepSeconds": 30}
		},
		"downgraded mode": func(result map[string]any) { result["queryMode"] = "instant"; result["executionWindow"] = nil },
	} {
		candidate := maps.Clone(valid[2])
		change(candidate)
		if err := verifyInspectionPromQLRangeWindow(valid[1], candidate); err == nil {
			return fmt.Errorf("inspection PromQL accepted %s range result", name)
		}
	}
	rangeWithoutWindow := map[string]any{
		"schemaKind": "inspection_promql_result_v1", "attemptId": 1, "inspectionRunId": 2, "checkKey": "latency", "queryMode": "range", "outcome": "success", "observedAt": "2026-08-28T00:05:01Z", "executionWindow": nil,
		"result": map[string]any{"resultType": "vector", "result": []any{map[string]any{"metric": map[string]any{}, "value": []any{1724716800, "1"}}}}, "warnings": []any{}, "errors": []any{}, "gapReason": nil,
	}
	if err := schema.Validate(rangeWithoutWindow); err == nil {
		return fmt.Errorf("inspection PromQL accepted range result without executionWindow")
	}
	for _, gapReason := range []string{"identity_busy", "runtime_unavailable", "authentication_required", "authentication_probe_unavailable", "artifact_commit_failed", "journey_failed", "unknown_gap"} {
		value := map[string]any{
			"schemaKind": "inspection_promql_result_v1", "attemptId": 1, "inspectionRunId": 2, "checkKey": "availability", "queryMode": "instant", "outcome": "gap", "observedAt": "2026-08-28T00:00:00Z", "executionWindow": nil,
			"result": nil, "warnings": []any{}, "errors": []any{"gap"}, "gapReason": gapReason,
		}
		if err := schema.Validate(value); err == nil {
			return fmt.Errorf("inspection PromQL accepted non-PromQL gap reason %q", gapReason)
		}
	}
	invalid := []struct {
		name  string
		value map[string]any
	}{
		{
			name: "missing evidenceAt",
			value: map[string]any{
				"schemaKind": "inspection_promql_execution_v1", "attemptId": 1, "inspectionRunId": 2, "checkKey": "availability", "grantId": 3,
				"query": map[string]any{"mode": "instant", "expression": "up", "rangeSeconds": nil, "stepSeconds": nil},
			},
		},
		{
			name: "missing result",
			value: map[string]any{
				"schemaKind": "inspection_promql_result_v1", "attemptId": 1, "inspectionRunId": 2, "checkKey": "availability", "queryMode": "instant", "outcome": "success", "observedAt": "2026-08-28T00:00:00Z", "executionWindow": nil,
				"result": nil, "warnings": []any{}, "errors": []any{}, "gapReason": nil,
			},
		},
		{
			name: "empty result object",
			value: map[string]any{
				"schemaKind": "inspection_promql_result_v1", "attemptId": 1, "inspectionRunId": 2, "checkKey": "availability", "queryMode": "instant", "outcome": "success", "observedAt": "2026-08-28T00:00:00Z", "executionWindow": nil,
				"result": map[string]any{}, "warnings": []any{}, "errors": []any{}, "gapReason": nil,
			},
		},
		{
			name: "unknown result type",
			value: map[string]any{
				"schemaKind": "inspection_promql_result_v1", "attemptId": 1, "inspectionRunId": 2, "checkKey": "availability", "queryMode": "instant", "outcome": "success", "observedAt": "2026-08-28T00:00:00Z", "executionWindow": nil,
				"result": map[string]any{"resultType": "histogram", "result": []any{}}, "warnings": []any{}, "errors": []any{}, "gapReason": nil,
			},
		},
		{
			name: "empty vector",
			value: map[string]any{
				"schemaKind": "inspection_promql_result_v1", "attemptId": 1, "inspectionRunId": 2, "checkKey": "availability", "queryMode": "instant", "outcome": "success", "observedAt": "2026-08-28T00:00:00Z", "executionWindow": nil,
				"result": map[string]any{"resultType": "vector", "result": []any{}}, "warnings": []any{}, "errors": []any{}, "gapReason": nil,
			},
		},
	}
	for _, test := range invalid {
		if err := schema.Validate(test.value); err == nil {
			return fmt.Errorf("inspection PromQL accepted invalid %s", test.name)
		}
	}
	return nil
}

func verifyInspectionPromQLRangeWindow(input, result map[string]any) error {
	for _, field := range []string{"attemptId", "inspectionRunId", "checkKey"} {
		if input[field] != result[field] {
			return fmt.Errorf("result %s does not match frozen input", field)
		}
	}
	query, ok := input["query"].(map[string]any)
	if !ok {
		return fmt.Errorf("input has no query")
	}
	mode, ok := query["mode"].(string)
	if !ok || (mode != "instant" && mode != "range") {
		return fmt.Errorf("input has invalid query mode")
	}
	resultMode, ok := result["queryMode"].(string)
	if !ok || resultMode != mode {
		return fmt.Errorf("result queryMode does not match frozen input")
	}
	window := result["executionWindow"]
	if mode == "instant" {
		if window != nil {
			return fmt.Errorf("instant result must not return an executionWindow")
		}
		return nil
	}
	rangeSeconds, ok := query["rangeSeconds"].(int)
	if !ok || rangeSeconds < 1 {
		return fmt.Errorf("input has no positive rangeSeconds")
	}
	stepSeconds, ok := query["stepSeconds"].(int)
	if !ok || stepSeconds < 1 {
		return fmt.Errorf("input has no positive stepSeconds")
	}
	evidenceAt, err := time.Parse(time.RFC3339, input["evidenceAt"].(string))
	if err != nil {
		return fmt.Errorf("parse evidenceAt: %w", err)
	}
	actual, ok := window.(map[string]any)
	if !ok {
		return fmt.Errorf("range result has no executionWindow")
	}
	start, err := time.Parse(time.RFC3339, actual["startAt"].(string))
	if err != nil {
		return fmt.Errorf("parse startAt: %w", err)
	}
	end, err := time.Parse(time.RFC3339, actual["endAt"].(string))
	if err != nil {
		return fmt.Errorf("parse endAt: %w", err)
	}
	actualStep, ok := actual["stepSeconds"].(int)
	if !ok || actualStep != stepSeconds || !end.Equal(evidenceAt) || !start.Equal(end.Add(-time.Duration(rangeSeconds)*time.Second)) {
		return fmt.Errorf("executionWindow must exactly match frozen evidenceAt/rangeSeconds/stepSeconds")
	}
	return nil
}

func verifyOpenAPI(root string) error {
	path := filepath.Join(root, "docs/specs/quoin-v1/contracts/openapi.yaml")
	value, err := readDocument(path)
	if err != nil {
		return err
	}
	doc, ok := value.(map[string]any)
	if !ok || doc["openapi"] != "3.1.0" {
		return fmt.Errorf("openapi must be 3.1.0")
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		return fmt.Errorf("paths must be a non-empty object")
	}
	operationIDs := map[string]string{}
	methods := map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true}
	for route, raw := range paths {
		if !strings.HasPrefix(route, "/api/v1/") {
			return fmt.Errorf("route outside /api/v1: %s", route)
		}
		item, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("path item %s is not an object", route)
		}
		for method, rawOperation := range item {
			if !methods[method] {
				continue
			}
			operation, ok := rawOperation.(map[string]any)
			if !ok {
				return fmt.Errorf("operation %s %s is not an object", method, route)
			}
			id, ok := operation["operationId"].(string)
			if !ok || id == "" {
				return fmt.Errorf("operation %s %s has no operationId", method, route)
			}
			if previous, exists := operationIDs[id]; exists {
				return fmt.Errorf("operationId %s reused by %s and %s %s", id, previous, method, route)
			}
			operationIDs[id] = method + " " + route
		}
	}
	return walkContract(doc, doc, "")
}

func walkContract(root any, value any, path string) error {
	switch current := value.(type) {
	case map[string]any:
		if _, exists := current["nullable"]; exists {
			return fmt.Errorf("deprecated nullable at %s", path)
		}
		if ref, ok := current["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
			if _, err := resolveJSONPointer(root, ref); err != nil {
				return fmt.Errorf("%s at %s: %w", ref, path, err)
			}
		}
		for key, child := range current {
			if err := walkContract(root, child, path+"/"+key); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range current {
			if err := walkContract(root, child, fmt.Sprintf("%s/%d", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func resolveJSONPointer(root any, pointer string) (any, error) {
	current := root
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "#/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s does not resolve through an object", pointer)
		}
		current, ok = object[token]
		if !ok {
			return nil, fmt.Errorf("%s target is missing", pointer)
		}
	}
	return current, nil
}

func verifyProto(root string) error {
	contractDir := filepath.Join(root, "docs/specs/quoin-v1/contracts")
	resolver := protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{contractDir}})
	compiler := protocompile.Compiler{Resolver: resolver}
	_, err := compiler.Compile(context.Background(), "runtime.proto", "quoin/plinth/worker/v1/agent_worker.proto")
	return err
}

func verifySQLite(root string) error {
	schema, err := os.ReadFile(filepath.Join(root, "docs/specs/quoin-v1/contracts/sql/schema.sql"))
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(string(schema)); err != nil {
		return err
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("integrity_check=%q: %w", integrity, err)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("foreign_key_check returned violations")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return verifyMixedInspectionPromQLSQLite(root)
}

type ecmaRegexp regexp2.Regexp

func (re *ecmaRegexp) MatchString(value string) bool {
	matched, err := (*regexp2.Regexp)(re).MatchString(value)
	return err == nil && matched
}

func (re *ecmaRegexp) String() string {
	return (*regexp2.Regexp)(re).String()
}

func compileECMAScriptRegexp(pattern string) (jsonschema.Regexp, error) {
	re, err := regexp2.Compile(pattern, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	return (*ecmaRegexp)(re), nil
}

func readDocument(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value any
	if strings.HasSuffix(path, ".json") {
		err = json.Unmarshal(data, &value)
	} else {
		err = yaml.Unmarshal(data, &value)
		if err == nil {
			canonical, marshalErr := json.Marshal(value)
			if marshalErr != nil {
				return nil, marshalErr
			}
			err = json.Unmarshal(canonical, &value)
		}
	}
	return value, err
}

func runGo(root string, args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("repository root not found")
		}
		dir = parent
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
