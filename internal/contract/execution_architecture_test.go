package contract

// Execution architecture enforcement (ADR-0006, docs/audit-design.md §3): a
// static progress gate that keeps the uncontrolled database surface from
// growing while the legacy services migrate onto the shared execution runner
// and audit writer. This file owns the scanner deliberately as test-only
// code — no production package carries a static analyzer.
//
// Enforced surfaces (bounded to relevant Go declarations):
//   - sql_open: direct sql.Open outside the sanctioned constructors.
//   - tx_control_sql: Exec/ExecContext of BEGIN/COMMIT/ROLLBACK/SAVEPOINT/
//     RELEASE statement strings.
//   - tx_method: direct .Commit()/.Rollback() calls.
//   - audit_insert: direct INSERT INTO audit_events, audit_event_targets,
//     audit_cleanup_batches or client_commands outside the owning packages.
//   - sql_write_exec: a provably raw handle (a *sql.DB / *sql.Conn / *sql.Tx
//     parameter or field, a pool connection, or an explicit DB() capability
//     export) issuing a literal INSERT/UPDATE/DELETE/REPLACE through
//     Exec/ExecContext outside the owning packages. Business statements on
//     the runner's guarded *execution.Tx / execution.Executor are the
//     sanctioned path and are never flagged; receivers whose type cannot be
//     proven raw are left to the runtime guards, so the baseline never
//     inflates with speculative entries.
//
// The exact current legacy baseline below lists every tolerated site with
// path + symbol signature + the implementation-plan stage that owns its
// migration (docs/auth-audit-implementation-plan.md). It is not a broad
// package allow: the set is compared exactly, it only shrinks as migration
// progresses, and full adoption cannot be declared while entries remain.
// Sanctioned owners never appear as baseline entries: internal/quoin/audit
// (the writer) and internal/quoin/execution (the runner, readonly helper)
// are the controlled implementations themselves.
//
// HTTP/API declarations (Huma) and runtime behavior are out of scope here;
// the runtime guards (transaction guard, mode=ro helper, audit whitelist)
// and the ops agent's API checks cover them.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// executionAdoptionComplete flipped after the combined adoption pass: every
// production write family executes through the shared runner, the pending
// business class is zero, and the baseline retains only exact, individually
// classified inventories — enumerated low-level schema/migration authorities,
// the centralized heartbeat-telemetry renewal, and the retired browser drain
// set (per-symbol, closure-tested against the retired slot boundary). The
// baseline stays exact and can only shrink; any new uncontrolled site fails
// the gate regardless of this flag.
const executionAdoptionComplete = true

// Rule names reported by the scanner.
const (
	ruleSQLWriteOpen = "sql_open"
	ruleTxControlSQL = "tx_control_sql"
	ruleTxMethod     = "tx_method"
	ruleAuditInsert  = "audit_insert"
	ruleSQLWriteExec = "sql_write_exec"
)

// architectureViolation is one detected site attributed to its enclosing
// declaration.
type architectureViolation struct {
	Rule   string
	Path   string
	Symbol string
	Line   int
}

func (v architectureViolation) key() string {
	return v.Rule + "\x00" + v.Path + "\x00" + v.Symbol
}

// scanArchitecture walks the production tree (internal/ + cmd/, tests and
// generated code excluded) and returns the sorted violation set, minus the
// sanctioned owner packages.
func scanArchitecture(t *testing.T, root string) []architectureViolation {
	t.Helper()
	var violations []architectureViolation
	seen := map[string]bool{}
	// Raw handle fields are collected per package directory first: a method
	// on *Service may live in a different file than the struct declaration,
	// and the `service.db` selector must classify identically everywhere.
	rawFieldsByPackage := collectRawFields(root)
	err := filepath.WalkDir(root, func(fullPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if fullPath == root {
				return nil
			}
			relDir, err := filepath.Rel(root, fullPath)
			if err != nil {
				return err
			}
			relDir = filepath.ToSlash(relDir)
			if !inScanScope(relDir + "/") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			return nil // test harnesses are not the migration surface
		}
		relPath, err := filepath.Rel(root, fullPath)
		if err != nil {
			return err
		}
		relPath = filepath.ToSlash(relPath)
		if !inScanScope(relPath) || sanctionedOwnerPath(relPath) {
			return nil
		}
		src, err := os.ReadFile(fullPath)
		if err != nil {
			return err
		}
		found, err := inspectArchitectureSource(relPath, src, rawFieldsByPackage[filepath.Dir(relPath)])
		if err != nil {
			return err
		}
		for _, violation := range found {
			key := violation.key()
			if !seen[key] {
				seen[key] = true
				violations = append(violations, violation)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan architecture: %v", err)
	}
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Path != violations[j].Path {
			return violations[i].Path < violations[j].Path
		}
		if violations[i].Symbol != violations[j].Symbol {
			return violations[i].Symbol < violations[j].Symbol
		}
		if violations[i].Rule != violations[j].Rule {
			return violations[i].Rule < violations[j].Rule
		}
		return violations[i].Line < violations[j].Line
	})
	return violations
}

// inScanScope bounds the scan to Quoin production code.
func inScanScope(relPath string) bool {
	switch {
	case strings.HasPrefix(relPath, "internal/gen"):
		return false // generated contract projections
	case strings.HasPrefix(relPath, "internal/quoin/testfixture/"):
		return false // shared test harness; every entry point requires *testing.T
	case strings.HasPrefix(relPath, "internal/"), strings.HasPrefix(relPath, "cmd/"):
		return true
	default:
		return false
	}
}

// sanctionedOwnerPath reports the packages that are the controlled
// implementations themselves: the audit writer and the execution runner.
func sanctionedOwnerPath(relPath string) bool {
	return strings.HasPrefix(relPath, "internal/quoin/execution/") ||
		strings.HasPrefix(relPath, "internal/quoin/audit/")
}

// collectRawFields walks the tree once and unions every struct field name
// that holds a raw database handle, keyed by package directory.
func collectRawFields(root string) map[string]map[string]bool {
	fields := map[string]map[string]bool{}
	_ = filepath.WalkDir(root, func(fullPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relPath, err := filepath.Rel(root, fullPath)
		if err != nil || !inScanScope(filepath.ToSlash(relPath)) || sanctionedOwnerPath(filepath.ToSlash(relPath)) {
			return nil
		}
		src, err := os.ReadFile(fullPath)
		if err != nil {
			return nil
		}
		fileSet := token.NewFileSet()
		parsed, parseErr := parser.ParseFile(fileSet, relPath, src, parser.SkipObjectResolution)
		if parseErr != nil {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(relPath))
		if fields[dir] == nil {
			fields[dir] = map[string]bool{}
		}
		for name := range rawHandleFields(parsed) {
			fields[dir][name] = true
		}
		return nil
	})
	return fields
}

// inspectArchitectureSource parses one Go file and reports violations
// attributed to enclosing declarations. Resolution is deliberately bounded:
// string literals, constant identifiers and literal-concatenation chains;
// anything dynamic is invisible to this gate on purpose.
func inspectArchitectureSource(relPath string, src []byte, rawFields map[string]bool) ([]architectureViolation, error) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, relPath, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	pkgName := parsed.Name.Name
	constants := map[string]string{}
	if rawFields == nil {
		rawFields = rawHandleFields(parsed)
	}
	for _, decl := range parsed.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			continue
		}
		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range valueSpec.Names {
				if i < len(valueSpec.Values) {
					if literal, ok := architectureLiteral(valueSpec.Values[i], constants); ok {
						constants[name.Name] = literal
					}
				}
			}
		}
	}
	var violations []architectureViolation
	inspectCallsWithHandles := func(symbol string, node ast.Node, readOnlyReceivers map[string]bool, handles map[string]handleKind, rawFields map[string]bool) {
		ast.Inspect(node, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			rule, line := classifyArchitectureCall(fileSet, call, constants)
			// A provably raw handle issuing a literal business write through
			// Exec/ExecContext is its own violation class even without any
			// transaction-control keyword around it.
			if rule == "" && isSelectorName(call, "Exec", "ExecContext") {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && receiverRaw(selector.X, handles, rawFields) {
					for _, argument := range call.Args {
						if literal, ok := architectureLiteral(argument, constants); ok {
							normalized := strings.ToUpper(strings.Join(strings.Fields(literal), " "))
							for _, verb := range []string{"INSERT", "UPDATE", "DELETE", "REPLACE"} {
								if strings.HasPrefix(normalized, verb) {
									rule = ruleSQLWriteExec
									break
								}
							}
						}
						if rule != "" {
							break
						}
					}
				}
			}
			// Commit/Rollback is exempt only on a receiver that provably
			// received an explicitly read-only BeginTx in the same function:
			// an unrelated read-write handle in a mixed function stays
			// flagged.
			if rule == ruleTxMethod && isReadOnlyReceiver(call, readOnlyReceivers) {
				return true
			}
			if rule != "" {
				violations = append(violations, architectureViolation{Rule: rule, Path: relPath, Symbol: symbol, Line: line})
			}
			return true
		})
	}
	for _, decl := range parsed.Decls {
		switch declaration := decl.(type) {
		case *ast.FuncDecl:
			handles := handleKinds(declaration)
			inspectCallsWithHandles(architectureFuncSymbol(pkgName, declaration), declaration, readOnlyTxReceivers(declaration), handles, rawFields)
		case *ast.GenDecl:
			for _, spec := range declaration.Specs {
				valueSpec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range valueSpec.Names {
					inspectCallsWithHandles(pkgName+"."+name.Name, valueSpec, nil, nil, rawFields)
				}
			}
		}
	}
	return violations, nil
}

// handleKind classifies one type expression's database capability.
type handleKind int

const (
	handleUnknown handleKind = iota
	handleRaw                // *sql.DB / *sql.Conn / *sql.Tx
	handleGuarded            // *execution.Tx / execution.Executor
)

// handleExpr maps a type expression onto its handle kind. execution.Executor
// is a pointer ALIAS of the sealed *execution.Tx (main's trust closure: the
// unexported marker makes raw handles unable to satisfy it), so both the
// alias and the explicit pointer form classify as guarded.
func handleExpr(expr ast.Expr) handleKind {
	selectorOf := func(node ast.Node) *ast.SelectorExpr {
		switch value := node.(type) {
		case *ast.SelectorExpr:
			return value
		case *ast.StarExpr:
			if selector, ok := value.X.(*ast.SelectorExpr); ok {
				return selector
			}
		}
		return nil
	}
	selector := selectorOf(expr)
	if selector == nil {
		return handleUnknown
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok {
		return handleUnknown
	}
	switch {
	case pkg.Name == "sql" && (selector.Sel.Name == "DB" || selector.Sel.Name == "Conn" || selector.Sel.Name == "Tx"):
		return handleRaw
	case pkg.Name == "execution" && (selector.Sel.Name == "Tx" || selector.Sel.Name == "Executor" || selector.Sel.Name == "Snapshot" || selector.Sel.Name == "Reader"):
		return handleGuarded
	}
	return handleUnknown
}

// rawHandleFields collects the struct field names that hold raw database
// handles anywhere in the file, so a `service.db` style selector can be
// classified without full type checking.
func rawHandleFields(parsed *ast.File) map[string]bool {
	fields := map[string]bool{}
	ast.Inspect(parsed, func(n ast.Node) bool {
		structType, ok := n.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range structType.Fields.List {
			if len(field.Names) == 0 || field.Type == nil {
				continue
			}
			if handleExpr(field.Type) == handleRaw {
				for _, name := range field.Names {
					fields[name.Name] = true
				}
			}
		}
		return true
	})
	return fields
}

// handleKinds resolves the database capability of every in-scope identifier of
// one declaration: its parameters (including nested function literals), the
// parameters of functions it declares, and one-hop alias assignments.
func handleKinds(declaration *ast.FuncDecl) map[string]handleKind {
	kinds := map[string]handleKind{}
	// Parameters of the declaration itself and of every nested literal.
	ast.Inspect(declaration, func(n ast.Node) bool {
		var funcType *ast.FuncType
		switch node := n.(type) {
		case *ast.FuncDecl:
			funcType = node.Type
		case *ast.FuncLit:
			funcType = node.Type
		}
		if funcType == nil {
			return true
		}
		if funcType.Params != nil {
			for _, param := range funcType.Params.List {
				kind := handleExpr(param.Type)
				if kind == handleUnknown {
					continue
				}
				for _, name := range param.Names {
					kinds[name.Name] = kind
				}
			}
		}
		return true
	})
	// One-hop alias and connection assignments inside the body.
	ast.Inspect(declaration, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return true
		}
		kind := handleUnknown
		switch rhs := assign.Rhs[0].(type) {
		case *ast.Ident:
			kind = kinds[rhs.Name]
		case *ast.CallExpr:
			if selector, ok := rhs.Fun.(*ast.SelectorExpr); ok && selector.Sel != nil {
				// Conn opens a raw pool connection; BeginTx on a raw handle
				// yields a raw *sql.Tx. Both stay raw down the alias chain,
				// including chained receivers like `store.db.Conn(ctx)`.
				if selector.Sel.Name == "Conn" || selector.Sel.Name == "BeginTx" {
					switch base := selector.X.(type) {
					case *ast.Ident:
						if kinds[base.Name] == handleRaw {
							kind = handleRaw
						}
					case *ast.SelectorExpr:
						if selectorFieldRaw(base) {
							kind = handleRaw
						}
					}
				}
			}
		case *ast.SelectorExpr:
			if sel := selectorFieldRaw(rhs); sel {
				kind = handleRaw
			}
		}
		if kind == handleUnknown {
			return true
		}
		for _, name := range assign.Lhs {
			if ident, ok := name.(*ast.Ident); ok && ident.Name != "_" {
				kinds[ident.Name] = kind
			}
		}
		return true
	})
	return kinds
}

// selectorFieldRaw reports whether the selector expression ends in a field
// name that provably holds a raw handle in this file.
func selectorFieldRaw(selector *ast.SelectorExpr) bool {
	return selector.Sel != nil && selector.Sel.Name == "db" || selector.Sel != nil && selector.Sel.Name == "conn"
}

// receiverRaw classifies the receiver of an Exec/ExecContext call. An
// explicit DB() capability export is raw by definition; otherwise the
// receiver's collected handle kind decides, and an unprovable receiver stays
// unflagged so the baseline cannot inflate speculatively.
func receiverRaw(receiver ast.Expr, handles map[string]handleKind, rawFields map[string]bool) bool {
	switch value := receiver.(type) {
	case *ast.Ident:
		return handles[value.Name] == handleRaw
	case *ast.SelectorExpr:
		if value.Sel != nil && rawFields[value.Sel.Name] {
			return true
		}
		if base, ok := value.X.(*ast.Ident); ok {
			return handles[base.Name] == handleRaw
		}
	case *ast.CallExpr:
		if selector, ok := value.Fun.(*ast.SelectorExpr); ok && selector.Sel != nil && selector.Sel.Name == "DB" {
			return true
		}
	case *ast.ParenExpr:
		return receiverRaw(value.X, handles, rawFields)
	}
	return false
}

// readOnlyTxReceivers returns the variable names that provably received an
// explicitly read-only transaction within the same declaration: a BeginTx
// whose options literal sets ReadOnly: true, or the execution.Reader
// BeginSnapshot factory (the sanctioned read-snapshot authority, whose
// transaction is read-only by construction). The exemption is receiver-bound:
// a mixed function holding a second, read-write handle keeps its Commit
// flagged.
func readOnlyTxReceivers(declaration *ast.FuncDecl) map[string]bool {
	receivers := map[string]bool{}
	ast.Inspect(declaration, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel == nil {
			return true
		}
		if selector.Sel.Name == "BeginSnapshot" {
			if len(assign.Lhs) > 0 {
				if ident, ok := assign.Lhs[0].(*ast.Ident); ok && ident.Name != "_" {
					receivers[ident.Name] = true
				}
			}
			return true
		}
		if selector.Sel.Name != "BeginTx" || len(call.Args) < 2 {
			return true
		}
		unary, ok := call.Args[1].(*ast.UnaryExpr)
		if !ok {
			return true
		}
		composite, ok := unary.X.(*ast.CompositeLit)
		if !ok {
			return true
		}
		readOnly := false
		for _, element := range composite.Elts {
			keyValue, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if ident, ok := keyValue.Key.(*ast.Ident); ok && ident.Name == "ReadOnly" {
				if value, ok := keyValue.Value.(*ast.Ident); ok && value.Name == "true" {
					readOnly = true
				}
			}
		}
		if !readOnly || len(assign.Lhs) == 0 {
			return true
		}
		if ident, ok := assign.Lhs[0].(*ast.Ident); ok && ident.Name != "_" {
			receivers[ident.Name] = true
		}
		return true
	})
	return receivers
}

// isReadOnlyReceiver reports whether the call is a Commit/Rollback on a
// receiver recorded as a read-only transaction variable.
func isReadOnlyReceiver(call *ast.CallExpr, receivers map[string]bool) bool {
	if len(receivers) == 0 {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel == nil {
		return false
	}
	ident, ok := selector.X.(*ast.Ident)
	return ok && receivers[ident.Name]
}

func architectureFuncSymbol(pkgName string, declaration *ast.FuncDecl) string {
	if declaration.Recv == nil || len(declaration.Recv.List) == 0 {
		return pkgName + "." + declaration.Name.Name
	}
	receiverType := declaration.Recv.List[0].Type
	if star, ok := receiverType.(*ast.StarExpr); ok {
		receiverType = star.X
	}
	if ident, ok := receiverType.(*ast.Ident); ok {
		return pkgName + ".(*" + ident.Name + ")." + declaration.Name.Name
	}
	return pkgName + "." + declaration.Name.Name
}

func classifyArchitectureCall(fileSet *token.FileSet, call *ast.CallExpr, constants map[string]string) (string, int) {
	line := fileSet.Position(call.Pos()).Line
	selector, isSelector := call.Fun.(*ast.SelectorExpr)
	if isSelector && selector.Sel != nil {
		if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == "sql" && selector.Sel.Name == "Open" {
			return ruleSQLWriteOpen, line
		}
		if method := selector.Sel.Name; method == "Commit" || method == "Rollback" {
			return ruleTxMethod, line
		}
	}
	// SQL-string rules apply only to database write calls; a literal that
	// happens to contain BEGIN/COMMIT in unrelated APIs must not trip them.
	if !isSelector || selector.Sel == nil || (selector.Sel.Name != "Exec" && selector.Sel.Name != "ExecContext") {
		return "", line
	}
	for _, argument := range call.Args {
		if sql, ok := architectureLiteral(argument, constants); ok {
			normalized := strings.ToUpper(strings.Join(strings.Fields(sql), " "))
			for _, verb := range []string{"BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE"} {
				if strings.HasPrefix(normalized, verb) {
					return ruleTxControlSQL, line
				}
			}
			for _, table := range []string{"AUDIT_EVENTS", "AUDIT_EVENT_TARGETS", "AUDIT_CLEANUP_BATCHES", "CLIENT_COMMANDS"} {
				if strings.Contains(normalized, "INSERT INTO "+table) {
					return ruleAuditInsert, line
				}
			}
		}
	}
	return "", line
}

// inspectArchitectureSourceFixture runs the scanner on a synthetic fixture
// with per-file raw field collection only.
func inspectArchitectureSourceFixture(relPath string, src []byte) ([]architectureViolation, error) {
	return inspectArchitectureSource(relPath, src, nil)
}

// isSelectorName reports whether the call targets one of the given method
// names on any receiver.
func isSelectorName(call *ast.CallExpr, names ...string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel == nil {
		return false
	}
	for _, name := range names {
		if selector.Sel.Name == name {
			return true
		}
	}
	return false
}

func architectureLiteral(expr ast.Expr, constants map[string]string) (string, bool) {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind == token.STRING {
			if unquoted, err := strconv.Unquote(value.Value); err == nil {
				return unquoted, true
			}
		}
	case *ast.ParenExpr:
		return architectureLiteral(value.X, constants)
	case *ast.Ident:
		resolved, ok := constants[value.Name]
		return resolved, ok
	case *ast.BinaryExpr:
		if value.Op != token.ADD {
			return "", false
		}
		left, leftOK := architectureLiteral(value.X, constants)
		right, rightOK := architectureLiteral(value.Y, constants)
		if leftOK && rightOK {
			return left + right, true
		}
	}
	return "", false
}

// executionBaselineEntry is one tolerated legacy site. Planned references the
// implementation-plan stage that owns the migration; unassigned entries fail
// the plan validation.
// Baseline classes. "business" entries are pending domain migrations and
// must reach zero before full adoption; "lowlevel" entries are the explicit,
// enumerated low-level exceptions where raw database control is the product
// itself (offline schema bootstrap and the migration authority). The
// low-level list is fixed and annotated — it is not "all legacy forever".
const (
	classBusiness  = "business"
	classLowLevel  = "lowlevel"
	classTelemetry = "telemetry"
	classRetired   = "retired"
)

type executionBaselineEntry struct {
	Path    string
	Symbol  string
	Rule    string
	Planned string
	Class   string
}

// executionPlanStages mirrors docs/auth-audit-implementation-plan.md; the
// baseline list only shrinks as these stages complete.
var executionPlanStages = map[string]string{
	"stage1": "1 数据契约与迁移",
	"stage4": "4 认证生命周期与唯一管理员",
	"stage5": "5 全项目准入与审计迁移",
	"stage9": "9 部署、恢复、一次切换",
}

// executionPlanStageByPath assigns the owning stage by location; the default
// is the project-wide migration stage.
var executionPlanStageByPath = []struct{ prefix, stage string }{
	{"internal/quoin/bootstrap", "stage1"},
	{"internal/quoin/upgrade", "stage1"},
	{"cmd/quoin/migrate", "stage1"},
	{"internal/quoin/auth", "stage4"},
	{"cmd/quoin/", "stage9"},
}

// retiredDrainSymbols is the exact, per-symbol inventory of the retired
// browser drain writes (ADR-0007): the browser plugin and the Lintel slot
// are retired, every normal route and the Lintel boundary refuse them, and
// the only remaining reachability is the recovery drain exercised through
// runner operations. Each symbol is classified individually — never a
// package exemption — and the close-entry tests owned by the browser
// retirement verify the route and slot boundaries, so a silent
// reactivation surfaces as a new uncontrolled site and fails the gate.
// Rule-reason annotations (browser retirement evidence): the drain
// residual below is reachable only through the retired Lintel Hello
// admission or unregistered websocket reconnects — both refuse on the
// retired slot boundary; HandleStopAck is Lintel-slot gated.
var retiredDrainSymbols = map[string]bool{
	"internal/quoin/browser/boot.go:browser.downgradeInterruptedExplorationClaims":                          true,
	"internal/quoin/browser/boot.go:browser.sealInterruptedExplorationActions":                              true,
	"internal/quoin/browser/reconnect.go:browser.(*Service).ExpireReconnect":                                true,
	"internal/quoin/browser/reconnect.go:browser.(*Service).ResumeReconnect":                                true,
	"internal/quoin/browser/stop.go:browser.(*Service).HandleStopAck":                                       true,
	"internal/quoin/app/browser_exploration.go:app.(*RuntimeService).closeRejectedExplorationStart":         true,
	"internal/quoin/app/browser_exploration.go:app.(*RuntimeService).handleBrowserExplorationActionResult":  true,
	"internal/quoin/app/browser_exploration.go:app.(*RuntimeService).handleBrowserExplorationTerminalClaim": true,
	"internal/quoin/app/browser_exploration.go:app.(*RuntimeService).handleExplorationCompletion":           true,
	"internal/quoin/app/browser_exploration.go:app.(*RuntimeService).persistTerminalExplorationResult":      true,
	"internal/quoin/browser/boot.go:browser.(*Service).InterruptForQuoinRestart":                            true,
	"internal/quoin/browser/boot.go:browser.(*Service).InterruptMissingPhysicalOperations":                  true,
	"internal/quoin/browser/boot.go:browser.(*Service).InterruptOldBootOperations":                          true,
	"internal/quoin/browser/completion.go:browser.(*Service).HandleCompletion":                              true,
	"internal/quoin/browser/inventory.go:browser.(*Service).ReconcileInventory":                             true,
	"internal/quoin/browser/publish.go:browser.(*Service).HandlePublishRejected":                            true,
	"internal/quoin/browser/publish.go:browser.(*Service).HandlePublishResult":                              true,
	"internal/quoin/browser/publish.go:browser.(*Service).HandlePublishUnauthenticated":                     true,
	"internal/quoin/browser/reconnect.go:browser.(*Service).AwaitReconnect":                                 true,
	"internal/quoin/browser/runtime.go:browser.(*Service).HandleStartAck":                                   true,
	"internal/quoin/browser/runtime.go:browser.(*Service).prepareDispatch":                                  true,
	"internal/quoin/browser/runtime.go:browser.(*Service).preparePublish":                                   true,
	"internal/quoin/browser/service.go:browser.(*Service).startManualLogin":                                 true,
	"internal/quoin/browser/service.go:browser.recordCommand":                                               true,
	"internal/quoin/browser/standalone.go:browser.(*Service).ConfigureStandalone":                           true,
}

// Only schema authorities qualify; ordinary maintenance commands do not.
func baselineClassForEntry(entry executionBaselineEntry) string {
	if retiredDrainSymbols[entry.Path+":"+entry.Symbol] {
		return classRetired
	}
	// The whole upgrade package is the offline schema migration authority:
	// its helpers issue raw multi-statement DDL/DML by nature (main's
	// canonical migration ownership), so their write surface is a mechanism,
	// not business state change.
	if strings.HasPrefix(entry.Path, "internal/quoin/upgrade/") {
		return classLowLevel
	}
	if entry.Path+":"+entry.Symbol == "internal/quoin/attempt/recovery.go:attempt.(*Service).RenewLeaseForBoot" {
		return classTelemetry
	}
	switch entry.Path + ":" + entry.Symbol {
	case "internal/quoin/bootstrap/database.go:bootstrap.OpenDatabase",
		"internal/quoin/bootstrap/database.go:bootstrap.OpenMigrationDatabase",
		"internal/quoin/bootstrap/database.go:bootstrap.PeekHasUsers",
		"internal/quoin/bootstrap/database.go:bootstrap.PeekSchemaState",
		"internal/quoin/bootstrap/database.go:bootstrap.initializeDatabase",
		"internal/quoin/maintenance/rebind.go:maintenance.openOfflineDatabase",
		"internal/quoin/maintenance/rebind.go:maintenance.openReadOnlyDatabase",
		"internal/quoin/recovery/recovery.go:recovery.normalizeStagedSQLite",
		"internal/quoin/upgrade/legacy.go:upgrade.migrateReleasedSchemaTransaction",
		"internal/quoin/upgrade/schemagate.go:upgrade.MigrateWithOptions",
		"internal/quoin/upgrade/schemagate.go:upgrade.finishReleasedMigrationOn":
		return classLowLevel
	default:
		return classBusiness
	}
}

// class resolves the baseline class; the zero value is the pending business
// class so an unannotated entry can never silently become a low-level
// exception.
func (e executionBaselineEntry) class() string {
	if e.Class == "" {
		return classBusiness
	}
	return e.Class
}

// pendingBusinessEntries counts the baseline entries that block full
// adoption.
func pendingBusinessEntries() int {
	count := 0
	for _, entry := range executionBaseline {
		if entry.class() == classBusiness {
			count++
		}
	}
	return count
}

// telemetryEntries are the enumerated high-frequency mechanism writes the
// audit design excepts from per-operation audit rows (heartbeats, lease
// renewals): durable, fenced, and deliberately unaudited per tick. The list
// is exact and annotated — it is not a general audit:false switch, and a
// genuine state transition must never be added here.
func (e executionBaselineEntry) isTelemetry() bool {
	return e.Class == classTelemetry
}

func executionPlanStageForPath(path string) string {
	for _, mapping := range executionPlanStageByPath {
		if strings.HasPrefix(path, mapping.prefix) {
			return mapping.stage
		}
	}
	return "stage5"
}

// executionBaseline is the exact, shrinking legacy tolerance list. Regenerate
// it with:
//
//	CONTRACT_EXECUTION_BASELINE=refresh go test ./internal/contract/ -run TestExecutionArchitectureBaseline
//
// then reassign plan stages that changed and commit the diff. Entries whose
// symbols disappear fail the test and must be removed — the list never
// grows.
var executionBaseline = []executionBaselineEntry{
	{Path: "internal/quoin/app/browser_exploration.go", Symbol: "app.(*RuntimeService).closeRejectedExplorationStart", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/app/browser_exploration.go", Symbol: "app.(*RuntimeService).handleBrowserExplorationActionResult", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/app/browser_exploration.go", Symbol: "app.(*RuntimeService).handleBrowserExplorationTerminalClaim", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/app/browser_exploration.go", Symbol: "app.(*RuntimeService).handleExplorationCompletion", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/app/browser_exploration.go", Symbol: "app.(*RuntimeService).persistTerminalExplorationResult", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/attempt/recovery.go", Symbol: "attempt.(*Service).RenewLeaseForBoot", Rule: "sql_write_exec", Planned: "stage5", Class: "telemetry"},
	{Path: "internal/quoin/bootstrap/database.go", Symbol: "bootstrap.OpenDatabase", Rule: "sql_open", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/bootstrap/database.go", Symbol: "bootstrap.OpenMigrationDatabase", Rule: "sql_open", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/bootstrap/database.go", Symbol: "bootstrap.PeekHasUsers", Rule: "sql_open", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/bootstrap/database.go", Symbol: "bootstrap.PeekSchemaState", Rule: "sql_open", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/bootstrap/database.go", Symbol: "bootstrap.initializeDatabase", Rule: "tx_control_sql", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/browser/boot.go", Symbol: "browser.(*Service).InterruptForQuoinRestart", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/boot.go", Symbol: "browser.(*Service).InterruptForQuoinRestart", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/boot.go", Symbol: "browser.(*Service).InterruptMissingPhysicalOperations", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/boot.go", Symbol: "browser.(*Service).InterruptMissingPhysicalOperations", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/boot.go", Symbol: "browser.(*Service).InterruptOldBootOperations", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/boot.go", Symbol: "browser.(*Service).InterruptOldBootOperations", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/boot.go", Symbol: "browser.downgradeInterruptedExplorationClaims", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/boot.go", Symbol: "browser.sealInterruptedExplorationActions", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/completion.go", Symbol: "browser.(*Service).HandleCompletion", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/completion.go", Symbol: "browser.(*Service).HandleCompletion", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/inventory.go", Symbol: "browser.(*Service).ReconcileInventory", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/inventory.go", Symbol: "browser.(*Service).ReconcileInventory", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/publish.go", Symbol: "browser.(*Service).HandlePublishRejected", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/publish.go", Symbol: "browser.(*Service).HandlePublishRejected", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/publish.go", Symbol: "browser.(*Service).HandlePublishResult", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/publish.go", Symbol: "browser.(*Service).HandlePublishResult", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/publish.go", Symbol: "browser.(*Service).HandlePublishUnauthenticated", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/publish.go", Symbol: "browser.(*Service).HandlePublishUnauthenticated", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/reconnect.go", Symbol: "browser.(*Service).AwaitReconnect", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/reconnect.go", Symbol: "browser.(*Service).AwaitReconnect", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/reconnect.go", Symbol: "browser.(*Service).ExpireReconnect", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/reconnect.go", Symbol: "browser.(*Service).ResumeReconnect", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/runtime.go", Symbol: "browser.(*Service).HandleStartAck", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/runtime.go", Symbol: "browser.(*Service).HandleStartAck", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/runtime.go", Symbol: "browser.(*Service).prepareDispatch", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/runtime.go", Symbol: "browser.(*Service).prepareDispatch", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/runtime.go", Symbol: "browser.(*Service).preparePublish", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/service.go", Symbol: "browser.(*Service).startManualLogin", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/service.go", Symbol: "browser.(*Service).startManualLogin", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/service.go", Symbol: "browser.recordCommand", Rule: "audit_insert", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/standalone.go", Symbol: "browser.(*Service).ConfigureStandalone", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/standalone.go", Symbol: "browser.(*Service).ConfigureStandalone", Rule: "tx_control_sql", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/browser/stop.go", Symbol: "browser.(*Service).HandleStopAck", Rule: "sql_write_exec", Planned: "stage5", Class: "retired"},
	{Path: "internal/quoin/maintenance/rebind.go", Symbol: "maintenance.openOfflineDatabase", Rule: "sql_open", Planned: "stage5", Class: "lowlevel"},
	{Path: "internal/quoin/maintenance/rebind.go", Symbol: "maintenance.openReadOnlyDatabase", Rule: "sql_open", Planned: "stage5", Class: "lowlevel"},
	{Path: "internal/quoin/recovery/recovery.go", Symbol: "recovery.normalizeStagedSQLite", Rule: "sql_open", Planned: "stage5", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/authaudit.go", Symbol: "upgrade.migrateAuthAuditOn", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/authaudit.go", Symbol: "upgrade.normalizeAdminTopologyOn", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/authaudit.go", Symbol: "upgrade.renameRetainedAdminLogin", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/authaudit.go", Symbol: "upgrade.seedAuditRetentionSingletons", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/authsimplification.go", Symbol: "upgrade.migrateAuthSimplificationOn", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/inspectionfreeze.go", Symbol: "upgrade.migrateInspectionFreezeOn", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/alertviewattribution.go", Symbol: "upgrade.migrateAlertViewAttributionOn", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/legacy.go", Symbol: "upgrade.cutoverCurrentLegacyConfiguration", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/legacy.go", Symbol: "upgrade.insertLegacySuccessorProjections", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/legacy.go", Symbol: "upgrade.migrateDeclarationCutoverOn", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/legacy.go", Symbol: "upgrade.migrateDirectInvestigationMetricsOn", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/legacy.go", Symbol: "upgrade.migrateLegacyMetricsBusinessOn", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/legacy.go", Symbol: "upgrade.migratePluginRegistryOn", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/legacy.go", Symbol: "upgrade.migrateReleasedSchemaTransaction", Rule: "tx_control_sql", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/legacy.go", Symbol: "upgrade.preserveSQLiteSequence", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/legacy.go", Symbol: "upgrade.rebuildCanonicalSchema", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/legacy.go", Symbol: "upgrade.retireLegacyRefreshAttempts", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/schemagate.go", Symbol: "upgrade.MigrateWithOptions", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/schemagate.go", Symbol: "upgrade.MigrateWithOptions", Rule: "tx_control_sql", Planned: "stage1", Class: "lowlevel"},
	{Path: "internal/quoin/upgrade/schemagate.go", Symbol: "upgrade.finishReleasedMigrationOn", Rule: "sql_write_exec", Planned: "stage1", Class: "lowlevel"},
}

// TestExecutionArchitectureBaseline compares the exact current violation set
// against the baseline in both directions: new uncontrolled sites fail, and
// stale entries (already migrated symbols) fail too so the list can only
// shrink.
func TestExecutionArchitectureBaseline(t *testing.T) {
	root := filepath.Join("..", "..")
	actual := scanArchitecture(t, root)
	actualKeys := map[string]architectureViolation{}
	for _, violation := range actual {
		actualKeys[violation.key()] = violation
	}
	baselineKeys := map[string]executionBaselineEntry{}
	for _, entry := range executionBaseline {
		if _, ok := executionPlanStages[entry.Planned]; !ok {
			t.Errorf("baseline entry %s %s references unknown plan stage %q", entry.Path, entry.Symbol, entry.Planned)
			continue
		}
		if expected := executionPlanStageForPath(entry.Path); entry.Planned != expected {
			t.Errorf("baseline entry %s %s claims %q but its location maps to %q", entry.Path, entry.Symbol, entry.Planned, expected)
		}
		if expected := baselineClassForEntry(entry); entry.class() != expected {
			t.Errorf("baseline entry %s %s claims class %q but its location maps to %q", entry.Path, entry.Symbol, entry.Class, expected)
		}
		baselineKeys[entry.Rule+"\x00"+entry.Path+"\x00"+entry.Symbol] = entry
	}

	var newSites, staleEntries []string
	for key, violation := range actualKeys {
		if _, ok := baselineKeys[key]; !ok {
			newSites = append(newSites, fmt.Sprintf("%s %s %s:%d", violation.Rule, violation.Symbol, violation.Path, violation.Line))
		}
	}
	for key, entry := range baselineKeys {
		if _, ok := actualKeys[key]; !ok {
			staleEntries = append(staleEntries, fmt.Sprintf("%s %s %s (planned %s)", entry.Rule, entry.Symbol, entry.Path, entry.Planned))
		}
	}
	sort.Strings(newSites)
	sort.Strings(staleEntries)

	if os.Getenv("CONTRACT_EXECUTION_BASELINE") == "refresh" {
		t.Logf("current violation set (%d sites) as baseline literal:", len(actual))
		fmt.Printf("var executionBaseline = []executionBaselineEntry{\n")
		for _, violation := range actual {
			fmt.Printf("\t{Path: %q, Symbol: %q, Rule: %q, Planned: %q},\n",
				violation.Path, violation.Symbol, violation.Rule, executionPlanStageForPath(violation.Path))
		}
		fmt.Printf("}\n")
		t.Skip("baseline refreshed: paste the printed literal into executionBaseline")
	}

	if len(newSites) > 0 {
		t.Errorf("%d uncontrolled site(s) not in the baseline — route them through the execution runner/audit writer, or, if they are sanctioned legacy, regenerate the baseline and assign the owning plan stage:\n%s",
			len(newSites), strings.Join(newSites, "\n"))
	}
	if len(staleEntries) > 0 {
		t.Errorf("%d baseline entr(y/ies) no longer match — the migration happened, remove them (the list only shrinks):\n%s",
			len(staleEntries), strings.Join(staleEntries, "\n"))
	}
	if len(newSites) == 0 && len(staleEntries) == 0 && len(executionBaseline) == 0 {
		t.Log("legacy baseline is empty: full adoption achieved")
	}
}

// TestExecutionArchitectureAdoptionGate keeps the completion claim honest:
// full adoption cannot be declared while legacy baseline entries remain.
func TestExecutionArchitectureAdoptionGate(t *testing.T) {
	pending := pendingBusinessEntries()
	if executionAdoptionComplete && pending != 0 {
		t.Fatalf("executionAdoptionComplete is declared with %d pending business baseline entries remaining; complete the migrations and empty the business class first (only the enumerated low-level exceptions may remain)", pending)
	}
	telemetry, retired := 0, 0
	for _, entry := range executionBaseline {
		switch {
		case entry.isTelemetry():
			telemetry++
		case entry.class() == classRetired:
			retired++
		}
	}
	if !executionAdoptionComplete {
		t.Logf("progress gate active: %d pending business site symbol(s) tolerated, shrinking as plan stages complete; %d enumerated low-level exceptions; %d telemetry exceptions; %d retired drain exceptions (exact per-symbol, closure-tested)", pending, len(executionBaseline)-pending-telemetry-retired, telemetry, retired)
	}
}

// TestExecutionArchitectureSnippetFixtures feeds deliberately violating and
// clean snippets through the scanner so the rules themselves stay tested
// even while the repository baseline is green.
func TestExecutionArchitectureSnippetFixtures(t *testing.T) {
	violating := []struct {
		name   string
		source string
		symbol string
		rule   string
	}{
		{
			name: "direct sql open",
			source: `package demo
import "database/sql"
func OpenDatabase(dsn string) (*sql.DB, error) {
	return sql.Open("sqlite", dsn)
}`,
			symbol: "demo.OpenDatabase",
			rule:   ruleSQLWriteOpen,
		},
		{
			name: "method commit call",
			source: `package demo
import "database/sql"
type Store struct{ tx *sql.Tx }
func (s *Store) Save() error {
	return s.tx.Commit()
}`,
			symbol: "demo.(*Store).Save",
			rule:   ruleTxMethod,
		},
		{
			name: "exec commit statement",
			source: `package demo
import "database/sql"
func Finish(conn *sql.Conn) error {
	_, err := conn.ExecContext(nil, "COMMIT")
	return err
}`,
			symbol: "demo.Finish",
			rule:   ruleTxControlSQL,
		},
		{
			name: "multiline rollback statement",
			source: `package demo
import "database/sql"
func Undo(conn *sql.Conn) error {
	_, err := conn.ExecContext(nil, "ROLLBACK TO SAVEPOINT work")
	return err
}`,
			symbol: "demo.Undo",
			rule:   ruleTxControlSQL,
		},
		{
			name: "constant begin statement",
			source: `package demo
import "database/sql"
const beginStmt = "BEGIN IMMEDIATE"
func Start(conn *sql.Conn) error {
	_, err := conn.Exec(beginStmt)
	return err
}`,
			symbol: "demo.Start",
			rule:   ruleTxControlSQL,
		},
		{
			name: "direct audit insert",
			source: `package demo
import "github.com/Suknna/quoin/internal/quoin/execution"
func Audit(conn *execution.Tx) error {
	_, err := conn.Exec("INSERT INTO audit_events(actor_id) VALUES(1)")
	return err
}`,
			symbol: "demo.Audit",
			rule:   ruleAuditInsert,
		},
		{
			name: "concatenated ledger insert",
			source: `package demo
import "github.com/Suknna/quoin/internal/quoin/execution"
func Ledger(conn *execution.Tx) error {
	_, err := conn.Exec("INSERT INTO " + "client_commands(principal_id) VALUES(1)")
	return err
}`,
			symbol: "demo.Ledger",
			rule:   ruleAuditInsert,
		},
		{
			name: "raw pool field issuing a business update is flagged",
			source: `package demo
import (
	"database/sql"
	"context"
)

type Service struct{ db *sql.DB }

func (s *Service) Touch(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "UPDATE quoin_items SET name='x' WHERE id=1")
	return err
}`,
			symbol: "demo.(*Service).Touch",
			rule:   ruleSQLWriteExec,
		},
		{
			name: "chained field pool connection issuing a business update is flagged",
			source: `package demo
import (
	"database/sql"
	"context"
)

type Store struct{ db *sql.DB }

func (s *Store) Reject(ctx context.Context) {
	conn, _ := s.db.Conn(ctx)
	defer conn.Close()
	conn.ExecContext(ctx, "UPDATE quoin_items SET name='x' WHERE id=1")
}`,
			symbol: "demo.(*Store).Reject",
			rule:   ruleSQLWriteExec,
		},
		{
			name: "DB capability export issuing a business insert is flagged",
			source: `package demo
import (
	"database/sql"
	"context"
)

type Service struct{ db *sql.DB }

func (s *Service) DB() *sql.DB { return s.db }

func (s *Service) Seed(ctx context.Context) error {
	_, err := s.DB().ExecContext(ctx, "INSERT INTO quoin_items(name) VALUES('x')")
	return err
}`,
			symbol: "demo.(*Service).Seed",
			rule:   ruleSQLWriteExec,
		},
	}
	clean := []struct {
		name   string
		source string
	}{
		{
			name: "business insert on the guarded runner tx is not flagged",
			source: `package demo
import "github.com/Suknna/quoin/internal/quoin/execution"
func Add(conn *execution.Tx) error {
	_, err := conn.Exec("INSERT INTO quoin_items(name) VALUES('x')")
	return err
}`,
		},
		{
			name: "reading audit rows is allowed",
			source: `package demo
import "database/sql"
func Count(db *sql.DB) (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM audit_events").Scan(&count)
	return count, err
}`,
		},
		{
			name: "read-only transaction commit is a snapshot read",
			source: `package demo
import "database/sql"
func Snapshot(db *sql.DB, fn func(q audit.Reader) error) error {
	tx, err := db.BeginTx(nil, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}`,
		},
		{
			name: "dynamic statements stay invisible to the static gate",
			source: `package demo
import "database/sql"
func RunStatement(conn *sql.Conn, statement string) error {
	_, err := conn.Exec(statement)
	return err
}`,
		},
	}

	for _, fixture := range violating {
		violations, err := inspectArchitectureSourceFixture("fixtures/"+fixture.name+".go", []byte(fixture.source))
		if err != nil {
			t.Fatalf("%s: parse: %v", fixture.name, err)
		}
		if len(violations) != 1 || violations[0].Rule != fixture.rule || violations[0].Symbol != fixture.symbol {
			t.Fatalf("%s: violations=%+v, want exactly one %s on %s", fixture.name, violations, fixture.rule, fixture.symbol)
		}
	}
	// Mixed handles: a function holding both a read-only snapshot receiver
	// and an unrelated raw read-write transaction keeps the read-write
	// Commit flagged while the read-only receiver stays exempt; the raw
	// UPDATE additionally trips the sql_write_exec class.
	mixed := `package demo
import "database/sql"
func Mixed(db *sql.DB, fn func(q audit.Reader) error) error {
	roTx, err := db.BeginTx(nil, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	if err := fn(roTx); err != nil {
		_ = roTx.Rollback()
		return err
	}
	tx, err := db.BeginTx(nil, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(nil, "UPDATE x SET y=1"); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return roTx.Commit()
}`
	mixedViolations, mixedErr := inspectArchitectureSourceFixture("fixtures/mixed.go", []byte(mixed))
	if mixedErr != nil {
		t.Fatal(mixedErr)
	}
	// Both the read-write Rollback and Commit are flagged as tx_method and
	// the raw UPDATE as sql_write_exec; the read-only receiver's calls are
	// exempt.
	if len(mixedViolations) != 3 {
		t.Fatalf("mixed-handle violations=%+v, want the two read-write tx_method calls plus one sql_write_exec", mixedViolations)
	}
	txMethod, writeExec := 0, 0
	for _, violation := range mixedViolations {
		if violation.Symbol != "demo.Mixed" {
			t.Fatalf("mixed-handle violation=%+v, want demo.Mixed only", violation)
		}
		switch violation.Rule {
		case ruleTxMethod:
			txMethod++
			if violation.Line != 17 && violation.Line != 20 {
				t.Fatalf("mixed-handle tx_method=%+v, want the read-write receiver lines", violation)
			}
		case ruleSQLWriteExec:
			writeExec++
			if violation.Line != 16 {
				t.Fatalf("mixed-handle sql_write_exec=%+v, want the raw UPDATE line", violation)
			}
		default:
			t.Fatalf("mixed-handle unexpected rule %q", violation.Rule)
		}
	}
	if txMethod != 2 || writeExec != 1 {
		t.Fatalf("mixed-handle tx_method=%d sql_write_exec=%d, want 2 and 1", txMethod, writeExec)
	}

	// A read-write transaction Commit remains a tx_method violation while the
	// read-only snapshot variant is allowed.
	readWrite := `package demo
import "database/sql"
func Write(db *sql.DB) error {
	tx, err := db.BeginTx(nil, nil)
	if err != nil {
		return err
	}
	return tx.Commit()
}`
	violations, err := inspectArchitectureSourceFixture("fixtures/readwrite.go", []byte(readWrite))
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 || violations[0].Rule != ruleTxMethod || violations[0].Symbol != "demo.Write" {
		t.Fatalf("read-write tx violations=%+v, want exactly one tx_method on demo.Write", violations)
	}

	for _, fixture := range clean {
		violations, err := inspectArchitectureSourceFixture("fixtures/"+fixture.name+".go", []byte(fixture.source))
		if err != nil {
			t.Fatalf("%s: parse: %v", fixture.name, err)
		}
		if len(violations) != 0 {
			t.Fatalf("%s: violations=%+v, want none", fixture.name, violations)
		}
	}
}
