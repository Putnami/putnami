package putnamihome

import (
	"path/filepath"
	"testing"
)

// The Putnami home is PUTNAMI_HOME when it is set, else .putnami under the
// user's home directory, and none when the environment names neither. Values
// that are only white space name nothing.
func TestResolve_Precedence(t *testing.T) {
	env := map[string]string{}
	lookup := func(key string) string { return env[key] }

	if got := Resolve(nil); got != "" {
		t.Errorf("no lookup = %q, want no home", got)
	}
	if got := Resolve(lookup); got != "" {
		t.Errorf("empty environment = %q, want no home", got)
	}

	env[Env] = "  "
	env[UserHomeEnv()] = " \t"
	if got := Resolve(lookup); got != "" {
		t.Errorf("blank variables = %q, want no home", got)
	}

	env[UserHomeEnv()] = "/users/dev"
	if got, want := Resolve(lookup), filepath.Join("/users/dev", ".putnami"); got != want {
		t.Errorf("user home = %q, want %q", got, want)
	}

	env[Env] = "/relocated/putnami"
	if got, want := Resolve(lookup), "/relocated/putnami"; got != want {
		t.Errorf("PUTNAMI_HOME = %q, want %q", got, want)
	}
}

// A toolchain's releases live in toolchains/<name> under the Putnami home. A
// job whose environment names no home uses .putnami under the workspace root,
// which is the home the CLI resolves the putnami-home candidates of a runtime
// toolchain against in that case.
func TestToolchainRoot_FollowsThePutnamiHome(t *testing.T) {
	env := map[string]string{}
	lookup := func(key string) string { return env[key] }
	workspace := filepath.Join("/work", "repo")

	if got, want := ToolchainRoot(lookup, workspace, "bun"), filepath.Join(workspace, ".putnami", "toolchains", "bun"); got != want {
		t.Errorf("no home = %q, want %q", got, want)
	}
	if got, want := ToolchainRoot(nil, workspace, "bun"), filepath.Join(workspace, ".putnami", "toolchains", "bun"); got != want {
		t.Errorf("no lookup = %q, want %q", got, want)
	}

	env[UserHomeEnv()] = "/users/dev"
	if got, want := ToolchainRoot(lookup, workspace, "bun"), filepath.Join("/users/dev", ".putnami", "toolchains", "bun"); got != want {
		t.Errorf("user home = %q, want %q", got, want)
	}

	env[Env] = "/relocated/putnami"
	if got, want := ToolchainRoot(lookup, workspace, "go"), filepath.Join("/relocated/putnami", "toolchains", "go"); got != want {
		t.Errorf("PUTNAMI_HOME = %q, want %q", got, want)
	}
}

// An install is recognized by its place and its name, toolchains/<name>/
// <name>-<version>, so that a program can tell a toolchain Putnami installed
// from one the host holds without reading the disk.
func TestIsToolchainInstall(t *testing.T) {
	home := filepath.Join("/users/dev", ".putnami")
	cases := []struct {
		dir  string
		name string
		want bool
	}{
		{filepath.Join(home, "toolchains", "bun", "bun-1.4.0"), "bun", true},
		{filepath.Join(home, "toolchains", "bun", "bun-1.4.0") + string(filepath.Separator), "bun", true},
		{filepath.Join(home, "toolchains", "go", "go-1.26.0"), "go", true},
		{filepath.Join(home, "toolchains", "go", "go-1.26.0"), "bun", false},
		{filepath.Join(home, "toolchains", "bun", "bun-"), "bun", false},
		{filepath.Join(home, "toolchains", "bun", "1.4.0"), "bun", false},
		{filepath.Join(home, "toolchains", "bun"), "bun", false},
		{filepath.Join(home, "tools", "bun", "bun-1.4.0"), "bun", false},
		{filepath.Join("/users/dev", ".bun"), "bun", false},
		{"", "bun", false},
		{"  ", "bun", false},
		{filepath.Join(home, "toolchains", "bun", "bun-1.4.0"), "", false},
	}
	for _, tc := range cases {
		if got := IsToolchainInstall(tc.dir, tc.name); got != tc.want {
			t.Errorf("IsToolchainInstall(%q, %q) = %v, want %v", tc.dir, tc.name, got, tc.want)
		}
	}
}
