package manifest_test

// The local signing authority of the manifest fixture legs: an ephemeral
// Fulcio-shaped CA issues one code-signing certificate for the release
// workflow identity; every fixture evidence bundle is a real DSSE
// in-toto statement signed by it and offline-verifiable through the
// supplychain gate. No key outlives the test process.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/release/supplychain"
)

const (
	closureIdentity = "https://github.com/Suknna/quoin/.github/workflows/release.yml@refs/tags/v0.1.0-dev"
	closureIssuer   = "https://token.actions.githubusercontent.com"
)

type closureSigner struct {
	ca       *ecdsa.PrivateKey
	caCert   *x509.Certificate
	leaf     *ecdsa.PrivateKey
	leafCert *x509.Certificate
}

func newClosureSigner(t *testing.T) *closureSigner {
	t.Helper()
	ca, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "quoin-t42-closure-root"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(2 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &ca.PublicKey, ca)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identityURL, err := url.Parse(closureIdentity)
	if err != nil {
		t.Fatal(err)
	}
	issuerValue, err := asn1.Marshal(closureIssuer)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: closureIdentity},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(2 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{identityURL},
		ExtraExtensions: []pkix.Extension{{
			Id:    asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1},
			Value: issuerValue,
		}},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leaf.PublicKey, ca)
	if err != nil {
		t.Fatal(err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	return &closureSigner{ca: ca, caCert: caCert, leaf: leaf, leafCert: leafCert}
}

func (signer *closureSigner) trustRootPath(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "closure-root.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: signer.caCert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (signer *closureSigner) trust() supplychain.Trust {
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: signer.caCert.Raw})
	return supplychain.Trust{RootPEM: rootPEM, IdentityRegexp: "^https://github\\.com/Suknna/quoin/", Issuer: closureIssuer}
}

// signStatement writes one offline-verifiable DSSE bundle over an in-toto
// statement payload bound to one subject digest, returning the bundle bytes.
func (signer *closureSigner) signStatement(t *testing.T, dir, name, subjectName, subjectDigest string, payload any) []byte {
	t.Helper()
	statement := map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"predicateType": statementPredicateOf(payload),
		"subject":       []map[string]any{{"name": subjectName, "digest": map[string]string{"sha256": subjectDigest}}},
		"predicate":     payload,
	}
	encoded, err := json.Marshal(statement)
	if err != nil {
		t.Fatal(err)
	}
	paeBytes := supplychain.DSSEPAE("application/vnd.in-toto+json", encoded)
	digest := sha256.Sum256(paeBytes)
	signature, err := ecdsa.SignASN1(rand.Reader, signer.leaf, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	bundle := map[string]any{
		"mediaType": supplychain.BundleMediaType + ";v0.3",
		"verificationMaterial": map[string]any{
			"x590CertificateChain": map[string]any{
				"certificates": []map[string]any{
					{"rawBytes": base64.StdEncoding.EncodeToString(signer.leafCert.Raw)},
					{"rawBytes": base64.StdEncoding.EncodeToString(signer.caCert.Raw)},
				},
			},
		},
		"dsseEnvelope": map[string]any{
			"payloadType": "application/vnd.in-toto+json",
			"payload":     base64.StdEncoding.EncodeToString(encoded),
			"signatures":  []map[string]any{{"keyid": "", "sig": base64.StdEncoding.EncodeToString(signature)}},
		},
	}
	body, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if dir != "" {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return body
}

// statementPredicateOf picks the predicate type of a fixture payload by
// its shape: test-result projections carry result/passedTests, release
// reports carry kind.
func statementPredicateOf(payload any) string {
	if mapping, ok := payload.(map[string]any); ok {
		if _, carries := mapping["kind"]; carries {
			return "https://quoin.dev/release-subject/v1"
		}
	}
	return "https://in-toto.io/attestation/test-result/v0.1"
}

// testResultPayload builds one Test Result predicate fixture.
func testResultPayload(layer, verdict string, passedTests ...string) map[string]any {
	return map[string]any{
		"result":      verdict,
		"passedTests": passedTests,
		"quoin":       map[string]any{"layer": layer, "invocationId": "t42-fixture-invocation"},
	}
}

// reportPayload builds one release-report predicate fixture.
func reportPayload(kind, result string) map[string]any {
	return map[string]any{"kind": kind, "result": result, "summary": fmt.Sprintf("%s fixture", kind)}
}
