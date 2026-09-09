package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var protoAuthoritySources = []string{
	"docs/specs/quoin-v1/contracts/runtime.proto",
	"docs/specs/quoin-v1/contracts/quoin/plinth/worker/v1/agent_worker.proto",
}

var projections = map[string]string{
	"docs/specs/quoin-v1/contracts/sql/schema.sql":                                    "internal/gen/contracts/schema.sql",
	"docs/specs/quoin-v1/contracts/schemas/business-system-config.schema.json":        "internal/gen/contracts/business-system-config.schema.json",
	"docs/specs/quoin-v1/contracts/schemas/label-contract.schema.json":                "internal/gen/contracts/label-contract.schema.json",
	"docs/specs/quoin-v1/contracts/schemas/deployment-config.schema.json":             "internal/gen/contracts/deployment-config.schema.json",
	"docs/specs/quoin-v1/contracts/schemas/readiness-response.schema.json":            "internal/gen/contracts/readiness-response.schema.json",
	"docs/specs/quoin-v1/contracts/schemas/config-verification-execution.schema.json": "internal/gen/contracts/config-verification-execution.schema.json",
	"docs/specs/quoin-v1/contracts/schemas/inspection-promql-execution.schema.json":   "internal/gen/contracts/inspection-promql-execution.schema.json",
	"docs/specs/quoin-v1/contracts/schemas/inspection-report.schema.json":             "internal/gen/contracts/inspection-report.schema.json",
	"docs/specs/quoin-v1/contracts/schemas/resource-refresh-execution.schema.json":    "internal/gen/contracts/resource-refresh-execution.schema.json",
	"docs/specs/quoin-v1/contracts/schemas/browser-execution.schema.json":             "internal/gen/contracts/browser-execution.schema.json",
	"docs/specs/quoin-v1/contracts/schemas/deployment-verification.schema.json":       "internal/gen/contracts/deployment-verification.schema.json",
	"docs/specs/quoin-v1/contracts/schemas/verification-result.schema.json":           "internal/gen/contracts/verification-result.schema.json",
	"docs/specs/quoin-v1/contracts/verification-catalog.yaml":                         "internal/gen/contracts/verification-catalog.yaml",
	"docs/specs/quoin-v1/contracts/verification-result-profile.yaml":                  "internal/gen/contracts/verification-result-profile.yaml",
	"docs/specs/quoin-v1/contracts/schemas/browser-tool.schema.json":                  "internal/gen/contracts/browser-tool.schema.json",
	"docs/specs/quoin-v1/contracts/schemas/release-manifest.schema.json":              "internal/gen/contracts/release-manifest.schema.json",
	"docs/specs/quoin-v1/contracts/schemas/plinth-worker-tools.schema.json":           "internal/gen/contracts/plinth-worker-tools.schema.json",
	"docs/specs/quoin-v1/contracts/metrics.yaml":                                      "internal/gen/contracts/metrics.yaml",
	"docs/specs/quoin-v1/contracts/connection-probes.yaml":                            "internal/gen/contracts/connection-probes.yaml",
	"docs/specs/quoin-v1/contracts/plinth-worker-tools.yaml":                          "internal/gen/contracts/plinth-worker-tools.yaml",
	"docs/specs/quoin-v1/contracts/release-inputs.yaml":                               "internal/gen/contracts/release-inputs.yaml",
}

func main() {
	check := flag.Bool("check", false, "verify generated projections without writing")
	flag.Parse()
	root, err := findRoot()
	if err != nil {
		fatal(err)
	}
	if err := generateProtoAuthorityFingerprint(root, *check); err != nil {
		fatal(err)
	}
	for source, target := range projections {
		want, err := os.ReadFile(filepath.Join(root, source))
		if err != nil {
			fatal(fmt.Errorf("read %s: %w", source, err))
		}
		targetPath := filepath.Join(root, target)
		if *check {
			got, err := os.ReadFile(targetPath)
			if err != nil || !bytes.Equal(got, want) {
				fatal(fmt.Errorf("generated projection %s is stale; run go run ./ci/contracts/generate", target))
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(targetPath, want, 0o644); err != nil {
			fatal(err)
		}
	}
	webTypes, err := generateWebTypes(filepath.Join(root, "docs/specs/quoin-v1/contracts/openapi.yaml"))
	if err != nil {
		fatal(err)
	}
	webTarget := filepath.Join(root, "web/src/api/generated/types.ts")
	if *check {
		got, err := os.ReadFile(webTarget)
		if err != nil || !bytes.Equal(got, webTypes) {
			fatal(fmt.Errorf("generated projection web/src/api/generated/types.ts is stale; run go run ./ci/contracts/generate"))
		}
	} else if err := os.WriteFile(webTarget, webTypes, 0o644); err != nil {
		fatal(err)
	}
}

// generateProtoAuthorityFingerprint binds every binary to exactly the same
// ordered authority files. The length-prefixed paths make distinct file sets
// impossible to concatenate into an ambiguous digest input.
func generateProtoAuthorityFingerprint(root string, check bool) error {
	hash := sha256.New()
	for _, source := range protoAuthoritySources {
		body, err := os.ReadFile(filepath.Join(root, source))
		if err != nil {
			return fmt.Errorf("read Proto authority %s: %w", source, err)
		}
		if len(source) > 0xffff {
			return fmt.Errorf("Proto authority path is too long: %s", source)
		}
		hash.Write([]byte{byte(len(source) >> 8), byte(len(source))})
		hash.Write([]byte(source))
		hash.Write(body)
	}
	fingerprint := hex.EncodeToString(hash.Sum(nil))
	output := []byte("// Code generated by ci/contracts/generate. DO NOT EDIT.\n\npackage contract\n\nimport \"regexp\"\n\n// ProtoAuthorityFingerprint is the SHA-256 fingerprint of the complete,\n// ordered Proto authority set. It is compiled into every component and is not\n// configurable at deployment time.\nconst ProtoAuthorityFingerprint = \"" + fingerprint + "\"\n\nvar protoAuthorityFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)\n\n// ValidProtoAuthorityFingerprint accepts only the canonical lowercase SHA-256\n// wire form. Empty, malformed, and alternate encodings fail closed.\nfunc ValidProtoAuthorityFingerprint(value string) bool {\n\treturn protoAuthorityFingerprintPattern.MatchString(value)\n}\n")
	target := filepath.Join(root, "internal/contract/proto_fingerprint.go")
	if check {
		got, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(got, output) {
			return fmt.Errorf("generated Proto authority fingerprint is stale; run go run ./ci/contracts/generate")
		}
		return nil
	}
	return os.WriteFile(target, output, 0o644)
}

func generateWebTypes(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document struct {
		Components struct {
			Schemas map[string]map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	names := []string{"LoginRequest", "PasswordChangeRequest", "RuntimeSlot", "RuntimeStatus", "UserSummary", "ArtifactSummary", "DeploymentVerificationSummary", "VerificationInvocationItem", "VerificationItemResult", "VerificationResultConflict", "VerificationSubjectDrift", "DeploymentVerificationReceipt"}
	var output strings.Builder
	output.WriteString("// Code generated by ci/contracts/generate. DO NOT EDIT.\n")
	for _, name := range names {
		schema := document.Components.Schemas[name]
		if schema == nil {
			return nil, fmt.Errorf("OpenAPI schema %s is missing", name)
		}
		properties, _ := schema["properties"].(map[string]any)
		required := stringSet(schema["required"])
		keys := make([]string, 0, len(properties))
		for key := range properties {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteString("\nexport interface " + name + " {\n")
		for _, key := range keys {
			property, _ := properties[key].(map[string]any)
			optional := "?"
			if required[key] {
				optional = ""
			}
			output.WriteString(fmt.Sprintf("  %s%s: %s\n", key, optional, typescriptType(property)))
		}
		output.WriteString("}\n")
	}
	return []byte(output.String()), nil
}

func stringSet(value any) map[string]bool {
	result := map[string]bool{}
	values, _ := value.([]any)
	for _, item := range values {
		if text, ok := item.(string); ok {
			result[text] = true
		}
	}
	return result
}

func typescriptType(schema map[string]any) string {
	if ref, ok := schema["$ref"].(string); ok {
		parts := strings.Split(ref, "/")
		name := parts[len(parts)-1]
		switch name {
		case "LocatorId", "Timestamp":
			return "string"
		default:
			return name
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		values := make([]string, 0, len(enum))
		for _, value := range enum {
			if value == nil {
				values = append(values, "null")
			} else {
				values = append(values, strconv.Quote(fmt.Sprint(value)))
			}
		}
		return strings.Join(values, " | ")
	}
	typeValue := schema["type"]
	if types, ok := typeValue.([]any); ok {
		values := make([]string, 0, len(types))
		for _, value := range types {
			values = append(values, primitiveType(fmt.Sprint(value)))
		}
		return strings.Join(values, " | ")
	}
	return primitiveType(fmt.Sprint(typeValue))
}

func primitiveType(value string) string {
	switch value {
	case "string":
		return "string"
	case "integer", "number":
		return "number"
	case "boolean":
		return "boolean"
	case "null":
		return "null"
	default:
		return "unknown"
	}
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
