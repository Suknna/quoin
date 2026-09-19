package support

// Test-only deployment secret generation. The product binary deliberately
// does not generate deployment secrets: first-boot material is produced by
// the deployment operator (scripts/generate-deployment-secrets.sh or an
// equivalent PKI process, see docs/deployment.md). Tests still need a valid
// ADR-0009 secret set to boot the real mTLS surfaces, so the generation
// lives here, outside the product packages.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/Suknna/quoin/internal/contract"
)

// GenerateDeploymentSecrets writes a fresh ADR-0009 deployment secret set
// (root key, Runtime CA, runtime server TLS pair, stele/plinth client pairs)
// into the directory of config.RootKeyFile. It is for pristine test
// directories only: any pre-existing secret file is an error.
func GenerateDeploymentSecrets(config contract.QuoinConfig) error {
	dir := filepath.Dir(config.RootKeyFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create secret directory: %w", err)
	}
	rootKey := make([]byte, 32)
	if _, err := rand.Read(rootKey); err != nil {
		return fmt.Errorf("generate root key: %w", err)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	caTemplate := &x509.Certificate{
		SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: "Quoin Runtime CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	serverCert, serverKey, err := serverPair(caTemplate, caKey, now)
	if err != nil {
		return err
	}
	steleCert, steleKey, err := clientPair(caCertificate, caKey, "stele", now)
	if err != nil {
		return err
	}
	plinthCert, plinthKey, err := clientPair(caCertificate, caKey, "plinth", now)
	if err != nil {
		return err
	}
	caKeyPEM, err := marshalKey(caKey)
	if err != nil {
		return err
	}
	files := map[string][]byte{
		config.RootKeyFile:                      rootKey,
		config.RuntimeTLSCertificateFile:        serverCert,
		config.RuntimeTLSPrivateKeyFile:         serverKey,
		filepath.Join(dir, "runtime-ca.pem"):    caPEM,
		filepath.Join(dir, "runtime-ca.key"):    caKeyPEM,
		filepath.Join(dir, "stele-client.crt"):  steleCert,
		filepath.Join(dir, "stele-client.key"):  steleKey,
		filepath.Join(dir, "plinth-client.crt"): plinthCert,
		filepath.Join(dir, "plinth-client.key"): plinthKey,
	}
	for path, content := range files {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("secret file %s already exists; tests must generate into a pristine directory", filepath.Base(path))
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return fmt.Errorf("write secret file %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

func serverPair(caTemplate *x509.Certificate, caKey *ecdsa.PrivateKey, now time.Time) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: "quoin"},
		DNSNames:  []string{"quoin", "localhost"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(2, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caTemplate, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := marshalKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, nil
}

func clientPair(ca *x509.Certificate, caKey *ecdsa.PrivateKey, commonName string, now time.Time) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: commonName},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := marshalKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, nil
}

func marshalKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func randomSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, _ := rand.Int(rand.Reader, limit)
	return serial
}
