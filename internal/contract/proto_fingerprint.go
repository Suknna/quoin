// Generated contract projection. DO NOT EDIT.

package contract

import "regexp"

// ProtoAuthorityFingerprint is the SHA-256 fingerprint of the complete,
// ordered Proto authority set. It is compiled into every component and is not
// configurable at deployment time.
const ProtoAuthorityFingerprint = "587c6fe421a24c2ed4df729c364e3d87e66871ddc626521c89514462772b7125"

var protoAuthorityFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidProtoAuthorityFingerprint accepts only the canonical lowercase SHA-256
// wire form. Empty, malformed, and alternate encodings fail closed.
func ValidProtoAuthorityFingerprint(value string) bool {
	return protoAuthorityFingerprintPattern.MatchString(value)
}
