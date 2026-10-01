// Package iam is the feature plugin that owns the user-identity domain
// for the migrations-feature sample. It demonstrates the canonical
// pattern every Putnami feature follows:
//
//   - Migrations live inside the package, colocated with the code that
//     reads/writes them (migrations/NNN_name.up.sql, optional .down.sql).
//   - The Plugin embeds those files with //go:embed and implements
//     app.MigrationContributor.MigrationSources(); no manual file walks,
//     no DefaultRegistry.Register calls, no per-feature wiring in the
//     service's cmd/ binaries.
//   - The framework's lifecycle Migrate phase picks the migrations up
//     automatically when the plugin is .Use()'d.
package iam

import (
	"embed"

	"go.putnami.dev/database"
	"go.putnami.dev/migration"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Plugin is the iam feature plugin.
type Plugin struct{}

// New returns a configured Plugin. Real features would take config
// here; the sample is intentionally stateless.
func New() *Plugin { return &Plugin{} }

// Name is the plugin identifier — also the migration namespace prefix
// stamped on every iam migration (iam/20260520120000_create_users etc.).
func (p *Plugin) Name() string { return "iam" }

// MigrationSources implements app.MigrationContributor. The framework
// invokes this once during the Migrate lifecycle phase to discover
// what this plugin owns.
func (p *Plugin) MigrationSources() []migration.Source {
	return []migration.Source{Source()}
}

// Source returns the iam feature's SQL migration source. The lifecycle
// path (via MigrationSources above) is the default; Source is exposed
// so escape-hatch callers — privileged bootstrap binaries, tests —
// can apply the same migrations through database.ApplyToPool /
// database.LoadSQLSource without going through app.MigrationContributor.
// See cmd/bootstrap/main.go for the worked example.
//
// Returns the concrete database.SQLSource (rather than migration.Source)
// so callers can pass it directly to ApplyToPool's variadic SQLSource
// parameter; the type also satisfies migration.Source for the
// MigrationContributor path above.
func Source() database.SQLSource {
	// The migration namespace "iam" is the feature's identity (it prefixes
	// every migration name); the schema is declared separately. These tables
	// (iam_users, …) live in the default "public" schema, so that — not the
	// namespace — is what the infra requirements must name.
	return database.NewSQLSource("iam", database.Datasource{Name: "default", Schema: "public"}, migrationsFS)
}
