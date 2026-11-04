// Package migrations embeds the SQL schema migrations so that binaries and
// tests apply them without depending on filesystem paths. The files follow the
// golang-migrate naming scheme <version>_<name>.{up,down}.sql (ADR-019).
//
// This file lives next to the SQL files because go:embed cannot reach parent
// directories; platform/db runs the migrations.
package migrations

import "embed"

// FS holds every *.sql migration at its root.
//
//go:embed *.sql
var FS embed.FS
