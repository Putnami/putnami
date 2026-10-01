// Package wire centralizes the application graph the migrations-feature
// sample shares between its cmd/server and cmd/migrate binaries. Both
// binaries call BuildApp(); only their dispatch differs.
package wire

import (
	"os"

	"go.putnami.dev/app"
	"go.putnami.dev/database"
	"go.putnami.dev/http"
	"go.putnami.dev/platform"

	"go.putnami.dev/examples/migrations-feature/iam"
)

// BuildApp constructs the production application graph. The same
// function feeds:
//
//   - cmd/server/main.go → a.ListenAndServe() (full lifecycle, HTTP up).
//   - cmd/migrate/main.go → migratecli.Run(BuildApp) (Prepare only,
//     no HTTP listener, dispatches up/down/status/verify/inspect
//     against the per-app *migration.Registry).
//
// Adding another feature (secrets, wealth, configs, ...) is one
// extra .Use() line — the migrate CLI picks it up automatically on
// the next boot.
func BuildApp() *app.Application {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		// A defensible placeholder for the sample. Real services
		// resolve their DSN from putnami config; here we surface the
		// expectation when the binary runs without DATABASE_URL set.
		dsn = "postgres://postgres@localhost:5432/migrations_sample?sslmode=disable"
	}

	server := http.NewServerPlugin(http.ServerConfig{Port: 8080})
	server.Use(http.Recovery())
	server.Use(http.RequestID())
	// The feature's business read: it answers from the migrated database.
	server.GET(iam.UsersPath, http.Inject(iam.ListUsers))

	a := app.New("migrations-feature")
	a.Use(database.NewPlugin(database.PluginConfig{
		// Naming the datasource declared in infra/requirements.json lets a
		// managed binding (the "database" config section a deployment or
		// `putnami compose` injects) replace the DSN below; without one, the
		// DSN stays the connection.
		Datasource: "default",
		Pool:       database.PoolConfig{DSN: dsn},
		Migration: &database.MigrationConfig{
			// Sample default: apply on startup so the server entrypoint
			// brings up a working database in one command. Production
			// services typically leave AutoApply false and rely on the
			// migrate CLI / CronJob / pre-rollout migration job.
			AutoApply: true,
		},
	}))
	a.Use(iam.New())
	a.Use(server)
	// The operational surface: /livez, /healthz, /readyz and /version, mounted
	// on the server above. Deployment probes and `putnami qualify` wait on
	// /readyz before any business request; the database plugin's probe is
	// discovered and reported on /healthz.
	a.Use(platform.NewPlugin(platform.Config{}))
	// The single-endpoint probe, GET /_/health, mounted on the same server.
	a.Use(http.NewHealthPlugin())

	return a
}
