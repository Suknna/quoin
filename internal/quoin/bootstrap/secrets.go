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
	steleToken := make([]byte, 32)
	if _, err := randomRead(rootKey); err != nil {
		return fmt.Errorf("generate root key: %w", err)
	}
	if _, err := randomRead(steleToken); err != nil {
		return fmt.Errorf("generate Stele token: %w", err)
	}
	caCert, caKey, serverCert, serverKey, err := generateRuntimeTLS()
	if err != nil {
		return err
	}
	files := map[string][]byte{
		config.RootKeyFile:                                                rootKey,
		config.SteleServiceTokenFile:                                      steleToken,
		config.RuntimeTLSCertificateFile:                                  serverCert,
		config.RuntimeTLSPrivateKeyFile:                                   serverKey,
		filepath.Join(filepath.Dir(config.RootKeyFile), "runtime-ca.pem"): caCert,
		filepath.Join(filepath.Dir(config.RootKeyFile), "runtime-ca.key"): caKey,
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

func validateSecrets(config contract.QuoinConfig) error {
	for _, item := range []struct {
		path string
		size int
	}{{config.RootKeyFile, 32}, {config.SteleServiceTokenFile, 32}} {
		info, err := os.Lstat(item.path)
		if err != nil {
			return fmt.Errorf("read existing secret state: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != int64(item.size) {
			return fmt.Errorf("secret file %s has invalid type, mode, or length", filepath.Base(item.path))
		}
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
	return []string{config.RootKeyFile, config.SteleServiceTokenFile, config.RuntimeTLSCertificateFile,
		config.RuntimeTLSPrivateKeyFile, filepath.Join(dir, "runtime-ca.pem"), filepath.Join(dir, "runtime-ca.key")}
}

func regularNonempty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}
