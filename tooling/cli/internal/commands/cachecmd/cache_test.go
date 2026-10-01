package cachecmd

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// TestMain removes the program the cache command tests place (fixtureproc),
// which stands in for an extension's cache command on every platform.
func TestMain(m *testing.M) {
	code := m.Run()
	fixtureproc.Remove()
	os.Exit(code)
}

func TestCacheClean(t *testing.T) {
	dir := t.TempDir()

	// Point the resolver at this explicit store so the test does not touch the
	// real machine-global store under ~/.putnami.
	storeRoot := filepath.Join(dir, ".putnami", "store")
	t.Setenv("PUTNAMI_STORE_DIR", storeRoot)

	// Create a fake store with some cached content.
	blobDir := filepath.Join(storeRoot, "blobs", "ab", "abcdef1234")
	os.MkdirAll(blobDir, 0o755)
	os.WriteFile(filepath.Join(blobDir, "meta.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(blobDir, "result.json"), []byte("{}"), 0o644)
	os.MkdirAll(filepath.Join(storeRoot, "cas", "ab"), 0o755)
	os.WriteFile(filepath.Join(storeRoot, "cas", "ab", "abcdef"), []byte("bytes"), 0o644)

	if err := CacheClean(context.Background(), dir, false, nil, ""); err != nil {
		t.Fatalf("CacheClean: %v", err)
	}

	// Cached content (blobs/, cas/) should be gone; the store dir + lock file are
	// preserved so cross-process lock identity stays intact.
	if _, err := os.Stat(filepath.Join(storeRoot, "blobs")); !os.IsNotExist(err) {
		t.Error("blobs/ should have been removed")
	}
	if _, err := os.Stat(filepath.Join(storeRoot, "cas")); !os.IsNotExist(err) {
		t.Error("cas/ should have been removed")
	}
}

func TestCacheCleanEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(dir, ".putnami", "store"))

	// No store directory exists
	err := CacheClean(context.Background(), dir, false, nil, "")
	if err != nil {
		t.Fatalf("CacheClean on empty should not error: %v", err)
	}
}

// writeCacheCommandExtension materializes a workspace-local extension whose
// cache-clean command runs program, so a test can observe exactly what core
// hands a cache command.
func writeCacheCommandExtension(t *testing.T, wsRoot, dirName, extName string, program fixtureproc.Program) {
	t.Helper()
	extDir := filepath.Join(wsRoot, "extensions", dirName)
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "putnami.json"),
		[]byte(`{"name": "`+extName+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	command := fixtureproc.Write(t, filepath.Join(extDir, "cache-command"), program)
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(`{
		"name": "`+extName+`",
		"cliContract": `+strconv.Itoa(protocolcli.CurrentContract)+`,
		"commands": {
			"cache-clean": {
				"visibility": "internal",
				"run": [{"id": "cache-clean", "task": "cache-clean-exec"}]
			}
		},
		"tasks": {
			"cache-clean-exec": {
				"kind": "command",
				"command": "{extensionRoot}/`+filepath.Base(command)+`",
				"cache": false
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunExtensionCacheCommands_IncludesWorkspaceExtensions ensures a local
// extension discovered from workspace projects is asked to collect its caches
// even when it is not listed under the workspace's extensions config — and that
// it receives the C5 contract's roots.
func TestRunExtensionCacheCommands_IncludesWorkspaceExtensions(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv(extensionproto.MachineCacheDirEnv, filepath.Join(wsRoot, "machine-caches"))
	if err := os.WriteFile(filepath.Join(wsRoot, "putnami.workspace.json"), []byte(`{
		"includes": ["extensions/local"]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "runs.jsonl")
	writeCacheCommandExtension(t, wsRoot, "local", "@test/local", fixtureproc.Program{
		Record: record,
		Stdout: `{"v":1,"type":"summary","data":{"freedBytes":0}}` + "\n",
	})

	err := runExtensionCacheCommands(
		context.Background(), wsRoot, &wsproto.Config{}, extensionproto.CommandCacheClean, "")
	if err != nil {
		t.Fatalf("runExtensionCacheCommands: %v", err)
	}

	runs := fixtureproc.Runs(t, record)
	if len(runs) != 1 {
		t.Fatalf("workspace extension cache command ran %d times, want once", len(runs))
	}
	read := func(name string) string {
		value, _ := runs[0].LookupEnv(name)
		return value
	}
	wantMachine := filepath.Join(wsRoot, "machine-caches", "@test-local")
	if got := read("PUTNAMI_EXTENSION_CACHE_ROOT"); got != wantMachine {
		t.Errorf("PUTNAMI_EXTENSION_CACHE_ROOT = %q, want %q", got, wantMachine)
	}
	if _, statErr := os.Stat(wantMachine); statErr != nil {
		t.Errorf("core did not create the extension's machine cache root: %v", statErr)
	}
	// The loaded workspace root is canonicalized (/var → /private/var on macOS),
	// so compare the suffix rather than the literal temp path.
	if got, want := read("PUTNAMI_CACHE_ROOT"), filepath.Join(".putnami", "cache"); !strings.HasSuffix(got, want) {
		t.Errorf("PUTNAMI_CACHE_ROOT = %q, want a path ending in %q", got, want)
	}
	if got := read("PUTNAMI_CACHE_COMMAND"); got != extensionproto.CommandCacheClean {
		t.Errorf("PUTNAMI_CACHE_COMMAND = %q, want %q", got, extensionproto.CommandCacheClean)
	}
}

// TestRunExtensionCacheCommands_AggregatesEveryFailure is the deliberate C5
// behavior change.
//
// The pre-C5 fan-out returned on the FIRST extension error, so a broken
// extension sorted early left every extension after it silently uncollected
// while the command reported a single failure. Aggregation means the command
// still fails — and still ran every extension it could.
func TestRunExtensionCacheCommands_AggregatesEveryFailure(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv(extensionproto.MachineCacheDirEnv, filepath.Join(wsRoot, "machine-caches"))
	if err := os.WriteFile(filepath.Join(wsRoot, "putnami.workspace.json"), []byte(`{
		"includes": ["extensions/a-broken", "extensions/b-broken", "extensions/c-healthy"]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Named so the fan-out's name ordering puts both failures BEFORE the healthy
	// one: with abort-on-first-error the third would never run.
	writeCacheCommandExtension(t, wsRoot, "a-broken", "@test/a-broken", fixtureproc.Program{Stderr: "a exploded\n", Exit: 1})
	writeCacheCommandExtension(t, wsRoot, "b-broken", "@test/b-broken", fixtureproc.Program{Stderr: "b exploded\n", Exit: 1})
	writeCacheCommandExtension(t, wsRoot, "c-healthy", "@test/c-healthy", fixtureproc.Program{
		Record: filepath.Join(wsRoot, "c-ran"),
		Stdout: `{"v":1,"type":"summary","data":{"freedBytes":4096}}` + "\n",
	})

	err := runExtensionCacheCommands(
		context.Background(), wsRoot, &wsproto.Config{}, extensionproto.CommandCacheClean, "")
	if err == nil {
		t.Fatal("two failing extensions produced no error")
	}
	for _, want := range []string{"@test/a-broken", "@test/b-broken"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error does not name %s: %v", want, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(wsRoot, "c-ran")); statErr != nil {
		t.Fatalf("the healthy extension was never asked — the fan-out aborted early: %v", statErr)
	}
}

// TestRunExtensionCacheCommands_WorkspaceFree pins that the fan-out is a no-op
// without a workspace, which is what keeps `cache gc` and `cache clean --all`
// usable outside one.
func TestRunExtensionCacheCommands_WorkspaceFree(t *testing.T) {
	if err := runExtensionCacheCommands(
		context.Background(), "", &wsproto.Config{}, extensionproto.CommandCacheGC, ""); err != nil {
		t.Fatalf("workspace-free fan-out: %v", err)
	}
	if err := runExtensionCacheCommands(
		context.Background(), t.TempDir(), nil, extensionproto.CommandCacheGC, ""); err != nil {
		t.Fatalf("config-free fan-out: %v", err)
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{100, "100 B"},
		{1024, "1.0 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{500 * 1024 * 1024, "500.0 MB"},
	}

	for _, tt := range tests {
		got := formatSize(tt.bytes)
		if got != tt.want {
			t.Errorf("formatSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}
