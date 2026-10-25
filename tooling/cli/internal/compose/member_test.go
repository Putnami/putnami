package compose

import (
	"maps"
	"testing"
)

func TestMemberEnv_ProductionKeepsTheLocalConfigEnvironment(t *testing.T) {
	t.Setenv(envAppEnv, "")
	production := map[string]string{envPort: ephemeralPort, envNodeEnv: productionNodeEnv, envAppEnv: localAppEnv}
	if env := memberEnv(true, nil); !maps.Equal(env, production) {
		t.Errorf("production member env = %v, want production mode with APP_ENV=local", env)
	}
	// A watched member is not production: its serve step picks development, and
	// a TypeScript runtime then reads the local files without help.
	if env := memberEnv(false, nil); !maps.Equal(env, map[string]string{envPort: ephemeralPort}) {
		t.Errorf("watched member env = %v, want PORT only", env)
	}
	// memberEnv leaves an APP_ENV the caller exported to the inherited
	// environment, as putnami serve does.
	t.Setenv(envAppEnv, "staging")
	if env := memberEnv(true, nil); env[envAppEnv] != "" {
		t.Errorf("production member env = %v, overrode the exported APP_ENV", env)
	}
	// The CONFIG_DATA document travels in the environment document, as is.
	if env := memberEnv(false, []byte(`{"clients":{}}`)); env[ConfigDataEnv] != `{"clients":{}}` {
		t.Errorf("member env = %v, want the CONFIG_DATA document", env)
	}
}
