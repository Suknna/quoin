// Package runner executes one verification layer invocation deterministically
// (VERIFY-VERDICT-003/004): it computes applicability and the DAG, runs
// setup/action/assert/teardown per applicable cell without fail-fast,
// evaluates declared assertions from machine facts only, persists the
// evidence index and emits the in-toto Test Result statement.
package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Suknna/quoin/internal/verification/catalog"
	"github.com/Suknna/quoin/internal/verification/evidence"
	"github.com/Suknna/quoin/internal/verification/result"
)

// SubjectBinding is the immutable subject the invocation verdicts attach to.
type SubjectBinding struct {
	Name   string
	Digest string
}

// Options configures one invocation.
type Options struct {
	CatalogPath  string
	ProfilePath  string
	ContractsDir string // frozen schemas directory for self-validation
	OutputDir    string // invocation artifacts root
	RepoRoot     string // cwd for catalog phase commands
	Layer        string
	InvocationID string
	Subject      SubjectBinding
	Target       *catalog.Target // nil for the contract gate
	ToolVersion  string
	Now          func() time.Time
}

// RunReport is the completed invocation.
type RunReport struct {
	Verdict   string
	Index     evidence.Index
	Statement result.Statement
	Predicate result.Predicate
	OutputDir string
}

type invocation struct {
	opts            Options
	catalog         *catalog.Catalog
	profile         *result.Profile
	now             func() time.Time
	items           []evidence.Item
	byScenario      map[string][]evidence.Item
	toolchainDigest string
}

// Run executes the whole layer invocation and writes its artifacts.
func Run(opts Options) (*RunReport, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ToolVersion == "" {
		opts.ToolVersion = "quoin-verify/dev"
	}
	loaded, err := catalog.LoadAndValidate(opts.CatalogPath)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	profile, err := result.LoadProfile(opts.ProfilePath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return nil, err
	}
	state := &invocation{
		opts: opts, catalog: loaded, profile: profile, now: opts.Now,
		byScenario:      map[string][]evidence.Item{},
		toolchainDigest: evidence.Digest([]byte(opts.ToolVersion)),
	}
	if err := state.executeLayer(); err != nil {
		return nil, err
	}
	return state.finalize()
}

func (state *invocation) executeLayer() error {
	for _, scenarioID := range catalog.ExecutionOrder(state.catalog, state.opts.Layer) {
		scenario := state.catalog.Scenario(scenarioID)
		if scenario.Requirement != "required" {
			continue
		}
		if err := state.executeScenario(scenario); err != nil {
			return err
		}
	}
	return state.executeDiagnostics()
}

// dependencyFailure returns the causal item references that block a scenario:
// every dependency item that did not pass in this invocation, or a synthetic
// reference when a dependency produced no applicable result at all.
func (state *invocation) dependencyFailure(scenario *catalog.Scenario) []string {
	var causes []string
	for _, dependency := range scenario.DependsOn {
		dependencyItems := state.byScenario[dependency]
		if len(dependencyItems) == 0 {
			causes = append(causes, fmt.Sprintf("%s:%s:no-applicable-result", state.opts.InvocationID, dependency))
			continue
		}
		for _, item := range dependencyItems {
			if item.Outcome != "passed" {
				causes = append(causes, fmt.Sprintf("%s:%s:%s", state.opts.InvocationID, item.ScenarioID, item.CellID))
			}
		}
	}
	return causes
}

func (state *invocation) executeScenario(scenario *catalog.Scenario) error {
	cells := catalog.ApplicableCells(scenario, state.opts.Target)
	if len(cells) == 0 {
		return nil
	}
	if causes := state.dependencyFailure(scenario); len(causes) > 0 {
		for _, cell := range cells {
			state.record(scenario, cell, state.notRunItem(scenario, cell, causes))
		}
		return nil
	}
	for _, cell := range cells {
		item, err := state.executeCell(scenario, &cell)
		if err != nil {
			return err
		}
		state.record(scenario, cell, item)
	}
	return nil
}

// executeDiagnostics runs diagnostic scenarios only from persisted
// required-item facts (VERIFY-VERDICT-005). Their items are recorded for
// evidence but never enter the required set or the suite verdict.
func (state *invocation) executeDiagnostics() error {
	observed := map[string]bool{}
	for _, item := range state.items {
		// Diagnostic triggers speak the profile's trigger vocabulary
		// (failed | warned | not_run), not the finer category codes.
		if item.Category == "not_run" {
			observed["not_run"] = true
			continue
		}
		observed[state.profile.Outcome(item.Category)] = true
	}
	for _, scenarioID := range catalog.ExecutionOrder(state.catalog, state.opts.Layer) {
		scenario := state.catalog.Scenario(scenarioID)
		if scenario.Requirement != "diagnostic" || scenario.Status != "active" {
			continue
		}
		triggered := false
		for _, category := range scenario.DiagnosticTriggerCategories {
			if observed[category] {
				triggered = true
			}
		}
		if !triggered {
			continue
		}
		var triggers []string
		for _, item := range state.items {
			for _, category := range scenario.DiagnosticTriggerCategories {
				triggered := item.Category == category ||
					(item.Category != "not_run" && state.profile.Outcome(item.Category) == category)
				if triggered {
					triggers = append(triggers, fmt.Sprintf("%s:%s:%s", state.opts.InvocationID, item.ScenarioID, item.CellID))
				}
			}
		}
		for _, cell := range catalog.ApplicableCells(scenario, state.opts.Target) {
			if depCauses := state.dependencyFailure(scenario); len(depCauses) > 0 {
				state.record(scenario, cell, state.notRunItem(scenario, cell, depCauses))
				continue
			}
			item, err := state.executeCell(scenario, &cell)
			if err != nil {
				return err
			}
			// The diagnostic cites the persisted facts that triggered it;
			// it appends evidence and never rewrites the trigger result.
			item.CausalIDs = triggers
			state.record(scenario, cell, item)
		}
	}
	return nil
}

func (state *invocation) record(scenario *catalog.Scenario, cell catalog.Cell, item evidence.Item) {
	state.items = append(state.items, item)
	state.byScenario[scenario.ID] = append(state.byScenario[scenario.ID], item)
}

// finalize writes attachments' index, validates both documents against the
// frozen schemas and assembles the statement (VERIFY-EVIDENCE-001/003).
func (state *invocation) finalize() (*RunReport, error) {
	// Diagnostics persist as evidence but never enter the required set,
	// the test-name lists or the suite verdict (VERIFY-VERDICT-005).
	var required []evidence.Item
	for i := range state.items {
		scenario := state.catalog.Scenario(state.items[i].ScenarioID)
		if scenario != nil && scenario.Requirement == "diagnostic" {
			continue
		}
		required = append(required, state.items[i])
	}
	if required == nil {
		required = []evidence.Item{}
	}
	for i := range state.items {
		item := &state.items[i]
		if item.Assertions == nil {
			item.Assertions = []evidence.Assertion{}
		}
		if item.Attachments == nil {
			item.Attachments = []evidence.Attachment{}
		}
		if item.CausalIDs == nil {
			item.CausalIDs = []string{}
		}
		if item.ProofRefs == nil {
			item.ProofRefs = []string{}
		}
		if item.ArgvSanitized == nil {
			item.ArgvSanitized = []string{}
		}
		if item.Cleanup.Assertions == nil {
			item.Cleanup.Assertions = []evidence.Assertion{}
		}
		if item.Environment.CapabilityIDs == nil {
			item.Environment.CapabilityIDs = []string{}
		}
	}
	index := evidence.Index{
		ContractVersion:           1,
		InvocationID:              state.opts.InvocationID,
		Layer:                     state.opts.Layer,
		SubjectDigest:             state.opts.Subject.Digest,
		VerificationCatalogDigest: digestOfFileOrEmpty(state.opts.CatalogPath),
		ResultProfileDigest:       digestOfFileOrEmpty(state.opts.ProfilePath),
		GeneratedAt:               state.now().UTC().Format(time.RFC3339Nano),
		RedactionProfile:          evidence.RedactionProfile,
		Items:                     append([]evidence.Item{}, state.items...),
	}
	indexBody, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return nil, err
	}
	indexBody = append(indexBody, '\n')
	indexPath := filepath.Join(state.opts.OutputDir, "evidence.json")
	if err := os.WriteFile(indexPath, indexBody, 0o644); err != nil {
		return nil, err
	}
	if err := validateJSONAgainstSchema(filepath.Join(state.opts.ContractsDir, "schemas", "verification-evidence.schema.json"), indexBody); err != nil {
		return nil, fmt.Errorf("evidence index violates frozen schema: %w", err)
	}

	passed, warned, failed := result.TestNameSets(required)
	started, finished := state.window(required)
	matrix := result.EnvironmentMatrixDigest(required)
	statement := result.Statement{
		Type:          result.StatementType,
		PredicateType: result.PredicateType,
		Subject: []result.Subject{{
			Name:   state.opts.Subject.Name,
			Digest: map[string]string{"sha256": state.opts.Subject.Digest},
		}},
		Predicate: result.Predicate{
			Result:        state.profile.Aggregate(required),
			Configuration: state.configuration(),
			PassedTests:   passed,
			WarnedTests:   warned,
			FailedTests:   failed,
			Quoin: result.QuoinExtension{
				ProfileVersion: result.ProfileVersion,
				InvocationID:   state.opts.InvocationID,
				Layer:          state.opts.Layer,
				StartedAt:      started,
				FinishedAt:     finished,
				Environment:    matrix,
				EvidenceIndex: result.EvidenceIndexRef{
					SHA256:    evidence.Digest(indexBody),
					MediaType: evidence.IndexMediaType,
					SizeBytes: int64(len(indexBody)),
					URI:       "evidence.json",
				},
				ObservationSummary: result.Summarize(required),
			},
		},
	}
	statementBody, err := json.MarshalIndent(statement, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := validateJSONAgainstSchema(filepath.Join(state.opts.ContractsDir, "schemas", "verification-result.schema.json"), statementBody); err != nil {
		return nil, fmt.Errorf("test result violates frozen schema: %w", err)
	}
	if err := os.WriteFile(filepath.Join(state.opts.OutputDir, "test-result.json"), append(statementBody, '\n'), 0o644); err != nil {
		return nil, err
	}
	return &RunReport{
		Verdict:   statement.Predicate.Result,
		Index:     index,
		Statement: statement,
		Predicate: statement.Predicate,
		OutputDir: state.opts.OutputDir,
	}, nil
}

func (state *invocation) window(items []evidence.Item) (string, string) {
	if len(items) == 0 {
		now := state.now().UTC().Format(time.RFC3339Nano)
		return now, now
	}
	started, finished := items[0].StartedAt, items[0].FinishedAt
	for _, item := range items {
		if item.StartedAt < started {
			started = item.StartedAt
		}
		if item.FinishedAt > finished {
			finished = item.FinishedAt
		}
	}
	return started, finished
}

// configuration freezes the contract digests the verdict binds to
// (VERIFY-EVIDENCE-003).
func (state *invocation) configuration() []result.ResourceDescriptor {
	relative := []struct{ name, path, media string }{
		{"verification-catalog.yaml", state.opts.CatalogPath, "application/yaml"},
		{"verification-result-profile.yaml", state.opts.ProfilePath, "application/yaml"},
		{"verification-catalog.schema.json", filepath.Join(state.opts.ContractsDir, "schemas", "verification-catalog.schema.json"), "application/schema+json"},
		{"verification-result.schema.json", filepath.Join(state.opts.ContractsDir, "schemas", "verification-result.schema.json"), "application/schema+json"},
		{"verification-evidence.schema.json", filepath.Join(state.opts.ContractsDir, "schemas", "verification-evidence.schema.json"), "application/schema+json"},
		{"connection-probes.yaml", filepath.Join(state.opts.ContractsDir, "connection-probes.yaml"), "application/yaml"},
	}
	descriptors := make([]result.ResourceDescriptor, 0, len(relative)+1)
	for _, item := range relative {
		descriptors = append(descriptors, result.ResourceDescriptor{
			Name: item.name, MediaType: item.media,
			Digest: map[string]string{"sha256": digestOfFileOrEmpty(item.path)},
		})
	}
	descriptors = append(descriptors, result.ResourceDescriptor{
		Name: "toolchain", MediaType: "text/plain",
		Digest: map[string]string{"sha256": state.toolchainDigest},
	})
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].Name < descriptors[j].Name })
	return descriptors
}

func digestOfFileOrEmpty(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return evidence.Digest(nil)
	}
	return evidence.Digest(body)
}
