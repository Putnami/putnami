package cli

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// What the single validation pass now rejects, what it
// only warns about, and what it must keep accepting.
//
// An earlier test deliberately left invalid input unpinned (ADR 0001); this change
// alters that. These are the rows that replace it. The complementary guarantee —
// that a VALID invocation still parses to the same tokens, and therefore to the
// same cache keys — lives in parse_acceptance_test.go and
// parse_key_stability_test.go.

// TestValidateStructuredFlags_RejectsUndeclaredFlag covers the hard-rejection
// half of the strictness policy: a built-in structured command is authoritative
// about its own flags, so a typo cannot be an extension's flag and is rejected
// with the accepted list.
func TestValidateStructuredFlags_RejectsUndeclaredFlag(t *testing.T) {
	t.Parallel()
	err := validateStructuredFlags("projects", "create", []string{"my-app", "--templat", "typescript-web"})
	if err == nil {
		t.Fatal("a misspelled --template should be rejected")
	}
	if !errors.Is(err, cmderr.ErrUsage) {
		t.Errorf("error = %v, want a usage-classified error", err)
	}
	if !strings.Contains(err.Error(), "--templat") || !strings.Contains(err.Error(), "--template") {
		t.Errorf("error %q should name both the typo and the accepted flags", err)
	}
}

// TestValidateStructuredFlags_AcceptsDeclaredAndGlobalFlags is the other half:
// everything a valid invocation can legitimately carry still passes.
func TestValidateStructuredFlags_AcceptsDeclaredAndGlobalFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		root, sub string
		args      []string
	}{
		{"projects", "create", []string{"my-app", "--template", "x", "--path", "apps/x", "--force"}},
		{"projects", "sync", []string{"--prune", "--skip-install"}},
		{"projects", "tag", []string{"my-app", "web", "--set"}},
		{"upgrade", "", []string{"--from-source", "--global"}},
		{"install", "", []string{"--latest"}},
		{"extensions", "install", []string{"@putnami/go", "--latest"}},
		// Cross-platform materialization must survive the strict pass,
		// including the inline spelling the global parser normalizes away.
		{"extensions", "install", []string{"--platform", "linux/amd64", "--dest", "./.gen/warm"}},
		{"extensions", "install", []string{"--platform=linux/amd64", "--dest=./.gen/warm"}},
		{"dev", "template", []string{"test", "--keep", "--skip-build"}},
		{"dev", "template", []string{"package", "--version", "1.2.3", "--stable"}},
		{"version", "use", []string{"0.1.0", "-g"}},
		{"doctor", "", []string{"--project", "@putnami/cli"}},
		// Globals are always accepted alongside a command's own flags.
		{"projects", "list", []string{"--output=jsonl", "--quiet"}},
		{"cache", "clean", []string{"--all"}},
		// A bare root resolves through DefaultSub, so it inherits that
		// subcommand's flags rather than looking flag-free.
		{"version", "", []string{"--scope", "libs"}},
		// "init" shares "workspace init"'s documentation, and therefore its flags.
		{"init", "", []string{"--extension", "go", "--force"}},
	}
	for _, tc := range cases {
		if err := validateStructuredFlags(tc.root, tc.sub, tc.args); err != nil {
			t.Errorf("validateStructuredFlags(%q, %q, %v): %v", tc.root, tc.sub, tc.args, err)
		}
	}
}

func TestValidateStructuredFlags_RejectsRemovedPreOSSAliases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		root string
		args []string
	}{
		{root: "upgrade", args: []string{"--putnami-version", "1.2.3"}},
		{root: "upgrade", args: []string{"--local"}},
		{root: "pin", args: []string{"--unpin"}},
	} {
		if err := validateStructuredFlags(tc.root, "", tc.args); err == nil {
			t.Errorf("validateStructuredFlags(%q, %v) accepted a removed pre-OSS alias", tc.root, tc.args)
		}
	}
}

// TestValidateStructuredFlags_LeavesUnknownPathsAlone keeps the rejection
// provable: when the catalog does not know the exact path, it cannot prove a
// flag wrong, so it must not guess. `context pack` is a real, undocumented
// subcommand that reads --check.
func TestValidateStructuredFlags_LeavesUnknownPathsAlone(t *testing.T) {
	t.Parallel()
	if err := validateStructuredFlags("context", "pack", []string{"--check"}); err != nil {
		t.Errorf("an unknown catalog path must stay lenient: %v", err)
	}
	if err := validateStructuredFlags("cloud", "deploy", []string{"--env", "prod"}); err != nil {
		t.Errorf("an extension group is not a built-in path: %v", err)
	}
}

// TestValidateStructuredFlags_PassthroughIsNeverValidated pins the one rule that
// holds on every surface: after a bare "--" the tokens belong to the user.
func TestValidateStructuredFlags_PassthroughIsNeverValidated(t *testing.T) {
	t.Parallel()
	if err := validateStructuredFlags("projects", "list", []string{"--", "--not-a-flag", "-x"}); err != nil {
		t.Errorf("passthrough must never be validated: %v", err)
	}
}

// TestValidateStructuredFlags_IgnoresNegativeNumbers keeps a positional that
// merely starts with a dash out of the flag vocabulary.
func TestValidateStructuredFlags_IgnoresNegativeNumbers(t *testing.T) {
	t.Parallel()
	if err := validateStructuredFlags("config", "set", []string{"retries", "-5"}); err != nil {
		t.Errorf("-5 is a value, not a flag: %v", err)
	}
}

// TestParseArgs_RejectsUndeclaredStructuredFlag wires the rejection through the
// real entry point, and proves --help still works on the same bad invocation.
func TestParseArgs_RejectsUndeclaredStructuredFlag(t *testing.T) {
	t.Parallel()
	if parsed := ParseArgs([]string{"projects", "create", "app", "--bogus"}, nil, nil); parsed.Err == nil {
		t.Fatal("an undeclared built-in flag should be a usage error")
	}
	if parsed := ParseArgs([]string{"projects", "create", "app", "--bogus", "--help"}, nil, nil); parsed.Err != nil {
		t.Fatalf("--help must survive a malformed invocation: %v", parsed.Err)
	}
}

// TestParseArgs_JobCommandFlagsStayLenient is the counterweight: a job command's
// flags come from manifests, so ParseArgs must not reject them. The warning
// window is applied later, where the manifests are known.
func TestParseArgs_JobCommandFlagsStayLenient(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"build", "--some-extension-flag", "value"}, nil, nil)
	if parsed.Err != nil {
		t.Fatalf("job command flags must not be rejected at parse time: %v", parsed.Err)
	}
	if len(parsed.RawJobArgs) != 2 {
		t.Errorf("RawJobArgs = %v, want the tokens untouched", parsed.RawJobArgs)
	}
}

// TestParseArgs_RejectsUnparseableGlobalValues threads the two demonstrated
// silent failures through ParseArgs itself.
func TestParseArgs_RejectsUnparseableGlobalValues(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"build", "--retry", "bogus"},
		{"build", "--max-parallel", "bogus"},
		{"build", "--max-parallel"},
	} {
		if parsed := ParseArgs(args, nil, nil); parsed.Err == nil {
			t.Errorf("ParseArgs(%v) should be a usage error", args)
		}
	}
}

// TestParseArgs_EmptyCommandListIsAUsageError covers a spelling that used to
// panic: every comma segment blank left ParseArgs indexing commands[0].
func TestParseArgs_EmptyCommandListIsAUsageError(t *testing.T) {
	t.Parallel()
	if parsed := ParseArgs([]string{","}, nil, nil); parsed.Err == nil {
		t.Fatal(`ParseArgs(",") should be a usage error`)
	}
}

// --- the deprecation warning window ---

func boolFlagDef() extension.FlagDefinition  { return proto.FlagDefinition{Type: "boolean"} }
func valueFlagDef() extension.FlagDefinition { return proto.FlagDefinition{Type: "string"} }

func TestUndeclaredFlagWarnings_WarnsInsteadOfFailing(t *testing.T) {
	t.Parallel()
	declared := map[string]extension.FlagDefinition{"minify": boolFlagDef()}
	warnings := undeclaredFlagWarnings("build", []string{"--minify", "--mnify"}, declared)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
	if !strings.Contains(warnings[0], "--mnify") || !strings.Contains(warnings[0], undeclaredFlagDeprecation) {
		t.Errorf("warning %q should name the flag and the deprecation window", warnings[0])
	}
}

func TestUndeclaredFlagWarnings_QuietForKnownShapes(t *testing.T) {
	t.Parallel()
	declared := map[string]extension.FlagDefinition{
		"minify": boolFlagDef(),
		"target": valueFlagDef(),
		"token":  {Type: "string", Short: "t"},
		"config": {Type: "string", Short: "-c"},
	}
	cases := [][]string{
		{"--minify"},
		{"--target=wasm"},
		{"--target", "wasm"},
		// Short aliases are manifest-declared spellings even though the map is
		// keyed by the long flag name.
		{"-t", "secret"},
		{"-c=config.json"},
		// buildCommandParams maps --no-minify onto the declared "minify" flag, so
		// the negation is a use of a declared flag, not an unknown one.
		{"--no-minify"},
		// CLI-wide globals are never an extension's business.
		{"--verbose", "--projects", "x"},
		// Passthrough is the user's payload.
		{"--", "--anything", "-x"},
		// A positional is not a flag.
		{"@putnami/cli"},
	}
	for _, args := range cases {
		if warnings := undeclaredFlagWarnings("build", args, declared); len(warnings) != 0 {
			t.Errorf("undeclaredFlagWarnings(%v) = %v, want none", args, warnings)
		}
	}
}

// The core reads --channel, --baseline-channel, --visibility and --release from
// a job-running command itself: no manifest declares them, because the
// coordinator that consumes them is the CLI. Without this exemption every
// release-set publish would warn about the exact flag it requires.
func TestUndeclaredFlagWarningsExemptCoreJobFlags(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{"--channel", "canary"},
		{"--channel=canary,next"},
		{"--baseline-channel", "canary"},
		{"--baseline-channel=canary"},
		{"--visibility", "public"},
		{"--scope", "typescript"},
		{"--release", "rs_" + strings.Repeat("a", 64)},
	}
	for _, args := range cases {
		if warnings := undeclaredFlagWarnings("publish", args, nil); len(warnings) != 0 {
			t.Errorf("undeclaredFlagWarnings(%v) = %v, want none", args, warnings)
		}
	}
	// The exemption is exact: a neighboring spelling is still undeclared.
	if warnings := undeclaredFlagWarnings("publish", []string{"--channels", "canary"}, nil); len(warnings) != 1 {
		t.Errorf("undeclaredFlagWarnings(--channels) = %v, want one warning", warnings)
	}
}

func TestUndeclaredFlagWarnings_ReportsEachFlagOnce(t *testing.T) {
	t.Parallel()
	warnings := undeclaredFlagWarnings("build", []string{"--a", "--b", "--a"}, nil)
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want one per distinct flag", warnings)
	}
	if !strings.Contains(warnings[0], "--a") || !strings.Contains(warnings[1], "--b") {
		t.Errorf("warnings %v should be in first-appearance order", warnings)
	}
}

// --- conflicting multi-command flags ---

func jobWithFlag(ext, name, flag string, def extension.FlagDefinition) *extension.JobDefinition {
	return &extension.JobDefinition{
		Name:          name,
		ExtensionName: ext,
		Flags:         map[string]extension.FlagDefinition{flag: def},
	}
}

func TestConflictingTaskFlags_NamesTheConflictingTasks(t *testing.T) {
	t.Parallel()
	jobMap := map[string][]*extension.JobDefinition{
		"build":   {jobWithFlag("@putnami/go", "build", "stable", boolFlagDef())},
		"publish": {jobWithFlag("@putnami/cloud", "publish", "stable", valueFlagDef())},
	}

	err := conflictingTaskFlags([]string{"build", "publish"}, jobMap, []string{"--stable"})
	if err == nil {
		t.Fatal("one token cannot carry two flag definitions")
	}
	for _, want := range []string{"--stable", "build", "publish"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestConflictingTaskFlags_SilentWhenCompatibleOrUnused(t *testing.T) {
	t.Parallel()
	compatible := map[string][]*extension.JobDefinition{
		"build": {jobWithFlag("@putnami/go", "build", "stable", boolFlagDef())},
		"test":  {jobWithFlag("@putnami/ts", "test", "stable", boolFlagDef())},
	}
	if err := conflictingTaskFlags([]string{"build", "test"}, compatible, []string{"--stable"}); err != nil {
		t.Errorf("identical definitions are not a conflict: %v", err)
	}

	conflicting := map[string][]*extension.JobDefinition{
		"build":   {jobWithFlag("@putnami/go", "build", "stable", boolFlagDef())},
		"publish": {jobWithFlag("@putnami/cloud", "publish", "stable", valueFlagDef())},
	}
	// The conflict only matters when the flag is actually supplied…
	if err := conflictingTaskFlags([]string{"build", "publish"}, conflicting, []string{"--other"}); err != nil {
		t.Errorf("an unsupplied flag must not fail the run: %v", err)
	}
	// …and never for a single task, which has nothing to conflict with.
	if err := conflictingTaskFlags([]string{"build"}, conflicting, []string{"--stable"}); err != nil {
		t.Errorf("a single task cannot conflict with itself: %v", err)
	}
	// Passthrough is out of scope on this surface too.
	if err := conflictingTaskFlags([]string{"build", "publish"}, conflicting, []string{"--", "--stable"}); err != nil {
		t.Errorf("passthrough must never be validated: %v", err)
	}
}

// --- declared value flags ---

// runFlagSurface is the flag surface of the TypeScript extension's `run`
// command (typescript/extension/putnami.extension.json), plus one array and one
// boolean flag and a short alias, so every declared type is represented.
func runFlagSurface() map[string]extension.FlagDefinition {
	return map[string]extension.FlagDefinition{
		"entrypoint": {Type: "string"},
		"port":       {Type: "number"},
		"args":       {Type: "string"},
		"platforms":  {Type: "array"},
		"minify":     {Type: "boolean"},
		"switch":     {Type: ""},
		"token":      {Type: "string", Short: "t"},
	}
}

// renderBinding binds rawArgs against declared and lists the params as sorted
// "name=GoType(value)" entries, the form parse_key_stability_test.go pins, so a
// value's Go type is part of every assertion.
func renderBinding(rawArgs []string, declared map[string]extension.FlagDefinition) string {
	params := buildCommandParams(rawArgs, declared)
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]string, 0, len(names))
	for _, name := range names {
		entries = append(entries, fmt.Sprintf("%s=%T(%v)", name, params[name], params[name]))
	}
	return strings.Join(entries, " ")
}

// TestBuildCommandParams_DeclaredValueFlagTakesTheNextToken pins the binding
// rule both forms of `putnami run --args "--check --dry-run"` reach: a flag the
// selected tasks declare with a value type takes the next token as its value
// whatever its shape, and every other spelling keeps the shape rule.
func TestBuildCommandParams_DeclaredValueFlagTakesTheNextToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		args     []string
		declared map[string]extension.FlagDefinition
		want     string
	}{
		{"declared string, separate hyphen value", []string{"--args", "--check --dry-run"}, runFlagSurface(),
			"args=string(--check --dry-run)"},
		{"declared string, value with one hyphen", []string{"--args", "-x"}, runFlagSurface(),
			"args=string(-x)"},
		{"declared number, negative value", []string{"--port", "-1"}, runFlagSurface(),
			"port=string(-1)"},
		{"declared array, hyphen value", []string{"--platforms", "--all-of-them"}, runFlagSurface(),
			"platforms=string(--all-of-them)"},
		{"a consumed value is never a flag of its own", []string{"--entrypoint", "--args", "--check"}, runFlagSurface(),
			"check=bool(true) entrypoint=string(--args)"},
		{"single-dash long name", []string{"-args", "--check"}, runFlagSurface(),
			"args=string(--check)"},
		{"undeclared flag keeps the shape rule", []string{"--args", "--check --dry-run"}, nil,
			"args=bool(true) check --dry-run=bool(true)"},
		{"declared boolean keeps the shape rule", []string{"--minify", "--check"}, runFlagSurface(),
			"check=bool(true) minify=bool(true)"},
		{"untyped switch keeps the shape rule", []string{"--switch", "--check"}, runFlagSurface(),
			"check=bool(true) switch=bool(true)"},
		{"short alias keeps the shape rule", []string{"-t", "-x"}, runFlagSurface(),
			"t=bool(true) x=bool(true)"},
		{"negation keeps the shape rule", []string{"--no-args", "--check"}, runFlagSurface(),
			"args=bool(false) check=bool(true)"},
		{"declared value flag last takes no value", []string{"--args"}, runFlagSurface(),
			"args=bool(true)"},
		{"declared value flag never takes the separator", []string{"--args", "--", "--check"}, runFlagSurface(),
			"=bool(true) args=bool(true) check=bool(true)"},
		{"declarations stop at the separator", []string{"--", "--args", "--check"}, runFlagSurface(),
			"=bool(true) args=bool(true) check=bool(true)"},
		{"a non-hyphen value binds the same either way", []string{"--args", "scan"}, runFlagSurface(),
			"args=string(scan)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := renderBinding(tc.args, tc.declared); got != tc.want {
				t.Fatalf("params = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestParsedRunArgs_BothSpellingsBindOneValueWithoutWarning drives both
// spellings of the issue's invocation through ParseArgs, then through the
// binding and the warning pass the terminal adapter runs, against the `run`
// flag surface.
func TestParsedRunArgs_BothSpellingsBindOneValueWithoutWarning(t *testing.T) {
	t.Parallel()
	for _, argv := range [][]string{
		{"run", "--projects", "example", "--args", "--check --dry-run"},
		{"run", "--projects", "example", "--args=--check --dry-run"},
	} {
		parsed := ParseArgs(argv, nil, nil)
		if parsed.Err != nil {
			t.Fatalf("ParseArgs(%q): %v", argv, parsed.Err)
		}
		if parsed.Global.DryRun {
			t.Errorf("ParseArgs(%q) read --dry-run inside the value as the global flag", argv)
		}
		params := buildCommandParams(parsed.RawJobArgs, runFlagSurface())
		if len(params) != 1 || params["args"] != "--check --dry-run" {
			t.Errorf("params for %q = %#v, want only args=\"--check --dry-run\"", argv, params)
		}
		if warnings := undeclaredFlagWarnings("run", parsed.RawJobArgs, runFlagSurface()); len(warnings) != 0 {
			t.Errorf("warnings for %q = %v, want none", argv, warnings)
		}
	}
}

// TestUndeclaredFlagWarnings_SkipTheValueADeclaredFlagTakes holds the warning
// pass to the binding: a token a declared value flag takes is a value, so it is
// never reported, while a flag after it still is.
func TestUndeclaredFlagWarnings_SkipTheValueADeclaredFlagTakes(t *testing.T) {
	t.Parallel()
	if warnings := undeclaredFlagWarnings("run", []string{"--args", "--check --dry-run"}, runFlagSurface()); len(warnings) != 0 {
		t.Errorf("warnings = %v, want none for a declared flag's value", warnings)
	}
	warnings := undeclaredFlagWarnings("run", []string{"--args", "--check", "--bogus"}, runFlagSurface())
	if len(warnings) != 1 || !strings.Contains(warnings[0], "--bogus") {
		t.Errorf("warnings = %v, want exactly one, for --bogus", warnings)
	}
	// Without the declaration the token is a flag, and the warning stays.
	warnings = undeclaredFlagWarnings("run", []string{"--args", "--check --dry-run"}, nil)
	if len(warnings) != 2 || !strings.Contains(warnings[1], "--check --dry-run") {
		t.Errorf("warnings = %v, want one each for --args and --check --dry-run", warnings)
	}
}

// TestConflictingTaskFlags_SkipTheValueADeclaredFlagTakes is the same rule on
// the conflict pass: a value that spells a conflicting flag is not that flag.
func TestConflictingTaskFlags_SkipTheValueADeclaredFlagTakes(t *testing.T) {
	t.Parallel()
	jobMap := map[string][]*extension.JobDefinition{
		"build": {{
			Name: "build", ExtensionName: "@putnami/go",
			Flags: map[string]extension.FlagDefinition{"args": valueFlagDef(), "stable": boolFlagDef()},
		}},
		"publish": {jobWithFlag("@putnami/cloud", "publish", "stable", valueFlagDef())},
	}
	if err := conflictingTaskFlags([]string{"build", "publish"}, jobMap, []string{"--args", "--stable"}); err != nil {
		t.Errorf("a declared flag's value is not a conflicting flag: %v", err)
	}
	if err := conflictingTaskFlags([]string{"build", "publish"}, jobMap, []string{"--args", "x", "--stable"}); err == nil {
		t.Error("a conflicting flag after the value must still fail")
	}
}

// TestJobFlagTokens_MatchTheBinding pins that the validation walk and the
// binding agree token by token: every flag the walk returns is a param name the
// binding produced, and no value the binding took is returned as a flag.
func TestJobFlagTokens_MatchTheBinding(t *testing.T) {
	t.Parallel()
	args := []string{"--entrypoint", "--args", "--port", "-1", "--args", "--check --dry-run", "--minify", "--x", "--", "--args", "--y"}
	got := jobFlagTokens(args, runFlagSurface())
	assertStrings(t, "flags", got, []string{"--entrypoint", "--port", "--args", "--minify", "--x"})
	params := buildCommandParams(args, runFlagSurface())
	for _, flag := range got {
		if _, ok := params[strings.TrimLeft(flag, "-")]; !ok {
			t.Errorf("walk returned %s, which the binding did not bind (params %#v)", flag, params)
		}
	}
}

// --- the one positional-promotion implementation ---

func TestPromoteProjectSelector(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		selector     string
		args         []string
		wantSelector string
		wantArgs     []string
	}{
		{"promotes a leading positional", "", []string{"@putnami/cli", "--target", "wasm"}, "@putnami/cli", []string{"--target", "wasm"}},
		{"promotes the dot selector", "", []string{"."}, ".", nil},
		{"leaves a leading flag alone", "", []string{"--target", "wasm"}, "", []string{"--target", "wasm"}},
		{"leaves a key=value token alone", "", []string{"a=b"}, "", []string{"a=b"}},
		// The unified rule: an explicit selector is never silently replaced.
		{"explicit --projects wins", "@putnami/cli", []string{"stray"}, "@putnami/cli", []string{"stray"}},
		{"--all wins", "*", []string{"stray"}, "*", []string{"stray"}},
		{"no args", "", nil, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := GlobalFlags{Projects: tc.selector}
			got := promoteProjectSelector(&g, tc.args)
			if g.Projects != tc.wantSelector {
				t.Errorf("Projects = %q, want %q", g.Projects, tc.wantSelector)
			}
			assertStrings(t, "args", got, tc.wantArgs)
		})
	}
}

// TestPromoteProjectSelector_NeverMovesParams is the cache-key half of the
// unification: whether or not a leading positional is promoted, the params map
// buildCommandParams derives is identical, because a leading positional never
// becomes a param and never supplies a preceding flag's value.
func TestPromoteProjectSelector_NeverMovesParams(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"@putnami/cli"},
		{"@putnami/cli", "--target", "wasm"},
		{"@putnami/cli", "--minify"},
		{"@putnami/cli", "--minify", "--target=wasm"},
	} {
		promoted := GlobalFlags{}
		kept := GlobalFlags{Projects: "already-selected"}

		withPromotion := buildCommandParams(promoteProjectSelector(&promoted, args), nil)
		withoutPromotion := buildCommandParams(promoteProjectSelector(&kept, args), nil)

		if len(withPromotion) != len(withoutPromotion) {
			t.Fatalf("params differ for %v: %v vs %v", args, withPromotion, withoutPromotion)
		}
		for name, value := range withPromotion {
			if withoutPromotion[name] != value {
				t.Errorf("params[%q] = %v with promotion, %v without (args %v)", name, value, withoutPromotion[name], args)
			}
		}
	}
}
