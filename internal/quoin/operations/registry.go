package operations

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// AccessRegistry holds the fixed operation declarations of one surface.
// Construction fails on duplicates or malformed declarations, and
// ValidateSurface fails construction of the HTTP handler when any registered
// operation lacks its exact declaration (or a declaration drifted from the
// registered method/path). Nothing is ever inferred from method or path.
type AccessRegistry struct {
	decls map[string]Declaration
}

// NewAccessRegistry validates and indexes the declarations of one surface.
// The same declaration set can be shared by registries of different surfaces
// only through explicit slices; a registry is authoritative for what it was
// built with.
func NewAccessRegistry(declarations ...Declaration) (*AccessRegistry, error) {
	registry := &AccessRegistry{decls: make(map[string]Declaration, len(declarations))}
	for _, declaration := range declarations {
		if err := declaration.validate(); err != nil {
			return nil, err
		}
		if _, exists := registry.decls[declaration.ID]; exists {
			return nil, fmt.Errorf("operations: declaration %q is registered twice", declaration.ID)
		}
		registry.decls[declaration.ID] = declaration
	}
	return registry, nil
}

// Lookup returns the declaration with the exact operation id.
func (r *AccessRegistry) Lookup(id string) (Declaration, bool) {
	declaration, ok := r.decls[id]
	return declaration, ok
}

// Len returns the number of declarations, including planned ones.
func (r *AccessRegistry) Len() int { return len(r.decls) }

// forEach iterates every declaration in undefined order.
func (r *AccessRegistry) forEach(fn func(Declaration)) {
	for _, declaration := range r.decls {
		fn(declaration)
	}
}

// Declarations returns every declaration in undefined order.
func (r *AccessRegistry) Declarations() []Declaration {
	declarations := make([]Declaration, 0, len(r.decls))
	for _, declaration := range r.decls {
		declarations = append(declarations, declaration)
	}
	return declarations
}

// RemainingPlanned lists the ids still marked planned. Planned is a transient
// state, never a final escape hatch: adoption completes only when every
// desired operation is registered and this list is asserted empty.
func (r *AccessRegistry) RemainingPlanned() []string {
	var ids []string
	r.forEach(func(declaration Declaration) {
		if declaration.Planned {
			ids = append(ids, declaration.ID)
		}
	})
	sort.Strings(ids)
	return ids
}

// ValidateSurface cross-checks one built Huma API against the registry. It
// fails when any registered operation has no exact (id, method, path)
// declaration, when a declaration drifted from its registered route, or when
// a non-planned declaration is missing from the surface. All violations are
// reported together so one construction round fixes everything.
func (r *AccessRegistry) ValidateSurface(api huma.API) error {
	registered := map[string]int{}
	var problems []string
	for path, item := range api.OpenAPI().Paths {
		if item == nil {
			continue
		}
		for method, operation := range map[string]*huma.Operation{
			http.MethodGet:     item.Get,
			http.MethodPut:     item.Put,
			http.MethodPost:    item.Post,
			http.MethodDelete:  item.Delete,
			http.MethodOptions: item.Options,
			http.MethodHead:    item.Head,
			http.MethodPatch:   item.Patch,
			http.MethodTrace:   item.Trace,
		} {
			if operation == nil {
				continue
			}
			registered[operation.OperationID]++
			declaration, declared := r.decls[operation.OperationID]
			if !declared {
				problems = append(problems, fmt.Sprintf(
					"operation %q registered as %s %s has no access declaration; add the exact declaration, never infer it from the route",
					operation.OperationID, method, path))
				continue
			}
			if declaration.Method != method || declaration.Path != path {
				problems = append(problems, fmt.Sprintf(
					"operation %q is registered as %s %s but declared as %s %s",
					operation.OperationID, method, path, declaration.Method, declaration.Path))
			}
		}
	}
	for _, declaration := range r.decls {
		if declaration.Planned || declaration.Raw {
			// Raw wrappers are wired explicitly through Admission.Wrap (which
			// fails for unknown ids); they never appear in the Huma document.
			continue
		}
		count := registered[declaration.ID]
		if count == 0 {
			problems = append(problems, fmt.Sprintf(
				"declaration %q (%s %s) is not registered on this surface", declaration.ID, declaration.Method, declaration.Path))
		} else if count > 1 {
			problems = append(problems, fmt.Sprintf(
				"declaration %q is registered %d times on this surface", declaration.ID, count))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("operations: access registration is incomplete:\n\t%s", strings.Join(problems, "\n\t"))
}
