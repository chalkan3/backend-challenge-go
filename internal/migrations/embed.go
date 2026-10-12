// Package migrations embeds the SQL migration files so the binary can apply
// them on startup (and expose status/down for reversal).
package migrations

import "embed"

// FS holds every .sql migration, consumed by goose.
//
//go:embed *.sql
var FS embed.FS
