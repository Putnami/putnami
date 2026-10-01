package toolchain

import (
	"path/filepath"
	"testing"
)

// The Putnami home is PUTNAMI_HOME when it is set, else .putnami under the
// user's home directory, and none when the environment names neither. Values
// that are only white space name nothing.
func TestResolvePutnamiHome_Precedence(t *testing.T) {
	env := map[string]string{}
	lookup := func(key string) string { return env[key] }

	if got := ResolvePutnamiHome(nil); got != "" {
		t.Errorf("no lookup = %q, want no home", got)
	}
	if got := ResolvePutnamiHome(lookup); got != "" {
		t.Errorf("empty environment = %q, want no home", got)
	}

	env[PutnamiHomeEnv] = "  "
	env[homeEnvName()] = " \t"
	if got := ResolvePutnamiHome(lookup); got != "" {
		t.Errorf("blank variables = %q, want no home", got)
	}

	env[homeEnvName()] = "/users/dev"
	if got, want := ResolvePutnamiHome(lookup), filepath.Join("/users/dev", ".putnami"); got != want {
		t.Errorf("user home = %q, want %q", got, want)
	}

	env[PutnamiHomeEnv] = "/relocated/putnami"
	if got, want := ResolvePutnamiHome(lookup), "/relocated/putnami"; got != want {
		t.Errorf("PUTNAMI_HOME = %q, want %q", got, want)
	}
}

// The Go releases live in toolchains/go under the Putnami home. A job whose
// environment names no home uses .putnami under the workspace root, which is
// the home the CLI resolves the putnami-home candidates of a runtime toolchain
// against in that case.
func TestResolveGoToolchainRoot_FollowsThePutnamiHome(t *testing.T) {
	env := map[string]string{}
	lookup := func(key string) string { return env[key] }
	workspace := filepath.Join("/work", "repo")

	if got, want := ResolveGoToolchainRoot(lookup, workspace), filepath.Join(workspace, ".putnami", "toolchains", "go"); got != want {
		t.Errorf("no home = %q, want %q", got, want)
	}
	if got, want := ResolveGoToolchainRoot(nil, workspace), filepath.Join(workspace, ".putnami", "toolchains", "go"); got != want {
		t.Errorf("no lookup = %q, want %q", got, want)
	}

	env[homeEnvName()] = "/users/dev"
	if got, want := ResolveGoToolchainRoot(lookup, workspace), filepath.Join("/users/dev", ".putnami", "toolchains", "go"); got != want {
		t.Errorf("user home = %q, want %q", got, want)
	}

	env[PutnamiHomeEnv] = "/relocated/putnami"
	if got, want := ResolveGoToolchainRoot(lookup, workspace), filepath.Join("/relocated/putnami", "toolchains", "go"); got != want {
		t.Errorf("PUTNAMI_HOME = %q, want %q", got, want)
	}
}
