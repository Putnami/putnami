package activate_test

import (
	"testing"

	"go.putnami.dev/app"
	config "go.putnami.dev/config"

	// Blank-import under test: its init() must register the config-server
	// source discoverer with the framework loader.
	_ "go.putnami.dev/cloud/runtime/activate"

	"go.putnami.dev/protocol/features/spectest"
)

// TestActivationRegistersConfigServerSource proves the blank-import wires the
// remote config source into the framework loader: with CONFIG_SERVER_URL set,
// DiscoverSources must include the "config-server" source.
func TestActivationRegistersConfigServerSource(t *testing.T) {
	spectest.Proves(t, "cloud/go-runtime-destination", "source-discovery", "activation-registers-config-server-source")
	t.Setenv("CONFIG_SERVER_URL", "https://config.example.test/api/configs/resolve?appName=x&environment=prod&secretsMode=reveal")

	if !hasSourceNamed(config.DiscoverSources(), "config-server") {
		t.Fatalf("expected a config-server source after activation; discoverer not registered")
	}
}

// TestActivationInertWithoutConfigServerURL guards the typed-nil path: with no
// CONFIG_SERVER_URL the discoverer must contribute nothing (a boxed nil source
// would panic on Load), so local-only workloads are unaffected by the import.
func TestActivationInertWithoutConfigServerURL(t *testing.T) {
	spectest.Proves(t, "cloud/go-runtime-destination", "source-discovery", "activation-is-inert-without-server-url")
	t.Setenv("CONFIG_SERVER_URL", "")

	if hasSourceNamed(config.DiscoverSources(), "config-server") {
		t.Fatalf("config-server source must not appear when CONFIG_SERVER_URL is unset")
	}
}

func hasSourceNamed(sources []config.Source, name string) bool {
	for _, s := range sources {
		if s != nil && s.Name() == name {
			return true
		}
	}
	return false
}

// TestActivationStartsEveryApplicationWithTelemetry proves the blank-import
// registers the default telemetry plugin: with a collector endpoint, a new
// application starts with it; without one, or with the opt-out, it does not.
func TestActivationStartsEveryApplicationWithTelemetry(t *testing.T) {
	spectest.Proves(t, "cloud/go-runtime-destination", "default-telemetry", "activation-adds-the-telemetry-plugin-when-a-collector-is-set")
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example.test")
	if !hasPluginNamed(app.New("svc"), "telemetry-environment") {
		t.Fatalf("an application started with a collector endpoint has no telemetry plugin")
	}

	t.Setenv("OTEL_SDK_DISABLED", "true")
	if hasPluginNamed(app.New("svc"), "telemetry-environment") {
		t.Fatalf("OTEL_SDK_DISABLED=true must leave the application without the telemetry plugin")
	}

	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if hasPluginNamed(app.New("svc"), "telemetry-environment") {
		t.Fatalf("an application without a collector endpoint must not get the telemetry plugin")
	}
}

func hasPluginNamed(a *app.Application, name string) bool {
	for _, po := range a.CollectPlugins() {
		if po.Plugin.Name() == name {
			return true
		}
	}
	return false
}
