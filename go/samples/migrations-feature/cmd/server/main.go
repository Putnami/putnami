// Server binary for the migrations-feature sample.
//
// Runs the application graph defined in internal/wire — when AutoApply
// is enabled on the database plugin (it is, by default in this sample),
// migrations are applied on startup before HTTP serves traffic.
package main

import (
	"os"

	"go.putnami.dev/logger"

	"go.putnami.dev/examples/migrations-feature/internal/wire"
)

func main() {
	if err := wire.BuildApp().ListenAndServe(); err != nil {
		logger.New("main", logger.LevelError).Error("application failed", err)
		os.Exit(1)
	}
}
