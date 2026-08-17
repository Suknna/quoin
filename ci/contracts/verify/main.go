package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

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
	return rows.Err()
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
