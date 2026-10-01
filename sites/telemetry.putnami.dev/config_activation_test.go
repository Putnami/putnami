package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/config"
)

// TestProductionBinaryHasNoRemoteConfigSource pins the contract: the
// receiver reads local YAML, CONFIG_DATA, and env — never an in-process
// config-server client. The deploy target injects the resolved receiver.*
// values as environment or CONFIG_DATA. A config-server source appearing here
// means the binary grew a dependency on one platform's private client.
func TestProductionBinaryHasNoRemoteConfigSource(t *testing.T) {
	t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test/api/configs/resolve?appName=telemetry&environment=prod&secretsMode=reveal")
	t.Setenv("CONFIG_SERVER_TOKEN", "test-token")

	for _, source := range config.DiscoverSources() {
		if source != nil && source.Name() == "config-server" {
			t.Fatal("the production binary registered a config-server source; the receiver must resolve config from env/CONFIG_DATA only")
		}
	}
}

// TestReceiverSettingsResolveFromEnvironment proves the deploy-injection path
// the cut relies on: every receiver.* value arrives as a plain environment
// variable and loadConfig resolves it without any remote source.
func TestReceiverSettingsResolveFromEnvironment(t *testing.T) {
	t.Setenv("TELEMETRY_COLLECTOR_ENDPOINT", "https://collector.internal.example")
	t.Setenv("TELEMETRY_AGGREGATE_AUDIENCE", "https://telemetry.putnami.dev")

	settings, err := config.Load(settingsSchema)
	if err != nil {
		t.Fatalf("load settings from env: %v", err)
	}
	if got, want := settings.CollectorEndpoint, "https://collector.internal.example"; got != want {
		t.Errorf("collector endpoint = %q, want the env-injected %q", got, want)
	}
	if got, want := settings.AggregateAudience, "https://telemetry.putnami.dev"; got != want {
		t.Errorf("aggregate audience = %q, want the env-injected %q", got, want)
	}
}

// TestProductionConfigLeavesDeploymentIdentitiesToThePlatform pins the
// published production configuration: it names no collector and no caller.
// Both are facts of one deployment, so the platform supplies them as
// environment variables, and the published file alone keeps the aggregate
// reader closed and re-emit off.
func TestProductionConfigLeavesDeploymentIdentitiesToThePlatform(t *testing.T) {
	// The env tags take precedence over the file; blank them so a value in the
	// test's own environment cannot stand in for the published one.
	t.Setenv("TELEMETRY_COLLECTOR_ENDPOINT", "")
	t.Setenv("TELEMETRY_AGGREGATE_CALLERS", "")
	t.Setenv(otlpEndpointEnv, "")
	settings := loadProductionSettings(t)

	if got := collectorEndpoint(settings); got != "" {
		t.Errorf("collector endpoint = %q, want empty: the deploying platform supplies %s", got, otlpEndpointEnv)
	}
	if settings.AggregateCallers != "" {
		t.Errorf("aggregate callers = %q, want empty: the deploying platform supplies TELEMETRY_AGGREGATE_CALLERS", settings.AggregateCallers)
	}
	if got, want := settings.AggregateAudience, "https://telemetry.putnami.dev"; got != want {
		t.Errorf("aggregate audience = %q, want %q", got, want)
	}
	if got, want := settings.AggregateIssuer, "https://accounts.google.com"; got != want {
		t.Errorf("aggregate issuer = %q, want %q", got, want)
	}
	if productionAggregateAuth(settings).configured() {
		t.Fatal("the published configuration alone must keep the aggregate reader closed")
	}
}

// TestPlatformOTLPEndpointBecomesTheCollector proves the collector delivery
// path: with no receiver.collectorEndpoint, the receiver re-emits to the
// standard OTLP endpoint the deploying platform sets, and an explicit
// receiver.collectorEndpoint still wins over it.
func TestPlatformOTLPEndpointBecomesTheCollector(t *testing.T) {
	const platform = "https://collector.platform.example"
	t.Setenv("TELEMETRY_COLLECTOR_ENDPOINT", "")
	t.Setenv(otlpEndpointEnv, platform)

	settings := loadProductionSettings(t)
	if got := collectorEndpoint(settings); got != platform {
		t.Fatalf("collector endpoint = %q, want the platform's %s %q", got, otlpEndpointEnv, platform)
	}

	const explicit = "https://collector.explicit.example"
	t.Setenv("TELEMETRY_COLLECTOR_ENDPOINT", explicit)
	if got := collectorEndpoint(loadProductionSettings(t)); got != explicit {
		t.Fatalf("collector endpoint = %q, want the explicit TELEMETRY_COLLECTOR_ENDPOINT %q", got, explicit)
	}
}

// TestPlatformSuppliedCallersOpenTheAggregateReader proves the delivery path
// the published configuration relies on: a caller allowlist injected as an
// environment variable completes the aggregate auth over the published file.
// It loads configuration only; token validation stays covered by the route
// tests and is never attempted against a live issuer here.
func TestPlatformSuppliedCallersOpenTheAggregateReader(t *testing.T) {
	const caller = "reader@example.iam.gserviceaccount.com"
	t.Setenv("TELEMETRY_AGGREGATE_CALLERS", caller)

	auth := productionAggregateAuth(loadProductionSettings(t))
	if !auth.configured() {
		t.Fatal("an injected caller allowlist must complete the production aggregate auth")
	}
	if got := auth.Callers; len(got) != 1 || got[0] != caller {
		t.Fatalf("parsed aggregate callers = %q, want [%q]", got, caller)
	}
}

func loadProductionSettings(t *testing.T) Settings {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate config activation test source")
	}
	settings, err := config.Load(
		settingsSchema,
		config.NewYAMLFileSource(filepath.Join(filepath.Dir(testFile), "conf", "env.prod.yaml"), 50),
	)
	if err != nil {
		t.Fatalf("load production receiver config: %v", err)
	}
	return settings
}

func productionAggregateAuth(settings Settings) aggregateAuth {
	return aggregateAuth{
		Audience: strings.TrimSpace(settings.AggregateAudience),
		Issuer:   strings.TrimSpace(settings.AggregateIssuer),
		Callers:  parseCSV(settings.AggregateCallers),
	}
}
