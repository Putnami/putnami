package cli

import (
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// TestNoCacheProjectsIsAnExecutionPolicyFlag pins --no-cache-projects on all
// three surfaces a global flag has to reach at once — the parser, the
// help/completion catalog, and the run-shaping flags the engine reads — and
// pins what it must NOT reach: the flag is execution policy, exactly like
// --max-parallel and --retry-failed, so it never becomes a command parameter
// and therefore never moves a cache key.
func TestNoCacheProjectsIsAnExecutionPolicyFlag(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "scoped-cache-bypass",
		"no-cache-projects-is-execution-policy-only")

	parsed := ParseArgs([]string{"build", "--no-cache-projects", "/go/samples/service-to-service"}, nil, nil)
	if parsed.Global.NoCacheProjects != "/go/samples/service-to-service" {
		t.Fatalf("--no-cache-projects did not reach the run-shaping flags: %q", parsed.Global.NoCacheProjects)
	}
	// It is the SCOPED form, so it must not imply the run-wide one: --no-cache
	// drops the cache manager entirely and is forwarded to extensions as
	// `cache: false`, neither of which this flag asks for.
	if parsed.Global.NoCache || parsed.Global.NoCacheExplicit {
		t.Error("--no-cache-projects implied --no-cache")
	}
	// It must also not steal the run's project SELECTION: the two are separate
	// lists, and a run that scoped the cache out of one project still builds
	// whatever --projects named.
	if parsed.Global.Projects != "" {
		t.Errorf("--no-cache-projects wrote the project selection: %q", parsed.Global.Projects)
	}
	// Consumed by the global parser, so nothing hands it to a job subprocess or
	// to the params a cache key is computed from.
	for _, arg := range parsed.RawJobArgs {
		if strings.Contains(arg, "no-cache-projects") {
			t.Errorf("--no-cache-projects leaked into the job arguments: %v", parsed.RawJobArgs)
		}
	}
	for name := range parsed.JobFlags {
		if strings.Contains(name, "cache") {
			t.Errorf("--no-cache-projects leaked into the job flags: %v", parsed.JobFlags)
		}
	}

	// The inline spelling reaches the same field, and the value is not eaten by
	// the adjacent --no-cache token.
	inline := ParseArgs([]string{"build", "--no-cache-projects=/lib,@scope/app"}, nil, nil)
	if inline.Global.NoCacheProjects != "/lib,@scope/app" {
		t.Errorf("--no-cache-projects=<list> = %q", inline.Global.NoCacheProjects)
	}
	if both := ParseArgs([]string{"build", "--no-cache", "--no-cache-projects", "/lib"}, nil, nil); !both.Global.NoCache ||
		both.Global.NoCacheProjects != "/lib" {
		t.Errorf("--no-cache and --no-cache-projects did not survive together: %+v", both.Global)
	}

	// The catalog row is what help and every shell completion read.
	var found *commandmeta.GlobalFlag
	for _, flag := range commandmeta.GlobalFlags() {
		if flag.Long == "--no-cache-projects" {
			found = &flag
			break
		}
	}
	if found == nil {
		t.Fatal("--no-cache-projects is absent from the global-flag catalog")
	}
	if found.Type != commandmeta.FlagValue {
		t.Error("--no-cache-projects must take a value: it names a project selector")
	}
	if found.Category != "Execution" {
		t.Errorf("--no-cache-projects category = %q, want Execution", found.Category)
	}
	// Unbounded value: an enumerated candidate list would read as "these are the
	// only projects".
	if len(found.Values) != 0 {
		t.Errorf("--no-cache-projects offers candidate values %v", found.Values)
	}

	// Without the flag the default stands: the whole run may use the cache.
	if ParseArgs([]string{"build"}, nil, nil).Global.NoCacheProjects != "" {
		t.Error("--no-cache-projects defaulted to a non-empty selector")
	}
}

// TestNoCacheProjectsIsNotReadAsANegatedBooleanFlag closes the collision the
// name creates on the interactive extension path: every "--no-<name>" token
// that IS a global flag is delivered to a subcommand as `<name>: false` when it
// declares a boolean of that name. --no-cache-projects takes a VALUE
// and negates nothing, so a subcommand declaring `cache-projects` must not
// receive a parameter — and therefore a cache-key input — nobody wrote.
func TestNoCacheProjectsIsNotReadAsANegatedBooleanFlag(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "scoped-cache-bypass",
		"no-cache-projects-is-execution-policy-only")

	declared := map[string]extension.FlagDefinition{
		"cache-projects": {Type: "boolean"},
		"cache":          {Type: "boolean"},
		"color":          {Type: "boolean"},
	}
	got := negatedGlobalFlagNames([]string{"--no-cache-projects", "/lib", "--no-cache", "--no-color"}, declared)
	for _, name := range got {
		if name == "cache-projects" {
			t.Fatalf("--no-cache-projects was delivered as a negation: %v", got)
		}
	}
	// The genuine boolean negations still travel, so the guard is narrow.
	for _, want := range []string{"cache", "color"} {
		if !slices.Contains(got, want) {
			t.Errorf("negated names %v lost --no-%s", got, want)
		}
	}
}
