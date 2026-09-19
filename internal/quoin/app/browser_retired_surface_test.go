package app

// Structural closure pins for the retired browser surface (ADR-0007). The
// retained browser writers fall into two classes and this file pins the
// machine-checkable facts that keep BOTH classes from becoming live again:
//
//  1. The Lintel slot never maps onto the runtime authority, so every
//     Lintel-frame browser handler (start/stop/completion/publish acks,
//     interruption recovery) can never receive a frame.
//  2. The normal HTTP surface never declares or registers the retired
//     browser routes: identity authoring, manual login, publish, and the
//     noVNC WebSocket stay absent, and the one retained write command
//     (cancelStandaloneBrowserOperation, the Upgrade-drain allowlist entry)
//     exists only on the maintenance surface.
//
// If any pin fails because the closure was deliberately removed, the retired
// writers it protected stop being retired history and must migrate onto the
// execution runner before the surface goes live.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

func TestLintelSlotIsRejectedBeforeAnyBrowserHandler(t *testing.T) {
	if slot := (&RuntimeService{}).slotName(runtimev1.RuntimeSlot_RUNTIME_SLOT_LINTEL); slot != "" {
		t.Fatalf("the retired Lintel slot must not map onto the runtime authority, got %q", slot)
	}
	if slot := (&RuntimeService{}).slotName(runtimev1.RuntimeSlot_RUNTIME_SLOT_PLINTH); slot != qruntime.SlotPlinth {
		t.Fatalf("the Plinth slot must keep mapping: %q", slot)
	}
	// The registration-era Register RPC is retired (ADR-0009); the generated
	// service interface must no longer carry it at all.
	var probe interface {
		Register(interface{}, interface{}) (interface{}, error)
	}
	if _, ok := any(&RuntimeService{Slots: qruntime.NewService()}).(interface {
		Register(interface{}, interface{}) (interface{}, error)
	}); ok {
		_ = probe
		t.Fatal("RuntimeService must not implement a Register RPC surface")
	}
	// The only remaining lintel ingress is the Connect identity fence: a
	// CN=lintel client certificate is never issued by the deployment CA, so
	// the browser tunnel stays unreachable.
	ctx := context.Background()
	if requireComponentIdentity(ctx, qruntime.SlotLintel) {
		t.Fatal("an unauthenticated context must never satisfy the lintel identity fence")
	}
}

// TestNormalSurfaceDeclaresNoBrowserOperations pins the declaration-side
// closure: the validated normal access registry contains no browser
// operation at all. Re-registering a retired browser route on the normal
// surface requires declaring it here first, so this fails before the route
// can serve traffic.
func TestNormalSurfaceDeclaresNoBrowserOperations(t *testing.T) {
	registry, err := NormalAccessRegistry()
	if err != nil {
		t.Fatalf("normal access registry: %v", err)
	}
	for _, declaration := range registry.Declarations() {
		if strings.Contains(strings.ToLower(declaration.ID), "browser") || strings.Contains(strings.ToLower(declaration.Path), "browser") {
			t.Errorf("normal surface declares retired browser operation %q at %s %s; the browser business is retired and must stay on no normal route", declaration.ID, declaration.Method, declaration.Path)
		}
	}
}

// TestStandaloneBrowserCancelIsOnlyTheUpgradeDrain pins the one retained
// browser write command: cancelStandaloneBrowserOperation is declared, kept
// out of the normal surface by the maintenance-only mark, and preserved on
// the Upgrade drain allowlist. Losing any of the three facts either
// reactivates a retired normal route or silently drops the retained drain.
func TestStandaloneBrowserCancelIsOnlyTheUpgradeDrain(t *testing.T) {
	var declared int
	for _, declaration := range accessDeclarationTable() {
		if declaration.ID == "cancelStandaloneBrowserOperation" {
			declared++
			if declaration.Method != "POST" || declaration.Path != "/api/v1/browser-identities/{identityKey}/operations/{operationId}/cancel" {
				t.Fatalf("retained drain route contract changed: %s %s", declaration.Method, declaration.Path)
			}
		}
	}
	if declared != 1 {
		t.Fatalf("cancelStandaloneBrowserOperation must be declared exactly once, found %d", declared)
	}
	if !maintenanceOnly["cancelStandaloneBrowserOperation"] {
		t.Fatal("cancelStandaloneBrowserOperation must stay maintenance-only; the retired normal surface never re-registers it")
	}
	upgrade := maintenanceReasonAllowlists["Upgrade"]
	found := false
	for _, id := range upgrade {
		if id == "cancelStandaloneBrowserOperation" {
			found = true
		}
	}
	if !found {
		t.Fatal("the Upgrade drain allowlist must keep cancelStandaloneBrowserOperation")
	}
}

// retiredBrowserRegistrars are the route installers of the retired browser
// surface. registerBrowserRoutes is itself a dormant wrapper: it has no
// callers and merely composes registerBrowserStandaloneRoutes plus the two
// retained history reads. That one nested composition is allowed; any other
// callsite of either installer, or any caller of the dormant wrapper,
// resurrects the surface and fails.
var retiredBrowserRegistrars = []string{
	"registerBrowserRoutes",
	"registerBrowserStandaloneRoutes",
	"registerBrowserWebSocket",
}

// dormantComposition is the single allowed edge: the dormant
// registerBrowserRoutes wrapper composing the standalone installer it once
// served. The wrapper has no callers of its own, so the edge is dead code,
// not a live route.
const (
	dormantCompositionFrom = "registerBrowserRoutes"
	dormantCompositionCall = "registerBrowserStandaloneRoutes"
)

// TestRetiredBrowserRegistrarsAreNeverCalled scans the production tree and
// fails on any callsite of the retired browser route installers outside the
// single dormant composition edge, so the surface cannot silently register
// behind a future refactor.
func TestRetiredBrowserRegistrarsAreNeverCalled(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "node_modules", ".git", "testdata", "test", ".artifacts", ".local-lab", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, source, 0)
		if err != nil {
			return err
		}
		scanRegistrars(t, fileSet, path, file)
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
}

// scanRegistrars reports every call to a retired browser route installer in
// one file, attributed to its enclosing declaration. Package-level
// declarations outside any function count as root calls.
func scanRegistrars(t *testing.T, fileSet *token.FileSet, path string, file *ast.File) {
	check := func(enclosing string, call *ast.CallExpr) {
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		for _, registrar := range retiredBrowserRegistrars {
			if name != registrar {
				continue
			}
			if registrar == dormantCompositionCall && enclosing == dormantCompositionFrom {
				continue
			}
			t.Errorf("%s:%d calls retired browser route installer %q (enclosing %q)", filepath.ToSlash(path), fileSet.Position(call.Pos()).Line, registrar, enclosing)
		}
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					check(fn.Name.Name, call)
				}
				return true
			})
			continue
		}
		ast.Inspect(decl, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				check("", call)
			}
			return true
		})
	}
}
