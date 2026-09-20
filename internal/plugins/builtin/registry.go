// Package builtin declares the deployment's built-in plugins (ADR-0004,
// reworked by ADR-0011). Each plugin file registers itself from init()
// through plugins.Register — the interfaces + registry + blank-import
// assembly. Hosts opt in by blank-importing this package:
//
//	import _ "github.com/Suknna/quoin/internal/plugins/builtin"
//
// cmd/quoin and cmd/stele import it; cmd/plinth does not — Plinth is
// plugin-unaware by construction. Each plugin owns its identity, its
// declarative catalogs AND its typed tool implementations in one place, so
// declaration and implementation are one authority and adding a plugin never
// edits a core table.
//
// The metrics plugins (prometheus/thanos) share one PromQL query tool
// contract: the registry deduplicates identical manifests and provenance
// lists every enabled contributing provider; authorization resolves the
// actual source connection. Outbound execution runs through the Stele
// gateway (ADR-0011): handlers build protocol requests and interpret
// responses; credentials, TLS and rate limiting never appear here.
package builtin
