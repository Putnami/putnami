package main

import (
	"os"
	"strings"

	"go.putnami.dev/app"
	"go.putnami.dev/database"
	"go.putnami.dev/logger"
	telemetry "go.putnami.dev/protocol/telemetry"

	"telemetry.putnami.dev/cliagg"
)

// Environment configuration for durable daily aggregation.
// TELEMETRY_DB_DSN is the local/dev connection; at deploy the managed binding
// supplies the connection and this stays empty.
const envTelemetryDBDSN = "TELEMETRY_DB_DSN"

// installAggregation wires the emit-time tap → Postgres aggregation into the
// application and returns the emitter the receiver should use plus the
// aggregator itself, whose lazily resolved pool the aggregate read route
// reads through.
//
// The datasource is declared as a NAMED datasource rather than the primary one.
// That is deliberate and load-bearing: with only named datasources the plugin
// does not register the unnamed *Pool token (PluginConfig.hasPrimaryPool), so
// nothing opens a connection while the DI container is built. The pool is opened
// lazily by Pools.For on first use, which the aggregator calls during Start —
// the lifecycle phase where the resolved "database" config section carrying a
// managed binding is actually available.
//
// Two earlier cuts got this wrong and are worth not repeating: probing
// database.HasManagedBinding here (in main, before the lifecycle resolves that
// section) always reported "no binding" in a deployed workload and silently
// disabled aggregation; declaring a primary datasource instead made the DI build
// open the pool eagerly, so a missing binding crash-looped the container rather
// than degrading. The named-datasource form cannot fail startup and reports the
// real reason from Start.
//
// AutoApply is false on purpose: in a deployed workspace the platform applies
// this workload's migration bundle with a dedicated migrate Job before the new
// revision takes traffic, so the workload must not race it — and a local run
// without a database must not fail startup trying to migrate.
func installAggregation(a *app.Application, base Emitter, log *logger.Logger) (Emitter, *cliagg.Aggregator) {
	dsn := strings.TrimSpace(os.Getenv(envTelemetryDBDSN))

	agg := cliagg.NewAggregator(log.Named("aggregator"))
	a.Use(agg)

	a.Use(database.NewPlugin(database.PluginConfig{
		Datasources: []database.DatasourceConfig{{
			Name: cliagg.Datasource,
			Pool: telemetryPoolConfig(dsn),
		}},
		Migration: &database.MigrationConfig{AutoApply: false},
	}))

	return teeEmitter{emitters: []Emitter{base, agg}}, agg
}

// telemetryPoolConfig carries only the local/dev DSN. At deploy the managed
// binding supplies a Cloud SQL socket with no user or password, and the
// database package resolves the workload's GCP identity and per-connection IAM
// token natively for that shape — no hook to set here.
func telemetryPoolConfig(dsn string) database.PoolConfig {
	return database.PoolConfig{DSN: dsn}
}

// teeEmitter fans an Emit out to several emitters, so a sanitized batch reaches
// both the collector re-emit path and the aggregation tap. Each emitter is
// itself best-effort and non-blocking.
type teeEmitter struct{ emitters []Emitter }

func (t teeEmitter) Emit(resourceLogs []telemetry.ResourceLogs) {
	for _, e := range t.emitters {
		e.Emit(resourceLogs)
	}
}
