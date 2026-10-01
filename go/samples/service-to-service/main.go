// Command service-to-service is a Putnami Go sample: an Items provider that
// exposes a typed HTTP API, publishes its OpenAPI spec, and generates a typed Go
// client (clients/go) consumers import to call it. See ./consumer for the
// consumer side and README.md for the full walkthrough.
package main

import (
	"log"

	"go.putnami.dev/examples/service-to-service/service"
)

func main() {
	if err := service.NewApp().ListenAndServe(); err != nil {
		log.Fatalf("service-to-service: %v", err)
	}
}
