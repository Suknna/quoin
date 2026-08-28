package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/Suknna/quoin/internal/gen/contracts"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var (
	browserInputSchemaOnce sync.Once
	browserInputSchema     *jsonschema.Schema
	browserInputSchemaErr  error
)

func loadBrowserInputSchema() (*jsonschema.Schema, error) {
	browserInputSchemaOnce.Do(func() {
		document, err := jsonschema.UnmarshalJSON(strings.NewReader(string(contracts.BrowserExecutionSchema)))
		if err != nil {
			browserInputSchemaErr = fmt.Errorf("parse frozen browser execution schema: %w", err)
			return
		}
		compiler := jsonschema.NewCompiler()
		compiler.UseRegexpEngine(ecmaRegexp)
		const resource = "https://github.com/Suknna/quoin/schemas/browser-execution.schema.json"
		if err := compiler.AddResource(resource, document); err != nil {
			browserInputSchemaErr = fmt.Errorf("register frozen browser execution schema: %w", err)
			return
		}
		browserInputSchema, browserInputSchemaErr = compiler.Compile(resource)
	})
	return browserInputSchema, browserInputSchemaErr
}

// validateStartInput makes the frozen bytes and the typed control envelope a
// single capability. Lintel refuses an input before starting any process when
// its digest, schema kind, schema shape, or duplicated bindings disagree.
func validateStartInput(request *runtimev1.StartBrowserOperation) error {
	if request == nil || request.GetInput() == nil {
		return errors.New("browser operation input is required")
	}
	canonical := request.GetInput().GetCanonicalJson()
	digest := sha256.Sum256(canonical)
	if !bytes.Equal(digest[:], request.GetInput().GetContentDigest()) {
		return errors.New("browser operation input digest does not match canonical JSON")
	}
	var value map[string]any
	if err := json.Unmarshal(canonical, &value); err != nil {
		return fmt.Errorf("parse frozen browser input: %w", err)
	}
	schema, err := loadBrowserInputSchema()
	if err != nil {
		return err
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("validate frozen browser input: %w", err)
	}
	kind := ""
	switch request.GetKind() {
	case runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN:
		kind = "manual_login_v1"
	case runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_AUTHENTICATION_PROBE:
		kind = "authentication_probe_v1"
	case runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_EXPLORATION:
		kind = "exploration_v1"
	case runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY:
		kind = "inspection_collection_v1"
	default:
		return errors.New("unsupported browser operation kind")
	}
	if request.GetInput().GetSchemaKind() != kind || value["schemaKind"] != kind {
		return errors.New("browser operation schema kind does not match operation kind")
	}
	if !matchesID(value["operationId"], request.GetOperationId()) {
		return errors.New("browser operation ID does not match frozen input")
	}
	identity, ok := value["identity"].(map[string]any)
	if !ok || !matchesID(identity["identityId"], request.GetIdentityId()) || !matchesID(identity["identityRevisionId"], request.GetIdentityRevisionId()) {
		return errors.New("browser identity binding does not match frozen input")
	}
	profileID, hasProfileID := identity["profileGenerationId"]
	if request.GetProfileGenerationId() == 0 {
		if hasProfileID && profileID != nil {
			return errors.New("unexpected profile generation binding")
		}
	} else if !hasProfileID || !matchesID(profileID, request.GetProfileGenerationId()) {
		return errors.New("profile generation binding does not match frozen input")
	}
	bindingName := "probe"
	if kind == "manual_login_v1" || kind == "exploration_v1" || kind == "inspection_collection_v1" {
		bindingName = "authenticationProbe"
	}
	binding, ok := value[bindingName].(map[string]any)
	if !ok {
		return fmt.Errorf("browser journey binding is missing for %q", kind)
	}
	catalogBinding, ok := binding["catalog"].(map[string]any)
	if !ok || catalogBinding["digest"] != request.GetJourneyCatalogDigest() || catalogBinding["version"] != request.GetJourneyCatalogVersion() {
		return errors.New("browser catalog binding does not match control envelope")
	}
	return nil
}

// ecmaRegexp translates the frozen schema's ECMAScript Unicode escapes into
// RE2's equivalent form. The schema remains the authority; this is only the
// library adapter required to execute its regular expressions in Go.
func ecmaRegexp(pattern string) (jsonschema.Regexp, error) {
	translated := regexp.MustCompile(`\\u([0-9a-fA-F]{4})`).ReplaceAllStringFunc(pattern, func(token string) string {
		return `\x` + token[len(token)-2:]
	})
	return regexp.Compile(translated)
}

func matchesID(value any, want int64) bool {
	actual, ok := value.(float64)
	return ok && actual == float64(want)
}
