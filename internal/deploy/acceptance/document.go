// Package acceptance owns the offline Deployment Acceptance helper exchange.
// It deliberately validates the frozen exchange document before any backend
// verifier is started, so an invalid request cannot touch a deployment.
package acceptance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

const schemaURL = "https://raw.githubusercontent.com/Suknna/quoin/main/docs/specs/quoin-v1/contracts/schemas/deployment-verification.schema.json"

// Request is the strict helper-request projection. TypedLocator stays opaque:
// the exchange schema remains its sole authority and this helper only executes
// deployment.verify-only items.
type Request struct {
	SchemaVersion          int           `yaml:"schemaVersion"`
	DocumentType           string        `yaml:"documentType"`
	InvocationID           string        `yaml:"invocationId"`
	ManifestDigest         string        `yaml:"manifestDigest"`
	ItemSetDigest          string        `yaml:"itemSetDigest"`
	ReleaseSubjectDigest   string        `yaml:"releaseSubjectDigest"`
	CatalogDigest          string        `yaml:"catalogDigest"`
	ResultProfileDigest    string        `yaml:"resultProfileDigest"`
	DeploymentConfigDigest string        `yaml:"deploymentConfigDigest"`
	PublicOriginDigest     string        `yaml:"publicOriginDigest"`
	Backend                string        `yaml:"backend"`
	Architecture           string        `yaml:"architecture"`
	GeneratedAt            string        `yaml:"generatedAt"`
	DeadlineAt             string        `yaml:"deadlineAt"`
	Items                  []RequestItem `yaml:"items"`
}

type RequestItem struct {
	ItemID       string         `yaml:"itemId"`
	ScenarioID   string         `yaml:"scenarioId"`
	CellID       string         `yaml:"cellId"`
	InputDigest  string         `yaml:"inputDigest"`
	TypedLocator map[string]any `yaml:"typedLocator"`
}

type Report struct {
	SchemaVersion          int          `yaml:"schemaVersion"`
	DocumentType           string       `yaml:"documentType"`
	InvocationID           string       `yaml:"invocationId"`
	ManifestDigest         string       `yaml:"manifestDigest"`
	ItemSetDigest          string       `yaml:"itemSetDigest"`
	ReleaseSubjectDigest   string       `yaml:"releaseSubjectDigest"`
	CatalogDigest          string       `yaml:"catalogDigest"`
	ResultProfileDigest    string       `yaml:"resultProfileDigest"`
	DeploymentConfigDigest string       `yaml:"deploymentConfigDigest"`
	PublicOriginDigest     string       `yaml:"publicOriginDigest"`
	Backend                string       `yaml:"backend"`
	Architecture           string       `yaml:"architecture"`
	HelperRequestDigest    string       `yaml:"helperRequestDigest"`
	StartedAt              string       `yaml:"startedAt"`
	FinishedAt             string       `yaml:"finishedAt"`
	Items                  []ReportItem `yaml:"items"`
}

type ReportItem struct {
	ItemID         string       `yaml:"itemId"`
	ScenarioID     string       `yaml:"scenarioId"`
	CellID         string       `yaml:"cellId"`
	InputDigest    string       `yaml:"inputDigest"`
	ResultDigest   string       `yaml:"resultDigest"`
	Outcome        string       `yaml:"outcome"`
	Category       string       `yaml:"category"`
	StartedAt      string       `yaml:"startedAt"`
	FinishedAt     string       `yaml:"finishedAt"`
	ArgvSanitized  []string     `yaml:"argvSanitized"`
	ExitCode       int          `yaml:"exitCode"`
	Assertions     []Assertion  `yaml:"assertions"`
	Attachments    []Attachment `yaml:"attachments"`
	CleanupOutcome string       `yaml:"cleanupOutcome"`
}

type Assertion struct {
	ID       string `yaml:"id"`
	Expected any    `yaml:"expected"`
	Actual   any    `yaml:"actual"`
	Result   string `yaml:"result"`
}

type Attachment struct {
	Kind      string `yaml:"kind"`
	SHA256    string `yaml:"sha256"`
	SizeBytes int    `yaml:"sizeBytes"`
	MediaType string `yaml:"mediaType,omitempty"`
}

// ReadRequest loads and schema-validates exactly the bytes whose SHA-256 will
// be closed over by the report; YAML conversion is solely for schema checking.
func ReadRequest(path string) (Request, []byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Request{}, nil, fmt.Errorf("read helper request: %w", err)
	}
	if err := ValidateRequest(body); err != nil {
		return Request{}, nil, err
	}
	var request Request
	if err := yaml.Unmarshal(body, &request); err != nil {
		return Request{}, nil, fmt.Errorf("decode helper request: %w", err)
	}
	return request, body, nil
}

func ValidateRequest(body []byte) error { return validate(body, "helperRequest") }
func ValidateReport(body []byte) error  { return validate(body, "helperReport") }

func validate(body []byte, definition string) error {
	var instance any
	if err := yaml.Unmarshal(body, &instance); err != nil {
		return fmt.Errorf("parse deployment verification document: %w", err)
	}
	jsonBody, err := json.Marshal(instance)
	if err != nil {
		return fmt.Errorf("convert deployment verification YAML to JSON: %w", err)
	}
	if err := json.Unmarshal(jsonBody, &instance); err != nil {
		return fmt.Errorf("normalize deployment verification document: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(gen.DeploymentVerificationSchema))
	if err != nil {
		return fmt.Errorf("load deployment verification schema: %w", err)
	}
	if err := compiler.AddResource(schemaURL, schemaDocument); err != nil {
		return fmt.Errorf("add deployment verification schema: %w", err)
	}
	schema, err := compiler.Compile(schemaURL + "#/$defs/" + definition)
	if err != nil {
		return fmt.Errorf("compile deployment verification schema: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("validate deployment verification %s: %w", definition, err)
	}
	return nil
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// WriteReport marshals, validates, then atomically publishes the importable
// report. Validating our own output prevents a helper bug from producing an
// irrecoverable artifact that Quoin must reject later.
func WriteReport(path string, report Report) ([]byte, error) {
	body, err := yaml.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("marshal helper report: %w", err)
	}
	if err := ValidateReport(body); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create report directory: %w", err)
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open temporary helper report: %w", err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(body); err != nil {
		return nil, fmt.Errorf("write helper report: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync helper report: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close helper report: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return nil, fmt.Errorf("publish helper report: %w", err)
	}
	remove = false
	return body, nil
}
