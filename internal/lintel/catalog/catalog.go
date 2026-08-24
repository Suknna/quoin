// Package catalog owns the single build-time Journey Catalog artifact consumed
// byte-for-byte by both Lintel and Quoin.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Version identifies this immutable catalog artifact.
const Version = "v1"

// JCS is the RFC 8785 canonical JSON catalog. It deliberately exposes only
// the built-in authentication probe used to publish a manually logged-in
// Browser Identity. No user-authored browser program is accepted.
const JCS = `{"catalog_version":"v1","journeys":{"authentication.url-prefix.v1":{"evidence_kinds":[],"output_schema":{"additionalProperties":false,"properties":{"reasonCode":{"type":["string","null"]},"result":{"enum":["Authenticated","Unauthenticated","Indeterminate"]}},"required":["result","reasonCode"],"type":"object"},"params_schema":{"additionalProperties":false,"properties":{"authenticatedUrlPrefix":{"format":"uri","type":"string"}},"required":["authenticatedUrlPrefix"],"type":"object"},"purpose":"authentication_probe","steps_digest":"e7d3461d596a7b0b58fd0e2bd8403003903e876e16022f7f11b9852dd945d405","summary":"Reports whether Chromium's foreground URL has the configured authenticated prefix.","version":1}}}`

// Bytes returns the exact embedded catalog bytes. Callers must not re-marshal
// a parsed representation before calculating the digest.
func Bytes() []byte { return []byte(JCS) }

// Digest is lowercase SHA-256(JCS).
func Digest() string {
	sum := sha256.Sum256(Bytes())
	return hex.EncodeToString(sum[:])
}

// Document parses the embedded artifact for Quoin's offline schema checks.
func Document() (map[string]any, error) {
	var document map[string]any
	if err := json.Unmarshal(Bytes(), &document); err != nil {
		return nil, fmt.Errorf("decode embedded Journey Catalog: %w", err)
	}
	return document, nil
}
