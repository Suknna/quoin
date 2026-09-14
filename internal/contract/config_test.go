package contract_test

import (
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
steleServiceTokenFile: /run/secrets/stele-token
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
