// Generated contract projection. DO NOT EDIT.

package contract

import "regexp"

// ProtoAuthorityFingerprint is the SHA-256 fingerprint of the complete,
// ordered Proto authority set. It is compiled into every component and is not
// configurable at deployment time.
const ProtoAuthorityFingerprint = "61b257549e87b2582a48395bd99b8e95e95e5d3e756a96c434aec73882fc413f"

var protoAuthorityFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidProtoAuthorityFingerprint accepts only the canonical lowercase SHA-256
// wire form. Empty, malformed, and alternate encodings fail closed.
func ValidProtoAuthorityFingerprint(value string) bool {
	return protoAuthorityFingerprintPattern.MatchString(value)
}
