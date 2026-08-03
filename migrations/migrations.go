// Package migrations embeds the goose SQL migrations.
package migrations

import "embed"

// FS contains the *.sql goose migrations.
//
//go:embed *.sql
var FS embed.FS
