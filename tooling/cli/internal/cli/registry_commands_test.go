package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/store"
)

// TestDispatchUnknownSubcommandIsUsageError locks in that every command group
// with a subcommand switch routes an unrecognized subcommand to an
// ErrUsage-classified error (mapped to a stable exit code by exitCodeForError),
// rather than a generic failure. The handlers are invoked through the registry
// to exercise the real dispatch path.
func TestDispatchUnknownSubcommandIsUsageError(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()

	groups := []string{"extensions", "templates", "projects", "scopes", "workspace", "cli"}
	for _, name := range groups {
		t.Run(name, func(t *testing.T) {
			cmd, ok := lookupCommand(name)
			if !ok {
				t.Fatalf("command %q is not registered", name)
			}
			env := &CommandEnv{
				Ctx:    context.Background(),
				Cfg:    &wsproto.Config{},
				WsRoot: wsRoot,
				Sub:    "definitely-not-a-subcommand",
			}
			err := cmd.run(env)
			if err == nil {
				t.Fatalf("%s with unknown subcommand returned nil error", name)
			}
			if !errors.Is(err, cmderr.ErrUsage) {
				t.Errorf("%s unknown subcommand error = %v, want ErrUsage-classified", name, err)
			}
		})
	}
}

// TestWorkspaceScopedCommandsRequireWorkspace verifies that command groups
// which operate on a workspace surface ErrNoWorkspace when none is found,
// covering the requireWorkspace dispatch branch.
func TestWorkspaceScopedCommandsRequireWorkspace(t *testing.T) {
	t.Parallel()
	groups := []string{"projects", "scopes"}
	for _, name := range groups {
		t.Run(name, func(t *testing.T) {
			cmd, ok := lookupCommand(name)
			if !ok {
				t.Fatalf("command %q is not registered", name)
			}
			env := &CommandEnv{
				Ctx:    context.Background(),
				Cfg:    &wsproto.Config{},
				WsRoot: "", // no workspace
				Sub:    "list",
			}
			err := cmd.run(env)
			if !errors.Is(err, cmderr.ErrNoWorkspace) {
				t.Errorf("%s without workspace error = %v, want ErrNoWorkspace", name, err)
			}
		})
	}
}

func TestCmdCacheCleanAllUsesParsedGlobalFlag(t *testing.T) {
	dir := t.TempDir()
	storeRoot := filepath.Join(dir, "store")
	t.Setenv("PUTNAMI_STORE_DIR", storeRoot)
	// --all also clears the machine-global artifact store: without this the test
	// wipes the developer's own ~/.putnami/artifacts on every run.
	t.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(dir, "artifacts"))

	blobDir := filepath.Join(storeRoot, "blobs", "ab", "abcdef")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, "meta.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	casDir := filepath.Join(storeRoot, "cas", "ab")
	if err := os.MkdirAll(casDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(casDir, "abcdef"), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	parsed := ParseArgs([]string{"cache", "clean", "--all"}, nil, nil)
	if !parsed.Global.All {
		t.Fatal("cache clean --all did not set the parsed global All flag")
	}
	if len(parsed.RawJobArgs) != 0 {
		t.Fatalf("cache clean --all RawJobArgs = %v, want empty after global parsing", parsed.RawJobArgs)
	}

	app := &App{}
	code := ExitError
	captureStdout(t, func() {
		code = app.runStructuredCommand(context.Background(), parsed, &wsproto.Config{}, "")
	})
	if code != ExitSuccess {
		t.Fatalf("cache clean --all without workspace exit = %d, want %d", code, ExitSuccess)
	}
	if _, err := os.Stat(filepath.Join(storeRoot, "blobs")); !os.IsNotExist(err) {
		t.Fatalf("blobs/ still exists after cache clean --all: %v", err)
	}
	if _, err := os.Stat(filepath.Join(storeRoot, "cas")); !os.IsNotExist(err) {
		t.Fatalf("cas/ still exists after cache clean --all: %v", err)
	}
}

// TestCacheDryRunIsRefusedAtParse locks in that no cache subcommand reads
// --dry-run: the parse refuses it as a usage error that names the flag.
func TestCacheDryRunIsRefusedAtParse(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"cache", "clean", "--dry-run"},
		{"cache", "clean", "--all", "--dry-run"},
		{"cache", "gc", "--dry-run"},
		{"cache", "verify", "--dry-run"},
		{"cache", "--dry-run"},
	} {
		parsed := ParseArgs(args, nil, nil)
		if !errors.Is(parsed.Err, cmderr.ErrUsage) {
			t.Fatalf("ParseArgs(%v) error = %v, want ErrUsage-classified", args, parsed.Err)
		}
		if !strings.Contains(parsed.Err.Error(), "--dry-run") {
			t.Errorf("ParseArgs(%v) error = %q, want it to name --dry-run", args, parsed.Err)
		}
	}
}

// TestBareCacheListsItsSubcommandsAndTouchesNoStore proves a bare `putnami
// cache`, which a reader types to discover its subcommands, deletes nothing:
// it exits with a usage error that lists clean, gc and verify, and neither the
// shared store nor an extension cache hook is touched.
func TestBareCacheListsItsSubcommandsAndTouchesNoStore(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "a-bare-cache-runs-nothing",
		"a-bare-root-exits-with-its-subcommands-and-touches-no-store")
	invocations := map[string][]string{
		"bare": {"cache"},
		"all":  {"cache", "--all"},
	}
	for name, args := range invocations {
		t.Run(name, func(t *testing.T) {
			wsRoot := t.TempDir()
			storeRoot := filepath.Join(t.TempDir(), "store")
			t.Setenv("PUTNAMI_STORE_DIR", storeRoot)
			t.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(t.TempDir(), "artifacts"))
			if err := os.WriteFile(filepath.Join(wsRoot, "putnami.workspace.json"), []byte(`{
				"includes": ["extensions/cache-marker"]
			}`), 0o644); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(wsRoot, "cache-hook-ran")
			writeCacheDryRunMarkerExtension(t, wsRoot, marker)

			cacheStore := store.NewLocalStore(storeRoot)
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "kept.bin"), []byte("kept"), 0o644); err != nil {
				t.Fatal(err)
			}
			hash := strings.Repeat("b", 64)
			if err := cacheStore.Put(hash, &store.Entry{
				Result:   &store.EntryResult{Status: "success"},
				FilesDir: source,
			}); err != nil {
				t.Fatalf("seed cache entry: %v", err)
			}

			parsed := ParseArgs(args, nil, nil)
			if parsed.Err != nil {
				t.Fatalf("ParseArgs(%v): %v", args, parsed.Err)
			}
			registered, ok := lookupCommand("cache")
			if !ok {
				t.Fatal("cache command is not registered")
			}
			err := registered.run(&CommandEnv{
				Ctx:    context.Background(),
				Cfg:    wsproto.Load(wsRoot),
				WsRoot: wsRoot,
				Sub:    parsed.Subcommand,
				Args:   parsed.RawJobArgs,
				Global: parsed.Global,
			})
			if !errors.Is(err, cmderr.ErrUsage) {
				t.Fatalf("putnami %s error = %v, want ErrUsage-classified", strings.Join(args, " "), err)
			}
			for _, want := range []string{
				"cache needs a subcommand",
				"Usage: putnami cache <subcommand>",
				"clean   Clear this repo's shared build cache",
				"gc      Garbage-collect global caches to their budgets",
				"verify  Verify cache determinism and restore equivalence",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("putnami %s error = %q, want it to contain %q", strings.Join(args, " "), err, want)
				}
			}
			if entry, getErr := cacheStore.Get(hash); getErr != nil || entry == nil {
				t.Fatalf("putnami %s deleted a cache entry: entry=%v err=%v", strings.Join(args, " "), entry, getErr)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("putnami %s ran the extension cache hook: stat err=%v", strings.Join(args, " "), statErr)
			}
		})
	}
}

// TestBareRootsWithoutADefaultListTheirSubcommands proves a bare `putnami
// cli` or `putnami tree` answers with the usage error of a bare `putnami
// cache`, which lists the subcommands.
func TestBareRootsWithoutADefaultListTheirSubcommands(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "a-bare-cache-runs-nothing",
		"a-bare-root-exits-with-its-subcommands-and-touches-no-store")
	roots := map[string][]string{
		"cli":  {"list", "use"},
		"tree": {"fingerprint"},
	}
	for root, subcommands := range roots {
		t.Run(root, func(t *testing.T) {
			registered, ok := lookupCommand(root)
			if !ok {
				t.Fatalf("%s command is not registered", root)
			}
			err := registered.run(&CommandEnv{Ctx: context.Background(), Cfg: &wsproto.Config{}})
			if !errors.Is(err, cmderr.ErrUsage) {
				t.Fatalf("putnami %s error = %v, want ErrUsage-classified", root, err)
			}
			if !strings.Contains(err.Error(), root+" needs a subcommand") {
				t.Errorf("putnami %s error = %q, want it to name the missing subcommand", root, err)
			}
			for _, subcommand := range subcommands {
				if !strings.Contains(err.Error(), "\n  "+subcommand) {
					t.Errorf("putnami %s error = %q, want it to list %q", root, err, subcommand)
				}
			}
		})
	}
}

func writeCacheDryRunMarkerExtension(t *testing.T, wsRoot, marker string) {
	t.Helper()
	extDir := filepath.Join(wsRoot, "extensions", "cache-marker")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "putnami.json"), []byte(`{"name":"@test/cache-marker"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nset -eu\n: > %q\nprintf '%%s\\n' '{\"v\":1,\"type\":\"summary\",\"data\":{\"freedBytes\":0}}'\n", marker)
	if err := os.WriteFile(filepath.Join(extDir, "cache-marker.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{
		"name": "@test/cache-marker",
		"cliContract": %d,
		"commands": {
			"cache-clean": {"visibility": "internal", "run": [{"id": "cache-clean", "task": "cache-marker"}]},
			"cache-gc": {"visibility": "internal", "run": [{"id": "cache-gc", "task": "cache-marker"}]}
		},
		"tasks": {
			"cache-marker": {"kind": "command", "command": "{extensionRoot}/cache-marker.sh", "cache": false}
		}
	}`, protocolcli.CurrentContract)
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCmdUpgrade_GlobalOutsideWorkspaceRejectsWorkspacePhases locks in that
// --global outside a workspace cannot run the workspace-scoped phases:
// asking for them explicitly is a usage error rather than a partial run.
// Inside a workspace, --global runs them like a plain upgrade (see
// TestCmdUpgrade_GlobalInsideWorkspaceRunsWorkspacePhases).
func TestCmdUpgrade_GlobalOutsideWorkspaceRejectsWorkspacePhases(t *testing.T) {
	t.Parallel()
	cmd, ok := lookupCommand("upgrade")
	if !ok {
		t.Fatal("upgrade command is not registered")
	}
	for _, args := range [][]string{
		{"--global", "--deps"},
		{"--global", "--extensions"},
		{"-g", "--deps"},
	} {
		env := &CommandEnv{Ctx: context.Background(), Cfg: &wsproto.Config{}, WsRoot: "", Args: args}
		err := cmd.run(env)
		if !errors.Is(err, cmderr.ErrUsage) {
			t.Errorf("upgrade %v error = %v, want ErrUsage-classified", args, err)
		}
	}
}

// TestCmdUpgrade_GlobalInsideWorkspaceRunsWorkspacePhases verifies that
// --global inside a workspace behaves like a plain upgrade with the CLI
// phase retargeted at the global install: the extensions, templates, and
// dependency phases still run instead of stopping after the CLI.
func TestCmdUpgrade_GlobalInsideWorkspaceRunsWorkspacePhases(t *testing.T) {
	hometest.Temp(t)
	t.Setenv("SHELL", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", Version)
	}))
	defer srv.Close()
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")

	cmd, ok := lookupCommand("upgrade")
	if !ok {
		t.Fatal("upgrade command is not registered")
	}
	env := &CommandEnv{Ctx: context.Background(), Cfg: &wsproto.Config{}, WsRoot: t.TempDir(), Args: []string{"--global"}}
	var err error
	output := captureStdout(t, func() {
		err = cmd.run(env)
	})
	if err != nil {
		t.Fatalf("global upgrade inside workspace: %v", err)
	}
	for _, section := range []string{"CLI", "Extensions", "Templates", "Dependencies"} {
		if !strings.Contains(output, section) {
			t.Errorf("global upgrade inside a workspace did not run the %s phase; output:\n%s", section, output)
		}
	}
}

func TestCmdUpgrade_RequiresWorkspaceWithoutGlobal(t *testing.T) {
	t.Parallel()
	cmd, ok := lookupCommand("upgrade")
	if !ok {
		t.Fatal("upgrade command is not registered")
	}
	env := &CommandEnv{Ctx: context.Background(), Cfg: &wsproto.Config{}, WsRoot: "", Args: []string{"--cli"}}
	if err := cmd.run(env); !errors.Is(err, cmderr.ErrNoWorkspace) {
		t.Errorf("err = %v, want ErrNoWorkspace", err)
	}
}

// TestCmdUpgrade_GlobalNeedsNoWorkspace runs a --global upgrade with no
// workspace against a loopback registry that reports the running version, so
// the CLI phase resolves to "already up to date" without writing anything.
func TestCmdUpgrade_GlobalNeedsNoWorkspace(t *testing.T) {
	hometest.Temp(t)
	t.Setenv("SHELL", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", Version)
	}))
	defer srv.Close()
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")

	cmd, ok := lookupCommand("upgrade")
	if !ok {
		t.Fatal("upgrade command is not registered")
	}
	env := &CommandEnv{Ctx: context.Background(), Cfg: &wsproto.Config{}, WsRoot: "", Args: []string{"--global"}}
	var err error
	output := captureStdout(t, func() {
		err = cmd.run(env)
	})
	if err != nil {
		t.Fatalf("global upgrade without workspace: %v", err)
	}
	if !strings.Contains(output, "only the global CLI was upgraded") {
		t.Errorf("global upgrade outside a workspace should hint at the skipped workspace phases; output:\n%s", output)
	}
}

func TestResolveBinDir(t *testing.T) {
	home := hometest.Temp(t)
	global := filepath.Join(home, ".putnami", "bin")
	if got := resolveBinDir("/ws", []string{"--global"}); got != global {
		t.Errorf("--global binDir = %q, want %q", got, global)
	}
	if got := resolveBinDir("/ws", []string{"-g"}); got != global {
		t.Errorf("-g binDir = %q, want %q", got, global)
	}
	if got, want := resolveBinDir("/ws", nil), filepath.Join("/ws", ".putnami", "bin"); got != want {
		t.Errorf("workspace binDir = %q, want %q", got, want)
	}
}

// TestCmdCLI_BinaryManagementRequiresWorkspaceOrGlobal verifies that the
// CLI-binary subcommands (list/use) need a workspace unless --global retargets
// them at ~/.putnami/bin, mirroring `upgrade --global`.
func TestCmdCLI_BinaryManagementRequiresWorkspaceOrGlobal(t *testing.T) {
	t.Parallel()
	cmd, ok := lookupCommand("cli")
	if !ok {
		t.Fatal("cli command is not registered")
	}
	for _, sub := range []string{"list", "use"} {
		env := &CommandEnv{Ctx: context.Background(), Cfg: &wsproto.Config{}, WsRoot: "", Sub: sub, Args: []string{"go-dev"}}
		if err := cmd.run(env); !errors.Is(err, cmderr.ErrNoWorkspace) {
			t.Errorf("cli %s without workspace: err = %v, want ErrNoWorkspace", sub, err)
		}
	}
}

func TestCmdCLI_ListGlobalNeedsNoWorkspace(t *testing.T) {
	hometest.Temp(t)
	cmd, ok := lookupCommand("cli")
	if !ok {
		t.Fatal("cli command is not registered")
	}
	// Empty ~/.putnami/bin: list succeeds and reports nothing installed.
	env := &CommandEnv{Ctx: context.Background(), Cfg: &wsproto.Config{}, WsRoot: "", Sub: "list", Args: []string{"--global"}}
	output := captureStdout(t, func() {
		if err := cmd.run(env); err != nil {
			t.Fatalf("cli list --global: %v", err)
		}
	})
	if !strings.Contains(output, "No CLI versions installed") {
		t.Errorf("expected empty-list message; got:\n%s", output)
	}
}

// The core serves no version command: the installed-binary commands moved to
// the cli noun with no alias (D-018), and version get and tag to @putnami/dev
// (D-010). The catalog documents none of them, and the version root is refused
// as a root that left the core.
func TestVersionIsNoCoreCommand(t *testing.T) {
	t.Parallel()
	if _, ok := lookupCommand("version"); ok {
		t.Fatal("the core still registers a version command")
	}
	for _, path := range []string{"version", "version list", "version use", "version get", "version tag"} {
		if _, ok := commandmeta.Lookup(path); ok {
			t.Errorf("the catalog still documents `%s`", path)
		}
	}
	for _, sub := range []string{"list", "use"} {
		if _, ok := commandmeta.Lookup("cli " + sub); !ok {
			t.Errorf("the catalog does not document `cli %s`", sub)
		}
	}
	if err := refuseRemovedCoreRoot("version", nil, nil); !errors.Is(err, cmderr.ErrUsage) {
		t.Errorf("putnami version without an extension that declares it: err = %v, want a usage error", err)
	}
}

func TestCmdUpgrade_FromSourceRequiresWorkspace(t *testing.T) {
	t.Parallel()
	cmd, ok := lookupCommand("upgrade")
	if !ok {
		t.Fatal("upgrade command is not registered")
	}
	// --from-source needs the source tree even with --global.
	for _, args := range [][]string{{"--from-source"}, {"--from-source", "--global"}} {
		env := &CommandEnv{Ctx: context.Background(), Cfg: &wsproto.Config{}, WsRoot: "", Args: args}
		if err := cmd.run(env); !errors.Is(err, cmderr.ErrNoWorkspace) {
			t.Errorf("upgrade %v without workspace: err = %v, want ErrNoWorkspace", args, err)
		}
	}
}

func TestParseUpgradeFlags_FromSourceRegistryCommand(t *testing.T) {
	t.Parallel()
	fromSource := func(args ...string) bool {
		flags, err := parseUpgradeFlags(args)
		if err != nil {
			t.Fatalf("parseUpgradeFlags(%v): %v", args, err)
		}
		return flags.FromSource
	}
	if !fromSource("--from-source") {
		t.Error("--from-source did not set FromSource")
	}
	if fromSource("--cli") {
		t.Error("FromSource set without the flag")
	}
}

// TestCmdMigrateVNext_FlagWiring pins the dispatch half of `migrate vnext`:
// the catalog's two flags reach the handler, both at
// once is refused rather than resolved by argument order, and the read-only
// mode is the default so a bare invocation can never write the lock.
func TestCmdMigrateVNext_FlagWiring(t *testing.T) {
	t.Parallel()
	cmd, ok := lookupCommand("migrate")
	if !ok {
		t.Fatal("migrate command is not registered")
	}

	t.Run("check and apply are mutually exclusive", func(t *testing.T) {
		env := &CommandEnv{
			Ctx: context.Background(), Cfg: &wsproto.Config{},
			WsRoot: t.TempDir(), Sub: "vnext", Args: []string{"--check", "--apply"},
		}
		err := cmd.run(env)
		if !errors.Is(err, cmderr.ErrUsage) {
			t.Fatalf("err = %v, want ErrUsage", err)
		}
		if !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("err = %v, want it to say the flags conflict", err)
		}
	})

	t.Run("an undeclared flag is rejected", func(t *testing.T) {
		env := &CommandEnv{
			Ctx: context.Background(), Cfg: &wsproto.Config{},
			WsRoot: t.TempDir(), Sub: "vnext", Args: []string{"--force"},
		}
		if err := cmd.run(env); !errors.Is(err, cmderr.ErrUsage) {
			t.Fatalf("err = %v, want ErrUsage", err)
		}
	})

	t.Run("default mode is read-only", func(t *testing.T) {
		// No lock file: the check reports not-found without creating one, which
		// is only reachable if the handler ran in check mode.
		root := t.TempDir()
		env := &CommandEnv{
			Ctx: context.Background(), Cfg: &wsproto.Config{},
			WsRoot: root, Sub: "vnext",
		}
		if err := cmd.run(env); !errors.Is(err, cmderr.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if _, err := os.Stat(filepath.Join(root, "putnami.lock.json")); !os.IsNotExist(err) {
			t.Error("a bare `migrate vnext` created a lock file")
		}
	})

	t.Run("unknown migration source still lists vnext", func(t *testing.T) {
		env := &CommandEnv{
			Ctx: context.Background(), Cfg: &wsproto.Config{},
			WsRoot: t.TempDir(), Sub: "gradle",
		}
		err := cmd.run(env)
		if !errors.Is(err, cmderr.ErrUsage) {
			t.Fatalf("err = %v, want ErrUsage", err)
		}
		if !strings.Contains(err.Error(), "vnext") {
			t.Errorf("err = %v, want the available sources to include vnext", err)
		}
	})

	t.Run("agent-content is no longer a migration source", func(t *testing.T) {
		env := &CommandEnv{
			Ctx: context.Background(), Cfg: &wsproto.Config{},
			WsRoot: t.TempDir(), Sub: "agent-content", Args: []string{"@putnami/contributor"},
		}
		err := cmd.run(env)
		if !errors.Is(err, cmderr.ErrUsage) || !strings.Contains(err.Error(), "unknown migration source: agent-content") {
			t.Fatalf("err = %v, want agent-content refused as an unknown source", err)
		}
	})
}

// TestCmdSessionsExport_FlagWiring binds `sessions export` to the catalog's flag
// surface: --since takes a value and is forwarded, an undeclared flag and a
// positional argument are usage errors, and the subcommand really dispatches.
func TestCmdSessionsExport_FlagWiring(t *testing.T) {
	t.Parallel()
	cmd, ok := lookupCommand("sessions")
	if !ok {
		t.Fatal("sessions command is not registered")
	}
	newEnv := func(root string, args ...string) *CommandEnv {
		return &CommandEnv{
			Ctx: context.Background(), Cfg: &wsproto.Config{},
			WsRoot: root, Sub: "export", Args: args,
		}
	}

	t.Run("an undeclared flag is rejected", func(t *testing.T) {
		// A global flag (--all, --verbose, …) still passes through; this is a
		// spelling the catalog does not declare for any surface.
		if err := cmd.run(newEnv(t.TempDir(), "--until", "2026-01-01T00:00:00Z")); !errors.Is(err, cmderr.ErrUsage) {
			t.Fatalf("err = %v, want ErrUsage", err)
		}
	})

	t.Run("a positional argument is rejected", func(t *testing.T) {
		if err := cmd.run(newEnv(t.TempDir(), "latest")); !errors.Is(err, cmderr.ErrUsage) {
			t.Fatalf("err = %v, want ErrUsage", err)
		}
	})

	t.Run("--since is forwarded and validated", func(t *testing.T) {
		err := cmd.run(newEnv(t.TempDir(), "--since", "yesterday"))
		if !errors.Is(err, cmderr.ErrUsage) {
			t.Fatalf("err = %v, want ErrUsage", err)
		}
		if !strings.Contains(err.Error(), "RFC 3339") {
			t.Errorf("err = %v, want it to name the expected timestamp format", err)
		}
	})

	t.Run("an empty store exports successfully", func(t *testing.T) {
		if err := cmd.run(newEnv(t.TempDir(), "--since", "2026-01-01T00:00:00Z")); err != nil {
			t.Fatalf("sessions export on an empty store: %v", err)
		}
	})
}

// TestCmdSessionsSummary_FlagWiring binds `sessions summary` to the catalog's
// flag surface: --since and --command take values and are forwarded and
// validated, an undeclared flag and a positional argument are usage errors, and
// the subcommand really dispatches.
func TestCmdSessionsSummary_FlagWiring(t *testing.T) {
	t.Parallel()
	cmd, ok := lookupCommand("sessions")
	if !ok {
		t.Fatal("sessions command is not registered")
	}
	newEnv := func(root string, args ...string) *CommandEnv {
		return &CommandEnv{
			Ctx: context.Background(), Cfg: &wsproto.Config{},
			WsRoot: root, Sub: "summary", Args: args,
		}
	}

	t.Run("an undeclared flag is rejected", func(t *testing.T) {
		if err := cmd.run(newEnv(t.TempDir(), "--commands", "build")); !errors.Is(err, cmderr.ErrUsage) {
			t.Fatalf("err = %v, want ErrUsage", err)
		}
	})

	t.Run("a positional argument is rejected", func(t *testing.T) {
		if err := cmd.run(newEnv(t.TempDir(), "latest")); !errors.Is(err, cmderr.ErrUsage) {
			t.Fatalf("err = %v, want ErrUsage", err)
		}
	})

	t.Run("--since is forwarded and validated", func(t *testing.T) {
		err := cmd.run(newEnv(t.TempDir(), "--since", "yesterday"))
		if !errors.Is(err, cmderr.ErrUsage) {
			t.Fatalf("err = %v, want ErrUsage", err)
		}
		if !strings.Contains(err.Error(), "RFC 3339") {
			t.Errorf("err = %v, want it to name the expected timestamp format", err)
		}
	})

	t.Run("--command is forwarded and validated", func(t *testing.T) {
		err := cmd.run(newEnv(t.TempDir(), "--command", "lint,,test"))
		if !errors.Is(err, cmderr.ErrUsage) {
			t.Fatalf("err = %v, want ErrUsage", err)
		}
		if !strings.Contains(err.Error(), "comma-separated command list") {
			t.Errorf("err = %v, want it to name the expected list format", err)
		}
	})

	t.Run("an empty store summarizes successfully", func(t *testing.T) {
		if err := cmd.run(newEnv(t.TempDir(), "--command", "lint,test,build,validate")); err != nil {
			t.Fatalf("sessions summary on an empty store: %v", err)
		}
	})
}
