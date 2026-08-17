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
