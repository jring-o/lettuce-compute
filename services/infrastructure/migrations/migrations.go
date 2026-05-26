package migrations

import "embed"

// FS embeds all SQL migration files into the binary so migrations
// can be applied without external file dependencies at runtime.
//
//go:embed *.sql
var FS embed.FS
