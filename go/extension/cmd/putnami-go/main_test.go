package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/go/extension/internal/jobs/cachepolicy"
	"go.putnami.dev/go/extension/internal/jobs/depsupgrade"
	"go.putnami.dev/go/extension/internal/jobs/workspaceinstall"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/cli"
)

func TestRunEntrypointRuntimeInfo(t *testing.T) {
	originalVersion := runtimeVersion
	runtimeVersion = "1.2.3-test"
	t.Cleanup(func() { runtimeVersion = originalVersion })

	var out bytes.Buffer
	dispatched := false
	err := runEntrypoint(
		[]string{"__putnami", "runtime-info"},
		&out,
		func(string) string { return "" },
		func(map[string]cli.JobFunc) { dispatched = true },
	)
	if err != nil {
		t.Fatalf("runEntrypoint(runtime-info): %v", err)
	}
	if dispatched {
		t.Fatal("runtime-info must not enter normal command dispatch")
	}

	var info runtimeproto.Info
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatalf("decode runtime info: %v", err)
	}
	if info.Extension != "@putnami/go" || info.Version != "1.2.3-test" {
		t.Fatalf("runtime info = %+v", info)
	}
	if info.RuntimeABI == 0 || info.RuntimeProtocol == 0 || info.CLIContract == 0 {
		t.Fatalf("runtime info omitted compatibility fields: %+v", info)
	}
}

func TestRunEntrypointRuntimeInfoWriteFailure(t *testing.T) {
	err := runEntrypoint(
		[]string{"__putnami", "runtime-info"},
		failingWriter{},
		func(string) string { return "" },
		func(map[string]cli.JobFunc) { t.Fatal("must not dispatch") },
	)
	if err == nil {
		t.Fatal("runtime-info write failure must propagate")
	}
}

func TestRunEntrypointCacheClean(t *testing.T) {
	cacheRoot := t.TempDir()
	buildDir := filepath.Join(cacheRoot, "build")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "entry"), []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	dispatched := false
	err := runEntrypoint(
		[]string{cachepolicy.PhaseClean},
		&out,
		func(key string) string {
			if key == "PUTNAMI_GO_CACHE_DIR" {
				return cacheRoot
			}
			return ""
		},
		func(map[string]cli.JobFunc) { dispatched = true },
	)
	if err != nil {
		t.Fatalf("runEntrypoint(cache-clean): %v", err)
	}
	if dispatched {
		t.Fatal("cache-clean must not enter normal command dispatch")
	}
	if _, err := os.Stat(buildDir); !os.IsNotExist(err) {
		t.Fatalf("build cache still exists: %v", err)
	}

	var event struct {
		Type string `json:"type"`
		Data struct {
			FreedBytes int64 `json:"freedBytes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatalf("decode cache-clean event: %v", err)
	}
	if event.Type != "summary" || event.Data.FreedBytes != int64(len("cache")) {
		t.Fatalf("cache-clean event = %+v", event)
	}
}

// TestRunEntrypointCacheGC is the C5 addition: `cache-gc` is a reserved command
// this binary answers itself, not a CLI-owned collector any more. Like
// cache-clean it must never fall through to job dispatch, which would demand a
// project context a workspace-level command does not have.
func TestRunEntrypointCacheGC(t *testing.T) {
	cacheRoot := t.TempDir()

	var out bytes.Buffer
	dispatched := false
	err := runEntrypoint(
		[]string{cachepolicy.PhaseGC},
		&out,
		func(key string) string {
			if key == "PUTNAMI_GO_CACHE_DIR" {
				return cacheRoot
			}
			return ""
		},
		func(map[string]cli.JobFunc) { dispatched = true },
	)
	if err != nil {
		t.Fatalf("runEntrypoint(cache-gc): %v", err)
	}
	if dispatched {
		t.Fatal("cache-gc must not enter normal command dispatch")
	}

	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatalf("decode cache-gc event: %v", err)
	}
	if event.Type != "summary" {
		t.Fatalf("cache-gc event type = %q, want summary", event.Type)
	}
}

func TestRunEntrypointDispatchesCompleteCommandSet(t *testing.T) {
	var commands map[string]cli.JobFunc
	err := runEntrypoint(
		[]string{"build"},
		&bytes.Buffer{},
		func(string) string { return "" },
		func(got map[string]cli.JobFunc) { commands = got },
	)
	if err != nil {
		t.Fatalf("runEntrypoint(build): %v", err)
	}
	for _, name := range []string{
		"build", "build-generate", "build-describe", "test", "lint", "serve",
		"run", "package", "publish", "config-extract", "config-merge",
		"workspace-sync", "workspace-fetch", "workspace-install", "deps-upgrade",
		"validate-api",
	} {
		if commands[name] == nil {
			t.Errorf("command %q is not registered", name)
		}
	}
}

// The lifecycle jobs answer --help as the scripts did, before any job context
// is required: the value of a flag that takes one is never read as a flag.
func TestLifecycleHelp(t *testing.T) {
	cases := []struct {
		args  []string
		usage string
	}{
		{[]string{"workspace-fetch", "--help"}, workspaceinstall.FetchUsage},
		{[]string{"workspace-fetch", "--putnamiContext", "ctx.json", "-h"}, workspaceinstall.FetchUsage},
		{[]string{"workspace-fetch", "--output", "--help"}, ""},
		{[]string{"workspace-install", "--help"}, workspaceinstall.Usage},
		{[]string{"workspace-install", "--putnamiContext", "ctx.json", "-h"}, workspaceinstall.Usage},
		{[]string{"workspace-install", "--output", "--help"}, ""},
		{[]string{"deps-upgrade", "--dry-run", "--help"}, depsupgrade.Usage},
		{[]string{"deps-upgrade", "--version", "--help"}, ""},
		{[]string{"deps-upgrade", "--channel", "-h"}, ""},
		{[]string{"deps-upgrade", "--output=jsonl", "-h"}, depsupgrade.Usage},
		{[]string{"build", "--help"}, ""},
		{nil, ""},
	}
	for _, tc := range cases {
		usage, ok := lifecycleHelp(tc.args)
		if usage != tc.usage || ok != (tc.usage != "") {
			t.Errorf("lifecycleHelp(%q) = %q, %v; want %q", tc.args, usage, ok, tc.usage)
		}
	}
}

// A lifecycle job runs for the workspace: an empty project must not turn it
// into a SKIP, while every other job keeps the SDK default.
func TestDispatchOptionsRunLifecycleJobsWithoutAProject(t *testing.T) {
	for _, name := range []string{"workspace-fetch", "workspace-install", "deps-upgrade"} {
		if got := dispatchOptions([]string{name, "--putnamiContext", "ctx.json"}); len(got) != 1 {
			t.Errorf("dispatchOptions(%s) = %d options, want 1", name, len(got))
		}
	}
	if got := dispatchOptions([]string{"build"}); got != nil {
		t.Errorf("dispatchOptions(build) = %d options, want none", len(got))
	}
	if got := dispatchOptions(nil); got != nil {
		t.Errorf("dispatchOptions() = %d options, want none", len(got))
	}
}

// Asking a lifecycle job for help prints its usage and dispatches nothing.
func TestRunEntrypointAnswersLifecycleHelpWithoutDispatch(t *testing.T) {
	dispatched := false
	err := runEntrypoint(
		[]string{"deps-upgrade", "--help"},
		&bytes.Buffer{},
		func(string) string { return "" },
		func(map[string]cli.JobFunc) { dispatched = true },
	)
	if err != nil {
		t.Fatalf("runEntrypoint(deps-upgrade --help): %v", err)
	}
	if dispatched {
		t.Fatal("--help must not enter normal command dispatch")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}
