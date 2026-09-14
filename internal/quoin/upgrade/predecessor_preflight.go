package upgrade

// IsDeclarationPredecessor admits only released schema identities before the
// authenticated, read-only preflight checks their exact ledger and backup
// gates. The ADR-0004 predecessor is the last declaration-governed release.
func IsDeclarationPredecessor(version, digest string) bool {
	return version == "v1" && (digest == declarationCutoverSchemaDigest || digest == pluginRegistrySchemaDigest)
}
