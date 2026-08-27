package attempt

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestBrowserToolRejectsArbitraryArguments(t *testing.T) {
	valid := []byte(`{"action":"fill","sessionId":"42","locator":{"kind":"role","role":"textbox","name":"Search"},"value":"secret"}`)
	if err := ValidateToolArguments(BrowserTool, valid); err != nil {
		t.Fatalf("valid closed browser action rejected: %v", err)
	}
	for _, body := range [][]byte{
		[]byte(`{"action":"evaluate","sessionId":"42","script":"alert(1)"}`),
		[]byte(`{"action":"fill","sessionId":"42","locator":{"kind":"css","selector":"#x"},"value":"x"}`),
		[]byte(`{"action":"open","businessSystemKey":"payments","extra":true}`),
	} {
		if err := ValidateToolArguments(BrowserTool, body); err == nil {
			t.Fatalf("unsupported browser shape was accepted: %s", body)
		}
	}
}

func TestBrowserToolIngressUsesCompleteFrozenRequestSchema(t *testing.T) {
	// These shapes pass the legacy hand-written structural checks but violate
	// limits/type constraints that exist only in the authoritative JSON Schema.
	for _, body := range [][]byte{
		[]byte(`{"action":"open","businessSystemKey":"` + string(bytes.Repeat([]byte("x"), 201)) + `"}`),
		[]byte(`{"action":"scroll","sessionId":"42","deltaX":100001,"deltaY":0}`),
		[]byte(`{"action":"fill","sessionId":"42","locator":{"kind":"role","role":"textbox"},"value":` + string(mustJSONText(t, bytes.Repeat([]byte("x"), 16385))) + `}`),
	} {
		if err := ValidateToolArguments(BrowserTool, body); err == nil {
			t.Fatalf("frozen request schema bypass accepted: %s", body)
		}
	}
}

func mustJSONText(t *testing.T, value []byte) []byte {
	t.Helper()
	body, err := json.Marshal(string(value))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestBrowserToolResultPayloadIsClosed(t *testing.T) {
	valid := []byte(`{"outcome":"session_closed","action":"close_session","sessionId":"42","error":{"code":"Cancelled","message":"cancelled","retryableInSession":false}}`)
	if err := ValidateToolResultPayload("browser_tool_result_v1", valid); err != nil {
		t.Fatalf("valid browser failure rejected: %v", err)
	}
	if err := ValidateToolResultPayload("browser_tool_result_v1", []byte(`{"outcome":"success","action":"read","sessionId":"42","rawCDP":"Network.getCookies"}`)); err == nil {
		t.Fatal("browser result payload bypass was accepted")
	}
	// This nested extra field was silently ignored by the former handwritten
	// validator. The frozen JSON Schema must reject it as well.
	if err := ValidateToolResultPayload("browser_tool_result_v1", []byte(`{"outcome":"session_closed","action":"close_session","sessionId":"42","error":{"code":"Cancelled","message":"cancelled","retryableInSession":false,"rawCDP":"Network.getCookies"}}`)); err == nil {
		t.Fatal("nested browser error payload bypass was accepted")
	}
}

func TestBrowserToolProviderSchemaFreezesLocatorShape(t *testing.T) {
	parameters := browserToolParameters()
	encoded, err := json.Marshal(parameters)
	if err != nil {
		t.Fatal(err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseRegexpEngine(browserToolECMARegexp)
	if err := compiler.AddResource("browser-provider.json", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("browser-provider.json")
	if err != nil {
		t.Fatal(err)
	}
	// The provider-visible ToolDef must accept every frozen request union member,
	// not merely the hand-written ingress subset. In particular targeted read is
	// a distinct oneOf member from the whole-session read.
	for _, body := range []string{
		`{"action":"read","sessionId":"42"}`,
		`{"action":"read","sessionId":"42","locator":{"kind":"role","role":"button"}}`,
		`{"action":"accept_dialog","sessionId":"42","promptText":"yes"}`,
	} {
		var value any
		if err := json.Unmarshal([]byte(body), &value); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err != nil {
			t.Fatalf("provider schema rejected frozen request %s: %v", body, err)
		}
		if err := ValidateToolArguments(BrowserTool, []byte(body)); err != nil {
			t.Fatalf("real frozen request schema rejected %s: %v", body, err)
		}
	}
	for _, body := range []string{
		`{"action":"click","sessionId":"42","locator":{"kind":"role","role":"button","unexpected":true}}`,
		`{"action":"click","sessionId":"42","locator":{"kind":"css","selector":"#x"}}`,
		`{"action":"click","sessionId":"42","locator":{"kind":"elementRef","ref":"e1"}}`,
		`{"action":"dismiss_dialog","sessionId":"42","promptText":"must-not-leak"}`,
	} {
		var value any
		if err := json.Unmarshal([]byte(body), &value); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err == nil {
			t.Fatalf("provider schema accepted non-frozen locator: %s", body)
		}
		if err := ValidateToolArguments(BrowserTool, []byte(body)); err == nil {
			t.Fatalf("real frozen request schema accepted %s", body)
		}
	}
}
