package contract_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
)

const validQuoin = `component: quoin
publicOrigin: https://quoin.example.com
dataDirectory: /var/lib/quoin/data
backupDirectory: /var/lib/quoin/backups
rootKeyFile: /run/secrets/root-key
runtimeTlsCertificateFile: /run/secrets/runtime-tls.crt
runtimeTlsPrivateKeyFile: /run/secrets/runtime-tls.key
runtimeClientCaFile: /run/secrets/runtime-ca.pem
stelePublicURL: https://quoin.example.com/stele/alerts
`

func TestDecodeAcceptsStrictGeneratedConfiguration(t *testing.T) {
	var config contract.QuoinConfig
	if err := contract.Decode([]byte(validQuoin), &config); err != nil {
		t.Fatal(err)
	}
	if config.Component != "quoin" || config.PublicOrigin != "https://quoin.example.com" || config.StelePublicURL != "https://quoin.example.com/stele/alerts" {
		t.Fatalf("unexpected config: %+v", config)
	}
}

func TestDecodeRejectsNonCanonicalYAMLAndUnknownFields(t *testing.T) {
	cases := map[string]string{
		"duplicate": validQuoin + "component: quoin\n",
		"unknown":   validQuoin + "debugMode: true\n",
		"multi-doc": validQuoin + "---\ncomponent: quoin\n",
		"anchor":    strings.Replace(validQuoin, "component: quoin", "component: &name quoin", 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			var config contract.QuoinConfig
			if err := contract.Decode([]byte(input), &config); err == nil {
				t.Fatal("invalid deployment configuration was accepted")
			}
		})
	}
}

const bindingQuoin = validQuoin + `deploymentBinding:
  releaseVersion: v1.2.3
  releaseSubjectDigest: ` + bindingDigest + `
  deploymentConfigDigest: ` + bindingDigest + `
  backend: compose
  architecture: linux/amd64
  browserChromiumRevision: '1200.0.6099.109'
`

const bindingDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestDecodeAcceptsQuoinDeploymentBinding(t *testing.T) {
	var config contract.QuoinConfig
	if err := contract.Decode([]byte(bindingQuoin), &config); err != nil {
		t.Fatal(err)
	}
	binding := config.DeploymentBinding
	if binding == nil {
		t.Fatal("deployment binding was not decoded")
	}
	if binding.ReleaseVersion != "v1.2.3" || binding.ReleaseSubjectDigest != bindingDigest || binding.DeploymentConfigDigest != bindingDigest {
		t.Fatalf("unexpected binding digests: %+v", binding)
	}
	if binding.Backend != "compose" || binding.Architecture != "linux/amd64" {
		t.Fatalf("unexpected binding platform: %+v", binding)
	}
}

func TestDecodeRejectsInvalidDeploymentBinding(t *testing.T) {
	cases := map[string]string{
		"unknown-backend":      strings.Replace(bindingQuoin, "backend: compose", "backend: nomad", 1),
		"bad-digest":           strings.Replace(bindingQuoin, bindingDigest, "not-a-digest", 1),
		"unknown-architecture": strings.Replace(bindingQuoin, "architecture: linux/amd64", "architecture: linux/ppc64le", 1),
		"extra-field":          bindingQuoin + "  extra: value\n",
		"missing-version":      strings.Replace(bindingQuoin, "  releaseVersion: v1.2.3\n", "", 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			var config contract.QuoinConfig
			if err := contract.Decode([]byte(input), &config); err == nil {
				t.Fatal("invalid deployment binding was accepted")
			}
		})
	}
}

const pluginsQuoin = validQuoin + `enabledPlugins:
  - prometheus
  - thanos
  - alertmanager
  - kubernetes
  - browser
`

func TestDecodeAcceptsEnabledPluginsWhitelist(t *testing.T) {
	var config contract.QuoinConfig
	if err := contract.Decode([]byte(pluginsQuoin), &config); err != nil {
		t.Fatal(err)
	}
	want := []string{"prometheus", "thanos", "alertmanager", "kubernetes", "browser"}
	if len(config.EnabledPlugins) != len(want) {
		t.Fatalf("EnabledPlugins = %v, want %v", config.EnabledPlugins, want)
	}
	for index, id := range want {
		if config.EnabledPlugins[index] != id {
			t.Fatalf("EnabledPlugins = %v, want %v", config.EnabledPlugins, want)
		}
	}
}

func TestDecodeRejectsInvalidEnabledPlugins(t *testing.T) {
	cases := map[string]string{
		"unknown-shape":   validQuoin + "enabledPlugins: [Prometheus]\n",
		"duplicate":       validQuoin + "enabledPlugins: [prometheus, prometheus]\n",
		"not-a-list":      validQuoin + "enabledPlugins: prometheus\n",
		"non-string-item": validQuoin + "enabledPlugins: [1]\n",
		"too-long":        validQuoin + "enabledPlugins: [" + strings.Repeat("a", 65) + "]\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			var config contract.QuoinConfig
			if err := contract.Decode([]byte(input), &config); err == nil {
				t.Fatal("invalid enabledPlugins was accepted")
			}
		})
	}
}

const pluginsPlinth = `component: plinth
stateDirectory: /var/lib/plinth/state
workspaceDirectory: /var/lib/plinth/workspaces
quoinRuntimeEndpoint: https://quoin.example.com:8443
quoinRuntimeCaFile: /run/secrets/quoin-runtime-ca.crt
quoinRuntimeClientCertificateFile: /run/secrets/plinth-client.crt
quoinRuntimeClientPrivateKeyFile: /run/secrets/plinth-client.key
enabledPlugins: [browser]
`

func TestDecodeEmptyEnabledPluginsIsAnExplicitEmptyWhitelist(t *testing.T) {
	var config contract.QuoinConfig
	if err := contract.Decode([]byte(validQuoin+"enabledPlugins: []\n"), &config); err != nil {
		t.Fatal(err)
	}
	// Omission (nil) selects the default mainline; an explicit empty array
	// disables every plugin. The decoder must preserve the distinction.
	if config.EnabledPlugins == nil {
		t.Fatal("explicit empty enabledPlugins decoded as nil (omitted)")
	}
	if len(config.EnabledPlugins) != 0 {
		t.Fatalf("EnabledPlugins = %v, want empty", config.EnabledPlugins)
	}
}

func TestDecodePlinthEnabledPlugins(t *testing.T) {
	var config contract.PlinthConfig
	if err := contract.Decode([]byte(pluginsPlinth), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.EnabledPlugins) != 1 || config.EnabledPlugins[0] != "browser" {
		t.Fatalf("EnabledPlugins = %v, want [browser]", config.EnabledPlugins)
	}
}

const authDeliveryQuoin = validQuoin + `authentication:
  secretsFile: /run/quoin-secrets/auth-delivery-secrets.yaml
  configuration:
    email:
      kind: smtp
      host: smtp.example.com
      port: 587
      from: noreply@quoin.example.com
      username: quoin
      passwordRef: smtp-password
      tlsMode: starttls
      allowPrivateCIDRs: [192.168.0.0/16]
      rootCaPem: |
        -----BEGIN CERTIFICATE-----
        MIIB
        -----END CERTIFICATE-----
    sms:
      kind: webhook
      url: https://sms-gateway.example.com/send
      headers:
        X-Quoin-Env: production
      secretHeaders:
        X-Api-Key: sms-api-key
      encoding: json
      fields:
        code: "{code}"
      successField: accepted
      successValue: "true"
audit:
  retentionMonths: 6
`

func TestDecodeAcceptsAuthenticationAndAuditDeployment(t *testing.T) {
	var config contract.QuoinConfig
	if err := contract.Decode([]byte(authDeliveryQuoin), &config); err != nil {
		t.Fatal(err)
	}
	auth := config.Authentication
	if auth == nil {
		t.Fatal("authentication section was not decoded")
	}
	if auth.SecretsFile != "/run/quoin-secrets/auth-delivery-secrets.yaml" {
		t.Fatalf("SecretsFile = %q", auth.SecretsFile)
	}
	delivery := auth.Configuration
	if delivery == nil || delivery.Email == nil || delivery.SMS == nil {
		t.Fatalf("delivery channels were not decoded: %+v", delivery)
	}
	email := delivery.Email
	if email.Kind != "smtp" || email.Host != "smtp.example.com" || email.Port != 587 ||
		email.From != "noreply@quoin.example.com" || email.Username != "quoin" ||
		email.PasswordRef != "smtp-password" || email.TLSMode != "starttls" {
		t.Fatalf("unexpected email channel: %+v", email)
	}
	if len(email.AllowPrivateCIDRs) != 1 || email.AllowPrivateCIDRs[0] != "192.168.0.0/16" {
		t.Fatalf("AllowPrivateCIDRs = %v", email.AllowPrivateCIDRs)
	}
	if !strings.Contains(email.RootCAPEM, "BEGIN CERTIFICATE") {
		t.Fatalf("RootCAPEM = %q", email.RootCAPEM)
	}
	sms := delivery.SMS
	if sms.Kind != "webhook" || sms.URL != "https://sms-gateway.example.com/send" ||
		sms.Encoding != "json" || sms.SuccessField != "accepted" || sms.SuccessValue != "true" {
		t.Fatalf("unexpected sms channel: %+v", sms)
	}
	if sms.Headers["X-Quoin-Env"] != "production" || sms.SecretHeaders["X-Api-Key"] != "sms-api-key" {
		t.Fatalf("unexpected sms headers: %+v %+v", sms.Headers, sms.SecretHeaders)
	}
	if sms.Fields["code"] != "{code}" {
		t.Fatalf("unexpected sms fields: %+v", sms.Fields)
	}
	if config.Audit == nil || config.Audit.RetentionMonths != 6 {
		t.Fatalf("unexpected audit section: %+v", config.Audit)
	}
}

func TestDecodeAbsentAuthenticationAndAuditStayNil(t *testing.T) {
	// The deploy-sourced preset exists only when the YAML supplies it; the
	// decoder must preserve the distinction from runtime-administered
	// settings and defaults.
	var config contract.QuoinConfig
	if err := contract.Decode([]byte(validQuoin), &config); err != nil {
		t.Fatal(err)
	}
	if config.Authentication != nil {
		t.Fatalf("absent authentication decoded as %+v", config.Authentication)
	}
	if config.Audit != nil {
		t.Fatalf("absent audit decoded as %+v", config.Audit)
	}
}

func TestDecodeRejectsInvalidAuthentication(t *testing.T) {
	cases := map[string]string{
		"empty-section":           validQuoin + "authentication: {}\n",
		"unknown-section-field":   validQuoin + "authentication:\n  debug: true\n",
		"empty-configuration":     validQuoin + "authentication:\n  configuration: {}\n",
		"unknown-channel-field":   authDeliveryQuoin + "  extra: 1\n",
		"unknown-deployment-key":  validQuoin + "authentication:\n  configuration:\n    fax:\n      kind: webhook\n      url: https://gw.example.com\n",
		"sms-requires-webhook":    validQuoin + "authentication:\n  configuration:\n    sms:\n      kind: smtp\n      host: smtp.example.com\n      port: 587\n      from: noreply@quoin.example.com\n",
		"smtp-missing-port":       validQuoin + "authentication:\n  configuration:\n    email:\n      kind: smtp\n      host: smtp.example.com\n      from: noreply@quoin.example.com\n",
		"smtp-missing-host":       validQuoin + "authentication:\n  configuration:\n    email:\n      kind: smtp\n      port: 587\n      from: noreply@quoin.example.com\n",
		"webhook-missing-url":     validQuoin + "authentication:\n  configuration:\n    sms:\n      kind: webhook\n",
		"missing-kind":            validQuoin + "authentication:\n  configuration:\n    email:\n      host: smtp.example.com\n",
		"unknown-kind":            validQuoin + "authentication:\n  configuration:\n    email:\n      kind: ses\n      url: https://ses.example.com\n",
		"bad-tls-mode":            validQuoin + "authentication:\n  configuration:\n    email:\n      kind: smtp\n      host: smtp.example.com\n      port: 587\n      from: noreply@quoin.example.com\n      tlsMode: none\n",
		"bad-encoding":            validQuoin + "authentication:\n  configuration:\n    sms:\n      kind: webhook\n      url: https://gw.example.com\n      encoding: xml\n",
		"http-url":                validQuoin + "authentication:\n  configuration:\n    sms:\n      kind: webhook\n      url: http://gw.example.com/send\n",
		"relative-secrets-file":   validQuoin + "authentication:\n  secretsFile: secrets/auth.yaml\n",
		"bad-cidr":                validQuoin + "authentication:\n  configuration:\n    email:\n      kind: smtp\n      host: smtp.example.com\n      port: 587\n      from: noreply@quoin.example.com\n      allowPrivateCIDRs: [192.168.0.0]\n",
		"success-without-value":   validQuoin + "authentication:\n  configuration:\n    sms:\n      kind: webhook\n      url: https://gw.example.com\n      successField: ok\n",
		"username-without-ref":    validQuoin + "authentication:\n  configuration:\n    email:\n      kind: smtp\n      host: smtp.example.com\n      port: 587\n      from: noreply@quoin.example.com\n      username: quoin\n",
		"empty-secret-header-ref": validQuoin + "authentication:\n  configuration:\n    sms:\n      kind: webhook\n      url: https://gw.example.com\n      secretHeaders:\n        X-Api-Key: \"\"\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			var config contract.QuoinConfig
			if err := contract.Decode([]byte(input), &config); err == nil {
				t.Fatal("invalid authentication deployment was accepted")
			}
		})
	}
}

func TestDecodeRejectsInvalidAudit(t *testing.T) {
	cases := map[string]string{
		"empty-section":  validQuoin + "audit: {}\n",
		"below-minimum":  validQuoin + "audit:\n  retentionMonths: 5\n",
		"zero":           validQuoin + "audit:\n  retentionMonths: 0\n",
		"not-an-integer": validQuoin + "audit:\n  retentionMonths: six\n",
		"extra-field":    validQuoin + "audit:\n  retentionMonths: 6\n  enabled: true\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			var config contract.QuoinConfig
			if err := contract.Decode([]byte(input), &config); err == nil {
				t.Fatal("invalid audit deployment was accepted")
			}
		})
	}
}

func TestDecodeAcceptsExtendedAuditRetention(t *testing.T) {
	var config contract.QuoinConfig
	if err := contract.Decode([]byte(validQuoin+"audit:\n  retentionMonths: 12\n"), &config); err != nil {
		t.Fatal(err)
	}
	if config.Audit == nil || config.Audit.RetentionMonths != 12 {
		t.Fatalf("unexpected audit section: %+v", config.Audit)
	}
}

// TestAuthDeliveryDeploymentJSONShapeMatchesStoredSettings pins the contract
// struct to the exact JSON field names of the stored
// auth_delivery_settings.configuration_json document (currently decoded by
// the app-private DTO): the deploy preset and runtime settings must stay
// wire-compatible.
func TestAuthDeliveryDeploymentJSONShapeMatchesStoredSettings(t *testing.T) {
	const storedShape = `{
	  "email": {"kind": "smtp", "host": "smtp.example.com", "port": 587,
	            "from": "noreply@quoin.test", "username": "quoin",
	            "passwordRef": "smtp-password", "tlsMode": "implicit",
	            "allowPrivateCIDRs": ["127.0.0.0/8"], "rootCaPem": "PEM"},
	  "sms": {"kind": "webhook", "url": "https://gw.example.com/send",
	          "headers": {"X-Env": "e2e"}, "secretHeaders": {"X-Api-Key": "sms-key"},
	          "encoding": "form", "fields": {"code": "{code}"},
	          "successField": "ok", "successValue": "true"}
	}`
	var delivery contract.AuthDeliveryDeployment
	if err := json.Unmarshal([]byte(storedShape), &delivery); err != nil {
		t.Fatal(err)
	}
	email := delivery.Email
	if email == nil || email.Kind != "smtp" || email.Host != "smtp.example.com" ||
		email.Port != 587 || email.From != "noreply@quoin.test" ||
		email.Username != "quoin" || email.PasswordRef != "smtp-password" ||
		email.TLSMode != "implicit" || email.RootCAPEM != "PEM" ||
		len(email.AllowPrivateCIDRs) != 1 || email.AllowPrivateCIDRs[0] != "127.0.0.0/8" {
		t.Fatalf("email channel does not round-trip the stored JSON shape: %+v", email)
	}
	sms := delivery.SMS
	if sms == nil || sms.Kind != "webhook" || sms.URL != "https://gw.example.com/send" ||
		sms.Headers["X-Env"] != "e2e" || sms.SecretHeaders["X-Api-Key"] != "sms-key" ||
		sms.Encoding != "form" || sms.Fields["code"] != "{code}" ||
		sms.SuccessField != "ok" || sms.SuccessValue != "true" {
		t.Fatalf("sms channel does not round-trip the stored JSON shape: %+v", sms)
	}
}

// TestDeployConfigTemplateStaysContractValid protects the shipped default
// component template: it must keep decoding against the current deployment
// schema even as new sections land.
func TestDeployConfigTemplateStaysContractValid(t *testing.T) {
	var config contract.QuoinConfig
	if err := contract.DecodeFile(filepath.Join("..", "..", "deploy", "config", "quoin.yaml"), &config); err != nil {
		t.Fatal(err)
	}
	if config.Component != "quoin" {
		t.Fatalf("unexpected component: %q", config.Component)
	}
}
