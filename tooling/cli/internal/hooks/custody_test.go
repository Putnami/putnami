package hooks

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// On a hosted run every hook, and every cache command of an extension,
// records repository code before it starts: a credential holder asked for
// afterwards is refused, and the refusal names the hook.
func TestEveryHookRecordsRepositoryCodeOnAHostedRun(t *testing.T) {
	skipWithoutSh(t)
	ctx := context.Background()
	summary := fixtureproc.Program{Stdout: `{"v":1,"type":"summary","data":{"freedBytes":0}}` + "\n"}
	cases := []struct {
		reason string
		run    func(t *testing.T) error
	}{
		{"hook hooks.cli.before", func(t *testing.T) error {
			cfg := &wsproto.HooksConfig{CLI: &wsproto.HookPhaseConfig{Before: []string{"true"}}}
			return RunCLIHooks(ctx, cfg, "before", t.TempDir(), false)
		}},
		{"hook @putnami/cloud/preBuild", func(t *testing.T) error {
			ws, ext, proj := preBuildFixture(t, summary, 0)
			_, err := RunPreBuildHook(ctx, ws, ext, proj, false, nil)
			return err
		}},
		{"hook @putnami/cloud/onInstall", func(t *testing.T) error {
			dir := t.TempDir()
			script := fixtureproc.Write(t, filepath.Join(dir, "on-install"), fixtureproc.Program{})
			ext := &extension.ExtensionDescription{
				Name:  "@putnami/cloud",
				Path:  filepath.Join(dir, "ext"),
				Hooks: &extension.ManifestHooks{OnInstall: &extension.HookDefinition{Kind: "command", Command: script, Cwd: "{workspaceRoot}"}},
			}
			return RunOnInstallHook(ctx, workspace.NewWorkspace(dir, &wsproto.Config{Name: "test-ws"}, nil), ext, false)
		}},
		{"job @test/custody cache-clean", func(t *testing.T) error {
			ext := cacheCommandExtension(t, "@test/custody", summary)
			_, err := RunCacheCommand(ctx, &workspace.Workspace{Root: t.TempDir()}, ext, extensionproto.CommandCacheClean, false)
			return err
		}},
		{"job @test/custody cache-gc", func(t *testing.T) error {
			ext := cacheCommandExtension(t, "@test/custody", summary)
			if !StartDetachedCacheGC(&workspace.Workspace{Root: t.TempDir()}, ext) {
				return errors.New("the collector did not start")
			}
			return nil
		}},
	}
	for _, tc := range cases {
		restore := runcredential.SetForTest("run-bearer")
		if err := tc.run(t); err != nil {
			restore()
			t.Fatalf("%s: %v", tc.reason, err)
		}
		err := runcredential.RequireCustody("cache provider @a/cache")
		restore()
		var refusal *runcredential.CustodyError
		if !errors.As(err, &refusal) || refusal.Reason != tc.reason {
			t.Errorf("after %s: RequireCustody = %v, want a refusal that names it", tc.reason, err)
		}
	}
}
