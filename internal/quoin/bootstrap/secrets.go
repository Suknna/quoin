package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"golang.org/x/sys/unix"
)

var randomRead = rand.Read

func BootstrapSecrets(config contract.QuoinConfig) (bool, error) {
	secretDir := filepath.Dir(config.RootKeyFile)
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		return false, fmt.Errorf("create secret directory: %w", err)
	}
	if err := os.Chmod(secretDir, 0o700); err != nil {
		return false, fmt.Errorf("protect secret directory: %w", err)
	}
	lockFile, err := os.OpenFile(filepath.Join(secretDir, ".bootstrap.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("open secret bootstrap lock: %w", err)
	}
	defer lockFile.Close()
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		return false, fmt.Errorf("lock secret directory: %w", err)
	}
	defer unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)

	paths := secretPaths(config)
	existing := 0
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			existing++
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("inspect secret state: %w", err)
		}
	}
	databaseExists := regularNonempty(filepath.Join(config.DataDirectory, "quoin.db"))
	if existing == len(paths) {
		return false, validateSecrets(config)
	}
	if existing != 0 {
		return false, fmt.Errorf("secret state is partial; restore the original complete secret directory")
	}
	if databaseExists {
		return false, fmt.Errorf("persistent database exists but deployment secrets are missing; restore the original secrets")
	}
	return true, generateSecrets(config)
}

func generateSecrets(config contract.QuoinConfig) error {
	rootKey := make([]byte, 32)
	if _, err := randomRead(rootKey); err != nil {
		return fmt.Errorf("generate root key: %w", err)
	}
	caCert, caKeyPEM, serverCert, serverKey, err := generateRuntimeTLS()
	if err != nil {
		return err
	}
	caKey, err := parsePrivateKey(caKeyPEM)
	if err != nil {
		return fmt.Errorf("parse Runtime CA key: %w", err)
	}
	steleCert, steleKey, err := generateClientCertificate(caCert, caKey, "stele")
	if err != nil {
		return err
	}
	plinthCert, plinthKey, err := generateClientCertificate(caCert, caKey, "plinth")
	if err != nil {
		return err
	}
	files := map[string][]byte{
		config.RootKeyFile:                                                   rootKey,
		config.RuntimeTLSCertificateFile:                                     serverCert,
		config.RuntimeTLSPrivateKeyFile:                                      serverKey,
		filepath.Join(filepath.Dir(config.RootKeyFile), "runtime-ca.pem"):    caCert,
		filepath.Join(filepath.Dir(config.RootKeyFile), "runtime-ca.key"):    caKeyPEM,
		filepath.Join(filepath.Dir(config.RootKeyFile), "stele-client.crt"):  steleCert,
		filepath.Join(filepath.Dir(config.RootKeyFile), "stele-client.key"):  steleKey,
		filepath.Join(filepath.Dir(config.RootKeyFile), "plinth-client.crt"): plinthCert,
		filepath.Join(filepath.Dir(config.RootKeyFile), "plinth-client.key"): plinthKey,
	}
	created := make([]string, 0, len(files))
	for path, content := range files {
		if err := writeExclusive(path, content); err != nil {
			for _, createdPath := range created {
				_ = os.Remove(createdPath)
			}
			return err
		}
		created = append(created, path)
	}
	dir, err := os.Open(filepath.Dir(config.RootKeyFile))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync secret directory: %w", err)
	}
	return validateSecrets(config)
}

// IssueClientCertificates signs the Stele and Plinth client certificates from
// the deployment's existing Runtime CA. It serves two operators: deployments
// upgrading from the registration-era secret set (which lacks client
// certificates) and deliberate credential rotation. Existing files are only
// replaced with --force.
func IssueClientCertificates(config contract.QuoinConfig, force bool) error {
	dir := filepath.Dir(config.RootKeyFile)
	caCert, err := os.ReadFile(filepath.Join(dir, "runtime-ca.pem"))
	if err != nil {
		return fmt.Errorf("read Runtime CA: %w", err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(dir, "runtime-ca.key"))
	if err != nil {
		return fmt.Errorf("read Runtime CA key: %w", err)
	}
	caKey, err := parsePrivateKey(caKeyPEM)
	if err != nil {
		return fmt.Errorf("parse Runtime CA key: %w", err)
	}
	if !force {
		for _, name := range []string{"stele-client.crt", "plinth-client.crt"} {
			if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
				return fmt.Errorf("client certificate %s already exists; pass --force to replace it", name)
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}
	files := map[string][]byte{}
	for _, component := range []string{"stele", "plinth"} {
		certPEM, keyPEM, err := generateClientCertificate(caCert, caKey, component)
		if err != nil {
			return err
		}
		files[filepath.Join(dir, component+"-client.crt")] = certPEM
		files[filepath.Join(dir, component+"-client.key")] = keyPEM
	}
	for path, content := range files {
		if force {
			if err := os.WriteFile(path, content, 0o600); err != nil {
				return fmt.Errorf("replace client certificate %s: %w", filepath.Base(path), err)
			}
			continue
		}
		if err := writeExclusive(path, content); err != nil {
			return err
		}
	}
	return validateSecrets(config)
}

func validateSecrets(config contract.QuoinConfig) error {
	info, err := os.Lstat(config.RootKeyFile)
	if err != nil {
		return fmt.Errorf("read existing secret state: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != 32 {
		return fmt.Errorf("secret file %s has invalid type, mode, or length", filepath.Base(config.RootKeyFile))
	}
	certPEM, err := os.ReadFile(config.RuntimeTLSCertificateFile)
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(config.RuntimeTLSPrivateKeyFile)
	if err != nil {
		return err
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("validate Runtime TLS key pair: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(filepath.Dir(config.RootKeyFile), "runtime-ca.pem"))
	if err != nil {
		return err
	}
	if block, _ := pem.Decode(caPEM); block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("Runtime CA is not a PEM certificate")
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("Runtime CA certificate cannot be parsed")
	}
	dir := filepath.Dir(config.RootKeyFile)
	for _, component := range []string{"stele", "plinth"} {
		clientCert, err := tls.LoadX509KeyPair(filepath.Join(dir, component+"-client.crt"), filepath.Join(dir, component+"-client.key"))
		if err != nil {
			return fmt.Errorf("validate %s client identity: %w", component, err)
		}
		leaf, err := x509.ParseCertificate(clientCert.Certificate[0])
		if err != nil {
			return err
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: caPool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			return fmt.Errorf("%s client certificate is not signed by the Runtime CA: %w", component, err)
		}
		if leaf.Subject.CommonName != component {
			return fmt.Errorf("%s client certificate has CN %q, want %q", component, leaf.Subject.CommonName, component)
		}
	}
	return nil
}

func generateRuntimeTLS() ([]byte, []byte, []byte, []byte, error) {
	now := time.Now().UTC()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("generate Runtime CA key: %w", err)
	}
	caSerial, err := randomSerial()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("generate Runtime CA serial: %w", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: caSerial, Subject: pkix.Name{CommonName: "Quoin Runtime CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	serverSerial, err := randomSerial()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("generate Runtime server serial: %w", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: serverSerial, Subject: pkix.Name{CommonName: "quoin"},
		DNSNames:  []string{"quoin", "localhost"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(2, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	caKeyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	serverKeyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: caKeyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKeyDER}), nil
}

// generateClientCertificate signs one component client certificate (CN =
// component) with the deployment Runtime CA. Client identity is long-lived
// deployment material (ADR-0009): validity matches the CA and rotation is the
// explicit `quoin secrets issue-client-certs --force` operation.
func generateClientCertificate(caPEM []byte, caKey *ecdsa.PrivateKey, commonName string) ([]byte, []byte, error) {
	caBlock, _ := pem.Decode(caPEM)
	if caBlock == nil {
		return nil, nil, fmt.Errorf("parse Runtime CA certificate")
	}
	caCertificate, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate %s client key: %w", commonName, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, fmt.Errorf("generate %s client serial: %w", commonName, err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: commonName},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCertificate, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func parsePrivateKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ecdsaKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is %T, want ECDSA", key)
	}
	return ecdsaKey, nil
}

func writeExclusive(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create secret file %s: %w", filepath.Base(path), err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func secretPaths(config contract.QuoinConfig) []string {
	dir := filepath.Dir(config.RootKeyFile)
	return []string{
		config.RootKeyFile, config.RuntimeTLSCertificateFile,
		config.RuntimeTLSPrivateKeyFile, filepath.Join(dir, "runtime-ca.pem"), filepath.Join(dir, "runtime-ca.key"),
		filepath.Join(dir, "stele-client.crt"), filepath.Join(dir, "stele-client.key"),
		filepath.Join(dir, "plinth-client.crt"), filepath.Join(dir, "plinth-client.key"),
	}
}

func regularNonempty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}
