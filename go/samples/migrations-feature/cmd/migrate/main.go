// Migrate binary for the migrations-feature sample.
//
// Same plugin graph as cmd/server (wire.BuildApp), but the entry point
// is migratecli.Run — only the Configure + collectMigrationSources
// phases execute, then the CLI dispatches the requested subcommand
// against the per-app *migration.Registry. No HTTP listener starts.
//
// Adding more features to the sample is one extra .Use(...) line in
// internal/wire/wire.go; this file does not change.
package main

import (
	"os"

	"go.putnami.dev/migratecli"

	"go.putnami.dev/examples/migrations-feature/internal/wire"
)

func main() {
	os.Exit(migratecli.Run(wire.BuildApp))
}
