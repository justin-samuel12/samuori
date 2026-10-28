// Package migrations embeds the per-dialect schema migrations (ADR 0017).
package migrations

import "embed"

//go:embed sqlite/*.sql postgres/*.sql
var FS embed.FS
