package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// hostedHookTest makes the test a hosted run when hosted, or a local one, with
// a cache token and no offline signal in the environment it inherits.
func hostedHookTest(t *testing.T, hosted bool) {
	t.Helper()
	bearer := ""
	if hosted {
		bearer = "run-bearer"
	}
	t.Cleanup(runcredential.SetForTest(bearer))
	t.Setenv(runcredential.CacheTokenEnv, "cache-token")
}

// wantHostedEnv checks what a hook of a hosted run, or of a local one, sees:
// the offline signal and no cache token, or the inherited cache token and no
// offline signal.
func wantHostedEnv(t *testing.T, hosted bool, lookup func(string) (string, bool)) {
	t.Helper()
	offline, offlineSet := lookup(extensionproto.OfflineDependenciesEnv)
	token, tokenSet := lookup(runcredential.CacheTokenEnv)
	if hosted && (offline != "1" || tokenSet) {
		t.Errorf("a hosted hook saw %s=%q (set %v) and %s set %v; want 1 and no token",
			extensionproto.OfflineDependenciesEnv, offline, offlineSet, runcredential.CacheTokenEnv, tokenSet)
	}
	if !hosted && (offlineSet || token != "cache-token") {
		t.Errorf("a local hook saw %s set %v and %s=%q; want the environment it inherited",
			extensionproto.OfflineDependenciesEnv, offlineSet, runcredential.CacheTokenEnv, token)
	}
}

// Every extension hook of a hosted run runs offline and without the cache
// token, whatever its manifest declares. A local run's hooks see the
// environment they always did.
func TestExtensionHooksSeeTheOfflineSignalOfTheirRun(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		hostedHookTest(t, hosted)

		record := filepath.Join(t.TempDir(), "runs.jsonl")
		ws, ext, proj := preBuildFixture(t, fixtureproc.Program{Record: record}, 0)
		if hosted {
			ext.Hooks.PreBuild.Env = map[string]string{extensionproto.OfflineDependenciesEnv: "0"}
		}
		if _, err := RunPreBuildHook(context.Background(), ws, ext, proj, false, nil); err != nil {
			t.Fatalf("RunPreBuildHook: %v", err)
		}
		wantHostedEnv(t, hosted, onlyRun(t, record).LookupEnv)

		dir := t.TempDir()
		record = filepath.Join(dir, "runs.jsonl")
		script := fixtureproc.Write(t, filepath.Join(dir, "on-install"), fixtureproc.Program{Record: record})
		ext = &extension.ExtensionDescription{
			Name:  "@putnami/cloud",
			Path:  filepath.Join(dir, "ext"),
			Hooks: &extension.ManifestHooks{OnInstall: &extension.HookDefinition{Kind: "command", Command: script, Cwd: "{workspaceRoot}"}},
		}
		if err := RunOnInstallHook(context.Background(), workspace.NewWorkspace(dir, &wsproto.Config{Name: "test-ws"}, nil), ext, false); err != nil {
			t.Fatalf("RunOnInstallHook: %v", err)
		}
		wantHostedEnv(t, hosted, onlyRun(t, record).LookupEnv)

		env := cacheCommandEnv(&workspace.Workspace{Root: dir}, ext, &extension.JobDefinition{}, "prepare", dir, dir, nil)
		wantHostedEnv(t, hosted, func(name string) (string, bool) {
			for i := len(env) - 1; i >= 0; i-- {
				if value, ok := strings.CutPrefix(env[i], name+"="); ok {
					return value, true
				}
			}
			return "", false
		})
	}
}

// A workspace hook of a hosted run runs offline and without the cache token.
func TestWorkspaceHooksSeeTheOfflineSignalOfTheirRun(t *testing.T) {
	skipWithoutSh(t)
	for _, hosted := range []bool{false, true} {
		hostedHookTest(t, hosted)
		out := filepath.Join(t.TempDir(), "env")
		cfg := &wsproto.HooksConfig{CLI: &wsproto.HookPhaseConfig{After: []string{
			`printf '%s|%s|%s\n' "${` + extensionproto.OfflineDependenciesEnv + `-unset}" "${` + runcredential.CacheTokenEnv + `-unset}" "${PUTNAMI_HOOK}" > ` + out,
		}}}
		if err := RunCLIHooks(context.Background(), cfg, "after", t.TempDir(), false); err != nil {
			t.Fatalf("hook failed: %v", err)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		want := "unset|cache-token|hooks.cli.after"
		if hosted {
			want = "1|unset|hooks.cli.after"
		}
		if got := strings.TrimSpace(string(data)); got != want {
			t.Errorf("hosted=%v: the hook saw %q, want %q", hosted, got, want)
		}
	}
}
