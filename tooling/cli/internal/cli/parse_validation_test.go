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

// --- job flag values that begin with a hyphen ---

// renderParams binds rawArgs and lists the params as sorted "name=GoType(value)"
// entries, the form parse_key_stability_test.go pins, so a value's Go type is
// part of every assertion.
func renderParams(rawArgs []string) string {
	params := buildCommandParams(rawArgs)
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

// TestIsFlagValue pins the one shape predicate: the token after a flag is its
// value when it does not begin with a hyphen or when it holds whitespace.
func TestIsFlagValue(t *testing.T) {
	t.Parallel()
	for next, want := range map[string]bool{
		"scan":              true,
		"":                  true,
		"linux/amd64":       true,
		"--check --dry-run": true,
		"-x y":              true,
		"--a\t--b":          true,
		"--gate":            false,
		"-x":                false,
		"-1":                false,
		"-":                 false,
		"--":                false,
		"--port=3000":       false,
	} {
		if got := isFlagValue(next); got != want {
			t.Errorf("isFlagValue(%q) = %v, want %v", next, got, want)
		}
	}
}

// TestSplitInlineValues_KeepsAJobTokenWholeOnlyWhereTheSplitMisreadsIt pins
// which "--name=value" tokens a job command keeps whole, and that a global flag,
// a token past the separator, and every token of a built-in or command-group
// invocation are split.
func TestSplitInlineValues_KeepsAJobTokenWholeOnlyWhereTheSplitMisreadsIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		args    []string
		jobArgs bool
		want    []string
	}{
		{"one hyphen word stays on its flag", []string{"--args=--gate"}, true, []string{"--args=--gate"}},
		{"a negative number stays on its flag", []string{"--port=-1"}, true, []string{"--port=-1"}},
		{"a spelling with whitespace is a value", []string{"--watch --port=3000"}, true, []string{"--watch --port=3000"}},
		{"a multi-word hyphen value splits", []string{"--args=--check --dry-run"}, true, []string{"--args", "--check --dry-run"}},
		{"a value holding = splits at the first =", []string{"--args=--port=3000 --watch"}, true, []string{"--args", "--port=3000 --watch"}},
		{"a plain value splits", []string{"--target=linux"}, true, []string{"--target", "linux"}},
		{"a multi-word plain value splits", []string{"--filter=a b"}, true, []string{"--filter", "a b"}},
		{"a global flag always splits", []string{"--projects=-odd"}, true, []string{"--projects", "-odd"}},
		{"past the separator every token splits", []string{"--", "--args=--gate"}, true, []string{"--", "--args", "--gate"}},
		{"a single-dash token never splits", []string{"-x=-1"}, true, []string{"-x=-1"}},
		{"a built-in or command-group invocation always splits", []string{"--args=--gate", "--watch --port=3000"}, false,
			[]string{"--args", "--gate", "--watch --port", "3000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertStrings(t, "tokens", splitInlineValues(tc.args, tc.jobArgs), tc.want)
		})
	}
}

// TestBuildCommandParams_MultiWordValueBindsToTheFlagBefore pins the binding
// rule: before the passthrough separator a flag takes the next token when
// isFlagValue holds, and a bare flag before a one-word hyphen token stays a
// switch. Past the separator, only a token without a leading hyphen is a value.
func TestBuildCommandParams_MultiWordValueBindsToTheFlagBefore(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"multi-word hyphen value", []string{"--args", "--check --dry-run"}, "args=string(--check --dry-run)"},
		{"multi-word value after a single hyphen", []string{"--args", "-x y"}, "args=string(-x y)"},
		{"short spelling", []string{"-t", "-x y"}, "t=string(-x y)"},
		{"one hyphen word is a flag of its own", []string{"--args", "--gate"}, "args=bool(true) gate=bool(true)"},
		{"bare switch before a flag", []string{"--concurrent", "--update-snapshots"}, "concurrent=bool(true) update-snapshots=bool(true)"},
		{"inline one-word hyphen value", []string{"--args=--gate"}, "args=string(--gate)"},
		{"a negation takes no value", []string{"--no-args", "--check --dry-run"}, "args=bool(false) check --dry-run=bool(true)"},
		{"an inline value takes no second value", []string{"--args=x", "--check --dry-run"}, "args=string(x) check --dry-run=bool(true)"},
		{"a flag last takes no value", []string{"--args"}, "args=bool(true)"},
		{"a plain value", []string{"--args", "scan"}, "args=string(scan)"},
		{"a flag never takes the separator", []string{"--args", "--", "--check --dry-run"}, "=bool(true) args=bool(true) check --dry-run=bool(true)"},
		{"past the separator a multi-word hyphen token is a flag", []string{"--", "--args", "--check --dry-run"}, "=bool(true) args=bool(true) check --dry-run=bool(true)"},
		{"past the separator a plain value binds", []string{"--", "--args", "scan"}, "=bool(true) args=string(scan)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := renderParams(tc.args); got != tc.want {
				t.Errorf("params = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestJobFlagTokens_SkipEachValueTheBindingTakes pins the validation walk to the
// binding: it returns every flag token before the separator except a value the
// binding takes, and each flag it returns names a param the binding produced.
func TestJobFlagTokens_SkipEachValueTheBindingTakes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"--args", "--check --dry-run"}, []string{"--args"}},
		{[]string{"--args", "--gate"}, []string{"--args", "--gate"}},
		{[]string{"--args=--gate"}, []string{"--args=--gate"}},
		{[]string{"--args", "--watch --port=3000", "--minify"}, []string{"--args", "--minify"}},
		{[]string{"--no-args", "--check --dry-run"}, []string{"--no-args", "--check --dry-run"}},
		{[]string{"--args", "--check --dry-run", "--", "--x y"}, []string{"--args"}},
		{[]string{"-5", "-x y", "--z"}, []string{"--z"}},
	}
	for _, tc := range cases {
		got := jobFlagTokens(tc.args)
		assertStrings(t, strings.Join(tc.args, " "), got, tc.want)
		params := buildCommandParams(tc.args)
		for _, flag := range got {
			if _, ok := params[flagBaseName(flag)]; !ok {
				t.Errorf("%q: the walk returned %s, which the binding did not bind (%s)", tc.args, flag, renderParams(tc.args))
			}
		}
	}
}

// TestParseArgs_HyphenValueSpellingsBindAndWarn drives each spelling of a
// hyphen-leading `--args` value through ParseArgs, the binding, and the
// undeclared-flag warnings, against a `run` that declares only args.
func TestParseArgs_HyphenValueSpellingsBindAndWarn(t *testing.T) {
	t.Parallel()
	declared := map[string]extension.FlagDefinition{"args": valueFlagDef()}
	cases := []struct {
		name       string
		argv       []string
		wantParams string
		wantWarned []string
	}{
		{"separate multi-word value", []string{"run", ".", "--args", "--check --dry-run"}, "args=string(--check --dry-run)", nil},
		{"inline multi-word value", []string{"run", ".", "--args=--check --dry-run"}, "args=string(--check --dry-run)", nil},
		{"inline one-word value", []string{"run", ".", "--args=--gate"}, "args=string(--gate)", nil},
		{"inline value holding =", []string{"run", ".", "--args=--port=3000 --watch"}, "args=string(--port=3000 --watch)", nil},
		{"separate value with = after its first word", []string{"run", ".", "--args", "--watch --port=3000"}, "args=string(--watch --port=3000)", nil},
		{"separate one-word value is a flag", []string{"run", ".", "--args", "--gate"}, "args=bool(true) gate=bool(true)", []string{"--gate"}},
		{"separate value with = in its first word", []string{"run", ".", "--args", "--port=3000 --watch"}, "args=bool(true) port=string(3000 --watch)", []string{"--port"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parsed := ParseArgs(tc.argv, nil, nil)
			if parsed.Err != nil {
				t.Fatalf("ParseArgs(%q): %v", tc.argv, parsed.Err)
			}
			if parsed.Global.DryRun {
				t.Errorf("ParseArgs(%q) read --dry-run inside the value as the global flag", tc.argv)
			}
			if got := renderParams(parsed.RawJobArgs); got != tc.wantParams {
				t.Errorf("params = %s, want %s", got, tc.wantParams)
			}
			warnings := undeclaredFlagWarnings("run", parsed.RawJobArgs, declared)
			if len(warnings) != len(tc.wantWarned) {
				t.Fatalf("warnings = %q, want one each for %q", warnings, tc.wantWarned)
			}
			for i, spelling := range tc.wantWarned {
				if !strings.HasPrefix(warnings[i], "flag "+spelling+" ") {
					t.Errorf("warning %d = %q, want it for %s", i, warnings[i], spelling)
				}
			}
		})
	}
}

// TestConflictingTaskFlags_SkipOnlyTheValue pins that the conflict pass skips a
// multi-word value and still judges the flag after it.
func TestConflictingTaskFlags_SkipOnlyTheValue(t *testing.T) {
	t.Parallel()
	jobMap := map[string][]*extension.JobDefinition{
		"build":   {jobWithFlag("@putnami/go", "build", "stable", boolFlagDef())},
		"publish": {jobWithFlag("@putnami/cloud", "publish", "stable", valueFlagDef())},
	}
	if err := conflictingTaskFlags([]string{"build", "publish"}, jobMap, []string{"--args", "--check --dry-run", "--stable"}); err == nil {
		t.Error("a conflicting flag after a multi-word value must still fail")
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

		withPromotion := buildCommandParams(promoteProjectSelector(&promoted, args))
		withoutPromotion := buildCommandParams(promoteProjectSelector(&kept, args))

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
