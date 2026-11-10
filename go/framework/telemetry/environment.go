package telemetry

import (
	"context"
	"os"
	"strings"

	"go.putnami.dev/app"
	putnami_http "go.putnami.dev/http"
	"go.putnami.dev/logger"
)

// The standard OpenTelemetry environment variables FromEnvironment reads.
const (
	// EnvOTLPEndpoint is the collector base URL. Telemetry is on only when it
	// is set.
	EnvOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	// EnvSDKDisabled set to "true" turns the default telemetry off, whatever
	// the endpoint says.
	EnvSDKDisabled = "OTEL_SDK_DISABLED"
)

// EnvironmentOptions tunes the plugin FromEnvironment builds.
type EnvironmentOptions struct {
	// BearerTokenSource, when set, builds the credential every push to the
	// collector at endpoint carries. Nil sends no credential.
	BearerTokenSource func(endpoint string) func(ctx context.Context) (string, error)
}

// FromEnvironment returns the telemetry an application gets without asking for
// it, or nil when the environment names no collector. It reads the standard
// OpenTelemetry variables: OTEL_EXPORTER_OTLP_ENDPOINT turns it on,
// OTEL_SDK_DISABLED=true turns it off, and the service identity comes from
// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES.
//
// The plugin it returns does two things while the application configures:
//
//   - It adds HTTPMiddleware outermost on every http.ServerPlugin of the
//     application, so each request is counted, timed and traced, including
//     the ones an inner middleware refuses.
//   - It builds the tracer and meter providers that export to the collector,
//     unless the application installs its own Plugin from NewPlugin. That
//     plugin then owns the providers and this one only instruments the
//     servers.
//
// It registers nothing in the DI container. Register it as a default plugin
// so every application starts with it:
//
//	app.RegisterDefaultPlugin(func() app.Plugin {
//	    return telemetry.FromEnvironment(telemetry.EnvironmentOptions{})
//	})
func FromEnvironment(opts EnvironmentOptions) app.Plugin {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvSDKDisabled)), "true") {
		return nil
	}
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv(EnvOTLPEndpoint)), "/")
	if endpoint == "" {
		return nil
	}
	otlp := OTLPConfig{Endpoint: endpoint}
	if opts.BearerTokenSource != nil {
		otlp.BearerTokenSource = opts.BearerTokenSource(endpoint)
	}
	return &environmentPlugin{
		otlp: otlp,
		log:  logger.Default().Named("telemetry"),
	}
}

// environmentPlugin is the plugin FromEnvironment returns.
type environmentPlugin struct {
	otlp OTLPConfig
	log  *logger.Logger
	// instrumented is set once the servers carry the middleware: the chain is
	// composed once, so a second configure pass must not add it again.
	instrumented bool
	// providers is the Plugin this one configured, nil when the application
	// brings its own.
	providers *Plugin
}

func (p *environmentPlugin) Name() string { return "telemetry-environment" }

// Configure instruments every HTTP server of the application, then builds the
// providers unless the application installs its own telemetry Plugin.
// Configuring again instruments nothing twice and builds no second set of
// providers.
func (p *environmentPlugin) Configure(ctx context.Context, owner *app.Module) error {
	if owner == nil {
		return nil
	}
	root := owner.Root()
	if !p.instrumented {
		for _, server := range app.Collect[*putnami_http.ServerPlugin](root) {
			server.UseOutermost(HTTPMiddleware(root.Name()))
		}
		p.instrumented = true
	}
	if p.providers != nil {
		return nil
	}
	if len(app.Collect[*Plugin](root)) > 0 {
		p.log.Debug("the application installs its own telemetry plugin: the environment plugin only instruments its HTTP servers")
		return nil
	}
	providers := NewPlugin(Config{OTLP: &p.otlp, fromEnvironment: true})
	if err := providers.Configure(ctx, owner); err != nil {
		return err
	}
	p.providers = providers
	return nil
}

// Stop flushes and shuts down the providers this plugin built.
func (p *environmentPlugin) Stop(ctx context.Context, owner *app.Module) error {
	if p.providers == nil {
		return nil
	}
	providers := p.providers
	p.providers = nil
	return providers.Stop(ctx, owner)
}

var (
	_ app.Plugin     = (*environmentPlugin)(nil)
	_ app.Configurer = (*environmentPlugin)(nil)
	_ app.Stopper    = (*environmentPlugin)(nil)
)
