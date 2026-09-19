package support

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
)

func TestGenerateDeploymentSecretsProducesValidADR0009Set(t *testing.T) {
	dir := t.TempDir()
	config := contract.QuoinConfig{
		RootKeyFile:               filepath.Join(dir, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(dir, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(dir, "runtime-tls.key"),
	}
	if err := GenerateDeploymentSecrets(config); err != nil {
		t.Fatal(err)
	}
	rootKey, err := os.ReadFile(config.RootKeyFile)
	if err != nil || len(rootKey) != 32 {
		t.Fatalf("root key = %d bytes, err=%v, want 32 raw bytes", len(rootKey), err)
	}
	if _, err := tls.LoadX509KeyPair(config.RuntimeTLSCertificateFile, config.RuntimeTLSPrivateKeyFile); err != nil {
		t.Fatalf("runtime TLS pair: %v", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "runtime-ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("runtime CA is not a parseable certificate")
	}
	for _, component := range []string{"stele", "plinth"} {
		pair, err := tls.LoadX509KeyPair(filepath.Join(dir, component+"-client.crt"), filepath.Join(dir, component+"-client.key"))
		if err != nil {
			t.Fatalf("%s client pair: %v", component, err)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			t.Fatalf("%s client certificate: %v", component, err)
		}
		if leaf.Subject.CommonName != component {
			t.Fatalf("%s client certificate CN = %q", component, leaf.Subject.CommonName)
		}
	}
}

func TestGenerateDeploymentSecretsRefusesDirtyDirectory(t *testing.T) {
	dir := t.TempDir()
	config := contract.QuoinConfig{RootKeyFile: filepath.Join(dir, "root-key")}
	if err := os.WriteFile(config.RootKeyFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := GenerateDeploymentSecrets(config); err == nil {
		t.Fatal("generation into a dirty secret directory must fail")
	}
}
