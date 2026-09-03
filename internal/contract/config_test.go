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
`

func TestDecodeAcceptsStrictGeneratedConfiguration(t *testing.T) {
	var config contract.QuoinConfig
	if err := contract.Decode([]byte(validQuoin), &config); err != nil {
		t.Fatal(err)
	}
	if config.Component != "quoin" || config.PublicOrigin != "https://quoin.example.com" {
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
