package config

import (
	"os"
	"strings"
	"testing"
)

func TestBusinessSystemGuideExampleCompiles(t *testing.T) {
	body, err := os.ReadFile("../../../docs/business-system-declaration-guide.md")
	if err != nil {
		t.Fatal(err)
	}
	_, example, found := strings.Cut(string(body), "```yaml\n")
	if !found {
		t.Fatal("guide has no YAML example")
	}
	example, _, found = strings.Cut(example, "\n```")
	if !found {
		t.Fatal("guide YAML example is not closed")
	}
	declaration := parseBusinessSystem(t, example)
	document, err := CompileBusinessSystemDocument(declaration)
	if err != nil {
		t.Fatal(err)
	}
	if document.SystemKey != "mall" || document.AlertSourceLabels["system_id"] != "local-inspection-demo" {
		t.Fatalf("guide must demonstrate independent business identity and inherited alert scope: %#v", document)
	}
}
