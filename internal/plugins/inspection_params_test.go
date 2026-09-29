package plugins_test

import (
	"errors"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/test/plugins/synthetic"
)

func TestInspectionTemplateParamsUseFrozenPluginSchema(t *testing.T) {
	registry := plugins.NewRegistry()
	if err := registry.Register(synthetic.Plugin()); err != nil {
		t.Fatal(err)
	}
	if err := registry.ValidateInspectionParams("synthetic-plugin", synthetic.TemplateID, synthetic.TemplateVersion, map[string]any{"expression": "echo"}); err != nil {
		t.Fatal(err)
	}
	for _, params := range []map[string]any{
		{}, {"expression": ""}, {"expression": "echo", "unknown": true},
	} {
		if err := registry.ValidateInspectionParams("synthetic-plugin", synthetic.TemplateID, synthetic.TemplateVersion, params); !errors.Is(err, plugins.ErrInvalidSettings) {
			t.Fatalf("params=%v err=%v, want declared-shape rejection", params, err)
		}
	}
}

func TestInspectionTemplateDeclarationRejectsInvalidSchemaOrTool(t *testing.T) {
	broken := synthetic.Plugin()
	broken.InspectionTemplates[0].ParamsSchema = map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"expression": map[string]any{"type": "not-a-json-schema-type"}}}
	if err := plugins.NewRegistry().Register(broken); !errors.Is(err, plugins.ErrInvalidPlugin) {
		t.Fatalf("invalid template schema err=%v", err)
	}
	broken = synthetic.Plugin()
	broken.DefaultInspectionPlan = &plugins.DefaultInspectionPlan{TemplateID: synthetic.TemplateID, Params: map[string]any{"undeclared": true}}
	if err := plugins.NewRegistry().Register(broken); !errors.Is(err, plugins.ErrInvalidPlugin) {
		t.Fatalf("invalid starter plan params err=%v", err)
	}
	broken = synthetic.Plugin()
	broken.InspectionTemplates[0].CollectToolName = "missing_internal_tool"
	registry := plugins.NewRegistry()
	if err := registry.Register(broken); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("missing internal tool must prevent frozen host assembly")
		}
	}()
	registry.Plugins() // freeze verifies the owner and internal tool flag.
}
