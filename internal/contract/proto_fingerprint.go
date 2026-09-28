// Generated contract projection. DO NOT EDIT.

package contract

import "regexp"

// ProtoAuthorityFingerprint is the SHA-256 fingerprint of the complete,
// ordered Proto authority set. It is compiled into every component and is not
// configurable at deployment time.
const ProtoAuthorityFingerprint = "f3c1094038817d51aae8a9a8421371149246dd9c72010ab1818fce729a859279"

var protoAuthorityFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidProtoAuthorityFingerprint accepts only the canonical lowercase SHA-256
// wire form. Empty, malformed, and alternate encodings fail closed.
func ValidProtoAuthorityFingerprint(value string) bool {
	return protoAuthorityFingerprintPattern.MatchString(value)
}
