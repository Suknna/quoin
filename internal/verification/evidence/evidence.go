// Package evidence owns the verification evidence index
// (verification-evidence.schema.json): the digest-referenced per-item record
// of every scenario/cell execution, its assertions, attachments and cleanup
// outcome. Facts are collected mechanically; nothing here interprets product
// behaviour (VERIFY-VERDICT-004).
package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Media type of the evidence index document itself.
const IndexMediaType = "application/vnd.quoin.verification-evidence.v1+json"

// RedactionProfile is the only redaction vocabulary the frozen catalog
// declares.
const RedactionProfile = "verification-redaction-v1"

// Retention classes frozen in the evidence schema.
const (
	RetentionLongTerm  = "long_term"
	RetentionGenerated = "generated"
)

type Index struct {
	ContractVersion           int    `json:"contract_version"`
	InvocationID              string `json:"invocation_id"`
	Layer                     string `json:"layer"`
	SubjectDigest             string `json:"subject_digest"`
	VerificationCatalogDigest string `json:"verification_catalog_digest"`
	ResultProfileDigest       string `json:"result_profile_digest"`
	ManifestDigest            string `json:"manifest_digest,omitempty"`
	GeneratedAt               string `json:"generated_at"`
	RedactionProfile          string `json:"redaction_profile"`
	Items                     []Item `json:"items"`
}

// Item is one scenario/cell execution record (evidence schema $defs/item).
type Item struct {
	ScenarioID              string       `json:"scenario_id"`
	CellID                  string       `json:"cell_id"`
	InputDigest             string       `json:"input_digest"`
	ResultDigest            string       `json:"result_digest"`
	Outcome                 string       `json:"outcome"` // passed | warned | failed
	Category                string       `json:"category"`
	StartedAt               string       `json:"started_at"`
	FinishedAt              string       `json:"finished_at"`
	AuthoritativeRecordedAt string       `json:"authoritative_recorded_at"`
	AuthoritativeTimeSource string       `json:"authoritative_time_source"`
	EnvironmentDigest       string       `json:"environment_digest"`
	Environment             Environment  `json:"environment"`
	ToolVersion             string       `json:"tool_version"`
	ArgvSanitized           []string     `json:"argv_sanitized"`
	ExitCode                *int         `json:"exit_code"`
	Assertions              []Assertion  `json:"assertions"`
	Attachments             []Attachment `json:"attachments"`
	Cleanup                 Cleanup      `json:"cleanup"`
	CausalIDs               []string     `json:"causal_ids"`
	ProofRefs               []string     `json:"proof_refs"`
}

// TestName is the catalog test-name projection `<scenario>.<cell>`.
func (item Item) TestName() string {
	return item.ScenarioID + "." + item.CellID
}

type Environment struct {
	Backend             string   `json:"backend"`
	Architecture        string   `json:"architecture"`
	KubernetesVersion   string   `json:"kubernetes_version,omitempty"`
	DockerEngineVersion string   `json:"docker_engine_version,omitempty"`
	ComposeVersion      string   `json:"compose_version,omitempty"`
	BrowserArtifact     string   `json:"browser_artifact,omitempty"`
	BrowserVersion      string   `json:"browser_version,omitempty"`
	ToolchainDigest     string   `json:"toolchain_digest"`
	ExternalStackDigest string   `json:"external_stack_digest,omitempty"`
	FaultBackendDigest  string   `json:"fault_backend_digest,omitempty"`
	CapabilityIDs       []string `json:"capability_ids"`
}

type Assertion struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Expected   any    `json:"expected"`
	Actual     any    `json:"actual"`
	Result     string `json:"result"` // passed | failed
	DetailCode string `json:"detail_code,omitempty"`
}

type Attachment struct {
	Kind           string `json:"kind"`
	SHA256         string `json:"sha256"`
	SizeBytes      int64  `json:"size_bytes"`
	MediaType      string `json:"media_type"`
	Locator        string `json:"locator"`
	RetentionClass string `json:"retention_class"`
	Sensitive      bool   `json:"sensitive"`
}

type Cleanup struct {
	Required   bool        `json:"required"`
	Outcome    string      `json:"outcome"` // clean | residue | indeterminate | not_run
	Assertions []Assertion `json:"assertions"`
}

// Attachment kinds frozen in the evidence schema.
const (
	AttachmentStructuredResult = "structured_result"
	AttachmentStdout           = "stdout"
	AttachmentStderr           = "stderr"
)

// RetentionClassFor returns the frozen retention class of an attachment
// kind: raw execution output is generated and inherits the shared retention
// setting; structured results and manifests stay long-term
// (VERIFY-DA-RETENTION-001).
func RetentionClassFor(kind string) string {
	switch kind {
	case AttachmentStdout, AttachmentStderr, "logs", "metrics", "trace", "screenshot", "video", "database":
		return RetentionGenerated
	default:
		return RetentionLongTerm
	}
}

// Digest returns the SHA-256 hex digest of a byte slice.
func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// DigestFile returns the SHA-256 hex digest of a file's current content.
func DigestFile(path string) (string, int64, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	return Digest(body), int64(len(body)), nil
}

// CanonicalJSON serializes a value with sorted map keys so digests are
// stable across invocations.
func CanonicalJSON(value any) string {
	var builder strings.Builder
	writeCanonical(&builder, value)
	return builder.String()
}

func writeCanonical(builder *strings.Builder, value any) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		builder.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				builder.WriteByte(',')
			}
			builder.WriteString(fmt.Sprintf("%q:", key))
			writeCanonical(builder, typed[key])
		}
		builder.WriteByte('}')
	case []any:
		builder.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				builder.WriteByte(',')
			}
			writeCanonical(builder, item)
		}
		builder.WriteByte(']')
	case string:
		builder.WriteString(fmt.Sprintf("%q", typed))
	case time.Time:
		builder.WriteString(fmt.Sprintf("%q", typed.UTC().Format(time.RFC3339Nano)))
	default:
		builder.WriteString(fmt.Sprintf("%v", typed))
	}
}

// Write persists an attachment body into the invocation evidence tree and
// returns its descriptor with the computed digest and frozen retention class.
func Write(root, name, kind, mediaType string, body []byte, sensitive bool) (Attachment, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Attachment{}, err
	}
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return Attachment{}, err
	}
	return Attachment{
		Kind:           kind,
		SHA256:         Digest(body),
		SizeBytes:      int64(len(body)),
		MediaType:      mediaType,
		Locator:        path,
		RetentionClass: RetentionClassFor(kind),
		Sensitive:      sensitive,
	}, nil
}
