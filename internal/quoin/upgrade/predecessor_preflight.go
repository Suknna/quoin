package upgrade

// IsDeclarationPredecessor admits only the released schema identity before the
// authenticated, read-only preflight checks its exact ledger and backup gates.
func IsDeclarationPredecessor(version, digest string) bool {
	return version == "v1" && digest == declarationCutoverSchemaDigest
}
