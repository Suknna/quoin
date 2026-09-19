// Generated contract projection. DO NOT EDIT.

package contract

import "regexp"

// ProtoAuthorityFingerprint is the SHA-256 fingerprint of the complete,
// ordered Proto authority set. It is compiled into every component and is not
// configurable at deployment time.
const ProtoAuthorityFingerprint = "9d74eba1dcbb34e8b7cbb2ae8079ef9b7e3151dfb14aafc2a6a7aece8950d4b3"

var protoAuthorityFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidProtoAuthorityFingerprint accepts only the canonical lowercase SHA-256
// wire form. Empty, malformed, and alternate encodings fail closed.
func ValidProtoAuthorityFingerprint(value string) bool {
	return protoAuthorityFingerprintPattern.MatchString(value)
}
