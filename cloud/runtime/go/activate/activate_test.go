package activate_test

import (
	"testing"

	config "go.putnami.dev/config"

	// Blank-import under test: its init() must register the config-server
	// source discoverer with the framework loader.
	_ "go.putnami.dev/cloud/runtime/activate"
)

// TestActivationRegistersConfigServerSource proves the blank-import wires the
// remote config source into the framework loader: with CONFIG_SERVER_URL set,
// DiscoverSources must include the "config-server" source.
func TestActivationRegistersConfigServerSource(t *testing.T) {
	t.Setenv("CONFIG_SERVER_URL", "https://config.example.test/api/configs/resolve?appName=x&environment=prod&secretsMode=reveal")

	if !hasSourceNamed(config.DiscoverSources(), "config-server") {
		t.Fatalf("expected a config-server source after activation; discoverer not registered")
	}
}

// TestActivationInertWithoutConfigServerURL guards the typed-nil path: with no
// CONFIG_SERVER_URL the discoverer must contribute nothing (a boxed nil source
// would panic on Load), so local-only workloads are unaffected by the import.
func TestActivationInertWithoutConfigServerURL(t *testing.T) {
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
