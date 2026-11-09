package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/extension"
)

const shortAliasSpec = "cli/job-planning-execution"
const shortAliasRequirement = "a-short-alias-reaches-the-task-under-its-long-name"

func testFlagWithShort(short string) extension.FlagDefinition {
	return extension.FlagDefinition{Type: "string", Short: short}
}

// shortAliasDiscovery is one extension whose test job declares --test with
// the short alias -t, as @putnami/python and @putnami/typescript do.
func shortAliasDiscovery(short string) *extension.DiscoveryResult {
	return &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{{
		Name: "@test/runner",
		Jobs: map[string]*extension.JobDefinition{"test": {
			Name:          "test",
			ExtensionName: "@test/runner",
			Flags:         map[string]extension.FlagDefinition{"test": testFlagWithShort(short)},
		}},
	}}}
}

// resolvedJobParams is what the engine plans with for a job command line:
// the terminal mapping's params, after the validator and the resolver seam.
func resolvedJobParams(t *testing.T, discovered *extension.DiscoveryResult, args ...string) (map[string]any, error) {
	t.Helper()
	parsed := ParseArgs(args, nil, nil)
	if parsed.Err != nil {
		t.Fatal(parsed.Err)
	}
	var stderr strings.Builder
	if err := commandFlagValidator(parsed, &stderr)(discovered, parsed.Global); err != nil {
		return nil, err
	}
	return commandParamsResolver(parsed)(discovered, buildCommandParams(parsed.RawJobArgs)), nil
}

// `-t x` reaches the test task as `test: x`, the params `--test x` gives, so
// the task, its params inputs, its cache key and its run marker cannot tell the
// two spellings apart. A manifest spells the alias with or without its dash.
func TestAJobShortAliasBindsTheLongName(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, shortAliasSpec, shortAliasRequirement, "a-job-short-alias-binds-the-long-name")
	for _, short := range []string{"-t", "t"} {
		long, err := resolvedJobParams(t, shortAliasDiscovery(short), "test", "--test", "NoSuchTest")
		if err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"test", "-t", "NoSuchTest"},
			{"test", "-t=NoSuchTest"},
		} {
			params, err := resolvedJobParams(t, shortAliasDiscovery(short), args...)
			if err != nil {
				t.Fatalf("short %q, %v: %v", short, args, err)
			}
			if params["test"] != "NoSuchTest" || len(params) != len(long) {
				t.Errorf("short %q, %v: params = %v, want %v", short, args, params, long)
			}
			if _, kept := params["t"]; kept {
				t.Errorf("short %q, %v: params keep the short name: %v", short, args, params)
			}
		}
	}
}

// A spelling no selected task declares as a short alias keeps its name, and a
// token past the passthrough separator is the user's payload: neither moves.
func TestAShortNameNoTaskDeclaresKeepsItsName(t *testing.T) {
	t.Parallel()
	discovered := shortAliasDiscovery("-t")
	params, err := resolvedJobParams(t, discovered, "test", "--", "-t", "x")
	if err != nil {
		t.Fatal(err)
	}
	if params["t"] != "x" {
		t.Errorf("a passthrough -t moved: %v", params)
	}
	if _, moved := params["test"]; moved {
		t.Errorf("a passthrough -t reached the test param: %v", params)
	}

	// A short name that is also a declared long name keeps that meaning.
	declared := map[string]extension.FlagDefinition{"test": testFlagWithShort("-t"), "t": valueFlagDef()}
	renames, err := shortAliasRenames([]string{"-t", "x"}, declared)
	if err != nil || len(renames) != 0 {
		t.Errorf("shortAliasRenames = %v, %v; want no rename for a declared long name", renames, err)
	}

	// An alias two declarations give to different long names is not guessed.
	declared = map[string]extension.FlagDefinition{"test": testFlagWithShort("-t"), "target": testFlagWithShort("t")}
	if renames, _ := shortAliasRenames([]string{"-t", "x"}, declared); len(renames) != 0 {
		t.Errorf("shortAliasRenames = %v; want no rename for an ambiguous alias", renames)
	}

	// A renamed alias wins over a long name only the passthrough gave.
	resolved := resolveShortAliasParams(map[string]any{"t": "a", "test": "b"}, map[string]string{"t": "test"})
	if len(resolved) != 1 || resolved["test"] != "a" {
		t.Errorf("resolveShortAliasParams = %v, want test: a", resolved)
	}
}

// One flag spelled both ways binds one param twice: a usage error that names
// both spellings, before any task runs.
func TestBothSpellingsOfOneFlagAreRefused(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, shortAliasSpec, shortAliasRequirement, "both-spellings-of-one-flag-are-refused")
	for _, args := range [][]string{
		{"test", "-t", "a", "--test", "b"},
		{"test", "--test=b", "-t=a"},
		{"test", "-t", "a", "--no-test"},
	} {
		_, err := resolvedJobParams(t, shortAliasDiscovery("-t"), args...)
		if !errors.Is(err, cmderr.ErrUsage) {
			t.Fatalf("%v: validator = %v, want a usage error", args, err)
		}
		if !strings.Contains(err.Error(), "-t and --") {
			t.Errorf("%v: error %q does not name both spellings", args, err)
		}
	}
}

// An extension subcommand resolves its short aliases against its effective
// flags, for the params the job receives and for its run marker alike, and the
// flat job command resolves them through the engine, end to end.
func TestAnExtensionSubcommandShortAliasBindsTheLongName(t *testing.T) {
	requireShell(t)
	spectest.Proves(t, shortAliasSpec, shortAliasRequirement, "an-extension-subcommand-short-alias-binds-the-long-name")
	wsRoot := t.TempDir()
	writeWorkspaceAliasFixture(t, wsRoot)
	manifestPath := filepath.Join(wsRoot, "demo-extension", "putnami.extension.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	withShort := strings.Replace(string(manifest),
		`"dry-run": { "type": "boolean", "default": false, "description": "Preview without changes" }`,
		`"dry-run": { "type": "boolean", "default": false, "description": "Preview without changes" },
        "target": { "type": "string", "short": "-g", "description": "Deployment target" }`, 1)
	if withShort == string(manifest) {
		t.Fatal("the fixture manifest no longer declares the dry-run flag this test extends")
	}
	if err := os.WriteFile(manifestPath, []byte(withShort), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAliasFixtureScript(t, wsRoot, `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --putnamiContext) ctx="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if grep -q '"target":"edge"' "$ctx" && ! grep -q '"g":' "$ctx"; then
  exit 0
fi
printf '%s\n' '{"v":2,"type":"log","level":"error","message":"target did not arrive under its long name"}'
exit 9
`)
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

	runAlias(t, wsRoot, []string{"demo", "deploy", "app", "-g", "edge"}, ExitSuccess)
	runAlias(t, wsRoot, []string{"demo", "deploy", "app", "-g", "edge", "--target", "core"}, ExitUsage)
	// The flat job command resolves the same alias through the engine's
	// ResolveCommandParams seam, once discovery knows the flags.
	runAlias(t, wsRoot, []string{"deploy", "app", "-g", "edge"}, ExitSuccess)
	runAlias(t, wsRoot, []string{"deploy", "app", "-g", "edge", "--target", "core"}, ExitUsage)
}
