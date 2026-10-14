// Package migrations embeds the service's SQL migrations for dbkit.Migrate.
package migrations

import "embed"

// FS holds the NNN_name.{up,down}.sql files in this directory.
//
//go:embed *.sql
var FS embed.FS
