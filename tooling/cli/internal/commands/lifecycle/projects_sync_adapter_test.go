package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// `projects sync` stops WRITING the native manifests a workspace adapter's
// syncTask owns, and starts asking the adapter what marks a project.
//
// The tests below are about OWNERSHIP, not about the TypeScript extension: the
// fixture declares an adapter whose runtime does not exist, because what must
// be checked is that core stepped back — not that some particular extension
// stepped forward.

const adapterExtensionManifest = `{
  "name": "@fixture/ts",
  "version": "0.1.0",
  "cliContract": 4,
  "workspace": {
    "markers": ["package.json"],
    "inputs": ["package.json", "bun.lock"],
    "excludes": ["node_modules"],
    "syncTask": "workspace-sync-exec"
  },
  "commands": {
    "workspace-sync": {
      "description": "Synchronize native workspace state.",
      "visibility": "internal",
      "run": [{ "id": "workspace-sync", "task": "workspace-sync-exec" }]
    }
  },
  "tasks": {
    "workspace-sync-exec": { "kind": "command", "command": "echo", "cache": false }
  }
}`

// adapterWorkspace builds a workspace whose only extension declares a workspace
// adapter with a syncTask, plus one npm package that is not yet a member.
func adapterWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeProjectFile(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["ts-extension","apps"]}`)
	writeProjectFile(t, filepath.Join(root, "package.json"), `{"name":"repo","workspaces":["stale/path"]}`)
	writeProjectFile(t, filepath.Join(root, "ts-extension", "putnami.json"), `{"name":"@fixture/ts"}`)
	writeProjectFile(t, filepath.Join(root, "ts-extension", "putnami.extension.json"), adapterExtensionManifest)
	// A package with no name of its own, under a scope whose namePattern
	// resolves one: that is the shape core's name alignment exists for, and the
	// one the extension now writes.
	writeProjectFile(t, filepath.Join(root, "apps", "putnami.json"),
		`{"includes":["web"],"publishConfig":{"npm":{"namePattern":"@acme/{name}"}}}`)
	writeProjectFile(t, filepath.Join(root, "apps", "web", "package.json"), `{"version":"1.0.0"}`)

	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	return root
}

// The root `workspaces` array is the extension's to write. Core must leave it
// exactly as it found it — two writers for one field is how a file starts
// flipping between two spellings on alternate runs.
func TestProjectsSync_DoesNotWritePackageWorkspacesWhenAnAdapterOwnsThem(t *testing.T) {
	root := adapterWorkspace(t)
	before := readText(t, filepath.Join(root, "package.json"))

	var ran []string
	env := LifecycleEnv{Out: os.Stdout, RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
		ran = append(ran, req.Job)
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}
	if err := ProjectsSync(context.Background(), root, wsproto.Load(root),
		[]string{"--skip-install"}, false, env); err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	if after := readText(t, filepath.Join(root, "package.json")); after != before {
		t.Fatalf("core rewrote a manifest the adapter owns:\nbefore %s\nafter  %s", before, after)
	}
	// The adapter's COMMAND is what runs, resolved from the task it declares —
	// the manifest is free to name the two differently.
	if !slices.Contains(ran, "workspace-sync") {
		t.Fatalf("workspace jobs run = %v, want the adapter's sync command", ran)
	}
}

// The name arm is the same handoff: core still DETECTS the divergence so the
// report and the "nothing to do" verdict stay complete, and writes nothing.
func TestProjectsSync_ReportsButDoesNotWriteAnOwnedName(t *testing.T) {
	root := adapterWorkspace(t)
	manifest := filepath.Join(root, "apps", "web", "package.json")
	before := readText(t, manifest)

	env := LifecycleEnv{Out: os.Stdout, RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}
	output, err := captureStdout(t, func() error {
		return ProjectsSync(context.Background(), root, wsproto.Load(root), []string{"--skip-install"}, false, env)
	})
	if err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	if after := readText(t, manifest); after != before {
		t.Errorf("core rewrote an owned package.json name:\n%s", after)
	}
	if !strings.Contains(output, "web → @acme/web") {
		t.Errorf("sync report lost the divergence it no longer writes:\n%s", output)
	}
}

// An earlier cleanup deleted core's fallback writer for the root `workspaces` array.
//
// The fallback existed so a workspace without the TypeScript extension would not
// be left with a stale array. It is gone because the premise was wrong:
// a workspace with no TypeScript extension has no npm workspace to maintain, and
// a core that keeps writing npm membership "just in case" is exactly the
// language-specific orchestration this epic removes. The array is now written by
// whichever extension declares package.json with a syncTask, and by nobody else.
func TestProjectsSync_NeverWritesPackageWorkspacesItself(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["apps/web"]}`)
	writeProjectFile(t, filepath.Join(root, "package.json"), `{"name":"repo","workspaces":[]}`)
	writeProjectFile(t, filepath.Join(root, "apps", "web", "package.json"), `{"name":"@acme/web"}`)
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })

	before := readText(t, filepath.Join(root, "package.json"))
	if err := ProjectsSync(context.Background(), root, wsproto.Load(root),
		[]string{"--skip-install"}, false, LifecycleEnv{}); err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	if after := readText(t, filepath.Join(root, "package.json")); after != before {
		t.Fatalf("core wrote the npm workspaces array with no adapter to own it:\nbefore %s\nafter  %s",
			before, after)
	}
	var doc struct {
		Workspaces []string `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(root, "package.json"))), &doc); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(doc.Workspaces, []string{}) {
		t.Fatalf("workspaces = %v, want it untouched", doc.Workspaces)
	}
}

// A failing sync task is fatal: it means a manifest core no longer writes did
// not get written by anyone, and reporting success would leave the tree
// half-synced with no signal.
func TestProjectsSync_FailingSyncTaskFailsTheCommand(t *testing.T) {
	root := adapterWorkspace(t)
	env := LifecycleEnv{Out: os.Stdout, RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		return WorkspaceJobResult{Outcome: WorkspaceJobFailed}, nil
	}}
	err := ProjectsSync(context.Background(), root, wsproto.Load(root), []string{"--skip-install"}, false, env)
	if err == nil || !strings.Contains(err.Error(), "workspace sync") {
		t.Fatalf("ProjectsSync error = %v, want a workspace-sync failure", err)
	}
}

// A probe that cannot run must not take `projects sync` down: it is the command
// that REPAIRS a broken workspace. The fixture's adapter has no runtime at all,
// which is exactly that situation.
func TestProjectsSync_SurvivesAnUnavailableProbe(t *testing.T) {
	root := adapterWorkspace(t)
	env := LifecycleEnv{Out: os.Stdout, RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}
	if err := ProjectsSync(context.Background(), root, wsproto.Load(root),
		[]string{"--skip-install"}, false, env); err != nil {
		t.Fatalf("a broken provider took down the recovery command: %v", err)
	}
	// It also must not leave a half-written index behind.
	if _, err := workspace.LoadSnapshot(root); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot is unreadable after a failed probe: %v", err)
	}
}

// Two adapters, discovered in NON-alphabetical order, each declaring its OWN
// syncTask spelling.
//
// This is the shape that unpaired the adapter set: the extensions and the
// bindings were appended in lockstep in discovery order and only the bindings
// were sorted, so `bindings[i]` and `extensions[i]` named different extensions
// and syncCommands() looked up one extension's task in the other's job table.
// Both lookups miss, the command list comes back empty, and `projects sync`
// exits 0 having run NO provider's sync task. It was invisible in production
// only because the three shipped extensions all spell `syncTask`
// "workspace-sync-exec" — which syncCommands' own doc comment says core must
// never assume.
func twoAdapterExtensionManifest(name, task, command string) string {
	return `{
  "name": "` + name + `",
  "version": "0.1.0",
  "cliContract": 4,
  "workspace": {
    "markers": ["` + name[len(name)-1:] + `.marker"],
    "inputs": ["` + name[len(name)-1:] + `.marker"],
    "syncTask": "` + task + `"
  },
  "commands": {
    "` + command + `": {
      "description": "Synchronize native workspace state.",
      "visibility": "internal",
      "run": [{ "id": "sync", "task": "` + task + `" }]
    }
  },
  "tasks": {
    "` + task + `": { "kind": "command", "command": "echo", "cache": false }
  }
}`
}

func TestDiscoverWorkspaceAdapters_SyncCommandsPairEachTaskWithItsOwnExtension(t *testing.T) {
	root := t.TempDir()
	// Discovery walks the workspace's project paths in `includes` order, so
	// listing zeta first makes discovery order the reverse of extension-name
	// order — the only condition the old parallel-slice pairing needed.
	writeProjectFile(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["zeta","alpha"]}`)
	writeProjectFile(t, filepath.Join(root, "zeta", "putnami.json"), `{"name":"@fixture/zeta"}`)
	writeProjectFile(t, filepath.Join(root, "zeta", "putnami.extension.json"),
		twoAdapterExtensionManifest("@fixture/zeta", "zeta-sync-exec", "zeta-workspace-sync"))
	writeProjectFile(t, filepath.Join(root, "alpha", "putnami.json"), `{"name":"@fixture/alpha"}`)
	writeProjectFile(t, filepath.Join(root, "alpha", "putnami.extension.json"),
		twoAdapterExtensionManifest("@fixture/alpha", "alpha-sync-exec", "alpha-workspace-sync"))

	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })

	adapters := discoverWorkspaceAdapters(context.Background(), root, wsproto.Load(root))
	if adapters.discoveryErr != nil {
		t.Fatalf("discovery error: %v", adapters.discoveryErr)
	}
	if len(adapters.adapters) != 2 {
		t.Fatalf("resolved %d adapters, want 2", len(adapters.adapters))
	}
	// Every adapter carries the extension that DECLARED its binding.
	for _, adapter := range adapters.adapters {
		if adapter.ext == nil || adapter.ext.Name != adapter.binding.Scope.Extension {
			t.Fatalf("adapter pairs binding %q with extension %v",
				adapter.binding.Scope.Extension, adapter.ext)
		}
	}

	got := adapters.syncCommands()
	want := []string{"alpha-workspace-sync", "zeta-workspace-sync"}
	if !slices.Equal(got, want) {
		t.Fatalf("syncCommands() = %v, want %v — a mispaired adapter runs no sync task at all", got, want)
	}
}

// The end-to-end half of the same finding: `projects sync` must actually RUN
// both extensions' sync commands, not exit 0 having run neither.
func TestProjectsSync_RunsEverySyncTaskWhenExtensionsNameThemDifferently(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["zeta","alpha"]}`)
	writeProjectFile(t, filepath.Join(root, "zeta", "putnami.json"), `{"name":"@fixture/zeta"}`)
	writeProjectFile(t, filepath.Join(root, "zeta", "putnami.extension.json"),
		twoAdapterExtensionManifest("@fixture/zeta", "zeta-sync-exec", "zeta-workspace-sync"))
	writeProjectFile(t, filepath.Join(root, "alpha", "putnami.json"), `{"name":"@fixture/alpha"}`)
	writeProjectFile(t, filepath.Join(root, "alpha", "putnami.extension.json"),
		twoAdapterExtensionManifest("@fixture/alpha", "alpha-sync-exec", "alpha-workspace-sync"))

	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })

	var ran []string
	env := LifecycleEnv{Out: os.Stdout, RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
		ran = append(ran, req.Job)
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}
	if _, err := captureStdout(t, func() error {
		return ProjectsSync(context.Background(), root, wsproto.Load(root), []string{"--skip-install"}, false, env)
	}); err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	slices.Sort(ran)
	want := []string{"alpha-workspace-sync", "zeta-workspace-sync"}
	if !slices.Equal(ran, want) {
		t.Fatalf("workspace jobs run = %v, want %v", ran, want)
	}
}

// The scan consults extension-declared markers, so a provider that declares a
// marker core's own table does not know still makes its directories
// discoverable.
func TestProjectsSync_ScanHonorsExtensionDeclaredMarkers(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["ext"]}`)
	writeProjectFile(t, filepath.Join(root, "ext", "putnami.json"), `{"name":"@fixture/dotnet"}`)
	writeProjectFile(t, filepath.Join(root, "ext", "putnami.extension.json"), `{
  "name": "@fixture/dotnet",
  "version": "0.1.0",
  "cliContract": 4,
  "workspace": { "markers": ["*.csproj"], "inputs": ["*.csproj"] }
}`)
	writeProjectFile(t, filepath.Join(root, "apps", "api", "Api.csproj"), "<Project/>")
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })

	if err := ProjectsSync(context.Background(), root, wsproto.Load(root),
		[]string{"--skip-install"}, false, LifecycleEnv{}); err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	if got := readText(t, filepath.Join(root, "putnami.workspace.json")); !strings.Contains(got, "apps/api") {
		t.Fatalf("a directory marked only by an extension-declared marker was not discovered:\n%s", got)
	}
}
