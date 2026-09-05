// Package observation owns the typed 24-cell UI observation matrix of the
// frozen verification catalog (VERIFY-OBSERVATION-001..003). The two frozen
// scenarios ui.automated (CI browser automation) and ui.manual-observation
// (typed human observation) share one fixed cell matrix: three browser/arch
// subjects × four viewports × two motion modes. This package derives the
// matrix, the observation forms and the typed recording/evaluation rules from
// the catalog — never a second copy of the required cells — and binds every
// cell to the exact browser artifact digest, build and observer identity
// before any observation counts.
package observation

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Suknna/quoin/internal/verification/catalog"
)

// The two frozen scenarios this package serves. Both must exist in the
// catalog with the same 24 cell IDs.
const (
	ScenarioAutomated   = "ui.automated"
	ScenarioObservation = "ui.manual-observation"
)

// Browser subject vocabulary frozen by the catalog cell parameters
// (`browser_artifact`). playwright_chromium resolves per architecture from
// the release input lock; branded_chrome resolves once per qualification
// invocation on linux/amd64 and is frozen with its digest.
const (
	SubjectPlaywrightChromium = "playwright_chromium"
	SubjectBrandedChrome      = "branded_chrome"
)

// Motion modes of the matrix.
const (
	MotionNormal  = "normal"
	MotionReduced = "reduced"
)

// FixedViewports is the frozen viewport width set (CSS px). Used only to
// cross-check the catalog matrix, never to generate cells independently.
var FixedViewports = []int{320, 768, 1024, 1440}

// subjectSlugs maps catalog cell-ID prefixes to parameter vocabulary.
var subjectSlugs = map[string]string{
	SubjectPlaywrightChromium: "playwright-chromium",
	SubjectBrandedChrome:      "branded-chrome",
}

// CellKey is the typed identity of one matrix cell.
type CellKey struct {
	BrowserSubject string
	Architecture   string // linux/amd64 | linux/arm64
	ViewportCSSPx  int
	MotionMode     string
}

// CellID renders the catalog's stable cell ID for the key
// (e.g. playwright-chromium-linux-amd64-320-normal).
func (key CellKey) CellID() string {
	return fmt.Sprintf("%s-%s-%d-%s", subjectSlugs[key.BrowserSubject], strings.ReplaceAll(key.Architecture, "/", "-"), key.ViewportCSSPx, key.MotionMode)
}

// parseCellID is the inverse of CellID; unknown vocabulary fails closed.
func parseCellID(id string) (CellKey, error) {
	for subject := range subjectSlugs {
		for _, viewport := range FixedViewports {
			for _, motion := range []string{MotionNormal, MotionReduced} {
				key := CellKey{subject, "", viewport, motion}
				for _, arch := range []string{"linux/amd64", "linux/arm64"} {
					key.Architecture = arch
					if key.CellID() == id {
						return key, nil
					}
				}
			}
		}
	}
	return CellKey{}, fmt.Errorf("cell id %q is outside the fixed browser/arch × viewport × motion vocabulary", id)
}

// Cell is one catalog cell projected with its typed key and closed
// assertion set.
type Cell struct {
	ID         string
	Key        CellKey
	Assertions []catalog.Assertion
}

// Matrix is the fixed cell set of one of the two UI scenarios, in catalog
// order. It is derived — never hand-maintained — so a catalog change can
// never leave a second required-cell list behind (VERIFY-AUTHORITY-001).
type Matrix struct {
	ScenarioID string
	Cells      []Cell
}

// Cell looks up one cell by its catalog ID.
func (matrix *Matrix) Cell(id string) *Cell {
	for index := range matrix.Cells {
		if matrix.Cells[index].ID == id {
			return &matrix.Cells[index]
		}
	}
	return nil
}

// MatrixOf derives the matrix of one scenario from the loaded catalog and
// proves the frozen invariants: exactly the three subjects × four viewports
// × two motion modes, IDs consistent with parameters and architecture, and
// a closed per-cell assertion set.
func MatrixOf(loaded *catalog.Catalog, scenarioID string) (*Matrix, error) {
	scenario := loaded.Scenario(scenarioID)
	if scenario == nil {
		return nil, fmt.Errorf("catalog scenario %q missing", scenarioID)
	}
	matrix := &Matrix{ScenarioID: scenarioID}
	seen := map[string]bool{}
	for _, raw := range scenario.Cells {
		key, err := parseCellID(raw.ID)
		if err != nil {
			return nil, err
		}
		if raw.Architecture != key.Architecture {
			return nil, fmt.Errorf("cell %s architecture %q contradicts its id", raw.ID, raw.Architecture)
		}
		if err := parametersMatchKey(raw.Parameters, key); err != nil {
			return nil, fmt.Errorf("cell %s: %w", raw.ID, err)
		}
		if len(raw.Assertions) == 0 {
			return nil, fmt.Errorf("cell %s has no assertions", raw.ID)
		}
		if seen[raw.ID] {
			return nil, fmt.Errorf("cell %s declared twice", raw.ID)
		}
		seen[raw.ID] = true
		matrix.Cells = append(matrix.Cells, Cell{ID: raw.ID, Key: key, Assertions: raw.Assertions})
	}
	// The exact 24-cell shape, no more, no less (VERIFY-OBSERVATION-002:
	// the matrix must not gain a backend axis or extrapolate).
	if len(matrix.Cells) != 3*len(FixedViewports)*2 {
		return nil, fmt.Errorf("scenario %s carries %d cells, the frozen 3x4x2 matrix has %d", scenarioID, len(matrix.Cells), 3*len(FixedViewports)*2)
	}
	if violations := matrix.shapeViolations(); len(violations) > 0 {
		return nil, fmt.Errorf("scenario %s violates the frozen matrix shape: %s", scenarioID, strings.Join(violations, "; "))
	}
	return matrix, nil
}

// IDs lists the loaded matrix cell IDs in stable (sorted) order for
// closure reporting. The IDs come from the catalog document itself; this
// projection never generates or second-guesses the required-cell set.
func (matrix *Matrix) IDs() []string {
	ids := make([]string, 0, len(matrix.Cells))
	for _, cell := range matrix.Cells {
		ids = append(ids, cell.ID)
	}
	sort.Strings(ids)
	return ids
}

// parametersMatchKey proves the catalog parameters agree with the typed key
// so a drifted cell can never be recorded under a mismatched binding.
// shapeViolations proves the frozen matrix shape (VERIFY-OBSERVATION-002)
// against the loaded cells: three browser/arch subjects crossed with
// exactly the four frozen viewports and two motion modes, branded chrome
// never appearing on arm64. The catalog stays the required-cell
// authority; this check only proves it still carries the spec shape.
func (matrix *Matrix) shapeViolations() []string {
	var violations []string
	pairs := map[string]map[int]map[string]bool{}
	for _, cell := range matrix.Cells {
		if cell.Key.BrowserSubject == SubjectBrandedChrome && cell.Key.Architecture == "linux/arm64" {
			violations = append(violations, fmt.Sprintf("branded chrome cell %s exists outside its amd64-only vocabulary", cell.ID))
		}
		pair := cell.Key.BrowserSubject + "/" + cell.Key.Architecture
		if pairs[pair] == nil {
			pairs[pair] = map[int]map[string]bool{}
		}
		if pairs[pair][cell.Key.ViewportCSSPx] == nil {
			pairs[pair][cell.Key.ViewportCSSPx] = map[string]bool{}
		}
		pairs[pair][cell.Key.ViewportCSSPx][cell.Key.MotionMode] = true
	}
	if len(pairs) != 3 {
		violations = append(violations, fmt.Sprintf("matrix carries %d browser/arch subjects, the frozen shape has 3", len(pairs)))
	}
	for pair, perViewport := range pairs {
		if len(perViewport) != len(FixedViewports) {
			violations = append(violations, fmt.Sprintf("subject %s covers %d viewports, the frozen set has %d", pair, len(perViewport), len(FixedViewports)))
			continue
		}
		for _, viewport := range FixedViewports {
			if len(perViewport[viewport]) != 2 {
				violations = append(violations, fmt.Sprintf("subject %s viewport %d lacks one of the two motion modes", pair, viewport))
			}
		}
	}
	return violations
}

func parametersMatchKey(parameters map[string]any, key CellKey) error {
	subject, ok := parameters["browser_artifact"].(string)
	if !ok || subject != key.BrowserSubject {
		return fmt.Errorf("browser_artifact %v does not match cell vocabulary %q", parameters["browser_artifact"], key.BrowserSubject)
	}
	viewport, ok := parameters["viewport_width_css_px"].(int)
	if !ok || viewport != key.ViewportCSSPx {
		return fmt.Errorf("viewport_width_css_px %v does not match cell id width %d", parameters["viewport_width_css_px"], key.ViewportCSSPx)
	}
	motion, ok := parameters["motion_mode"].(string)
	if !ok || motion != key.MotionMode {
		return fmt.Errorf("motion_mode %v does not match cell vocabulary %q", parameters["motion_mode"], key.MotionMode)
	}
	return nil
}
