package compose

import (
	"slices"
	"testing"
)

func TestMemberEnv_ProductionKeepsTheLocalConfigEnvironment(t *testing.T) {
	t.Setenv(envAppEnv, "")
	if env := memberEnv(true, nil); !slices.Equal(env, []string{envPort, envProductionNodeEnv, envLocalAppEnv}) {
		t.Errorf("production member env = %v, want production mode with APP_ENV=local", env)
	}
	// A watched member is not production: its serve step picks development, and
	// a TypeScript runtime then reads the local files without help.
	if env := memberEnv(false, nil); !slices.Equal(env, []string{envPort}) {
		t.Errorf("watched member env = %v, want PORT only", env)
	}
	// memberEnv leaves an APP_ENV the caller exported to the inherited
	// environment, as putnami serve does.
	t.Setenv(envAppEnv, "staging")
	if env := memberEnv(true, nil); slices.Contains(env, envLocalAppEnv) {
		t.Errorf("production member env = %v, overrode the exported APP_ENV", env)
	}
}
