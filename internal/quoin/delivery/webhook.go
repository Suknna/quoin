package delivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrRedirectNotAllowed is wrapped into the error when the fixed destination
// answers with a redirect: redirects never carry the payload (which contains
// the verification code) elsewhere.
var ErrRedirectNotAllowed = errors.New("webhook redirects are not allowed")

type mappingField struct {
	path     []string // JSON path segments; nil for form keys
	key      string   // form key; "" for JSON fields
	template compiledTemplate
	numeric  bool // default expires_in_seconds entry is a JSON number
}

// WebhookSender posts each Message to one fixed HTTPS destination.
type WebhookSender struct {
	client      *http.Client
	policy      *destinationPolicy
	url         *url.URL
	encoding    string
	headers     [][2]string // static header name/value pairs, sorted
	secretRefs  [][2]string // header name -> secret reference, sorted
	fields      []mappingField
	metadata    map[string]bool
	successPath []string
	successWant any
	timeout     time.Duration
	maxResponse int64
	secrets     SecretFunc
}

// NewWebhookSender validates the configuration and compiles the mapping,
// success check and transport once. Errors never contain secret values.
func NewWebhookSender(config WebhookConfig, secrets SecretFunc) (*WebhookSender, error) {
	if secrets == nil {
		return nil, errors.New("webhook sender requires a secret resolver")
	}
	parsedURL, err := compileWebhookURL(config.URL)
	if err != nil {
		return nil, err
	}
	timeout, err := buildTimeout("webhook timeout", config.Timeout, defaultWebhookTimeout)
	if err != nil {
		return nil, err
	}
	maxResponse := config.MaxResponseBytes
	if maxResponse == 0 {
		maxResponse = defaultMaxResponseBytes
	}
	if maxResponse < minMaxResponseBytes || maxResponse > maxMaxResponseBytes {
		return nil, fmt.Errorf("webhook max response bytes must be between %d and %d", minMaxResponseBytes, maxMaxResponseBytes)
	}
	encoding := config.Encoding
	if encoding == "" {
		encoding = EncodingJSON
	}
	if encoding != EncodingJSON && encoding != EncodingForm {
		return nil, fmt.Errorf("webhook encoding %q must be %q or %q", encoding, EncodingJSON, EncodingForm)
	}
	metadata, err := compileMetadataKeys(config.MetadataKeys)
	if err != nil {
		return nil, fmt.Errorf("webhook metadata allowlist: %w", err)
	}
	headers, err := compileStaticHeaders(config.Headers)
	if err != nil {
		return nil, err
	}
	secretRefs, err := compileSecretHeaders(config.SecretHeaders)
	if err != nil {
		return nil, err
	}
	fields, err := compileMappingFields(encoding, config.JSONFields, config.FormFields, metadata)
	if err != nil {
		return nil, err
	}
	successPath, successWant, err := compileSuccessCheck(config.SuccessField, config.SuccessValue)
	if err != nil {
		return nil, err
	}
	policy, err := newDestinationPolicy(config.AllowPrivateCIDRs)
	if err != nil {
		return nil, fmt.Errorf("webhook allow CIDRs: %w", err)
	}
	tlsConfig, err := buildTLSClientConfig(parsedURL.Hostname(), config.RootCAPEM)
	if err != nil {
		return nil, fmt.Errorf("webhook TLS: %w", err)
	}
	sender := &WebhookSender{
		policy:      policy,
		url:         parsedURL,
		encoding:    encoding,
		headers:     headers,
		secretRefs:  secretRefs,
		fields:      fields,
		metadata:    metadata,
		successPath: successPath,
		successWant: successWant,
		timeout:     timeout,
		maxResponse: maxResponse,
		secrets:     secrets,
	}
	sender.client = &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// Fixed destination: a proxy would both move the target and
			// bypass dial-time validation.
			Proxy: nil,
			DialTLSContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return sender.dialTLS(ctx, network, address, tlsConfig.Clone())
			},
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: maxResponseHeaderBytes,
			TLSHandshakeTimeout:    timeout,
			ResponseHeaderTimeout:  timeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirectNotAllowed
		},
	}
	return sender, nil
}

// compileWebhookURL enforces the fixed-destination invariants: HTTPS only,
// no userinfo, no fragment, no braces (the target URL must stay static).
func compileWebhookURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("webhook URL is required")
	}
	if len(raw) > 2048 {
		return nil, errors.New("webhook URL exceeds 2048 bytes")
	}
	if strings.ContainsAny(raw, "{}") {
		return nil, errors.New("webhook URL must not contain braces; the destination is fixed and must not embed placeholders")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("webhook URL is not a valid URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("webhook URL scheme must be https, got %q", parsed.Scheme)
	}
	if parsed.User != nil {
		return nil, errors.New("webhook URL must not embed userinfo; use secret headers instead")
	}
	if parsed.Fragment != "" {
		return nil, errors.New("webhook URL must not contain a fragment")
	}
	if parsed.Hostname() == "" {
		return nil, errors.New("webhook URL has no host")
	}
	if port := parsed.Port(); port != "" {
		if _, err := net.LookupPort("tcp", port); err != nil {
			return nil, fmt.Errorf("webhook URL port is invalid: %w", err)
		}
	}
	return parsed, nil
}

func compileStaticHeaders(headers map[string]string) ([][2]string, error) {
	return compileHeaderMap(headers, false)
}

func compileSecretHeaders(headers map[string]string) ([][2]string, error) {
	return compileHeaderMap(headers, true)
}

func compileHeaderMap(headers map[string]string, references bool) ([][2]string, error) {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sortStrings(names)
	compiled := make([][2]string, 0, len(names))
	for _, name := range names {
		if err := validateHeaderName(name); err != nil {
			return nil, fmt.Errorf("webhook %s header: %w", headerKind(references), err)
		}
		if strings.EqualFold(name, "Host") || strings.EqualFold(name, "Content-Length") {
			return nil, fmt.Errorf("webhook %s header %q is controlled by the transport", headerKind(references), name)
		}
		value := headers[name]
		if references {
			if strings.TrimSpace(value) == "" || len(value) > maxIdentifierLength {
				return nil, fmt.Errorf("webhook secret header %q must carry a non-empty secret reference", name)
			}
		} else {
			if len(value) > maxHeaderValueLength {
				return nil, fmt.Errorf("webhook static header %q exceeds %d bytes", name, maxHeaderValueLength)
			}
		}
		compiled = append(compiled, [2]string{name, value})
	}
	return compiled, nil
}

func headerKind(references bool) string {
	if references {
		return "secret"
	}
	return "static"
}

func sortStrings(values []string) {
	sort.Strings(values)
}

// compileMappingFields compiles the restricted payload mapping; with no
// configured fields it falls back to the documented default payload shape.
func compileMappingFields(encoding string, jsonFields, formFields map[string]string, metadata map[string]bool) ([]mappingField, error) {
	if encoding == EncodingForm {
		if len(jsonFields) > 0 {
			return nil, errors.New("webhook JSON fields are configured while encoding is form")
		}
		return compileFormFields(formFields, metadata)
	}
	if len(formFields) > 0 {
		return nil, errors.New("webhook form fields are configured while encoding is json")
	}
	if len(jsonFields) == 0 {
		return []mappingField{
			{path: []string{"delivery_id"}, template: mustCompileTemplate("{delivery_id}", metadata)},
			{path: []string{"channel"}, template: mustCompileTemplate("{channel}", metadata)},
			{path: []string{"recipient"}, template: mustCompileTemplate("{recipient}", metadata)},
			{path: []string{"template"}, template: mustCompileTemplate("{template}", metadata)},
			{path: []string{"variables", "code"}, template: mustCompileTemplate("{code}", metadata)},
			{path: []string{"variables", "expires_in_seconds"}, template: mustCompileTemplate("{expires_in_seconds}", metadata), numeric: true},
		}, nil
	}
	return compileJSONFields(jsonFields, metadata)
}

func mustCompileTemplate(template string, metadata map[string]bool) compiledTemplate {
	compiled, err := compileTemplate("webhook default field", template, metadata)
	if err != nil {
		panic(err) // unreachable: fixed default templates only
	}
	return compiled
}

func compileFormFields(formFields map[string]string, metadata map[string]bool) ([]mappingField, error) {
	if len(formFields) > maxMappingFields {
		return nil, fmt.Errorf("webhook form mapping exceeds %d fields", maxMappingFields)
	}
	keys := make([]string, 0, len(formFields))
	for key := range formFields {
		keys = append(keys, key)
	}
	sortStrings(keys)
	fields := make([]mappingField, 0, len(keys))
	for _, key := range keys {
		if key == "" || len(key) > maxIdentifierLength {
			return nil, fmt.Errorf("webhook form field key %q is empty or longer than %d bytes", key, maxIdentifierLength)
		}
		if strings.ContainsAny(key, "{}") {
			return nil, fmt.Errorf("webhook form field key %q must not contain braces", key)
		}
		compiled, err := compileTemplate("webhook form field "+key, formFields[key], metadata)
		if err != nil {
			return nil, err
		}
		fields = append(fields, mappingField{key: key, template: compiled})
	}
	return fields, nil
}

func compileJSONFields(jsonFields map[string]string, metadata map[string]bool) ([]mappingField, error) {
	if len(jsonFields) > maxMappingFields {
		return nil, fmt.Errorf("webhook JSON mapping exceeds %d fields", maxMappingFields)
	}
	paths := make([]string, 0, len(jsonFields))
	for path := range jsonFields {
		paths = append(paths, path)
	}
	sortStrings(paths)
	fields := make([]mappingField, 0, len(paths))
	// Validate the object skeleton once so leaf/parent conflicts fail at
	// construction time.
	skeleton := map[string]any{}
	for _, path := range paths {
		segments, err := compileJSONPath(path)
		if err != nil {
			return nil, err
		}
		compiled, err := compileTemplate("webhook JSON field "+path, jsonFields[path], metadata)
		if err != nil {
			return nil, err
		}
		if err := placeSkeletonValue(skeleton, segments); err != nil {
			return nil, fmt.Errorf("webhook JSON field %q: %w", path, err)
		}
		fields = append(fields, mappingField{path: segments, template: compiled})
	}
	return fields, nil
}

func compileJSONPath(path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("webhook JSON field path is empty")
	}
	if strings.ContainsAny(path, "{}") {
		return nil, fmt.Errorf("webhook JSON field path %q must not contain braces", path)
	}
	segments := strings.Split(path, ".")
	if len(segments) > maxJSONPathDepth {
		return nil, fmt.Errorf("webhook JSON field path %q exceeds %d levels", path, maxJSONPathDepth)
	}
	for _, segment := range segments {
		if segment == "" || len(segment) > maxJSONPathSegmentBound {
			return nil, fmt.Errorf("webhook JSON field path %q has an empty or oversized segment", path)
		}
	}
	return segments, nil
}

func placeSkeletonValue(skeleton map[string]any, segments []string) error {
	current := skeleton
	for index, segment := range segments {
		if index == len(segments)-1 {
			if _, exists := current[segment]; exists {
				return errors.New("path is configured twice or conflicts with a parent path")
			}
			current[segment] = ""
			return nil
		}
		next, exists := current[segment]
		if !exists {
			child := map[string]any{}
			current[segment] = child
			current = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return errors.New("path is both a leaf and a parent")
		}
		current = child
	}
	return nil
}

func compileSuccessCheck(field, want string) ([]string, any, error) {
	if field == "" && want == "" {
		return nil, nil, nil
	}
	if field == "" || want == "" {
		return nil, nil, errors.New("webhook success check needs both field and value")
	}
	segments, err := compileJSONPath(field)
	if err != nil {
		return nil, nil, fmt.Errorf("webhook success field: %w", err)
	}
	var decoded any
	if err := json.Unmarshal([]byte(want), &decoded); err != nil {
		return nil, nil, fmt.Errorf("webhook success value is not a JSON literal: %w", err)
	}
	switch decoded.(type) {
	case string, float64, bool:
	default:
		return nil, nil, errors.New("webhook success value must be a JSON string, number or boolean")
	}
	return segments, decoded, nil
}

// dialTLS validates every resolved IP and then handshakes TLS against the
// configured server name.
func (s *WebhookSender) dialTLS(ctx context.Context, _ string, address string, tlsConfig *tls.Config) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("webhook destination address: %w", err)
	}
	dialer := &net.Dialer{Timeout: s.timeout}
	conn, err := s.policy.dialValidated(ctx, dialer, host, port)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(conn, tlsConfig)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("webhook TLS handshake: %w", err)
	}
	return tlsConn, nil
}

// Send performs exactly one delivery attempt: resolve secrets, render the
// restricted mapping, POST it. The code and secret header values never
// appear in returned errors.
func (s *WebhookSender) Send(ctx context.Context, message Message) error {
	if err := message.validate(); err != nil {
		return err
	}
	deadline := time.Now().Add(s.timeout)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	secretValues, err := s.resolveSecrets(ctx)
	if err != nil {
		return err
	}

	body, contentType, err := s.renderBody(message)
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook request: %w", err)
	}
	request.ContentLength = int64(len(body))
	request.Header.Set("Content-Type", contentType)
	for _, pair := range s.headers {
		request.Header.Set(pair[0], pair[1])
	}
	for _, pair := range s.secretRefs {
		request.Header.Set(pair[0], secretValues[pair[0]])
	}

	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("webhook delivery: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	limited := io.LimitReader(response.Body, s.maxResponse+1)
	body, readErr := io.ReadAll(limited)
	if readErr != nil {
		return fmt.Errorf("webhook response read: %w", readErr)
	}
	if int64(len(body)) > s.maxResponse {
		return errors.New("webhook response exceeds the configured size limit")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		// Only the status is surfaced; bodies can reflect payload content.
		return fmt.Errorf("webhook delivery: %w: unexpected status %d", ErrRejected, response.StatusCode)
	}
	if s.successPath != nil {
		if err := checkSuccessField(body, s.successPath, s.successWant); err != nil {
			return err
		}
	}
	return nil
}

func (s *WebhookSender) resolveSecrets(ctx context.Context) (map[string]string, error) {
	if len(s.secretRefs) == 0 {
		return nil, nil
	}
	values := make(map[string]string, len(s.secretRefs))
	for _, pair := range s.secretRefs {
		value, err := s.secrets(ctx, pair[1])
		if err != nil {
			return nil, fmt.Errorf("webhook secret header %q: %w", pair[0], err)
		}
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("webhook secret header %q resolved to an empty value", pair[0])
		}
		values[pair[0]] = value
	}
	return values, nil
}

// renderBody encodes rendered strings as whole JSON/form values, so the
// recipient and code can never break out of their field.
func (s *WebhookSender) renderBody(message Message) ([]byte, string, error) {
	if s.encoding == EncodingForm {
		return s.renderFormBody(message)
	}
	root := map[string]any{}
	for _, field := range s.fields {
		value, err := field.template.render(message)
		if err != nil {
			return nil, "", err
		}
		var payloadValue any = value
		if field.numeric {
			seconds, err := strconv.Atoi(value)
			if err != nil {
				return nil, "", fmt.Errorf("webhook numeric field is not a number: %w", err)
			}
			payloadValue = seconds
		}
		if err := setJSONValue(root, field.path, payloadValue); err != nil {
			return nil, "", err
		}
	}
	body, err := json.Marshal(root)
	if err != nil {
		return nil, "", fmt.Errorf("webhook payload encode: %w", err)
	}
	return body, "application/json", nil
}

func (s *WebhookSender) renderFormBody(message Message) ([]byte, string, error) {
	values := url.Values{}
	for _, field := range s.fields {
		value, err := field.template.render(message)
		if err != nil {
			return nil, "", err
		}
		values.Set(field.key, value)
	}
	return []byte(values.Encode()), "application/x-www-form-urlencoded", nil
}

// setJSONValue walks the compiled path, creating intermediate objects; the
// construction-time skeleton rules out leaf/parent conflicts.
func setJSONValue(root map[string]any, segments []string, value any) error {
	current := root
	for _, segment := range segments[:len(segments)-1] {
		child, ok := current[segment].(map[string]any)
		if !ok {
			child = map[string]any{}
			current[segment] = child
		}
		current = child
	}
	current[segments[len(segments)-1]] = value
	return nil
}

// checkSuccessField applies the optional success condition: the field must
// exist in the (size-capped) body and equal the configured literal.
func checkSuccessField(body []byte, path []string, want any) error {
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return fmt.Errorf("webhook success check: %w: response body is not JSON", ErrRejected)
	}
	current := document
	for _, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return fmt.Errorf("webhook success check: %w: field %q is missing", ErrRejected, strings.Join(path, "."))
		}
		current, ok = object[segment]
		if !ok {
			return fmt.Errorf("webhook success check: %w: field %q is missing", ErrRejected, strings.Join(path, "."))
		}
	}
	if !reflect.DeepEqual(current, want) {
		return fmt.Errorf("webhook success check: %w: field %q does not equal the expected value", ErrRejected, strings.Join(path, "."))
	}
	return nil
}
