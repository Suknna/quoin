// Package supplychain is the offline pre-qualification release gate for
// signed subjects and BuildKit attestations (OPS-SUPPLY-001/002). It verifies
// Sigstore bundles against a pinned trust root with certificate
// identity/issuer checks and subject-digest equality, and verifies that every
// image index carries SPDX SBOM and SLSA provenance attestations whose
// subjects equal the corresponding per-platform image manifest digests. It
// never signs and never stores keys.
package supplychain

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// BundleMediaType is the Sigstore bundle format family this gate consumes.
// cosign v2.5 emits ";version=0.3" while the protobuf-specs canonical form is
// ";v0.3"; both name the same v0.3 bundle schema.
const BundleMediaType = "application/vnd.dev.sigstore.bundle+json"

func isBundleMediaType(mediaType string) bool {
	return mediaType == BundleMediaType+";v0.3" || mediaType == BundleMediaType+";version=0.3"
}

// oidcIssuerExtensionOID is Fulcio's certificate extension carrying the OIDC
// issuer of the signing identity (1.3.6.1.4.1.57264.1.1).
var oidcIssuerExtensionOID = asn1ObjectIdentifier(1, 3, 6, 1, 4, 1, 57264, 1, 1)

// Trust pins the offline verification authority.
type Trust struct {
	// RootPEM is the pinned trust anchor (Fulcio root in CI, the ephemeral
	// qualification CA in the local acceptance run).
	RootPEM []byte
	// IdentityRegexp must match the signing certificate's SAN URI identity.
	IdentityRegexp string
	// Issuer is the expected OIDC issuer URL carried by the certificate.
	Issuer string
}

// Compile validates and resolves the trust expectations.
func (trust Trust) Compile() (*compiledTrust, error) {
	if len(trust.RootPEM) == 0 {
		return nil, errors.New("supplychain trust root is empty")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(trust.RootPEM) {
		return nil, errors.New("supplychain trust root is not a PEM certificate")
	}
	if trust.IdentityRegexp == "" || trust.Issuer == "" {
		return nil, errors.New("supplychain trust needs identity and issuer expectations")
	}
	identity, err := regexp.Compile(trust.IdentityRegexp)
	if err != nil {
		return nil, fmt.Errorf("identity regexp: %w", err)
	}
	return &compiledTrust{roots: pool, identity: identity, issuer: trust.Issuer}, nil
}

type compiledTrust struct {
	roots    *x509.CertPool
	identity *regexp.Regexp
	issuer   string
}

// bundle mirrors the Sigstore bundle v0.3 fields this gate reads. Content
// is either a DSSE envelope (attestation-style signatures) or a plain
// messageSignature over the payload bytes (cosign sign/sign-blob bundles).
type bundle struct {
	MediaType            string `json:"mediaType"`
	VerificationMaterial struct {
		X590CertificateChain struct {
			Certificates []struct {
				RawBytes string `json:"rawBytes"`
			} `json:"certificates"`
		} `json:"x590CertificateChain"`
	} `json:"verificationMaterial"`
	DSSEEnvelope     *dsseEnvelope     `json:"dsseEnvelope"`
	MessageSignature *messageSignature `json:"messageSignature"`
}

type messageSignature struct {
	MessageDigest struct {
		Algorithm string `json:"algorithm"`
		Digest    string `json:"digest"`
	} `json:"messageDigest"`
	Signature string `json:"signature"`
}

type dsseEnvelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []signature `json:"signatures"`
}

type signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// statement is the in-toto Statement v1 subjects this gate reads.
type statement struct {
	Type          string `json:"_type"`
	PredicateType string `json:"predicateType"`
	Subject       []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
}

// simpleSigning is cosign's image-signature payload when the DSSE envelope
// wraps a legacy SimpleSigning document.
type simpleSigning struct {
	Critical struct {
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
	} `json:"critical"`
}

// VerificationResult reports what the gate observed.
type VerificationResult struct {
	Identity     string `json:"identity"`
	Issuer       string `json:"issuer"`
	SubjectName  string `json:"subject_name"`
	HasTlogEntry bool   `json:"has_tlog_entry"`
}

// VerifyBundle verifies one Sigstore bundle over one subject digest: the DSSE
// signature must verify with the leaf certificate's public key, the chain
// must anchor in the pinned root, the certificate identity/issuer must match
// the expectations, and the payload's subject digest must equal the subject's
// digest exactly.
func VerifyBundle(bundleJSON []byte, subjectDigest string, trust Trust) (VerificationResult, error) {
	return VerifyBundleWithPayload(bundleJSON, nil, subjectDigest, trust)
}

// VerifyBundleWithPayload also accepts the signed payload companion for
// messageSignature bundles (cosign sign/sign-blob): the payload bytes are
// hashed and signature-verified directly, and the subject digest is either
// the SimpleSigning image digest inside the payload or the payload bytes'
// own SHA-256 (blob subjects).
func VerifyBundleWithPayload(bundleJSON, payloadCompanion []byte, subjectDigest string, trust Trust) (VerificationResult, error) {
	compiled, err := trust.Compile()
	if err != nil {
		return VerificationResult{}, err
	}
	var document bundle
	if err := json.Unmarshal(bundleJSON, &document); err != nil {
		return VerificationResult{}, fmt.Errorf("bundle: %w", err)
	}
	if !isBundleMediaType(document.MediaType) {
		return VerificationResult{}, fmt.Errorf("bundle media type %q", document.MediaType)
	}
	leaf, _, result, err := verifyChain(&document, compiled)
	if err != nil {
		return result, err
	}
	publicKey, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return result, fmt.Errorf("leaf certificate key is %T, want ECDSA", leaf.PublicKey)
	}
	switch {
	case document.DSSEEnvelope != nil:
		payload, err := verifyDSSE(document.DSSEEnvelope, publicKey)
		if err != nil {
			return result, err
		}
		digests, name, err := subjectDigests(document.DSSEEnvelope.PayloadType, payload)
		if err != nil {
			return result, err
		}
		if len(digests) != 1 {
			return result, fmt.Errorf("payload carries %d subjects, want exactly 1", len(digests))
		}
		if digests[0] != subjectDigest {
			return result, fmt.Errorf("bundle subject digest %s does not equal subject %s", digests[0], subjectDigest)
		}
		result.SubjectName = name
		return result, nil
	case document.MessageSignature != nil:
		if payloadCompanion == nil {
			return result, errors.New("messageSignature bundle needs its payload companion")
		}
		digest, err := verifyMessageSignature(document.MessageSignature, payloadCompanion, publicKey)
		if err != nil {
			return result, err
		}
		// The payload is either a SimpleSigning document (image subjects) or
		// the subject bytes themselves (blob subjects).
		var signing simpleSigning
		if json.Unmarshal(payloadCompanion, &signing) == nil && signing.Critical.Image.DockerManifestDigest != "" {
			if signing.Critical.Image.DockerManifestDigest != subjectDigest {
				return result, fmt.Errorf("payload image digest %s does not equal subject %s", signing.Critical.Image.DockerManifestDigest, subjectDigest)
			}
			return result, nil
		}
		if digest != subjectDigest {
			return result, fmt.Errorf("bundle subject digest %s does not equal subject %s", digest, subjectDigest)
		}
		return result, nil
	default:
		return result, errors.New("bundle carries neither dsse envelope nor message signature")
	}
}

// verifyMessageSignature checks the plain signature over the payload bytes
// and the recorded message digest, returning the payload SHA-256 digest.
func verifyMessageSignature(entry *messageSignature, payload []byte, publicKey *ecdsa.PublicKey) (string, error) {
	if entry.MessageDigest.Algorithm != "SHA2_256" {
		return "", fmt.Errorf("message digest algorithm %q", entry.MessageDigest.Algorithm)
	}
	rawDigest, err := base64.StdEncoding.DecodeString(entry.MessageDigest.Digest)
	if err != nil || len(rawDigest) != 32 {
		return "", errors.New("message digest is not a base64 sha256")
	}
	computed := sha256.Sum256(payload)
	if !bytes.Equal(computed[:], rawDigest) {
		return "", errors.New("payload companion digest does not equal the bundle message digest")
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(entry.Signature)
	if err != nil {
		return "", fmt.Errorf("signature base64: %w", err)
	}
	if !ecdsa.VerifyASN1(publicKey, computed[:], signatureBytes) {
		return "", errors.New("message signature does not verify with the leaf certificate key")
	}
	return "sha256:" + hex.EncodeToString(computed[:]), nil
}

func verifyChain(document *bundle, compiled *compiledTrust) (*x509.Certificate, []*x509.Certificate, VerificationResult, error) {
	chain := document.VerificationMaterial.X590CertificateChain.Certificates
	if len(chain) == 0 {
		return nil, nil, VerificationResult{}, errors.New("bundle carries no x509 certificate chain")
	}
	certificates := make([]*x509.Certificate, 0, len(chain))
	for _, entry := range chain {
		raw, err := base64.StdEncoding.DecodeString(entry.RawBytes)
		if err != nil {
			return nil, nil, VerificationResult{}, fmt.Errorf("certificate base64: %w", err)
		}
		certificate, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, nil, VerificationResult{}, fmt.Errorf("certificate parse: %w", err)
		}
		certificates = append(certificates, certificate)
	}
	leaf := certificates[0]
	intermediatePool := x509.NewCertPool()
	for _, certificate := range certificates[1:] {
		intermediatePool.AddCert(certificate)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: compiled.roots, Intermediates: intermediatePool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}); err != nil {
		return nil, nil, VerificationResult{}, fmt.Errorf("certificate chain: %w", err)
	}
	result := VerificationResult{HasTlogEntry: false}
	if len(leaf.URIs) == 0 {
		return nil, nil, result, errors.New("leaf certificate has no SAN URI identity")
	}
	identity := leaf.URIs[0].String()
	result.Identity = identity
	if !compiled.identity.MatchString(identity) {
		return nil, nil, result, fmt.Errorf("certificate identity %q does not match the expected identity", identity)
	}
	for _, extension := range leaf.Extensions {
		if !extension.Id.Equal(oidcIssuerExtensionOID) {
			continue
		}
		var issuer string
		if err := unmarshalIssuerExtension(extension.Value, &issuer); err != nil {
			return nil, nil, result, err
		}
		result.Issuer = issuer
		if issuer != compiled.issuer {
			return nil, nil, result, fmt.Errorf("certificate issuer %q does not equal the expected issuer %q", issuer, compiled.issuer)
		}
	}
	if result.Issuer == "" {
		return nil, nil, result, errors.New("leaf certificate carries no OIDC issuer extension")
	}
	return leaf, certificates[1:], result, nil
}

// verifyDSSE checks the envelope's first signature over the DSSE PAE with the
// leaf key and returns the decoded payload.
func verifyDSSE(envelope *dsseEnvelope, publicKey *ecdsa.PublicKey) ([]byte, error) {
	if len(envelope.Signatures) == 0 {
		return nil, errors.New("dsse envelope has no signature")
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return nil, fmt.Errorf("dsse payload base64: %w", err)
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(envelope.Signatures[0].Sig)
	if err != nil {
		return nil, fmt.Errorf("dsse signature base64: %w", err)
	}
	pae := pae(envelope.PayloadType, payload)
	digest := sha256.Sum256(pae)
	if !ecdsa.VerifyASN1(publicKey, digest[:], signatureBytes) {
		return nil, errors.New("dsse signature does not verify with the leaf certificate key")
	}
	return payload, nil
}

// pae implements the DSSE Pre-Authentication Encoding.
func pae(payloadType string, payload []byte) []byte {
	header := fmt.Sprintf("DSSEv1 %d %s %d ", len(payloadType), payloadType, len(payload))
	return append([]byte(header), payload...)
}

// DSSEPAE exposes the DSSE Pre-Authentication Encoding for signers that
// produce bundles the gate verifies.
func DSSEPAE(payloadType string, payload []byte) []byte {
	return pae(payloadType, payload)
}

// subjectDigests extracts the subject SHA-256 digests from either an in-toto
// Statement payload or a wrapped cosign SimpleSigning payload.
func subjectDigests(payloadType string, payload []byte) ([]string, string, error) {
	switch payloadType {
	case "application/vnd.in-toto+json":
		var document statement
		if err := json.Unmarshal(payload, &document); err != nil {
			return nil, "", fmt.Errorf("in-toto statement: %w", err)
		}
		digests := make([]string, 0, len(document.Subject))
		name := ""
		for _, subject := range document.Subject {
			digest := subject.Digest["sha256"]
			if digest == "" {
				return nil, "", fmt.Errorf("subject %q has no sha256 digest", subject.Name)
			}
			if !strings.HasPrefix(digest, "sha256:") {
				digest = "sha256:" + digest
			}
			digests = append(digests, digest)
			if name == "" {
				name = subject.Name
			}
		}
		return digests, name, nil
	case "application/vnd.dsse.payload.v1+json":
		var signing simpleSigning
		if err := json.Unmarshal(payload, &signing); err != nil {
			return nil, "", fmt.Errorf("simple signing payload: %w", err)
		}
		digest := signing.Critical.Image.DockerManifestDigest
		if digest == "" {
			return nil, "", errors.New("simple signing payload has no image digest")
		}
		return []string{digest}, "", nil
	default:
		return nil, "", fmt.Errorf("unsupported dsse payload type %q", payloadType)
	}
}

func sha256Of(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}
