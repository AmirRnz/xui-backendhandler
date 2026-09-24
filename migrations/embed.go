package migrations

import "embed"

// Files contains ordered database migrations.
//
//go:embed *.sql
var Files embed.FS
