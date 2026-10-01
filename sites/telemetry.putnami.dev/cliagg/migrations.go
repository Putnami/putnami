package cliagg

import (
	"embed"

	"go.putnami.dev/database"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	// Datasource is the logical database the CLI-usage aggregates live in. It is
	// declared (name + owning schema) so the build's describe phase emits an
	// infra requirement the deployer provisions and binds; the managed binding
	// supplies the physical connection at deploy.
	Datasource = "telemetry"

	// migrationSchema is the schema the aggregate tables are created in.
	migrationSchema = "public"

	// MigrationNamespace prefixes every migration name and the migration
	// state-store id. It is identity, not a schema name. Exported because it is
	// also the key of the migration and source-discoverer contributions the
	// build publishes, which the workload's feature declaration proves against.
	MigrationNamespace = "cliagg"
)

// Source returns the SQL migration source that owns aggregate counts,
// contributor membership, and the bounded operational ledgers. The retention
// migration deliberately requires Cloud SQL's pg_cron metadata database to be
// this telemetry database, with pg_cron admin-installed there and its scheduling
// function granted to the migration role; pg_cron permits only one metadata
// database per cluster, and silently omitting the scheduler would violate
// contributor expiry. The source is colocated with the code that
// writes the projection, following the per-feature migration-ownership pattern
// (see go/samples/migrations-feature/iam). It is exposed so escape-hatch callers
// — tests, one-off apply binaries — can apply the same migrations through
// database.ApplyToPool without the app lifecycle.
func Source() database.SQLSource {
	return database.NewSQLSource(
		MigrationNamespace,
		database.Datasource{Name: Datasource, Schema: migrationSchema},
		migrationsFS,
	)
}
