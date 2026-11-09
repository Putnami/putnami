package cli

import (
	"errors"
	"fmt"
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

// resolvedJobParams renders what the engine plans with for a job command
// line: the terminal mapping's params, after the validator and the renames
// seam, in fmt's sorted map form.
func resolvedJobParams(t *testing.T, discovered *extension.DiscoveryResult, args ...string) (string, error) {
	t.Helper()
	parsed := ParseArgs(args, nil, nil)
	if parsed.Err != nil {
		t.Fatal(parsed.Err)
	}
	var stderr strings.Builder
	if err := commandFlagValidator(parsed, &stderr)(discovered, parsed.Global); err != nil {
		return "", err
	}
	renames := commandParamRenames(parsed)(discovered)
	return fmt.Sprint(renameParams(buildCommandParams(parsed.RawJobArgs), renames)), nil
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
		if long != "map[test:NoSuchTest]" {
			t.Fatalf("--test: params = %s", long)
		}
		for _, args := range [][]string{
			{"test", "-t", "NoSuchTest"},
			{"test", "-t=NoSuchTest"},
		} {
			params, err := resolvedJobParams(t, shortAliasDiscovery(short), args...)
			if err != nil {
				t.Fatalf("short %q, %v: %v", short, args, err)
			}
			if params != long {
				t.Errorf("short %q, %v: params = %s, want %s", short, args, params, long)
			}
		}
	}
}

// A spelling no selected task declares as a short alias keeps its name, and a
// token past the passthrough separator is the user's payload: neither moves.
func TestAShortNameNoTaskDeclaresKeepsItsName(t *testing.T) {
	t.Parallel()
	params, err := resolvedJobParams(t, shortAliasDiscovery("-t"), "test", "--", "-t", "x")
	if err != nil {
		t.Fatal(err)
	}
	if params != "map[:true t:x]" {
		t.Errorf("a passthrough -t moved: %s", params)
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

	// A renamed alias wins over a long name only the passthrough gave, and
	// the map the rename read is left as it was.
	passthrough := map[string]string{"t": "a", "test": "b"}
	if got := fmt.Sprint(renameParams(passthrough, map[string]string{"t": "test"})); got != "map[test:a]" {
		t.Errorf("renameParams = %s, want map[test:a]", got)
	}
	if got := fmt.Sprint(passthrough); got != "map[t:a test:b]" {
		t.Errorf("renameParams changed its input: %s", got)
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
