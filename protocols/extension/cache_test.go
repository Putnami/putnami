package extension

import (
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestManifest_CacheHooksAreRejected pins the C5 migration: the cache lifecycle
// left the hook vocabulary for the reserved cache COMMANDS, and a manifest that
// still declares the hooks must FAIL rather than parse into a section nothing
// reads. A tolerated leftover would be the worst of both: the extension believes
// it collects its caches and core never asks it to.
func TestManifest_CacheHooksAreRejected(t *testing.T) {
	for _, hook := range []string{"cacheClean", "cacheGC"} {
		t.Run(hook, func(t *testing.T) {
			data := []byte(`{
				"name": "@putnami/typescript",
				"hooks": {
					"` + hook + `": {"kind": "command", "command": "{extensionRuntime}", "args": ["cache-clean"]}
				}
			}`)

			_, diags := ParseManifest(data)
			if !diag.HasErrors(diags) {
				t.Fatalf("hooks.%s still parses; C5 replaced it with the reserved %q/%q commands",
					hook, CommandCacheClean, CommandCacheGC)
			}
		})
	}
}

// TestManifest_CacheCommandsParse is the replacement shape: hidden commands
// named by the reserved vocabulary, each running one typed task.
func TestManifest_CacheCommandsParse(t *testing.T) {
	data := []byte(`{
		"name": "@putnami/typescript",
		"commands": {
			"cache-clean": {
				"visibility": "internal",
				"run": [{"id": "cache-clean", "task": "cache-clean-exec"}]
			},
			"cache-gc": {
				"visibility": "internal",
				"run": [{"id": "cache-gc", "task": "cache-gc-exec"}]
			}
		},
		"tasks": {
			"cache-clean-exec": {"kind": "command", "command": "{extensionRuntime}", "args": ["cache-clean"], "cache": false},
			"cache-gc-exec": {"kind": "command", "command": "{extensionRuntime}", "args": ["cache-gc"], "cache": false}
		}
	}`)

	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("strict parse produced errors: %v", diags)
	}
	for _, name := range ReservedCacheCommands {
		cmd, ok := m.Commands[name]
		if !ok {
			t.Fatalf("command %q not parsed", name)
		}
		if cmd.Visibility != "internal" {
			t.Errorf("command %q visibility = %q, want internal", name, cmd.Visibility)
		}
		if len(cmd.Run) != 1 {
			t.Fatalf("command %q has %d steps, want exactly one", name, len(cmd.Run))
		}
		if _, ok := m.Tasks[cmd.Run[0].Task]; !ok {
			t.Errorf("command %q runs undeclared task %q", name, cmd.Run[0].Task)
		}
	}
	if m.Hooks != nil {
		t.Error("no hooks were declared but a hooks section was parsed")
	}
}

func TestIsCacheCommand(t *testing.T) {
	for _, name := range ReservedCacheCommands {
		if !IsCacheCommand(name) {
			t.Errorf("IsCacheCommand(%q) = false", name)
		}
	}
	for _, name := range []string{"", "cache", "cacheClean", "build", "workspace-sync"} {
		if IsCacheCommand(name) {
			t.Errorf("IsCacheCommand(%q) = true", name)
		}
	}
	if len(ReservedCacheCommands) != 2 {
		t.Fatalf("ReservedCacheCommands has %d entries, want 2 — a third cache verb is a "+
			"protocol decision", len(ReservedCacheCommands))
	}
}

func TestSharedOCILayerCacheRootEnv(t *testing.T) {
	if SharedOCILayerCacheRootEnv != "PUTNAMI_OCI_CACHE_ROOT" {
		t.Fatalf("SharedOCILayerCacheRootEnv = %q, want PUTNAMI_OCI_CACHE_ROOT", SharedOCILayerCacheRootEnv)
	}
}

func TestCacheSegment(t *testing.T) {
	tests := map[string]string{
		"@putnami/go":         "@putnami-go",
		"@putnami/typescript": "@putnami-typescript",
		"putnami-extension":   "putnami-extension",
		"a/../b":              "a-..-b",
		"":                    "",
		"   ":                 "",
		"..":                  "",
		".":                   "",
		"go.putnami.dev/ext":  "go.putnami.dev-ext",
	}
	for name, want := range tests {
		if got := CacheSegment(name); got != want {
			t.Errorf("CacheSegment(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestCacheSegment_NeverEscapesItsParent is the security-shaped half of the
// mapping: a name is attacker-influenced text from a manifest, and a segment
// that resolved to the parent (or to two segments) would let one extension's
// "own" cache root reach another's — or reach $HOME itself.
func TestCacheSegment_NeverEscapesItsParent(t *testing.T) {
	for _, name := range []string{"../evil", "a/b", `a\b`, "../../../../etc", "./x", "..", "."} {
		segment := CacheSegment(name)
		if segment == "" {
			continue
		}
		if strings.ContainsAny(segment, `/\`) {
			t.Errorf("CacheSegment(%q) = %q contains a separator", name, segment)
		}
		joined := filepath.Join("/parent", segment)
		if !strings.HasPrefix(joined, "/parent/") {
			t.Errorf("CacheSegment(%q) = %q escapes its parent (%q)", name, segment, joined)
		}
	}
}

// TestMachineCacheRoot_Resolution walks the three arms in order. The override
// arm is what tests and locked-down hosts use; the home arm is the real one;
// the workspace arm is the degraded-but-usable fallback.
func TestMachineCacheRoot_Resolution(t *testing.T) {
	t.Run("override parent wins", func(t *testing.T) {
		t.Setenv(MachineCacheDirEnv, "/tmp/putnami-caches")
		got := MachineCacheRoot("@putnami/go", "/ws")
		if want := filepath.Join("/tmp/putnami-caches", "@putnami-go"); got != want {
			t.Fatalf("MachineCacheRoot = %q, want %q", got, want)
		}
	})

	t.Run("distinct extensions get distinct roots", func(t *testing.T) {
		t.Setenv(MachineCacheDirEnv, "/tmp/putnami-caches")
		if MachineCacheRoot("@putnami/go", "/ws") == MachineCacheRoot("@putnami/typescript", "/ws") {
			t.Fatal("two extensions resolved to the same machine cache root")
		}
	})

	t.Run("stable across calls", func(t *testing.T) {
		t.Setenv(MachineCacheDirEnv, "/tmp/putnami-caches")
		first := MachineCacheRoot("@putnami/go", "/ws")
		second := MachineCacheRoot("@putnami/go", "/other-workspace")
		if first != second {
			t.Fatalf("root moved with the workspace: %q vs %q", first, second)
		}
	})

	t.Run("unnamed extension has no root", func(t *testing.T) {
		t.Setenv(MachineCacheDirEnv, "/tmp/putnami-caches")
		if got := MachineCacheRoot("  ", "/ws"); got != "" {
			t.Fatalf("MachineCacheRoot(blank) = %q, want empty", got)
		}
	})

	t.Run("absolute", func(t *testing.T) {
		t.Setenv(MachineCacheDirEnv, "")
		got := MachineCacheRoot("@putnami/go", "/ws")
		if got == "" || !filepath.IsAbs(got) {
			t.Fatalf("MachineCacheRoot = %q, want an absolute path", got)
		}
	})
}

// TestValidateManifest_ReservedCacheCommandShape pins the C5 wave-3 integration
// repair: the fan-out only recognizes a reserved cache command that resolved to
// a runnable single-step job whose task exists, so any other shape must fail
// validation instead of passing the package gate and then being silently never
// asked.
func TestValidateManifest_ReservedCacheCommandShape(t *testing.T) {
	manifest := func(command, task string) *Manifest {
		data := []byte(`{
			"name": "@acme/x",
			"cliContract": 4,
			"commands": {"` + command + `": ` + task + `}
		}`)
		m, diags := ParseManifest(data)
		if m == nil || diag.HasErrors(diags) {
			t.Fatalf("fixture manifest failed to parse: %v", diags)
		}
		return m
	}
	hasCacheError := func(diags []diag.Diagnostic) bool {
		for _, d := range diags {
			if d.Code == "invalid-cache-command" && d.Severity == diag.Error {
				return true
			}
		}
		return false
	}

	t.Run("two steps is rejected", func(t *testing.T) {
		m := manifest("cache-clean",
			`{"run": [{"id": "a", "task": "clean-exec"}, {"id": "b", "task": "clean-exec"}]}`)
		m.Tasks = map[string]TaskDefinition{"clean-exec": {}}
		if !hasCacheError(ValidateManifest(m)) {
			t.Fatal("a two-step reserved cache command passed validation; core would never invoke it")
		}
	})

	t.Run("undefined task is an error not a warning", func(t *testing.T) {
		m := manifest("cache-gc", `{"run": [{"id": "gc", "task": "missing-exec"}]}`)
		if !hasCacheError(ValidateManifest(m)) {
			t.Fatal("a reserved cache command referencing an undefined task passed validation")
		}
	})

	t.Run("cached task is rejected", func(t *testing.T) {
		m := manifest("cache-clean", `{"run": [{"id": "clean", "task": "clean-exec"}]}`)
		m.Tasks = map[string]TaskDefinition{"clean-exec": {}} // cache defaults to enabled
		if !hasCacheError(ValidateManifest(m)) {
			t.Fatal("a reserved cache command running a cacheable task passed validation")
		}
	})

	t.Run("single step with cache disabled passes", func(t *testing.T) {
		m := manifest("cache-gc", `{"run": [{"id": "gc", "task": "gc-exec"}]}`)
		disabled := false
		m.Tasks = map[string]TaskDefinition{"gc-exec": {Cache: &TaskCachePolicy{Enabled: &disabled}}}
		if hasCacheError(ValidateManifest(m)) {
			t.Fatal("a well-shaped reserved cache command was rejected")
		}
	})

	t.Run("non-reserved command names are untouched", func(t *testing.T) {
		m := manifest("build", `{"run": [{"id": "a", "task": "t1"}, {"id": "b", "task": "t2"}]}`)
		if hasCacheError(ValidateManifest(m)) {
			t.Fatal("the reserved-shape rule leaked onto an ordinary command")
		}
	})
}
