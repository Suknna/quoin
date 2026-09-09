// Package deploy exposes the direct Compose document as the single source for
// generated Compose projections as well as user-operated auxiliary deployments.
package deploy

import _ "embed"

// ComposeTemplate is the authoritative six-service Compose topology. Renderers
// may patch deployment-specific image references, bind mounts, and host ports,
// but must not define a competing service graph.
//
//go:embed compose.yaml
var ComposeTemplate []byte
