// Package activate registers the Putnami Cloud config and secrets sources with
// the go.putnami.dev/config loader, and turns on the default telemetry of every
// application. Blank-import it from a workload's main package:
//
//	import _ "go.putnami.dev/cloud/runtime/activate"
//
// Without this import, config.Load reads only local YAML, CONFIG_DATA and
// env-tag overrides, and configuration held only by the config server is
// absent at runtime.
//
// With it, every go.putnami.dev/app application starts with the telemetry
// plugin from telemetry.FromEnvironment when OTEL_EXPORTER_OTLP_ENDPOINT is
// set: each HTTP request is counted, timed and traced, and pushes to the
// collector carry a GCP ID token whose audience is the collector URL.
// OTEL_SDK_DISABLED=true turns it off.
//
// Registration runs from init(), before the workload calls config.Load or
// app.New. Importing the parent package for its types does not touch the
// loader's global discoverer registry or the default plugins; only this blank
// import does.
package activate

import (
	"go.putnami.dev/app"
	config "go.putnami.dev/config"
	"go.putnami.dev/telemetry"

	cloudruntime "go.putnami.dev/cloud/runtime"
)

func init() {
	cloudruntime.Register(config.RegisterSourceDiscoverer)
	app.RegisterDefaultPlugin(defaultTelemetry)
}

// defaultTelemetry is the telemetry plugin every application starts with, or
// nil when the environment names no collector.
func defaultTelemetry() app.Plugin {
	return telemetry.FromEnvironment(telemetry.EnvironmentOptions{
		BearerTokenSource: telemetry.NewGCPIDTokenSource,
	})
}
