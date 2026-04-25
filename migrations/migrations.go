// Package migrations exposes the SQL migration files as an embedded
// filesystem. Lives alongside the .sql files so //go:embed resolves
// regardless of where the binary is built from.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
