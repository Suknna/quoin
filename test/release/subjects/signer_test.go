package subjects_test

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
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/release/supplychain"
)

const qualificationIdentity = "https://github.com/Suknna/quoin/.github/workflows/release-subjects.yml@refs/heads/main"
const qualificationIssuer = "https://token.actions.githubusercontent.com"

// qualificationSigner is the local pre-qualification signing authority: an
// ephemeral Fulcio-shaped CA issues one short-lived code-signing certificate
// for the release-subjects workflow identity; every subject is signed into an
// offline-verifiable Sigstore bundle. No key outlives the test process and
// nothing is written to the repository.
type qualificationSigner struct {
	ca       *ecdsa.PrivateKey
	caCert   *x509.Certificate
	leaf     *ecdsa.PrivateKey
	leafCert *x509.Certificate
}

func newQualificationSigner(t *testing.T) *qualificationSigner {
	t.Helper()
	ca, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "quoin-t39-qualification-root"},
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
	identityURL, err := url.Parse(qualificationIdentity)
	if err != nil {
		t.Fatal(err)
	}
	issuerValue, err := asn1.Marshal(qualificationIssuer)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: qualificationIdentity},
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
	return &qualificationSigner{ca: ca, caCert: caCert, leaf: leaf, leafCert: leafCert}
}

func (signer *qualificationSigner) trustRootPath(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "qualification-root.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: signer.caCert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newQualificationSignerWithIdentity issues a leaf for an arbitrary identity
// under the same qualification root, for the foreign-identity adversarial leg.
func newQualificationSignerWithIdentity(t *testing.T, identity string) *qualificationSigner {
	t.Helper()
	signer := newQualificationSigner(t)
	identityURL, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	issuerValue, err := asn1.Marshal(qualificationIssuer)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: identity},
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
	leafDER, err := x509.CreateCertificate(rand.Reader, template, signer.caCert, &leaf.PublicKey, signer.ca)
	if err != nil {
		t.Fatal(err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	signer.leaf = leaf
	signer.leafCert = leafCert
	return signer
}

// signSubject writes one offline-verifiable bundle over one subject digest.
func (signer *qualificationSigner) signSubject(t *testing.T, bundlesDir, bundleName, subjectName, subjectDigest string) {
	t.Helper()
	statement := map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"predicateType": "https://quoin.dev/release-subject/v1",
		"subject":       []map[string]any{{"name": subjectName, "digest": map[string]string{"sha256": subjectDigest}}},
	}
	payload, err := json.Marshal(statement)
	if err != nil {
		t.Fatal(err)
	}
	paeBytes := supplychain.DSSEPAE("application/vnd.in-toto+json", payload)
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
			"payload":     base64.StdEncoding.EncodeToString(payload),
			"signatures":  []map[string]any{{"keyid": "", "sig": base64.StdEncoding.EncodeToString(signature)}},
		},
	}
	encoded, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundlesDir, bundleName), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
}
