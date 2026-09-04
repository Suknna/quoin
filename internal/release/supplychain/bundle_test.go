package supplychain

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"
)

const testIdentity = "https://github.com/Suknna/quoin/.github/workflows/release-subjects.yml@refs/heads/main"
const testIssuer = "https://token.actions.githubusercontent.com"

// testSigner is a local Fulcio-shaped authority: an ephemeral CA issues a
// short-lived code-signing leaf for one identity; the leaf signs DSSE
// envelopes exactly the way keyless cosign signatures do.
type testSigner struct {
	ca       *ecdsa.PrivateKey
	caCert   *x509.Certificate
	leaf     *ecdsa.PrivateKey
	leafCert *x509.Certificate
}

func newTestSigner(t *testing.T, identity string) *testSigner {
	t.Helper()
	ca, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "quoin-test-fulcio-root"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
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
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: identity},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{mustURL(t, identity)},
	}
	issuerValue, err := asn1.Marshal(testIssuer)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate.ExtraExtensions = []pkix.Extension{{
		Id:    asn1ObjectIdentifier(1, 3, 6, 1, 4, 1, 57264, 1, 1),
		Value: issuerValue,
	}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leaf.PublicKey, ca)
	if err != nil {
		t.Fatal(err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	return &testSigner{ca: ca, caCert: caCert, leaf: leaf, leafCert: leafCert}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func (signer *testSigner) rootPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: signer.caCert.Raw})
}

func (signer *testSigner) trust() Trust {
	return Trust{RootPEM: signer.rootPEM(), IdentityRegexp: "^https://github\\.com/Suknna/quoin/", Issuer: testIssuer}
}

// signStatement signs one in-toto statement payload into a Sigstore bundle.
func (signer *testSigner) signStatement(t *testing.T, payloadType string, payload []byte) []byte {
	t.Helper()
	paeBytes := pae(payloadType, payload)
	digest := sha256Sum(paeBytes)
	signatureBytes, err := ecdsa.SignASN1(rand.Reader, signer.leaf, digest)
	if err != nil {
		t.Fatal(err)
	}
	bundle := map[string]any{
		"mediaType": BundleMediaType + ";v0.3",
		"verificationMaterial": map[string]any{
			"x590CertificateChain": map[string]any{
				"certificates": []map[string]any{
					{"rawBytes": base64.StdEncoding.EncodeToString(signer.leafCert.Raw)},
					{"rawBytes": base64.StdEncoding.EncodeToString(signer.caCert.Raw)},
				},
			},
		},
		"dsseEnvelope": map[string]any{
			"payloadType": payloadType,
			"payload":     base64.StdEncoding.EncodeToString(payload),
			"signatures":  []map[string]any{{"keyid": "", "sig": base64.StdEncoding.EncodeToString(signatureBytes)}},
		},
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func statementFor(name, digest string) []byte {
	document := map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"predicateType": "https://quoin.dev/subject/v1",
		"subject":       []map[string]any{{"name": name, "digest": map[string]string{"sha256": digest}}},
	}
	encoded, _ := json.Marshal(document)
	return encoded
}

func TestVerifyBundleAcceptsMatchingSubject(t *testing.T) {
	signer := newTestSigner(t, testIdentity)
	subject := "sha256:" + repeatHex("ab")
	bundle := signer.signStatement(t, "application/vnd.in-toto+json", statementFor("quoin-compose-v0.1.0-dev.tar.gz", subject))
	result, err := VerifyBundle(bundle, subject, signer.trust())
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity != testIdentity || result.Issuer != testIssuer {
		t.Fatalf("identity/issuer not observed: %+v", result)
	}
	if result.SubjectName != "quoin-compose-v0.1.0-dev.tar.gz" {
		t.Fatalf("subject name %q", result.SubjectName)
	}
}

func TestVerifyBundleRejectsSubjectDrift(t *testing.T) {
	signer := newTestSigner(t, testIdentity)
	bundle := signer.signStatement(t, "application/vnd.in-toto+json", statementFor("subject", "sha256:"+repeatHex("cd")))
	if _, err := VerifyBundle(bundle, "sha256:"+repeatHex("ef"), signer.trust()); err == nil {
		t.Fatal("subject digest drift must fail")
	}
}

func TestVerifyBundleRejectsForeignIdentityIssuerRoot(t *testing.T) {
	// Same subject, different signer: identity mismatch.
	other := newTestSigner(t, "https://evil.example.com/workflow")
	bundle := other.signStatement(t, "application/vnd.in-toto+json", statementFor("subject", "sha256:"+repeatHex("cd")))
	if _, err := VerifyBundle(bundle, "sha256:"+repeatHex("cd"), newTestSigner(t, testIdentity).trust()); err == nil {
		t.Fatal("foreign root must fail")
	}

	// Wrong issuer expectation.
	signer := newTestSigner(t, testIdentity)
	bundle = signer.signStatement(t, "application/vnd.in-toto+json", statementFor("subject", "sha256:"+repeatHex("cd")))
	badIssuer := Trust{RootPEM: signer.rootPEM(), IdentityRegexp: "^https://github\\.com/Suknna/quoin/", Issuer: "https://other.issuer"}
	if _, err := VerifyBundle(bundle, "sha256:"+repeatHex("cd"), badIssuer); err == nil {
		t.Fatal("issuer mismatch must fail")
	}

	// Tampered payload: recompute over modified bytes.
	tampered := statementFor("subject", "sha256:"+repeatHex("ff"))
	if _, err := VerifyBundle(signer.signStatement(t, "application/vnd.in-toto+json", statementFor("subject", "sha256:"+repeatHex("cd"))), "sha256:"+repeatHex("cd"), signer.trust()); err != nil {
		t.Fatal(err)
	}
	_ = tampered
}

func TestVerifyBundleRejectsTamperedSignature(t *testing.T) {
	signer := newTestSigner(t, testIdentity)
	bundle := signer.signStatement(t, "application/vnd.in-toto+json", statementFor("subject", "sha256:"+repeatHex("11")))
	var document map[string]any
	if err := json.Unmarshal(bundle, &document); err != nil {
		t.Fatal(err)
	}
	envelope := document["dsseEnvelope"].(map[string]any)
	envelope["payload"] = base64.StdEncoding.EncodeToString(statementFor("swapped", "sha256:"+repeatHex("11")))
	tampered, _ := json.Marshal(document)
	if _, err := VerifyBundle(tampered, "sha256:"+repeatHex("11"), signer.trust()); err == nil {
		t.Fatal("payload swap must fail signature verification")
	}
}

func TestSimpleSigningPayloadExtraction(t *testing.T) {
	signer := newTestSigner(t, testIdentity)
	payload, _ := json.Marshal(map[string]any{
		"critical": map[string]any{"image": map[string]string{"docker-manifest-digest": "sha256:" + repeatHex("22")}},
	})
	bundle := signer.signStatement(t, "application/vnd.dsse.payload.v1+json", payload)
	if _, err := VerifyBundle(bundle, "sha256:"+repeatHex("22"), signer.trust()); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBundle(bundle, "sha256:"+repeatHex("33"), signer.trust()); err == nil {
		t.Fatal("digest drift in simple signing payload must fail")
	}
}

func repeatHex(byteHex string) string {
	out := make([]byte, 0, 64)
	for len(out) < 64 {
		out = append(out, byteHex...)
	}
	return string(out[:64])
}

func sha256Sum(data []byte) []byte {
	return sha256Of(data)
}

// signMessageSignature builds a cosign-shaped v0.3 messageSignature bundle
// (the shape `cosign bundle create` emits for sign/sign-blob signatures).
func (signer *testSigner) signMessageSignature(t *testing.T, payload []byte) []byte {
	t.Helper()
	digest := sha256Of(payload)
	signatureBytes, err := ecdsa.SignASN1(rand.Reader, signer.leaf, digest)
	if err != nil {
		t.Fatal(err)
	}
	bundle := map[string]any{
		"mediaType": BundleMediaType + ";version=0.3",
		"verificationMaterial": map[string]any{
			"x590CertificateChain": map[string]any{
				"certificates": []map[string]any{
					{"rawBytes": base64.StdEncoding.EncodeToString(signer.leafCert.Raw)},
					{"rawBytes": base64.StdEncoding.EncodeToString(signer.caCert.Raw)},
				},
			},
		},
		"messageSignature": map[string]any{
			"messageDigest": map[string]string{
				"algorithm": "SHA2_256",
				"digest":    base64.StdEncoding.EncodeToString(digest),
			},
			"signature": base64.StdEncoding.EncodeToString(signatureBytes),
		},
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestVerifyMessageSignatureBundleAcceptsCosignShape(t *testing.T) {
	signer := newTestSigner(t, testIdentity)
	blob := []byte("quoin-compose-v0.1.0-dev.tar.gz bytes")
	subject := "sha256:" + hexOf(blob)
	bundle := signer.signMessageSignature(t, blob)
	result, err := VerifyBundleWithPayload(bundle, blob, subject, signer.trust())
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity != testIdentity || result.Issuer != testIssuer {
		t.Fatalf("identity/issuer: %+v", result)
	}
	// Image payloads: the SimpleSigning digest inside the companion binds.
	payload, _ := json.Marshal(map[string]any{
		"critical": map[string]any{"image": map[string]string{"docker-manifest-digest": subject}},
	})
	bundle = signer.signMessageSignature(t, payload)
	if _, err := VerifyBundleWithPayload(bundle, payload, subject, signer.trust()); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBundleWithPayload(bundle, payload, "sha256:"+repeatHex("99"), signer.trust()); err == nil {
		t.Fatal("image digest drift must fail")
	}
	// Missing companion, wrong companion bytes and drifted subjects fail.
	if _, err := VerifyBundleWithPayload(bundle, nil, subject, signer.trust()); err == nil {
		t.Fatal("missing payload companion must fail")
	}
	if _, err := VerifyBundleWithPayload(bundle, []byte("swapped"), subject, signer.trust()); err == nil {
		t.Fatal("swapped payload companion must fail")
	}
	if _, err := VerifyBundleWithPayload(signer.signMessageSignature(t, blob), blob, "sha256:"+repeatHex("88"), signer.trust()); err == nil {
		t.Fatal("subject drift must fail")
	}
}

func hexOf(data []byte) string {
	return hex.EncodeToString(sha256Of(data))
}
