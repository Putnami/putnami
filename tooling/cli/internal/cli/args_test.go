package cli

import (
	"strings"
	"testing"
)

func TestParseArgs_NoArgs(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs(nil, nil, nil)
	if !parsed.Global.Help {
		t.Error("no args should show help")
	}
}

func TestParseArgs_SingleCommand(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"build", "--all", "--plan"}, nil, nil)
	if len(parsed.Commands) != 1 || parsed.Commands[0] != "build" {
		t.Errorf("commands = %v, want [build]", parsed.Commands)
	}
	if !parsed.Global.All {
		t.Error("--all should be set")
	}
	if !parsed.Global.Plan {
		t.Error("--plan should be set")
	}
}

func TestParseArgs_MultiCommand(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"lint,test,build", "--all"}, nil, nil)
	if len(parsed.Commands) != 3 {
		t.Fatalf("commands = %v, want [lint, test, build]", parsed.Commands)
	}
	if parsed.Commands[0] != "lint" || parsed.Commands[1] != "test" || parsed.Commands[2] != "build" {
		t.Errorf("commands = %v", parsed.Commands)
	}
}

func TestParseArgs_Aliases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		aliases  map[string]string
		wantCmds []string
	}{
		{
			name:     "single builtin alias",
			args:     []string{"b", "--all"},
			wantCmds: []string{"build"},
		},
		{
			name:     "multi builtin aliases",
			args:     []string{"l,t,b", "--all"},
			wantCmds: []string{"lint", "test", "build"},
		},
		{
			name:     "publish alias remains available",
			args:     []string{"p", "--all"},
			wantCmds: []string{"publish"},
		},
		{
			name:     "uppercase publish alias",
			args:     []string{"P", "--all"},
			wantCmds: []string{"publish"},
		},
		{
			name:     "deploy alias",
			args:     []string{"d", "--all"},
			wantCmds: []string{"deploy"},
		},
		{
			name:     "uppercase deploy alias",
			args:     []string{"D", "--all"},
			wantCmds: []string{"deploy"},
		},
		{
			name:     "user alias",
			args:     []string{"check", "--all"},
			aliases:  map[string]string{"check": "lint,test"},
			wantCmds: []string{"lint,test"}, // resolveAlias doesn't split further
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := ParseArgs(tt.args, tt.aliases, nil)
			if len(parsed.Commands) != len(tt.wantCmds) {
				t.Fatalf("commands = %v, want %v", parsed.Commands, tt.wantCmds)
			}
			for i, want := range tt.wantCmds {
				if parsed.Commands[i] != want {
					t.Errorf("commands[%d] = %q, want %q", i, parsed.Commands[i], want)
				}
			}
		})
	}
}

func TestParseArgs_Positional(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		args         []string
		wantProjects string
	}{
		{
			name:         "dot positional",
			args:         []string{"build", "--retry", "3", ".", "--plan"},
			wantProjects: ".",
		},
		{
			name:         "path positional",
			args:         []string{"build", "tooling/cli", "--plan"},
			wantProjects: "tooling/cli",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := ParseArgs(tt.args, nil, nil)
			if parsed.Global.Projects != tt.wantProjects {
				t.Errorf("projects = %q, want %q", parsed.Global.Projects, tt.wantProjects)
			}
		})
	}
}

func TestParseArgs_DotPositional_WithRetry(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"build", "--retry", "3", ".", "--plan"}, nil, nil)
	if parsed.Global.Retry != 3 {
		t.Errorf("retry = %d, want 3", parsed.Global.Retry)
	}
}

func TestParseArgs_GlobalFlags(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"build", "--verbose", "--no-cache", "--debug", "--quiet", "--watch"}, nil, nil)
	if !parsed.Global.Verbose {
		t.Error("--verbose should be set")
	}
	if !parsed.Global.NoCache {
		t.Error("--no-cache should be set")
	}
	if !parsed.Global.NoCacheExplicit {
		t.Error("--no-cache should be recorded as the user's own request")
	}
	if !parsed.Global.Debug {
		t.Error("--debug should be set")
	}
	if !parsed.Global.Quiet {
		t.Error("--quiet should be set")
	}
	if !parsed.Global.Watch {
		t.Error("--watch should be set")
	}
}

func TestParseArgs_ValueFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		// assertions
		wantMaxParallel int
		wantMaxMode     string
		wantOutput      string
		wantRetry       int
	}{
		{
			name:            "space-separated values",
			args:            []string{"build", "--max-parallel", "4", "--output", "jsonl", "--retry", "3"},
			wantMaxParallel: 4,
			wantOutput:      "jsonl",
			wantRetry:       3,
		},
		{
			name:            "equals-separated values",
			args:            []string{"build", "--output=jsonl", "--max-parallel=8"},
			wantMaxParallel: 8,
			wantOutput:      "jsonl",
		},
		{
			name:        "symbolic max parallel mode",
			args:        []string{"build", "--max-parallel=auto"},
			wantMaxMode: "auto",
		},
		{
			name:        "space-separated symbolic max parallel mode",
			args:        []string{"build", "--max-parallel", "eco"},
			wantMaxMode: "eco",
		},
		{
			name:        "max symbolic max parallel mode",
			args:        []string{"build", "--max-parallel=max"},
			wantMaxMode: "max",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := ParseArgs(tt.args, nil, nil)
			if tt.wantMaxParallel != 0 && parsed.Global.MaxParallel != tt.wantMaxParallel {
				t.Errorf("max-parallel = %d, want %d", parsed.Global.MaxParallel, tt.wantMaxParallel)
			}
			if tt.wantMaxMode != "" && parsed.Global.MaxParallelMode != tt.wantMaxMode {
				t.Errorf("max-parallel mode = %q, want %q", parsed.Global.MaxParallelMode, tt.wantMaxMode)
			}
			if tt.wantOutput != "" && parsed.Global.Output != tt.wantOutput {
				t.Errorf("output = %q, want %q", parsed.Global.Output, tt.wantOutput)
			}
			if tt.wantRetry != 0 && parsed.Global.Retry != tt.wantRetry {
				t.Errorf("retry = %d, want %d", parsed.Global.Retry, tt.wantRetry)
			}
		})
	}
}

func TestParseArgs_RawJobArgs(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"build", "--all", "--transpile", "--target", "node"}, nil, nil) // --transpile and --target node should be in raw job args
	found := make(map[string]bool)
	for _, a := range parsed.RawJobArgs {
		found[a] = true
	}
	if !found["--transpile"] {
		t.Error("--transpile should be in raw job args")
	}
	if !found["--target"] {
		t.Error("--target should be in raw job args")
	}
	if !found["node"] {
		t.Error("node should be in raw job args")
	}
}

func TestParseArgs_StructuredCommand(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"extensions", "install", "--verbose"}, nil, nil)
	if len(parsed.Commands) != 1 || parsed.Commands[0] != "extensions" {
		t.Errorf("commands = %v, want [extensions]", parsed.Commands)
	}
	if parsed.Subcommand != "install" {
		t.Errorf("subcommand = %q, want %q", parsed.Subcommand, "install")
	}
}

func TestParseArgs_ExtensionStructuredCommand(t *testing.T) {
	t.Parallel()
	groups := map[string]bool{"cloud": true}
	parsed := ParseArgs([]string{"cloud", "login", "--tenant", "acme"}, nil, groups)
	if len(parsed.Commands) != 1 || parsed.Commands[0] != "cloud" {
		t.Fatalf("commands = %v, want [cloud]", parsed.Commands)
	}
	if parsed.Subcommand != "login" {
		t.Errorf("subcommand = %q, want %q", parsed.Subcommand, "login")
	}
	if parsed.Global.Projects != "" {
		t.Errorf("projects = %q, want empty (login is a subcommand, not a project selector)", parsed.Global.Projects)
	}
	// --tenant acme should still flow through as raw job args.
	foundTenant := false
	for i, a := range parsed.RawJobArgs {
		if a == "--tenant" && i+1 < len(parsed.RawJobArgs) && parsed.RawJobArgs[i+1] == "acme" {
			foundTenant = true
		}
	}
	if !foundTenant {
		t.Errorf("expected --tenant acme in RawJobArgs, got %v", parsed.RawJobArgs)
	}
}

// TestParseArgs_ExtensionGroupWithoutSubcommand verifies that "cloud" with no
// subcommand keeps the bare command form (so the dispatcher can print group
// help) instead of falling through to project selection.
func TestParseArgs_ExtensionGroupWithoutSubcommand(t *testing.T) {
	t.Parallel()
	groups := map[string]bool{"cloud": true}
	parsed := ParseArgs([]string{"cloud"}, nil, groups)
	if len(parsed.Commands) != 1 || parsed.Commands[0] != "cloud" {
		t.Fatalf("commands = %v, want [cloud]", parsed.Commands)
	}
	if parsed.Subcommand != "" {
		t.Errorf("subcommand = %q, want empty", parsed.Subcommand)
	}
	if parsed.Global.Projects != "" {
		t.Errorf("projects = %q, want empty", parsed.Global.Projects)
	}
}

// TestParseArgs_UnknownCommandStillProjectSelector confirms that unknown
// commands (not in the extension groups set) keep treating their first
// positional as a project selector, preserving flat-job behavior.
func TestParseArgs_UnknownCommandStillProjectSelector(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"build", "tooling/cli"}, nil, nil)
	if parsed.Subcommand != "" {
		t.Errorf("subcommand = %q, want empty", parsed.Subcommand)
	}
	if parsed.Global.Projects != "tooling/cli" {
		t.Errorf("projects = %q, want %q", parsed.Global.Projects, "tooling/cli")
	}
}

func TestParseArgs_HelpFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"help with command", []string{"build", "--help"}},
		{"help without command", []string{"--help"}},
		{"version flag", []string{"--version"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := ParseArgs(tt.args, nil, nil)
			if tt.name == "version flag" {
				if !parsed.Global.Version {
					t.Error("--version should set Version flag")
				}
			} else {
				if !parsed.Global.Help {
					t.Error("--help should be set")
				}
			}
		})
	}
}

func TestParseArgs_CommandVersionFlagIsNotGlobal(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"upgrade", "--version", "0.1.0", "--dry-run"}, nil, nil)
	if parsed.Global.Version {
		t.Fatal("command --version should not set the global version flag")
	}
	if !parsed.Global.DryRun {
		t.Fatal("--dry-run should still be parsed as a global flag")
	}
	wantArgs := []string{"--version", "0.1.0"}
	if len(parsed.RawJobArgs) != len(wantArgs) {
		t.Fatalf("RawJobArgs = %v, want %v", parsed.RawJobArgs, wantArgs)
	}
	for i := range wantArgs {
		if parsed.RawJobArgs[i] != wantArgs[i] {
			t.Fatalf("RawJobArgs = %v, want %v", parsed.RawJobArgs, wantArgs)
		}
	}
}

func TestResolveAlias(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		input     string
		aliases   map[string]string
		want      string
		wantCycle bool
	}{
		{
			name:    "chain resolution",
			input:   "x",
			aliases: map[string]string{"x": "y", "y": "build"},
			want:    "build",
		},
		{
			name:      "circular chain errors at use",
			input:     "x",
			aliases:   map[string]string{"x": "y", "y": "x"},
			wantCycle: true,
		},
		{
			name:      "self-referential alias is a cycle",
			input:     "x",
			aliases:   map[string]string{"x": "x"},
			wantCycle: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveAlias(tt.input, tt.aliases)
			if tt.wantCycle {
				if err == nil {
					t.Fatalf("resolveAlias(%q) = %q, want a cycle error", tt.input, got)
				}
				if !strings.Contains(err.Error(), "cycle") {
					t.Errorf("cycle error = %v, want it to say so", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAlias(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("resolveAlias(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestParseArgs_CyclicAliasErrorsAtUse pins settled decision R9: a dormant
// cyclic alias must not brick the whole workspace, so the failure lands on the
// invocation that actually uses it.
func TestParseArgs_CyclicAliasErrorsAtUse(t *testing.T) {
	t.Parallel()
	aliases := map[string]string{"loop": "spin", "spin": "loop"}

	if parsed := ParseArgs([]string{"build"}, aliases, nil); parsed.Err != nil {
		t.Fatalf("an unused cyclic alias broke an unrelated command: %v", parsed.Err)
	}
	if parsed := ParseArgs([]string{"loop"}, aliases, nil); parsed.Err == nil {
		t.Fatal("invoking the cyclic alias should fail")
	}
}

func TestIsStructuredCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		{"extensions", "extensions", true},
		{"projects", "projects", true},
		{"workspace", "workspace", true},
		{"version", "version", true},
		{"dev", "dev", true},
		{"deps", "deps", true},
		{"cache", "cache", true},
		{"config", "config", true},
		{"migrate", "migrate", true},
		{"init", "init", true},
		{"build is not structured", "build", false},
		{"test is not structured", "test", false},
		{"lint is not structured", "lint", false},
		{"serve is not structured", "serve", false},
		{"publish is not structured", "publish", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isStructuredCommand(tt.cmd)
			if got != tt.want {
				t.Errorf("isStructuredCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}
