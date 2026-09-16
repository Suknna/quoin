package upgrade

// isReleasedPredecessorDigest admits only released schema identities before
// the authenticated, read-only preflight checks their exact ledger and backup
// gates. Each digest is pinned to a byte-exact fixture in this package's
// testdata directory.
func isReleasedPredecessorDigest(digest string) bool {
	switch digest {
	case legacyMetricsBusinessSchemaDigest,
		directInvestigationMetricsSchemaDigest,
		declarationCutoverSchemaDigest,
		pluginRegistrySchemaDigest,
		authAuditPredecessorSchemaDigest:
		return true
	}
	return false
}

// IsDeclarationPredecessor admits only released schema identities before the
// authenticated, read-only preflight checks their exact ledger and backup
// gates. The ADR-0004 predecessor was the last declaration-governed release;
// the auth-audit predecessor is the last pre-audit release.
func IsDeclarationPredecessor(version, digest string) bool {
	return version == "v1" && isReleasedPredecessorDigest(digest)
}

// IsSupportedMigrationSource includes the one pinned unpublished acceptance
// schema without presenting that development build as a released predecessor,
// plus the released inspection-freeze predecessor.
func IsSupportedMigrationSource(version, digest string) bool {
	return version == "v1" && (isReleasedPredecessorDigest(digest) || digest == authSimplificationPredecessorDigest || digest == inspectionFreezePredecessorSchemaDigest)
}
