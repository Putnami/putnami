package hometest_test

import (
	"path/filepath"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

// TestEnvIsTheUserHomeOnEveryPlatform pins the reason the helper exists: the
// env it returns resolves the user home and the Putnami home to dir on Windows
// as well as on Unix, so no test run falls back to the real user home.
func TestEnvIsTheUserHomeOnEveryPlatform(t *testing.T) {
	dir := t.TempDir()
	env := hometest.Env(dir, nil)
	for _, goos := range []string{"windows", "linux", "darwin"} {
		if got := clicore.UserHomeDirFor(env, goos); got != dir {
			t.Errorf("UserHomeDirFor on %s = %q, want the test home %q", goos, got, dir)
		}
	}
	if got, want := clicore.PutnamiHome(env), filepath.Join(dir, ".putnami"); got != want {
		t.Errorf("PutnamiHome = %q, want %q", got, want)
	}
}

func TestEnvKeepsTheOtherVariablesAndReturnsTheSameMap(t *testing.T) {
	env := map[string]string{"PUTNAMI_AUTH_URL": "https://auth.test", "HOME": "/stale"}
	got := hometest.Env("/test-home", env)
	want := map[string]string{"PUTNAMI_AUTH_URL": "https://auth.test", "HOME": "/test-home", "USERPROFILE": "/test-home"}
	if len(got) != len(want) {
		t.Fatalf("Env = %v, want %v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("Env[%s] = %q, want %q (env %v)", key, got[key], value, got)
		}
	}
	got["PROBE"] = "x"
	if env["PROBE"] != "x" {
		t.Fatal("Env returned a copy; callers that keep the map they passed must see the home")
	}
}
