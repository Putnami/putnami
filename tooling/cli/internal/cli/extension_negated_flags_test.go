package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// A `--no-<flag>` token the HOST owns must still reach an extension that
// declares `<flag>`.
//
// `--no-cache` is a CLI-wide global, so parseGlobalFlags consumes it before it
// can reach RawJobArgs; the extension used to receive only the framework's own
// spelling, `no-cache: true`. A subcommand declaring a `cache` boolean — the
// Cloud extension's `ci retry` and `ci start` — reads `cache` with a `true`
// default, so an explicitly forced-cold run was warm and the server recorded
// nothing. These tests pin the repair on BOTH dispatch paths, through the real
// parse -> dispatch -> subprocess boundary: a params map built by hand cannot
// see a token the parser ate.

func TestNegatedGlobalFlagNames(t *testing.T) {
	t.Parallel()
	boolean := map[string]extension.FlagDefinition{
		"cache": {Type: "boolean", Default: true},
	}
	cases := []struct {
		name  string
		args  []string
		flags map[string]extension.FlagDefinition
		want  []string
	}{
		{
			name:  "a host-consumed negation of a declared boolean is forwarded",
			args:  []string{"cloud", "ci", "retry", "run-1", "--no-cache"},
			flags: boolean,
			want:  []string{"cache"},
		},
		{
			name:  "an undeclared flag receives nothing",
			args:  []string{"cloud", "ci", "retry", "--no-cache"},
			flags: map[string]extension.FlagDefinition{"force": {Type: "boolean"}},
			want:  nil,
		},
		{
			// A manifest that omits `type` means a switch, which is how most
			// manifests spell one.
			name:  "an untyped flag counts as boolean",
			args:  []string{"cloud", "ci", "retry", "--no-cache"},
			flags: map[string]extension.FlagDefinition{"cache": {}},
			want:  []string{"cache"},
		},
		{
			name:  "a declared non-boolean is never negated",
			args:  []string{"cloud", "ci", "retry", "--no-cache"},
			flags: map[string]extension.FlagDefinition{"cache": {Type: "string"}},
			want:  nil,
		},
		{
			// `--no-force` is not a global: it survives into RawJobArgs, where
			// buildCommandParams already turns it into force=false. Forwarding it
			// here too would be a second convention for one fact.
			name:  "a negation the host does not own is left to buildCommandParams",
			args:  []string{"cloud", "ci", "retry", "--no-force"},
			flags: map[string]extension.FlagDefinition{"force": {Type: "boolean"}},
			want:  nil,
		},
		{
			name:  "the positive spelling is not a negation",
			args:  []string{"cloud", "ci", "retry", "--cache"},
			flags: boolean,
			want:  nil,
		},
		{
			name:  "a repeated token is reported once",
			args:  []string{"cloud", "ci", "retry", "--no-cache", "--no-cache"},
			flags: boolean,
			want:  []string{"cache"},
		},
		{
			// --no-color is a global too, so the rule is not written around
			// `cache`; a manifest may not declare `color`, so nothing is forwarded.
			name:  "every negating global is covered by the same rule",
			args:  []string{"cloud", "ci", "retry", "--no-color"},
			flags: map[string]extension.FlagDefinition{"color": {Type: "boolean"}},
			want:  []string{"color"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := negatedGlobalFlagNames(c.args, c.flags)
			if len(got) != len(c.want) {
				t.Fatalf("negatedGlobalFlagNames = %v, want %v", got, c.want)
			}
			for i, name := range c.want {
				if got[i] != name {
					t.Fatalf("negatedGlobalFlagNames = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// The two dispatch paths an extension subcommand can take deliver the negation
// the same way, so an extension author cannot be surprised by which one their
// manifest selected. The fixture subprocess reads its own job context, which is
// the only place the answer is observable.
func TestAppRun_ExtensionSubcommand_ForwardsNegatedBooleanFlag(t *testing.T) {
	requireShell(t)

	cases := []struct {
		name string
		argv []string
	}{
		// `retry` is interactive: the scheduler bypass in
		// runExtensionStructuredCommand.
		{"interactive subcommand", []string{"ops", "retry", "--no-cache"}},
		// `sweep` is planned: runPlannedExtensionAlias -> Engine.Run.
		{"planned alias", []string{"ops", "sweep", "app", "--no-cache"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wsRoot := t.TempDir()
			writeNegationFixture(t, wsRoot)
			writeNegationProbe(t, wsRoot, `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    ctx="$2"
  fi
  shift
done
if grep -q '"cache":false' "$ctx"; then
  exit 0
fi
printf '%s\n' '{"v":2,"type":"log","level":"error","message":"the declared cache flag did not arrive negated"}'
exit 9
`)
			t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
			runAlias(t, wsRoot, c.argv, ExitSuccess)
		})
	}
}

// Without the token the extension keeps its own declared default, so the
// forwarding cannot be a permanent override of a flag nobody typed.
func TestAppRun_ExtensionSubcommand_KeepsTheDeclaredDefaultWithoutTheToken(t *testing.T) {
	requireShell(t)
	wsRoot := t.TempDir()
	writeNegationFixture(t, wsRoot)
	writeNegationProbe(t, wsRoot, `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    ctx="$2"
  fi
  shift
done
if grep -q '"cache":true' "$ctx"; then
  exit 0
fi
printf '%s\n' '{"v":2,"type":"log","level":"error","message":"the declared cache default did not survive"}'
exit 9
`)
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	runAlias(t, wsRoot, []string{"ops", "retry"}, ExitSuccess)
}

// The interactive forwarding is GATED on the declared surface, exactly like the
// dry-run forwarding beside it: a subcommand that never declared `cache` grows
// no stray param. It still receives the framework's own `no-cache`, which is
// what an extension reading the global's own name has always seen.
func TestAppRun_InteractiveSubcommand_LeavesAnUndeclaredFlagAlone(t *testing.T) {
	requireShell(t)
	wsRoot := t.TempDir()
	writeNegationFixture(t, wsRoot)
	writeNegationProbe(t, wsRoot, `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    ctx="$2"
  fi
  shift
done
if grep -q '"cache":' "$ctx"; then
  printf '%s\n' '{"v":2,"type":"log","level":"error","message":"an undeclared cache param reached the subcommand"}'
  exit 9
fi
if grep -q '"no-cache":true' "$ctx"; then
  exit 0
fi
printf '%s\n' '{"v":2,"type":"log","level":"error","message":"the framework no-cache param went missing"}'
exit 9
`)
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	runAlias(t, wsRoot, []string{"ops", "login", "--no-cache"}, ExitSuccess)
}

// A lifecycle job runs uncached because the HOST chose to, not because
// anyone typed --no-cache. The Cloud extension's workspace-install reads its own
// `cache` boolean to decide whether to write .putnami/cache.json, so receiving
// the negation left fresh worktrees without a remote cache. A --no-cache the
// user typed on the lifecycle command itself still arrives, as it does for
// every other command.
func TestRunWorkspaceJob_ForwardsOnlyATypedNoCacheToExtensions(t *testing.T) {
	cases := []struct {
		name    string
		noCache bool
		// check exits 0 when the task context matches the expectation.
		check string
	}{
		{
			name:  "the host's own no-cache stays with the host",
			check: `! grep -q '"cache":false' "$ctx" && ! grep -q '"no-cache":true' "$ctx"`,
		},
		{
			name:    "a typed no-cache reaches the extension",
			noCache: true,
			check:   `grep -q '"cache":false' "$ctx" && grep -q '"no-cache":true' "$ctx"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wsRoot := t.TempDir()
			writeLifecycleFixture(t, wsRoot, "app")
			if err := os.WriteFile(filepath.Join(wsRoot, "installer-extension", "install.sh"), []byte(`#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    ctx="$2"
  fi
  shift
done
if [ -z "$ctx" ] || [ ! -f "$ctx" ]; then
  printf '%s\n' '{"v":2,"type":"log","level":"error","message":"no task context was delivered"}'
  exit 9
fi
if `+c.check+`; then
  exit 0
fi
printf '%s\n' '{"v":2,"type":"log","level":"error","message":"the lifecycle job received the wrong cache params"}'
exit 9
`), 0o755); err != nil {
				t.Fatalf("write install probe: %v", err)
			}
			t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

			var result lifecycle.WorkspaceJobResult
			var err error
			_, stderr := captureLifecycleStreams(t, func() {
				result, err = RunWorkspaceJob(context.Background(), lifecycle.WorkspaceJobRequest{
					WorkspaceRoot: wsRoot,
					Config:        wsproto.Load(wsRoot),
					Job:           "workspace-install",
					NoCache:       c.noCache,
					Out:           os.Stderr,
					Display:       lifecycle.LifecycleDisplay{Verbose: true},
				})
			})
			if err != nil || result.Outcome != lifecycle.WorkspaceJobOK {
				t.Fatalf("RunWorkspaceJob = %+v, %v\n%s", result, err, stderr)
			}
		})
	}
}

// Only the typed flag becomes the lifecycle's NoCache: an adapter that set
// Global.NoCache for itself must not turn it into an instruction to extensions.
func TestLifecycleEnv_ForwardsOnlyATypedNoCache(t *testing.T) {
	t.Parallel()
	typed := lifecycleEnv(&CommandEnv{Global: GlobalFlags{NoCache: true, NoCacheExplicit: true}})
	if !typed.NoCache {
		t.Error("a typed --no-cache must reach the lifecycle jobs")
	}
	internal := lifecycleEnv(&CommandEnv{Global: GlobalFlags{NoCache: true}})
	if internal.NoCache {
		t.Error("an internal NoCache must not reach the lifecycle jobs")
	}
}

// writeNegationProbe replaces the fixture extension's single task script.
func writeNegationProbe(t *testing.T, wsRoot, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wsRoot, "ops-extension", "probe.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write probe script: %v", err)
	}
}

// writeNegationFixture builds a workspace whose one extension exposes the two
// dispatch shapes over the same task: `ops retry` (interactive) and `ops sweep`
// (planned), both declaring a `cache` boolean that defaults to true, plus
// `ops login`, which declares nothing, as the control.
func writeNegationFixture(t *testing.T, wsRoot string) {
	t.Helper()

	writeNegationFile(t, filepath.Join(wsRoot, "putnami.workspace.json"),
		`{"name":"fixture-ws","includes":["ops-extension","app"]}`)

	extDir := filepath.Join(wsRoot, "ops-extension")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extension: %v", err)
	}
	writeNegationFile(t, filepath.Join(extDir, "putnami.json"), `{"name":"@putnami/ops-extension"}`)

	appDir := filepath.Join(wsRoot, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir app: %v", err)
	}
	writeNegationFile(t, filepath.Join(appDir, "putnami.json"),
		`{"name":"app","extensions":["@putnami/ops-extension"]}`)
	writeNegationFile(t, filepath.Join(appDir, "ops.sweep"), "")

	manifest := `{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "name": "@putnami/ops-extension",
  "version": "0.1.0",
  "cliContract": 4,
  "commandGroups": {
    "ops": {
      "description": "Ops group",
      "subcommands": {
        "retry": { "command": "ops-retry", "interactive": true },
        "login": { "command": "ops-login", "interactive": true },
        "sweep": { "command": "ops-sweep" }
      }
    }
  },
  "commands": {
    "ops-retry": {
      "description": "Interactive retry fixture.",
      "flags": {
        "cache": { "type": "boolean", "default": true, "description": "Reuse the remote run cache" }
      },
      "run": [{ "id": "retry", "task": "probe" }]
    },
    "ops-login": {
      "description": "Interactive fixture declaring no cache flag.",
      "run": [{ "id": "login", "task": "probe" }]
    },
    "ops-sweep": {
      "description": "Planned sweep fixture.",
      "flags": {
        "cache": { "type": "boolean", "default": true, "description": "Reuse the remote run cache" }
      },
      "activationFiles": ["ops.sweep"],
      "run": [{ "id": "sweep", "task": "probe" }]
    }
  },
  "tasks": {
    "probe": {
      "kind": "command",
      "command": "{extensionRoot}/probe.sh",
      "cache": false,
      "timeoutMs": 10000
    }
  }
}`
	writeNegationFile(t, filepath.Join(extDir, "putnami.extension.json"), manifest)
}

func writeNegationFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
