package cli

import (
	"reflect"
	"testing"
)

// A parse-acceptance table for VALID invocations only.
//
// The program invariant is "preserve successful human workflows and keystroke
// economy". This table is the machine-readable statement of what "successful"
// means today: after the parser is replaced with one catalog-driven pass,
// every row here must still parse to exactly the same ParsedArgs fields.
//
// Deliberately absent: invalid invocations. A value-taking flag with no value,
// a bad --retry, and a bad --max-parallel are usage errors in flags.go, and an
// unknown flag no extension can own is a usage error in parse.go; this table
// holds only invocations that succeed. See
// doc/adr/0001-cli-foundation-boundaries.md.

// parseAcceptanceCase is one valid invocation and the fields of the parse that
// later slices must reproduce. Zero-valued expectations are asserted too, so a
// row silently gaining a subcommand or a project selector fails.
type parseAcceptanceCase struct {
	name            string
	args            []string
	userAliases     map[string]string
	extensionGroups map[string]bool

	wantCommands   []string
	wantSubcommand string
	wantProjects   string
	wantRawJobArgs []string
	wantHelp       bool
	wantVersion    bool
	wantAll        bool
	wantImpacted   bool
	wantOutput     string
	wantWatch      bool
	wantPlan       bool
	wantVerbose    bool
	wantMaxPar     int
	wantMaxParMode string
	wantRetry      int
}

func TestParseAcceptance_ValidInvocations(t *testing.T) {
	t.Parallel()
	cases := []parseAcceptanceCase{
		// ── Root tasks ────────────────────────────────────────────────
		{
			name:         "bare root task",
			args:         []string{"build"},
			wantCommands: []string{"build"},
		},
		{
			name:         "root task with explicit project list",
			args:         []string{"test", "--projects", "@putnami/cli"},
			wantCommands: []string{"test"},
			wantProjects: "@putnami/cli",
		},
		{
			name:         "root task with --projects=value form",
			args:         []string{"test", "--projects=@putnami/cli"},
			wantCommands: []string{"test"},
			wantProjects: "@putnami/cli",
		},
		{
			name:         "root task with a positional project target",
			args:         []string{"build", "@putnami/cli"},
			wantCommands: []string{"build"},
			wantProjects: "@putnami/cli",
		},
		{
			name:         "root task with --all",
			args:         []string{"build", "--all"},
			wantCommands: []string{"build"},
			wantProjects: "*",
			wantAll:      true,
		},
		{
			name:         "root task with --impacted",
			args:         []string{"build", "--impacted"},
			wantCommands: []string{"build"},
			wantProjects: "[impacted]",
			wantImpacted: true,
		},

		// ── Single-letter aliases ─────────────────────────────────────
		{
			name:         "builtin alias b",
			args:         []string{"b"},
			wantCommands: []string{"build"},
		},
		{
			name:         "builtin alias t",
			args:         []string{"t"},
			wantCommands: []string{"test"},
		},
		{
			name:         "builtin uppercase alias D",
			args:         []string{"D"},
			wantCommands: []string{"deploy"},
		},
		{
			name:         "user alias resolves through a builtin alias",
			args:         []string{"ci"},
			userAliases:  map[string]string{"ci": "b"},
			wantCommands: []string{"build"},
		},

		// ── Comma commands ────────────────────────────────────────────
		{
			name:         "comma commands",
			args:         []string{"lint,test,build"},
			wantCommands: []string{"lint", "test", "build"},
		},
		{
			name:         "comma commands with aliases and --impacted",
			args:         []string{"l,t,b", "--impacted"},
			wantCommands: []string{"lint", "test", "build"},
			wantProjects: "[impacted]",
			wantImpacted: true,
		},
		{
			name:         "comma commands tolerate spaces and empty segments",
			args:         []string{"lint, ,test"},
			wantCommands: []string{"lint", "test"},
		},

		// ── The "." selector ──────────────────────────────────────────
		{
			name:         "dot selector as positional",
			args:         []string{"build", "."},
			wantCommands: []string{"build"},
			wantProjects: ".",
		},
		{
			name:         "dot selector via --projects",
			args:         []string{"build", "--projects", "."},
			wantCommands: []string{"build"},
			wantProjects: ".",
		},
		{
			name:         "dot selector with comma commands",
			args:         []string{"lint,test", "."},
			wantCommands: []string{"lint", "test"},
			wantProjects: ".",
		},

		// ── Project filters ───────────────────────────────────────────
		{
			name:         "comma-separated --projects list",
			args:         []string{"build", "--projects", "@putnami/cli,@putnami/go"},
			wantCommands: []string{"build"},
			wantProjects: "@putnami/cli,@putnami/go",
		},
		{
			name:         "execution and output flags alongside a selector",
			args:         []string{"build", "--projects", "a", "--max-parallel", "4", "--retry", "2", "--output", "jsonl"},
			wantCommands: []string{"build"},
			wantProjects: "a",
			wantOutput:   "jsonl",
			wantMaxPar:   4,
			wantRetry:    2,
		},
		{
			name:           "--max-parallel accepts the named modes",
			args:           []string{"build", "--max-parallel", "eco"},
			wantCommands:   []string{"build"},
			wantMaxParMode: "eco",
		},
		{
			name:         "watch and plan flags",
			args:         []string{"build", "--watch", "--plan", "-v"},
			wantCommands: []string{"build"},
			wantWatch:    true,
			wantPlan:     true,
			wantVerbose:  true,
		},

		// ── Structured command groups ─────────────────────────────────
		{
			name:           "structured command with subcommand",
			args:           []string{"projects", "list"},
			wantCommands:   []string{"projects"},
			wantSubcommand: "list",
		},
		{
			name:           "structured command with subcommand and flags",
			args:           []string{"projects", "create", "my-app", "--template", "typescript-web"},
			wantCommands:   []string{"projects"},
			wantSubcommand: "create",
			wantRawJobArgs: []string{"my-app", "--template", "typescript-web"},
		},
		{
			name:           "structured command with a structured output flag",
			args:           []string{"sessions", "list", "--output=jsonl"},
			wantCommands:   []string{"sessions"},
			wantSubcommand: "list",
			wantOutput:     "jsonl",
		},
		{
			name:         "structured command root with no subcommand",
			args:         []string{"doctor"},
			wantCommands: []string{"doctor"},
		},
		{
			name:           "structured command keeps a leading positional out of --projects",
			args:           []string{"upgrade", "1.2.3"},
			wantCommands:   []string{"upgrade"},
			wantSubcommand: "1.2.3",
		},
		{
			name:           "structured command with a flag before any subcommand",
			args:           []string{"upgrade", "--cli"},
			wantCommands:   []string{"upgrade"},
			wantRawJobArgs: []string{"--cli"},
		},
		{
			name:           "--version after a command stays a command flag",
			args:           []string{"upgrade", "--version", "1.2.3"},
			wantCommands:   []string{"upgrade"},
			wantRawJobArgs: []string{"--version", "1.2.3"},
		},

		// ── Extension command groups ──────────────────────────────────
		{
			name:            "extension command group with subcommand",
			args:            []string{"cloud", "login"},
			extensionGroups: map[string]bool{"cloud": true},
			wantCommands:    []string{"cloud"},
			wantSubcommand:  "login",
		},
		{
			name:            "extension command group passes trailing args through",
			args:            []string{"cloud", "deploy", "--env", "prod"},
			extensionGroups: map[string]bool{"cloud": true},
			wantCommands:    []string{"cloud"},
			wantSubcommand:  "deploy",
			wantRawJobArgs:  []string{"--env", "prod"},
		},
		{
			name:            "unknown group name is a job command, so its positional is a project",
			args:            []string{"cloud", "login"},
			extensionGroups: map[string]bool{"other": true},
			wantCommands:    []string{"cloud"},
			wantProjects:    "login",
		},

		// ── Top-level help and version ────────────────────────────────
		{
			name:     "no arguments prints help",
			args:     nil,
			wantHelp: true,
		},
		{
			name:        "--version alone",
			args:        []string{"--version"},
			wantVersion: true,
		},
		{
			name:        "-V alone",
			args:        []string{"-V"},
			wantVersion: true,
		},
		{
			name:         "--help after a command",
			args:         []string{"build", "--help"},
			wantCommands: []string{"build"},
			wantHelp:     true,
		},
		{
			name:           "help for a structured command",
			args:           []string{"help", "projects"},
			wantCommands:   []string{"help"},
			wantSubcommand: "projects",
		},
		{
			name:         "leading flag with no command prints help",
			args:         []string{"--man"},
			wantHelp:     true,
			wantCommands: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed := ParseArgs(tc.args, tc.userAliases, tc.extensionGroups)

			assertStrings(t, "Commands", parsed.Commands, tc.wantCommands)
			assertStrings(t, "RawJobArgs", parsed.RawJobArgs, tc.wantRawJobArgs)
			if parsed.Subcommand != tc.wantSubcommand {
				t.Errorf("Subcommand = %q, want %q", parsed.Subcommand, tc.wantSubcommand)
			}
			if parsed.Global.Projects != tc.wantProjects {
				t.Errorf("Global.Projects = %q, want %q", parsed.Global.Projects, tc.wantProjects)
			}
			if parsed.Global.Help != tc.wantHelp {
				t.Errorf("Global.Help = %v, want %v", parsed.Global.Help, tc.wantHelp)
			}
			if parsed.Global.Version != tc.wantVersion {
				t.Errorf("Global.Version = %v, want %v", parsed.Global.Version, tc.wantVersion)
			}
			if parsed.Global.All != tc.wantAll {
				t.Errorf("Global.All = %v, want %v", parsed.Global.All, tc.wantAll)
			}
			if parsed.Global.Impacted != tc.wantImpacted {
				t.Errorf("Global.Impacted = %v, want %v", parsed.Global.Impacted, tc.wantImpacted)
			}
			if parsed.Global.Output != tc.wantOutput {
				t.Errorf("Global.Output = %q, want %q", parsed.Global.Output, tc.wantOutput)
			}
			if parsed.Global.Watch != tc.wantWatch {
				t.Errorf("Global.Watch = %v, want %v", parsed.Global.Watch, tc.wantWatch)
			}
			if parsed.Global.Plan != tc.wantPlan {
				t.Errorf("Global.Plan = %v, want %v", parsed.Global.Plan, tc.wantPlan)
			}
			if parsed.Global.Verbose != tc.wantVerbose {
				t.Errorf("Global.Verbose = %v, want %v", parsed.Global.Verbose, tc.wantVerbose)
			}
			if parsed.Global.MaxParallel != tc.wantMaxPar {
				t.Errorf("Global.MaxParallel = %d, want %d", parsed.Global.MaxParallel, tc.wantMaxPar)
			}
			if parsed.Global.MaxParallelMode != tc.wantMaxParMode {
				t.Errorf("Global.MaxParallelMode = %q, want %q", parsed.Global.MaxParallelMode, tc.wantMaxParMode)
			}
			if parsed.Global.Retry != tc.wantRetry {
				t.Errorf("Global.Retry = %d, want %d", parsed.Global.Retry, tc.wantRetry)
			}
			assertStrings(t, "OriginalArgs", parsed.OriginalArgs, tc.args)
		})
	}
}

// assertStrings compares two string slices treating nil and empty as equal, so
// a row does not have to distinguish "no raw args" from "an empty raw slice".
func assertStrings(t *testing.T, field string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", field, got, want)
	}
}
