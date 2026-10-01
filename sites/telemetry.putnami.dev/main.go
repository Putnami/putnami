// Command telemetry.putnami.dev is the anonymous Putnami CLI-usage telemetry
// receiver. It is an ordinary Putnami tenant workload — no
// control-plane coupling — that serves the standard OTLP/JSON logs endpoint
// POST /v1/logs on its own host, sanitizes each payload against the shared
// CLI-usage vocabulary (go.putnami.dev/protocol/telemetry/cliusage), stamps an
// unspoofable origin=cli-anon, and best-effort re-emits accepted records into
// the workspace's own collector. It is anonymous by design: no session, API
// key, or cookie, and it never persists a client IP.
package main

import (
	"context"
	"os"
	"strings"

	"go.putnami.dev/app"
	"go.putnami.dev/config"
	"go.putnami.dev/logger"
	protocaps "go.putnami.dev/protocol/capabilities"
	ptelemetry "go.putnami.dev/telemetry"

	"telemetry.putnami.dev/cliagg"
)

// appName is the application name. It is also the provenance project stamped on
// every route in the described schema/http-routes.json, so the public route
// contract test can rebuild the artifact from the same name the runtime uses.
const appName = "telemetry-receiver"

// Settings is the receiver's operational configuration, declared as a framework
// config block so the platform supplies it at deploy time (the build extracts it
// into schema/config.json, which the workspace registers) while the same names
// keep working as environment variables locally.
//
// Deliberately absent: the database DSN. In a deployed workload the connection
// arrives through the managed database binding for the "telemetry" datasource —
// a DSN is a credential and must never travel in published, non-secret config.
// Local runs still use the TELEMETRY_DB_DSN escape hatch (see aggregation.go).
type Settings struct {
	// CollectorEndpoint is the base URL of the collector accepted telemetry is
	// re-emitted to. Empty falls back to the platform's OTEL_EXPORTER_OTLP_ENDPOINT
	// (see collectorEndpoint); with neither, re-emit is disabled (accepted records
	// are dropped).
	CollectorEndpoint string `json:"collectorEndpoint" env:"TELEMETRY_COLLECTOR_ENDPOINT"`

	// TrustedProxies is the comma-separated allowlist of proxy addresses/CIDRs
	// whose X-Forwarded-For may be believed. Empty is the fail-safe default: the
	// direct peer is used, which over-throttles behind a shared proxy but never
	// honors a forgeable header.
	TrustedProxies string `json:"trustedProxies" env:"TELEMETRY_TRUSTED_PROXIES"`

	// PerIPPerMinute caps requests per client IP; zero uses the built-in default.
	PerIPPerMinute int `json:"perIpPerMinute" env:"TELEMETRY_PER_IP_PER_MINUTE"`

	// GlobalQPS caps total per-instance throughput; zero uses the built-in default.
	GlobalQPS int `json:"globalQps" env:"TELEMETRY_GLOBAL_QPS"`

	// AggregateAudience is the exact "aud" claim a caller's token must carry to
	// read the private aggregate route. Empty disables the route (it stays
	// mounted and denies every request) — the read side fails closed.
	AggregateAudience string `json:"aggregateAudience" env:"TELEMETRY_AGGREGATE_AUDIENCE"`

	// AggregateIssuer is the OIDC issuer whose JWKS validates aggregate-read
	// tokens, and the exact "iss" those tokens must carry. Empty disables the
	// route.
	AggregateIssuer string `json:"aggregateIssuer" env:"TELEMETRY_AGGREGATE_ISSUER"`

	// AggregateCallers is the comma-separated allowlist of service identities
	// (token subject, or email claim) permitted to read aggregates. Empty
	// disables the route: an authenticated but unlisted caller is denied.
	AggregateCallers string `json:"aggregateCallers" env:"TELEMETRY_AGGREGATE_CALLERS"`
}

// settingsSchema is the workload's config block. Its path is what appears in the
// published schema and in conf/*.yaml.
var settingsSchema = config.Config[Settings]("receiver")

func main() {
	log := logger.Default().Named(appName)

	cfg := loadConfig(log)
	base, closer := buildEmitter(cfg, log)

	a := app.New(appName)

	// The product outcome this workload implements, declared once. Proves names
	// the two contributions that make "expiry-runs-without-the-service" true:
	// cliagg.Source() carries the migration that calls cron.schedule_in_database,
	// and the source discoverer is what applies it. Neither alone schedules the
	// expiry, and both come from that one declaration. The build derives the
	// evidence; nothing here asserts a stage or a source binding.
	a.Feature(app.Feature{
		ID:      "telemetry-putnami-dev/cli-usage-receiver",
		Name:    "Anonymous CLI-usage telemetry receiver",
		Outcome: "Putnami maintainers see aggregate CLI usage without the receiver ever retaining a raw event, a caller address, or anything that identifies a user, and a CLI run is never affected by whether the receiver answers",
		Owner:   "telemetry.putnami.dev",
		Proves: []app.FeatureProof{
			{
				Requirement: "expiry-runs-without-the-service",
				Contribution: app.ContributionRef{
					Kind: protocaps.ContributionKindMigration, Subkind: "sql", Key: cliagg.MigrationNamespace,
				},
			},
			{
				Requirement: "expiry-runs-without-the-service",
				Contribution: app.ContributionRef{
					Kind:    protocaps.ContributionKindDiscoverer,
					Subkind: string(protocaps.DiscovererKindSource),
					Key:     "sql:" + cliagg.MigrationNamespace,
				},
			},
		},
	})

	// Tee the sanitized emit path into the durable daily-aggregation tap
	// alongside the existing collector re-emit.
	emitter, agg := installAggregation(a, base, log)

	// The private aggregate read route reads the same lazily
	// resolved named-datasource pool the aggregator writes through — it never
	// declares a datasource of its own.
	aggregate := &aggregateEndpoint{
		source: cliagg.PoolSource{Pool: agg.Pool},
		auth:   cfg.aggregateAuth,
		log:    log.Named("aggregate-read"),
	}
	if !cfg.aggregateAuth.configured() {
		log.Warn("aggregate read route disabled: set receiver.aggregateAudience, receiver.aggregateIssuer, and receiver.aggregateCallers to enable it")
	}

	// The domain's declared cross-domain access, enforced by a component and
	// recorded as describe evidence. A contract this workload cannot
	// enforce is a startup failure rather than a silently missing row: the whole
	// point is that the manifest and the code say the same thing.
	contracts, err := newDomainAccessPlugin()
	if err != nil {
		log.Error("domain access contracts are not enforceable", err)
		os.Exit(1)
	}
	a.Use(contracts)

	a.Use(newServer(cfg, emitter, aggregate))
	if closer != nil {
		a.Use(closer)
	}

	if err := a.ListenAndServe(); err != nil {
		log.Error("application failed", err)
		os.Exit(1)
	}
}

// loadConfig resolves the workload settings through the framework config system
// (discovered conf/*.yaml, CONFIG_DATA, then the env tags above) and falls back
// to the built-in limits when a value is absent. A config load error is not
// fatal: the receiver starts on defaults rather than refusing to serve.
func loadConfig(log *logger.Logger) receiverConfig {
	settings, err := config.Load(settingsSchema)
	if err != nil {
		log.Warn("using default receiver settings: config load failed: " + err.Error())
		settings = Settings{}
	}

	cfg := receiverConfig{
		port:              8080, // ServerConfig honors the PORT env override
		collectorEndpoint: collectorEndpoint(settings),
		trustedProxies:    parseCSV(settings.TrustedProxies),
		perIPPerMinute:    settings.PerIPPerMinute,
		globalQPS:         settings.GlobalQPS,
		aggregateAuth: aggregateAuth{
			Audience: strings.TrimSpace(settings.AggregateAudience),
			Issuer:   strings.TrimSpace(settings.AggregateIssuer),
			Callers:  parseCSV(settings.AggregateCallers),
		},
	}
	if cfg.perIPPerMinute <= 0 {
		cfg.perIPPerMinute = defaultPerIPPerMinute
	}
	if cfg.globalQPS <= 0 {
		cfg.globalQPS = defaultGlobalQPS
	}
	return cfg
}

// otlpEndpointEnv is the standard OpenTelemetry exporter endpoint. A deploying
// platform that runs a collector for its workloads sets it on every workload.
const otlpEndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"

// collectorEndpoint returns the configured collector, else the platform's
// standard OTLP endpoint, so a deployment needs no collector value of its own.
// An explicit receiver.collectorEndpoint always wins.
func collectorEndpoint(settings Settings) string {
	if endpoint := strings.TrimSpace(settings.CollectorEndpoint); endpoint != "" {
		return endpoint
	}
	return strings.TrimSpace(os.Getenv(otlpEndpointEnv))
}

// buildEmitter returns the persist mechanism plus an optional lifecycle plugin
// that final-flushes on shutdown. When no collector endpoint is configured the
// workload still runs and accepts traffic, dropping it (best-effort).
func buildEmitter(cfg receiverConfig, log *logger.Logger) (Emitter, app.Plugin) {
	if cfg.collectorEndpoint == "" {
		log.Warn("no collector endpoint configured (receiver.collectorEndpoint / TELEMETRY_COLLECTOR_ENDPOINT / " + otlpEndpointEnv + "); accepted telemetry will be dropped")
		return noopEmitter{}, nil
	}
	tokens := ptelemetry.NewGCPIDTokenSource(cfg.collectorEndpoint)
	exp := newOTLPExporter(cfg.collectorEndpoint, 0, 0, tokens, log.Named("exporter"))
	return exp, &exporterPlugin{exp: exp}
}

func parseCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// exporterPlugin binds the OTLP exporter's final flush to the application
// shutdown phase, honoring the ExportContract's FinalFlushOnShutdown.
type exporterPlugin struct{ exp *otlpExporter }

func (*exporterPlugin) Name() string { return "telemetry-exporter" }

func (p *exporterPlugin) Stop(context.Context, *app.Module) error {
	return p.exp.Close()
}
