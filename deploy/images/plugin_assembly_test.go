package images_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A new inbound plugin must be linked into both compiled hosts. This catches
// forgotten blank imports at build time; runtime deployment skew still needs
// a versioned handshake and cannot be inferred from a repository test.
func TestQuoinAndStelePluginAssembliesMatch(t *testing.T) {
	imports := func(component string) []string {
		t.Helper()
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "cmd", component, "main.go"), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		var plugins []string
		for _, entry := range file.Imports {
			path, err := strconv.Unquote(entry.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(path, "/plugins/") {
				if entry.Name == nil || entry.Name.Name != "_" {
					t.Fatalf("%s imports plugin %s without blank-import registration", component, path)
				}
				plugins = append(plugins, path)
			}
		}
		slices.Sort(plugins)
		return plugins
	}
	quoin, stele := imports("quoin"), imports("stele")
	if len(quoin) == 0 || !slices.Equal(quoin, stele) {
		t.Fatalf("Quoin plugin assembly %v differs from Stele %v", quoin, stele)
	}
}
