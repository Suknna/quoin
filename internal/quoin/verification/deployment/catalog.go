package deployment

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/verification/catalog"
)

var (
	catalogOnce sync.Once
	catalogDoc  *catalog.Catalog
	catalogErr  error
)

// frozenCatalog loads the frozen Deployment Acceptance catalog from the
// generated projection. The docs/ contract is the authority; the embedded
// bytes are its verbatim copy, so their digest equals the build-gate digest.
func frozenCatalog() (*catalog.Catalog, error) {
	catalogOnce.Do(func() {
		directory, err := os.MkdirTemp("", "quoin-verification-catalog-*")
		if err != nil {
			catalogErr = err
			return
		}
		defer os.RemoveAll(directory)
		path := filepath.Join(directory, "verification-catalog.yaml")
		if err := os.WriteFile(path, gen.VerificationCatalogYAML, 0o644); err != nil {
			catalogErr = err
			return
		}
		loaded, err := catalog.Load(path)
		if err != nil {
			catalogErr = fmt.Errorf("parse frozen verification catalog: %w", err)
			return
		}
		catalogDoc = loaded
	})
	return catalogDoc, catalogErr
}

// CatalogDigest is the SHA-256 of the frozen catalog projection bytes.
func CatalogDigest() string { return sha256Hex(gen.VerificationCatalogYAML) }

// ResultProfileDigest is the SHA-256 of the frozen result profile bytes.
func ResultProfileDigest() string { return sha256Hex(gen.VerificationResultProfileYAML) }

// ProbeContractDigest is the SHA-256 of the frozen connection probe contract.
func ProbeContractDigest() string { return sha256Hex(gen.ConnectionProbesYAML) }

// deploymentLocator mirrors verification_deployment_item_locators.
type deploymentLocator struct {
	ReleaseSubjectDigest   string `json:"releaseSubjectDigest"`
	DeploymentConfigDigest string `json:"deploymentConfigDigest"`
	PublicOriginDigest     string `json:"publicOriginDigest"`
	Backend                string `json:"backend"`
	Architecture           string `json:"architecture"`
}

type connectionLocator struct {
	ConnectionID         int64  `json:"connectionId"`
	ConnectionType       string `json:"connectionType"`
	RevisionID           int64  `json:"revisionId"`
	CredentialGeneration int64  `json:"credentialGenerationId"`
	RootBindingRevision  int64  `json:"rootBindingRevision"`
	ProbeContractDigest  string `json:"probeContractDigest"`
}

type configLocator struct {
	BusinessSystemID     int64 `json:"businessSystemId"`
	ConfigVersionID      int64 `json:"configVersionId"`
	LabelContractVersion int64 `json:"labelContractVersionId"`
}

type browserLocator struct {
	BrowserIdentityID      int64  `json:"browserIdentityId"`
	IdentityRevisionID     int64  `json:"identityRevisionId"`
	CurrentGenerationID    int64  `json:"currentGenerationId"`
	CurrentInventoryDigest string `json:"currentInventoryDigest"`
}

type observationLocator struct {
	BrowserArtifact string `json:"browserArtifact"`
	BrowserVersion  string `json:"browserVersion"`
	Architecture    string `json:"architecture"`
	ViewportCssPx   int    `json:"viewportCssPx"`
	Motion          string `json:"motion"`
}

// resolvedItem is one frozen manifest item before persistence.
type resolvedItem struct {
	ScenarioID  string
	CellID      string
	ObjectKind  string
	InputDigest string
	Input       any

	Deployment  *deploymentLocator
	Connection  *connectionLocator
	Config      *configLocator
	Browser     *browserLocator
	Observation *observationLocator
}

func (item resolvedItem) inputDocument() map[string]any {
	return map[string]any{
		"scenarioId": item.ScenarioID,
		"cellId":     item.CellID,
		"objectKind": item.ObjectKind,
		"input":      item.Input,
	}
}

// objectKindOf maps the catalog subject kind onto the frozen locator
// vocabulary (ui_surface observations are ui_observation items).
func objectKindOf(subjectKind string) string {
	switch subjectKind {
	case "deployment":
		return "deployment"
	case "connection":
		return "connection"
	case "config":
		return "config"
	case "browser_identity":
		return "browser_identity"
	case "ui_surface":
		return "ui_observation"
	default:
		return ""
	}
}

// resolveApplicable computes the complete applicable Deployment Acceptance
// item set for this deployment: matching deployment-target cells, the frozen
// current-object expansions, the recovery scenario when a real unconfirmed
// deployment-verification fence exists, and the release-frozen UI observation
// cells. Healthy sites never fabricate an incident for browser.lintel-recovery.
func (service *Service) resolveApplicable(ctx context.Context, conn *sql.Conn) ([]resolvedItem, error) {
	if service.binding == nil {
		return nil, service.ErrBindingUnavailable()
	}
	loaded, err := frozenCatalog()
	if err != nil {
		return nil, err
	}
	originDigest := sha256Text(service.publicOrigin)
	base := deploymentLocator{
		ReleaseSubjectDigest:   service.binding.ReleaseSubjectDigest,
		DeploymentConfigDigest: service.binding.DeploymentConfigDigest,
		PublicOriginDigest:     originDigest,
		Backend:                service.binding.Backend,
		Architecture:           service.binding.Architecture,
	}
	items := []resolvedItem{}
	for index := range loaded.Scenarios {
		scenario := &loaded.Scenarios[index]
		if scenario.Layer != catalog.LayerDeploymentAcceptance || scenario.Status != "active" {
			continue
		}
		objectKind := objectKindOf(scenario.Subject.Kind)
		if objectKind == "" {
			return nil, service.fail(errState, "catalog scenario %s has unsupported subject kind %q", scenario.ID, scenario.Subject.Kind)
		}
		for _, cell := range scenario.Cells {
			switch cell.Applicability.Mode {
			case "deployment_target":
				if cell.Applicability.Backend != service.binding.Backend || cell.Applicability.Architecture != service.binding.Architecture {
					continue
				}
				if scenario.ID == "browser.lintel-recovery" {
					affected, err := lowestUnconfirmedDeploymentFence(ctx, conn)
					if err != nil {
						return nil, err
					}
					if affected == nil {
						// A healthy site has no indeterminate deployment cleanup
						// fence; the recovery scenario is not fabricated.
						continue
					}
					items = append(items, resolvedItem{ScenarioID: scenario.ID, CellID: cell.ID, ObjectKind: objectKind, Browser: affected, Input: affected})
					continue
				}
				locator := base
				items = append(items, resolvedItem{ScenarioID: scenario.ID, CellID: cell.ID, ObjectKind: objectKind, Deployment: &locator, Input: &locator})
			case "for_each_current_object", "deployment_target_for_each_current_object":
				// The cartesian mode additionally requires the cell to match
				// this deployment's backend/architecture (VERIFY-CATALOG-005).
				if cell.Applicability.Mode == "deployment_target_for_each_current_object" &&
					(cell.Applicability.Backend != service.binding.Backend || cell.Applicability.Architecture != service.binding.Architecture) {
					continue
				}
				switch cell.Applicability.ObjectKind {
				case "connection":
					expanded, err := service.expandConnections(ctx, conn, scenario.ID, cell.ID)
					if err != nil {
						return nil, err
					}
					items = append(items, expanded...)
				case "config":
					expanded, err := service.expandConfigs(ctx, conn, scenario.ID, cell.ID)
					if err != nil {
						return nil, err
					}
					items = append(items, expanded...)
				case "browser_identity":
					expanded, err := service.expandBrowserIdentities(ctx, conn, scenario.ID, cell.ID)
					if err != nil {
						return nil, err
					}
					items = append(items, expanded...)
				default:
					return nil, service.fail(errState, "catalog scenario %s expands unsupported object kind %q", scenario.ID, cell.Applicability.ObjectKind)
				}
			case "always":
				// Only backend-independent ui_surface typed-observation cells
				// enter the site denominator through `always`.
				if objectKind != "ui_observation" {
					continue
				}
				observation, err := observationFromCell(cell, service.binding.BrowserChromiumRevision)
				if err != nil {
					return nil, err
				}
				if observation == nil {
					continue
				}
				items = append(items, resolvedItem{ScenarioID: scenario.ID, CellID: cell.ID, ObjectKind: objectKind, Observation: observation})
			}
		}
	}
	for index := range items {
		digest, err := canonicalDigest(items[index].inputDocument())
		if err != nil {
			return nil, err
		}
		items[index].InputDigest = digest
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ScenarioID != items[j].ScenarioID {
			return items[i].ScenarioID < items[j].ScenarioID
		}
		if items[i].CellID != items[j].CellID {
			return items[i].CellID < items[j].CellID
		}
		return items[i].InputDigest < items[j].InputDigest
	})
	return items, nil
}

// observationFromCell freezes one UI observation cell. The only browser
// version authority the release carries is the Playwright Chromium revision;
// branded_chrome cells have no frozen build in the release manifest and are
// therefore not part of the site denominator (never extrapolated).
func observationFromCell(cell catalog.Cell, chromiumRevision string) (*observationLocator, error) {
	artifact, _ := cell.Parameters["browser_artifact"].(string)
	motion, _ := cell.Parameters["motion_mode"].(string)
	viewport, _ := cell.Parameters["viewport_width_css_px"].(int)
	if artifact != "playwright_chromium" {
		return nil, nil
	}
	if artifact == "" || motion == "" || viewport == 0 || cell.Architecture == "" || chromiumRevision == "" {
		return nil, fmt.Errorf("ui observation cell %s is missing its frozen typed fields", cell.ID)
	}
	return &observationLocator{BrowserArtifact: artifact, BrowserVersion: chromiumRevision, Architecture: cell.Architecture, ViewportCssPx: viewport, Motion: motion}, nil
}

func (service *Service) expandConnections(ctx context.Context, conn *sql.Conn, scenarioID, cellID string) ([]resolvedItem, error) {
	probeDigest := ProbeContractDigest()
	rows, err := conn.QueryContext(ctx, `SELECT c.id,c.type,c.current_revision_id,g.id,g.key_binding_revision
		FROM connections c JOIN credential_generations g ON g.id=c.current_credential_generation_id
		WHERE c.enabled=1 AND c.current_revision_id IS NOT NULL ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []resolvedItem{}
	for rows.Next() {
		locator := &connectionLocator{ProbeContractDigest: probeDigest}
		if err := rows.Scan(&locator.ConnectionID, &locator.ConnectionType, &locator.RevisionID, &locator.CredentialGeneration, &locator.RootBindingRevision); err != nil {
			return nil, err
		}
		items = append(items, resolvedItem{ScenarioID: scenarioID, CellID: cellID, ObjectKind: "connection", Connection: locator, Input: locator})
	}
	return items, rows.Err()
}

func (service *Service) expandConfigs(ctx context.Context, conn *sql.Conn, scenarioID, cellID string) ([]resolvedItem, error) {
	rows, err := conn.QueryContext(ctx, `SELECT b.id,b.current_config_version_id,COALESCE(l.current_contract_id,0)
		FROM business_systems b LEFT JOIN label_contract_state l ON l.id=1
		WHERE b.enabled=1 AND b.current_config_version_id IS NOT NULL ORDER BY b.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []resolvedItem{}
	for rows.Next() {
		locator := &configLocator{}
		if err := rows.Scan(&locator.BusinessSystemID, &locator.ConfigVersionID, &locator.LabelContractVersion); err != nil {
			return nil, err
		}
		if locator.LabelContractVersion == 0 {
			return nil, service.fail(errState, "business system %d has a published config but no current Label Contract", locator.BusinessSystemID)
		}
		items = append(items, resolvedItem{ScenarioID: scenarioID, CellID: cellID, ObjectKind: "config", Config: locator, Input: locator})
	}
	return items, rows.Err()
}

func (service *Service) expandBrowserIdentities(ctx context.Context, conn *sql.Conn, scenarioID, cellID string) ([]resolvedItem, error) {
	rows, err := conn.QueryContext(ctx, `SELECT b.id,b.current_revision_id,g.id,g.profile_manifest_digest
		FROM browser_identities b JOIN browser_profile_generations g ON g.id=b.current_profile_generation_id
		WHERE b.current_profile_generation_id IS NOT NULL ORDER BY b.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []resolvedItem{}
	for rows.Next() {
		locator := &browserLocator{}
		if err := rows.Scan(&locator.BrowserIdentityID, &locator.IdentityRevisionID, &locator.CurrentGenerationID, &locator.CurrentInventoryDigest); err != nil {
			return nil, err
		}
		items = append(items, resolvedItem{ScenarioID: scenarioID, CellID: cellID, ObjectKind: "browser_identity", Browser: locator, Input: locator})
	}
	return items, rows.Err()
}

// lowestUnconfirmedDeploymentFence finds the oldest deployment verification
// operation whose physical cleanup fence is still unconfirmed. Its frozen
// identity binding becomes the browser.lintel-recovery locator.
func lowestUnconfirmedDeploymentFence(ctx context.Context, conn *sql.Conn) (*browserLocator, error) {
	locator := &browserLocator{}
	err := conn.QueryRowContext(ctx, `SELECT o.identity_id,o.identity_revision_id,o.profile_generation_id,g.profile_manifest_digest
		FROM browser_operations o
		JOIN browser_profile_generations g ON g.id=o.profile_generation_id
		WHERE o.kind='deployment_verification' AND o.start_dispatched_at IS NOT NULL AND o.stop_confirmed_at IS NULL
		ORDER BY o.id LIMIT 1`).
		Scan(&locator.BrowserIdentityID, &locator.IdentityRevisionID, &locator.CurrentGenerationID, &locator.CurrentInventoryDigest)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return locator, nil
}
