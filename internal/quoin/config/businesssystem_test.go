package config

import (
	"strings"
	"testing"
)

const validBusinessSystemYAML = `apiVersion: quoin/v1
kind: BusinessSystem
metadata:
  name: payments
  displayName: Payments
  description: Handles payment authorization.
spec:
  metrics:
    connectionRef: observability-main
    matchLabels:
      business_system: payments
    resources:
      - name: checkout-pods
        displayName: Checkout Pods
        matchLabels:
          job: checkout
        discoveryMetric: kube_pod_info
        identityLabels: [namespace, pod]
        allowedMetrics: [kube_pod_info, http_requests_*]
  alerts:
    sourceRefs: [alertmanager-primary]
    matchLabels: {}
  inspections:
    - name: availability
      displayName: Availability
      schedule: "*/5 * * * *"
      timezone: UTC
      checks:
        - name: requests
          resourceRef: checkout-pods
          expression: sum(rate(http_requests_total{code="500"}[5m]))
          question: Are errors elevated?
`

func parseBusinessSystem(t *testing.T, body string) BusinessSystem {
	t.Helper()
	document, fields := ParseBusinessSystem([]byte(body), Limits{})
	if len(fields) != 0 {
		t.Fatalf("parse failed: %#v", fields)
	}
	return document
}

func TestParseBusinessSystemCompilesScopeAndDefaultRefresh(t *testing.T) {
	document := parseBusinessSystem(t, validBusinessSystemYAML)
	if document.DiscoveryRefresh() != DefaultDiscoveryRefresh {
		t.Fatalf("refresh = %s", document.DiscoveryRefresh())
	}
	scope, err := document.CompileResourceScope("checkout-pods")
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := scope.ScopeExpression(`sum(rate(http_requests_total{code="500"}[5m]))`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`business_system="payments"`, `job="checkout"`} {
		if !strings.Contains(scoped, want) {
			t.Fatalf("scoped query %q lacks %s", scoped, want)
		}
	}
	compiled, err := CompileBusinessSystemDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Description != "Handles payment authorization." || compiled.MetricsConnectionRef != "observability-main" || compiled.DiscoveryRefreshIntervalSeconds != 300 || len(compiled.AlertSourceRefs) != 1 || len(compiled.Resources) != 1 {
		t.Fatalf("unexpected compiled declaration: %#v", compiled)
	}
	if compiled.MetricsConnectionID != 0 {
		t.Fatalf("parser must not resolve connection name: %#v", compiled)
	}
	if got := compiled.Resources[0].MatchLabels; got["business_system"] != "payments" || got["job"] != "checkout" {
		t.Fatalf("runtime resource policy must contain merged selectors: %#v", got)
	}
	if !compiled.Enabled || compiled.Timezone != "UTC" || len(compiled.Discoveries) != 1 {
		t.Fatalf("compatibility defaults/projections missing: %#v", compiled)
	}
	if !strings.Contains(compiled.Discoveries[0].Selector, `business_system="payments"`) || !strings.Contains(compiled.Discoveries[0].Selector, `job="checkout"`) {
		t.Fatalf("discovery selector must be scoped: %q", compiled.Discoveries[0].Selector)
	}
	if len(compiled.Plans) != 1 || len(compiled.Plans[0].Checks) != 1 || compiled.Plans[0].Checks[0].ResourceRef != "checkout-pods" {
		t.Fatalf("inspection resource link missing from compiled document: %#v", compiled.Plans)
	}
	if !strings.Contains(compiled.Plans[0].Checks[0].Expression, `business_system="payments"`) || !strings.Contains(compiled.Plans[0].Checks[0].Expression, `job="checkout"`) {
		t.Fatalf("inspection expression must be scoped: %q", compiled.Plans[0].Checks[0].Expression)
	}
	if got := compiled.AlertSourceLabels; got["business_system"] != "payments" {
		t.Fatalf("alert labels must inherit metrics scope when omitted: %#v", got)
	}
	runtimeScope, err := compiled.CompileResourceScope("checkout-pods")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeScope.ScopeExpression(`http_requests_total`); err != nil {
		t.Fatalf("runtime-decoded document scope must execute: %v", err)
	}
}

func TestBusinessSystemDocumentDigestCoversCompiledPolicy(t *testing.T) {
	base, err := CompileBusinessSystemDocument(parseBusinessSystem(t, validBusinessSystemYAML))
	if err != nil {
		t.Fatal(err)
	}
	changedDescription := base
	changedDescription.Description = "Different operational boundary description."
	if base.Digest() == changedDescription.Digest() {
		t.Fatal("description must participate in the declaration digest")
	}
	changedPolicy := base
	changedPolicy.Resources = append([]ResourceProjection(nil), base.Resources...)
	changedPolicy.Resources[0].AllowedMetrics = []string{"kube_pod_info"}
	if base.Digest() == changedPolicy.Digest() {
		t.Fatal("resource metric policy must participate in the declaration digest")
	}
	changedCheck := base
	changedCheck.Plans = append([]PlanProjection(nil), base.Plans...)
	changedCheck.Plans[0].Checks = append([]CheckProjection(nil), base.Plans[0].Checks...)
	changedCheck.Plans[0].Checks[0].ResourceRef = "other-resource"
	if base.Digest() == changedCheck.Digest() {
		t.Fatal("check resource reference must participate in the declaration digest")
	}
}

func TestBusinessSystemRejectsScopeEscapes(t *testing.T) {
	cases := []struct{ name, body, reason string }{
		{"conflicting labels", strings.Replace(validBusinessSystemYAML, "          job: checkout", "          business_system: billing", 1), "conflicts"},
		{"disallowed check metric", strings.Replace(validBusinessSystemYAML, "http_requests_total", "process_cpu_seconds_total", 1), "not allowed"},
		{"wrong mandatory label", strings.Replace(validBusinessSystemYAML, `code="500"`, `job="other"`, 1), "must exactly equal"},
		{"broad wildcard rejected by schema", strings.Replace(validBusinessSystemYAML, "http_requests_*", `"*"`, 1), "allowedMetrics"},
		{"range missing bounds", strings.Replace(validBusinessSystemYAML, "          question: Are errors elevated?", "          queryMode: range\n          question: Are errors elevated?", 1), "rangeSeconds"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, fields := ParseBusinessSystem([]byte(test.body), Limits{})
			if len(fields) == 0 {
				t.Fatal("must reject")
			}
			if !containsAny(fields, test.reason) && !containsPath(fields, test.reason) {
				t.Fatalf("expected %q in %#v", test.reason, fields)
			}
		})
	}
}

func TestBusinessSystemRejectsDuplicateSemanticNames(t *testing.T) {
	cases := []struct {
		name, body, path string
	}{
		{
			name: "resource name",
			body: strings.Replace(validBusinessSystemYAML, "  alerts:", `      - name: checkout-pods
        displayName: Duplicate Checkout Pods
        matchLabels:
          job: checkout-canary
        discoveryMetric: kube_pod_info
        identityLabels: [namespace, pod]
        allowedMetrics: [kube_pod_info, http_requests_*]
  alerts:`, 1),
			path: "spec.metrics.resources[1].name",
		},
		{
			name: "inspection name",
			body: validBusinessSystemYAML + `    - name: availability
      displayName: Duplicate Availability
      schedule: "*/10 * * * *"
      timezone: UTC
      checks:
        - name: latency
          resourceRef: checkout-pods
          expression: http_requests_total
          question: Is latency elevated?
`,
			path: "spec.inspections[1].name",
		},
		{
			name: "check name within plan",
			body: strings.Replace(validBusinessSystemYAML, "          question: Are errors elevated?", `          question: Are errors elevated?
        - name: requests
          resourceRef: checkout-pods
          expression: http_requests_total
          question: Are requests elevated?`, 1),
			path: "spec.inspections[0].checks[1].name",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, fields := ParseBusinessSystem([]byte(test.body), Limits{})
			if !containsPath(fields, test.path) {
				t.Fatalf("expected field error at %q, got %#v", test.path, fields)
			}
		})
	}
}

func TestBusinessSystemAllowsCheckNameInSeparatePlans(t *testing.T) {
	body := validBusinessSystemYAML + `    - name: latency
      displayName: Latency
      schedule: "*/10 * * * *"
      timezone: UTC
      checks:
        - name: requests
          resourceRef: checkout-pods
          expression: http_requests_total
          question: Is latency elevated?
`
	if _, fields := ParseBusinessSystem([]byte(body), Limits{}); len(fields) != 0 {
		t.Fatalf("check names must only be unique within their plan: %#v", fields)
	}
}

func TestBusinessSystemRejectsInvalidMatchLabelNames(t *testing.T) {
	cases := []struct {
		name, body, path string
	}{
		{"metrics", strings.Replace(validBusinessSystemYAML, "      business_system: payments", "      invalid-label: payments", 1), "spec.metrics.matchLabels.invalid-label"},
		{"resource", strings.Replace(validBusinessSystemYAML, "          job: checkout", "          invalid-label: checkout", 1), "spec.metrics.resources[0].matchLabels.invalid-label"},
		{"alerts", strings.Replace(validBusinessSystemYAML, "    matchLabels: {}", "    matchLabels:\n      invalid-label: payments", 1), "spec.alerts.matchLabels.invalid-label"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, fields := ParseBusinessSystem([]byte(test.body), Limits{})
			if !containsPath(fields, test.path) {
				t.Fatalf("expected field error at %q, got %#v", test.path, fields)
			}
		})
	}
}

func TestScopeExpressionRejectsRegexBypassAndAllVectors(t *testing.T) {
	document := parseBusinessSystem(t, validBusinessSystemYAML)
	scope, err := document.CompileResourceScope("checkout-pods")
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`http_requests_total{job=~".*"}`,
		`http_requests_total{business_system=~"pay.*"}`,
		`http_requests_total{job="checkout"} or process_cpu_seconds_total`,
	} {
		if _, err := scope.ScopeExpression(query); err == nil {
			t.Fatalf("query must reject: %s", query)
		}
	}
}

func containsPath(fields []FieldError, text string) bool {
	for _, field := range fields {
		if strings.Contains(field.Path, text) {
			return true
		}
	}
	return false
}
