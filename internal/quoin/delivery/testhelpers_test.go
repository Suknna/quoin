package delivery

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Compile-time assertions: both senders satisfy the delivery contract.
var (
	_ Sender = (*WebhookSender)(nil)
	_ Sender = (*SMTPSender)(nil)
)

// generateTestCert builds a throwaway CA plus a leaf certificate valid for
// the given loopback addresses, returning the CA and leaf PEMs. Tests use
// real TLS end to end; no sender ever runs with validation disabled.
func generateTestCert(t *testing.T, ips []net.IP, dnsNames []string) (caPEM, certPEM, certKeyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "quoin delivery test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "quoin delivery test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}

	return pemEncode("CERTIFICATE", caDER), pemEncode("CERTIFICATE", leafDER), pemEncode("EC PRIVATE KEY", leafKeyDER)
}

func pemEncode(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}

// startTLSTestServer runs a real TLS HTTPS server on the loopback interface
// and returns it together with the CA PEM the sender must trust.
func startTLSTestServer(t *testing.T, handler http.Handler) (*httptest.Server, []byte) {
	t.Helper()
	caPEM, certPEM, certKeyPEM := generateTestCert(t,
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		[]string{"localhost"})
	cert, err := tls.X509KeyPair(certPEM, certKeyPEM)
	if err != nil {
		t.Fatalf("load test certificate: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, caPEM
}

// loopbackWebhookConfig returns a valid webhook configuration targeting the
// test server: loopback is reachable only through the explicit allow CIDR.
func loopbackWebhookConfig(server *httptest.Server, caPEM []byte) WebhookConfig {
	return WebhookConfig{
		URL:               server.URL,
		AllowPrivateCIDRs: []string{"127.0.0.0/8"},
		RootCAPEM:         caPEM,
	}
}

func testMessage() Message {
	return Message{
		ID:        "delivery-1",
		Channel:   ChannelSMS,
		Recipient: "+8613800000000",
		Template:  "login_verification",
		Code:      "123456",
		ExpiresIn: 5 * time.Minute,
	}
}

var errUnknownSecret = errors.New("unknown secret reference")

func staticSecrets(values map[string]string) SecretFunc {
	return func(_ context.Context, reference string) (string, error) {
		if value, ok := values[reference]; ok {
			return value, nil
		}
		return "", errUnknownSecret
	}
}
