// Package web embeds the reviewed production frontend build produced by
// `pnpm --dir web build`; it is a generated projection, never hand-edited.
package web

import "embed"

//go:embed all:dist
var Files embed.FS
