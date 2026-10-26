package cli

import (
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// groupDefaultPositionalsExtension is the extension of the workspace the
// e2e/groupdefault tests run: `stage` defaults to `up`, which takes a project
// and runs the serve-workload command stage-up; `list` runs stage-list, which
// declares no workload; `notes` defaults to `show`.
const groupDefaultPositionalsExtension = "e2e/groupdefault/testdata/group-default-positionals/stage-extension"

// groupDefaultPositionalsExtensions loads the fixture's extension.
func groupDefaultPositionalsExtensions(t *testing.T) []*extension.ExtensionDescription {
	t.Helper()
	dir, err := filepath.Abs(groupDefaultPositionalsExtension)
	if err != nil {
		t.Fatal(err)
	}
	ext := extension.LoadExtensionFromDir(dir, "@acme/stage")
	if ext == nil {
		t.Fatal("the fixture extension does not load")
	}
	return []*extension.ExtensionDescription{ext}
}

// A group invocation whose subcommand, or default, runs a serve-workload
// command owns --no-watch as that command does, so `--watch=false` reaches it
// as --no-watch; an invocation that runs another command does not.
func TestParseArgs_GroupInvocationThatServesOwnsNoWatch(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "workload-lifecycle", "a-group-invocation-that-serves-owns-no-watch")
	extensions := groupDefaultPositionalsExtensions(t)
	aliases, _ := processAliases(nil, false, extensions)
	groups := extension.CommandGroupNames(extensions)
	for _, args := range [][]string{
		{"stage", "/apps/web", "--watch=false"},
		{"stage", "up", "/apps/web", "--watch=false"},
		{"stage", "--watch=false"},
		{"stage", "/apps/web", "--no-watch"},
	} {
		parsed := parseArgs(args, aliases, groups)
		if parsed.Err != nil {
			t.Fatalf("%v: %v", args, parsed.Err)
		}
		if parsed.Global.Watch || !slices.Contains(parsed.RawJobArgs, "--no-watch") {
			t.Errorf("%v: watch=%v raw=%v, want --no-watch", args, parsed.Global.Watch, parsed.RawJobArgs)
		}
	}
	for _, args := range [][]string{{"stage", "list", "--watch=false"}, {"notes", "--watch=false"}} {
		parsed := parseArgs(args, aliases, groups)
		if parsed.Err != nil || parsed.Global.Watch || slices.Contains(parsed.RawJobArgs, "--no-watch") {
			t.Errorf("%v: watch=%v raw=%v err=%v, want watch off and no --no-watch", args, parsed.Global.Watch, parsed.RawJobArgs, parsed.Err)
		}
	}
}
