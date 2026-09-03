// Package result owns the in-toto Test Result projection
// (verification-result.schema.json) and the frozen outcome profile
// (verification-result-profile.yaml). The deterministic program computes
// verdicts from categories; nothing may override the mapping
// (VERIFY-VERDICT-001..005).
package result

import (
	"fmt"
	"os"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Suknna/quoin/internal/verification/evidence"
)

// Statement field constants frozen by the profile schema.
const (
	StatementType  = "https://in-toto.io/Statement/v1"
	PredicateType  = "https://in-toto.io/attestation/test-result/v0.1"
	ProfileVersion = 1
)

// Verdicts.
const (
	VerdictPassed = "PASSED"
	VerdictWarned = "WARNED"
	VerdictFailed = "FAILED"
)

type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

type ResourceDescriptor struct {
	Name      string            `json:"name"`
	URI       string            `json:"uri,omitempty"`
	MediaType string            `json:"mediaType,omitempty"`
	Digest    map[string]string `json:"digest"`
}

type EnvironmentMatrix struct {
	MatrixDigest string `json:"matrixDigest"`
	CellCount    int    `json:"cellCount"`
}

type ObservationSummary struct {
	RequiredCells int `json:"requiredCells"`
	PassedCells   int `json:"passedCells"`
	WarnedCells   int `json:"warnedCells"`
	FailedCells   int `json:"failedCells"`
}

type QuoinExtension struct {
	ProfileVersion     int                `json:"profileVersion"`
	InvocationID       string             `json:"invocationId"`
	Layer              string             `json:"layer"`
	StartedAt          string             `json:"startedAt"`
	FinishedAt         string             `json:"finishedAt"`
	Environment        EnvironmentMatrix  `json:"environment"`
	EvidenceIndex      EvidenceIndexRef   `json:"evidenceIndex"`
	ObservationSummary ObservationSummary `json:"observationSummary,omitempty"`
}

type EvidenceIndexRef struct {
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType"`
	SizeBytes int64  `json:"sizeBytes"`
	URI       string `json:"uri,omitempty"`
}

type Predicate struct {
	Result        string               `json:"result"`
	Configuration []ResourceDescriptor `json:"configuration"`
	URL           string               `json:"url,omitempty"`
	PassedTests   []string             `json:"passedTests"`
	WarnedTests   []string             `json:"warnedTests"`
	FailedTests   []string             `json:"failedTests"`
	Quoin         QuoinExtension       `json:"quoin"`
}

type Statement struct {
	Type          string    `json:"_type"`
	Subject       []Subject `json:"subject"`
	PredicateType string    `json:"predicateType"`
	Predicate     Predicate `json:"predicate"`
}

// Profile mirrors verification-result-profile.yaml.
type Profile struct {
	ContractVersion int                `yaml:"contract_version"`
	ProfileID       string             `yaml:"profile_id"`
	Outcomes        []string           `yaml:"outcomes"`
	Categories      []ProfileCategory  `yaml:"categories"`
	TimeClosure     ProfileTimeClosure `yaml:"time_closure"`
}

type ProfileCategory struct {
	Code     string `yaml:"code"`
	Outcome  string `yaml:"outcome"`
	Priority int    `yaml:"priority"`
	Meaning  string `yaml:"meaning"`
}

type ProfileTimeClosure struct {
	ObservationDeadlineSeconds int `yaml:"observation_deadline_seconds"`
}

// LoadProfile parses the frozen result profile document.
func LoadProfile(path string) (*Profile, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read result profile: %w", err)
	}
	var profile Profile
	if err := yaml.Unmarshal(body, &profile); err != nil {
		return nil, fmt.Errorf("parse result profile %s: %w", path, err)
	}
	if len(profile.Categories) == 0 {
		return nil, fmt.Errorf("result profile %s declares no categories", path)
	}
	return &profile, nil
}

// Outcome returns the frozen outcome (passed|warned|failed) of a category.
// An unknown category is a verifier invariant violation: it maps to failed,
// never to a silent pass (VERIFY-VERDICT-002).
func (p *Profile) Outcome(category string) string {
	for _, candidate := range p.Categories {
		if candidate.Code == category {
			return candidate.Outcome
		}
	}
	return "failed"
}

// Aggregate computes the suite verdict from item outcomes using the frozen
// aggregation rules: any failed/conflicted item fails the suite, otherwise
// any warned item warns it, otherwise all passed.
func (p *Profile) Aggregate(items []evidence.Item) string {
	verdict := VerdictPassed
	for _, item := range items {
		switch item.Outcome {
		case "failed":
			return VerdictFailed
		case "warned":
			verdict = VerdictWarned
		case "passed":
		default:
			// Unknown outcome classes are invariant violations.
			return VerdictFailed
		}
	}
	return verdict
}

// TestNameSets partitions the executed items into the passed/warned/failed
// test-name lists. Names are catalog scenario/cell IDs only.
func TestNameSets(items []evidence.Item) (passed, warned, failed []string) {
	passed, warned, failed = []string{}, []string{}, []string{}
	for _, item := range items {
		switch item.Outcome {
		case "passed":
			passed = append(passed, item.TestName())
		case "warned":
			warned = append(warned, item.TestName())
		default:
			failed = append(failed, item.TestName())
		}
	}
	sort.Strings(passed)
	sort.Strings(warned)
	sort.Strings(failed)
	return passed, warned, failed
}

// Summarize builds the observation summary over the executed items.
func Summarize(items []evidence.Item) ObservationSummary {
	summary := ObservationSummary{RequiredCells: len(items)}
	for _, item := range items {
		switch item.Outcome {
		case "passed":
			summary.PassedCells++
		case "warned":
			summary.WarnedCells++
		default:
			summary.FailedCells++
		}
	}
	return summary
}

// EnvironmentMatrixDigest freezes the resolved environment matrix: the
// digest covers every executed cell's environment identity and capability
// set, and the count is the full matrix cell count — a single environment
// can never impersonate a multi-cell invocation (VERIFY-CATALOG-005).
func EnvironmentMatrixDigest(items []evidence.Item) EnvironmentMatrix {
	type cellDescriptor struct {
		Scenario  string   `json:"scenario"`
		Cell      string   `json:"cell"`
		Backend   string   `json:"backend"`
		Arch      string   `json:"architecture"`
		Toolchain string   `json:"toolchain_digest"`
		Caps      []string `json:"capability_ids"`
	}
	descriptors := make([]cellDescriptor, 0, len(items))
	for _, item := range items {
		caps := append([]string{}, item.Environment.CapabilityIDs...)
		sort.Strings(caps)
		descriptors = append(descriptors, cellDescriptor{
			Scenario: item.ScenarioID, Cell: item.CellID,
			Backend: item.Environment.Backend, Arch: item.Environment.Architecture,
			Toolchain: item.Environment.ToolchainDigest, Caps: caps,
		})
	}
	sort.Slice(descriptors, func(i, j int) bool {
		left, right := descriptors[i], descriptors[j]
		if left.Scenario != right.Scenario {
			return left.Scenario < right.Scenario
		}
		return left.Cell < right.Cell
	})
	return EnvironmentMatrix{
		MatrixDigest: evidence.Digest([]byte(evidence.CanonicalJSON(descriptors))),
		CellCount:    len(descriptors),
	}
}

// Deadline is the frozen observation deadline of an invocation
// (VERIFY-DA-TIME-001): started_at + eight hours, not user-configurable.
func (p *Profile) Deadline(startedAt time.Time) time.Time {
	return startedAt.Add(time.Duration(p.TimeClosure.ObservationDeadlineSeconds) * time.Second)
}

// CheckTimeClosure validates the frozen finalization arithmetic: snapshot
// and finalization must sit inside [started, deadline], finalization not
// before snapshot, and the whole invocation within the observation window.
func (p *Profile) CheckTimeClosure(startedAt, snapshotAt, finalizedAt time.Time) error {
	deadline := p.Deadline(startedAt)
	switch {
	case snapshotAt.Before(startedAt):
		return fmt.Errorf("snapshot_at %s before started_at %s", snapshotAt, startedAt)
	case snapshotAt.After(deadline):
		return fmt.Errorf("snapshot_at %s after deadline %s", snapshotAt, deadline)
	case finalizedAt.Before(snapshotAt):
		return fmt.Errorf("finalized_at %s before snapshot_at %s", finalizedAt, snapshotAt)
	case finalizedAt.After(deadline):
		return fmt.Errorf("finalized_at %s after deadline %s", finalizedAt, deadline)
	}
	return nil
}
