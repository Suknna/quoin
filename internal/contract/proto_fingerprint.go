// Generated contract projection. DO NOT EDIT.

package contract

import "regexp"

// ProtoAuthorityFingerprint is the SHA-256 fingerprint of the complete,
// ordered Proto authority set. It is compiled into every component and is not
// configurable at deployment time.
const ProtoAuthorityFingerprint = "8d562800308b7e50c4a510cffad4e5481e5d4d182789da657d3d57361066eb6d"

var protoAuthorityFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidProtoAuthorityFingerprint accepts only the canonical lowercase SHA-256
// wire form. Empty, malformed, and alternate encodings fail closed.
func ValidProtoAuthorityFingerprint(value string) bool {
	return protoAuthorityFingerprintPattern.MatchString(value)
}
