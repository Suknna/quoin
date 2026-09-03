// Package catalog loads and validates Quoin's frozen verification catalog
// (VERIFY-AUTHORITY-001). The YAML document and its JSON Schema are the only
// machine authority; this package adds the cross-contract build-gate checks
// the schema cannot express (VERIFY-COVERAGE-002, VERIFY-CATALOG-002/003/005)
// and resolves applicability and the same-layer execution DAG.
package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dlclark/regexp2"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// Layer names as frozen in the catalog schema.
const (
	LayerContractGate         = "contract_gate"
	LayerReleaseQualification = "release_qualification"
	LayerDeploymentAcceptance = "deployment_acceptance"
)

var layerRank = map[string]int{
	LayerContractGate:         0,
	LayerReleaseQualification: 1,
	LayerDeploymentAcceptance: 2,
}

// Target is the deployment context applicability modes resolve against. The
// contract-gate layer runs without one, so only `always` cells apply.
type Target struct {
	Backend       string
	Architecture  string
	CurrentObject bool // for_each_current_object modes need the site's frozen object set
}

type Capability struct {
	ID   string `yaml:"id"`
	Kind string `yaml:"kind"`
}

type ValidationRoot struct {
	ID     string `yaml:"id"`
	Source string `yaml:"source"`
	Anchor string `yaml:"anchor"`
}

type Environment struct {
	ID                       string `yaml:"id"`
	Kind                     string `yaml:"kind"`
	Native                   bool   `yaml:"native"`
	RequiresReleaseArtifacts bool   `yaml:"requires_release_artifacts"`
	Description              string `yaml:"description"`
}

type Applicability struct {
	Mode         string `yaml:"mode"`
	Backend      string `yaml:"backend"`
	Architecture string `yaml:"architecture"`
	ObjectKind   string `yaml:"object_kind"`
}

type Assertion struct {
	ID       string `yaml:"id"`
	Kind     string `yaml:"kind"`
	Expected any    `yaml:"expected"`
}

type Cell struct {
	ID                   string         `yaml:"id"`
	EnvironmentID        string         `yaml:"environment_id"`
	Architecture         string         `yaml:"architecture"`
	Applicability        Applicability  `yaml:"applicability"`
	RequiredCapabilities []string       `yaml:"required_capabilities"`
	Parameters           map[string]any `yaml:"parameters"`
	Assertions           []Assertion    `yaml:"assertions"`
}

type Executor struct {
	Kind                       string `yaml:"kind"`
	Entrypoint                 string `yaml:"entrypoint"`
	RequiresAdminSession       bool   `yaml:"requires_admin_session"`
	RequiresProductCredentials bool   `yaml:"requires_product_credentials"`
}

type Phases struct {
	Setup    string `yaml:"setup"`
	Action   string `yaml:"action"`
	Assert   string `yaml:"assert"`
	Teardown string `yaml:"teardown"`
}

type Subject struct {
	Kind        string   `yaml:"kind"`
	Selector    string   `yaml:"selector"`
	DriftFields []string `yaml:"drift_fields"`
}

type Fixture struct {
	ID             string `yaml:"id"`
	Kind           string `yaml:"kind"`
	Locator        string `yaml:"locator"`
	DigestRequired bool   `yaml:"digest_required"`
}

type Evidence struct {
	TestNames        []string `yaml:"test_names"`
	Attachments      []string `yaml:"attachments"`
	RedactionProfile string   `yaml:"redaction_profile"`
}

type Cleanup struct {
	Required          bool     `yaml:"required"`
	Entrypoint        string   `yaml:"entrypoint"`
	SuccessAssertions []string `yaml:"success_assertions"`
}

type Scenario struct {
	ID                          string    `yaml:"id"`
	Title                       string    `yaml:"title"`
	Status                      string    `yaml:"status"`
	Successor                   string    `yaml:"successor"`
	RetirementReason            string    `yaml:"retirement_reason"`
	Layer                       string    `yaml:"layer"`
	Requirement                 string    `yaml:"requirement"`
	ValidationRoots             []string  `yaml:"validation_roots"`
	Executor                    Executor  `yaml:"executor"`
	Phases                      Phases    `yaml:"phases"`
	DependsOn                   []string  `yaml:"depends_on"`
	ProofRefs                   []string  `yaml:"proof_refs"`
	RequiredCapabilities        []string  `yaml:"required_capabilities"`
	Subject                     Subject   `yaml:"subject"`
	Fixtures                    []Fixture `yaml:"fixtures"`
	Cells                       []Cell    `yaml:"cells"`
	Evidence                    Evidence  `yaml:"evidence"`
	Cleanup                     Cleanup   `yaml:"cleanup"`
	TimeoutSeconds              int       `yaml:"timeout_seconds"`
	DiagnosticTriggerCategories []string  `yaml:"diagnostic_trigger_categories"`
}

type Catalog struct {
	ContractVersion int    `yaml:"contract_version"`
	CatalogID       string `yaml:"catalog_id"`
	CatalogState    string `yaml:"catalog_state"`
	ResultProfileID string `yaml:"result_profile_id"`
	ContractRefs    struct {
		ConnectionProbesSHA256 string `yaml:"connection_probes_sha256"`
		ResultProfileSHA256    string `yaml:"result_profile_sha256"`
	} `yaml:"contract_refs"`
	Capabilities    []Capability     `yaml:"capability_definitions"`
	ValidationRoots []ValidationRoot `yaml:"validation_roots"`
	Environments    []Environment    `yaml:"environments"`
	Scenarios       []Scenario       `yaml:"scenarios"`

	scenarioByID   map[string]*Scenario
	environmentIDs map[string]bool
	capabilityIDs  map[string]bool
	rootIDs        map[string]bool
}

// Load parses the catalog document. It performs no validation beyond YAML
// decoding; use LoadAndValidate for the full gate.
func Load(path string) (*Catalog, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	var loaded Catalog
	decoder := yaml.NewDecoder(strings.NewReader(string(body)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&loaded); err != nil {
		return nil, fmt.Errorf("parse catalog %s: %w", path, err)
	}
	loaded.index()
	return &loaded, nil
}

func (c *Catalog) index() {
	c.scenarioByID = make(map[string]*Scenario, len(c.Scenarios))
	for i := range c.Scenarios {
		c.scenarioByID[c.Scenarios[i].ID] = &c.Scenarios[i]
	}
	c.environmentIDs = make(map[string]bool, len(c.Environments))
	for _, env := range c.Environments {
		c.environmentIDs[env.ID] = true
	}
	c.capabilityIDs = make(map[string]bool, len(c.Capabilities))
	for _, capability := range c.Capabilities {
		c.capabilityIDs[capability.ID] = true
	}
	c.rootIDs = make(map[string]bool, len(c.ValidationRoots))
	for _, root := range c.ValidationRoots {
		c.rootIDs[root.ID] = true
	}
}

// Scenario returns the scenario with the given catalog ID.
func (c *Catalog) Scenario(id string) *Scenario {
	return c.scenarioByID[id]
}

// Environment returns the declared environment with the given ID.
func (c *Catalog) Environment(id string) *Environment {
	for i := range c.Environments {
		if c.Environments[i].ID == id {
			return &c.Environments[i]
		}
	}
	return nil
}

// LoadAndValidate loads the catalog, validates it against the frozen
// verification-catalog JSON Schema (sibling `schemas` directory by default)
// and then runs the cross-contract build gate. Every violation is reported
// with its stable code.
func LoadAndValidate(path string) (*Catalog, error) {
	loaded, err := Load(path)
	if err != nil {
		return nil, err
	}
	schemaPath := filepath.Join(filepath.Dir(path), "schemas", "verification-catalog.schema.json")
	if err := validateAgainstSchema(path, schemaPath); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	if violations := loaded.CrossContractViolations(); len(violations) > 0 {
		messages := make([]string, 0, len(violations))
		for _, violation := range violations {
			messages = append(messages, violation.String())
		}
		return nil, fmt.Errorf("catalog violations: %s", strings.Join(messages, "; "))
	}
	return loaded, nil
}

type Violation struct {
	Code   string
	Detail string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s: %s", v.Code, v.Detail)
}

func validateAgainstSchema(catalogPath, schemaPath string) error {
	schemaBody, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	catalogBody, err := os.ReadFile(catalogPath)
	if err != nil {
		return fmt.Errorf("read catalog: %w", err)
	}
	var document any
	if err := yaml.Unmarshal(catalogBody, &document); err != nil {
		return fmt.Errorf("decode catalog: %w", err)
	}
	var schemaDocument any
	if err := yaml.Unmarshal(schemaBody, &schemaDocument); err != nil {
		return fmt.Errorf("decode schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseRegexpEngine(compileECMAScriptRegexp)
	if err := compiler.AddResource(schemaPath, schemaDocument); err != nil {
		return fmt.Errorf("load schema: %w", err)
	}
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	if err := schema.Validate(document); err != nil {
		return err
	}
	return nil
}

type ecmaRegexp regexp2.Regexp

func (re *ecmaRegexp) MatchString(value string) bool {
	matched, err := (*regexp2.Regexp)(re).MatchString(value)
	return err == nil && matched
}

func (re *ecmaRegexp) String() string {
	return (*regexp2.Regexp)(re).String()
}

// compileECMAScriptRegexp matches the engine the frozen contract gate
// (ci/verify-contracts) uses, so both gates accept the same patterns.
func compileECMAScriptRegexp(pattern string) (jsonschema.Regexp, error) {
	re, err := regexp2.Compile(pattern, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	return (*ecmaRegexp)(re), nil
}
