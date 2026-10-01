// Command describe is this sample's build-time describe entrypoint. It exists so
// the Go generator can emit a committed infra/requirements.json declaring the
// Postgres datasource the proof's tests exercise: under PUTNAMI_DESCRIBE,
// app.ListenAndServe runs the describe phase (Configure + Describe hooks) and
// exits without opening a connection or serving, and the database plugin's
// Describe hook records the "default" datasource as a framework infra
// requirement. `putnami test` in auto mode then provisions that declared
// datasource and injects DATABASE_TEST_BINDINGS.
//
// The proof itself lives entirely in the sample's tests (uow_test.go,
// conformance_test.go), which drive a provisioned Postgres through the shared
// test provider; this binary is never run as a server.
package main

import (
	"os"

	"go.putnami.dev/app"
	"go.putnami.dev/database"
	"go.putnami.dev/logger"
)

func main() {
	a := app.New("unit-of-work-proof")
	a.Use(database.NewPlugin(database.PluginConfig{Datasource: "default"}))
	if err := a.ListenAndServe(); err != nil {
		logger.New("describe", logger.LevelError).Error("describe failed", err)
		os.Exit(1)
	}
}
