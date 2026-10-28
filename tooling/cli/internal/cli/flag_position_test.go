package cli

import (
	"errors"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cmderr"
)

// A flag before the command word used to print the top-level help in place of
// the command, with exit 0. It is a usage error that names the flag. Flags
// alone, each global flag with its value, still print help; the core cannot
// tell the value of a flag it does not know from a command word.
func TestParseArgs_FlagBeforeTheCommandIsRefused(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "flags-follow-the-command", "a-flag-before-the-command-is-refused")

	refused := []struct {
		args []string
		flag string
	}{
		{[]string{"-v", "projects", "list"}, "-v"},
		{[]string{"--verbose", "build"}, "--verbose"},
		{[]string{"--projects", "web", "build"}, "--projects"},
		{[]string{"--projects=web", "build"}, "--projects"},
		{[]string{"--custom", "build"}, "--custom"},
		{[]string{"--port", "3000"}, "--port"},
	}
	for _, tc := range refused {
		parsed := ParseArgs(tc.args, nil, nil)
		if !errors.Is(parsed.Err, cmderr.ErrUsage) {
			t.Errorf("ParseArgs(%v) error = %v, want a usage error", tc.args, parsed.Err)
			continue
		}
		if parsed.Global.Help {
			t.Errorf("ParseArgs(%v) prints help, want the usage error", tc.args)
		}
		for _, want := range []string{tc.flag + " comes before the command", "putnami <command>"} {
			if !strings.Contains(parsed.Err.Error(), want) {
				t.Errorf("ParseArgs(%v) error = %q, want it to contain %q", tc.args, parsed.Err, want)
			}
		}
	}

	helped := [][]string{
		{"-v"},
		{"--projects", "web"},
		{"--output", "json", "-v"},
		{"--foo=bar"},
		{"-v", "--help"},
		{"--help", "projects", "list"},
		{"-v", "projects", "-h"},
		{"--", "build"},
	}
	for _, args := range helped {
		parsed := ParseArgs(args, nil, nil)
		if parsed.Err != nil || !parsed.Global.Help {
			t.Errorf("ParseArgs(%v): err = %v, help = %v, want help with no error", args, parsed.Err, parsed.Global.Help)
		}
	}

	if parsed := ParseArgs([]string{"--version"}, nil, nil); parsed.Err != nil || !parsed.Global.Version {
		t.Errorf("--version: err = %v, version = %v, want the version", parsed.Err, parsed.Global.Version)
	}
}

// A flag between a command root and its subcommand used to leave the
// subcommand word a stray argument, so the root ran its default subcommand:
// `projects -v describe web` listed the projects, and `extensions -v list`
// installed an extension named list.
func TestParseArgs_FlagBeforeASubcommandIsRefused(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "flags-follow-the-command", "a-flag-before-a-subcommand-is-refused")

	groups := map[string]bool{"cloud": true}
	aliases := commandAliases{groupSubcommand: func(group, word string) bool {
		return group == "cloud" && word == "login"
	}}
	refused := []struct {
		args []string
		want string
	}{
		{[]string{"projects", "-v", "describe", "web"}, `-v comes before the subcommand "describe"; a flag goes after it: putnami projects describe [options]`},
		{[]string{"extensions", "-v", "list"}, `-v comes before the subcommand "list"`},
		{[]string{"extensions", "--output", "json", "install"}, `--output comes before the subcommand "install"`},
		{[]string{"projects", "--output=json", "describe"}, `--output comes before the subcommand "describe"`},
		{[]string{"cloud", "--tenant", "acme", "login"}, `--tenant comes before the subcommand "login"`},
	}
	for _, tc := range refused {
		parsed := parseArgs(tc.args, aliases, groups)
		if !errors.Is(parsed.Err, cmderr.ErrUsage) {
			t.Errorf("parseArgs(%v) error = %v, want a usage error", tc.args, parsed.Err)
			continue
		}
		if !strings.Contains(parsed.Err.Error(), tc.want) {
			t.Errorf("parseArgs(%v) error = %q, want it to contain %q", tc.args, parsed.Err, tc.want)
		}
	}

	accepted := [][]string{
		{"projects", "-v"},
		{"projects", "describe", "-v", "web"},
		{"projects", "--output", "json"},
		{"cloud", "login", "--tenant", "acme"},
		{"cloud", "-v", "acme"},
	}
	for _, args := range accepted {
		if parsed := parseArgs(args, aliases, groups); parsed.Err != nil {
			t.Errorf("parseArgs(%v) error = %v, want none", args, parsed.Err)
		}
	}

	helped := parseArgs([]string{"projects", "-v", "describe", "--help"}, aliases, groups)
	if helped.Err != nil || !helped.Global.Help {
		t.Errorf("projects -v describe --help: err = %v, help = %v, want help with no error", helped.Err, helped.Global.Help)
	}
}

// help reads the command it describes from its first positional, so a flag
// before that command used to hide it: `help -v projects` printed the
// top-level help with exit 0.
func TestParseArgs_FlagBeforeTheCommandHelpNamesIsRefused(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "flags-follow-the-command", "a-flag-before-the-command-help-names-is-refused")

	refused := []struct {
		args []string
		want string
	}{
		{[]string{"help", "-v", "projects"}, `-v comes before the command "projects"; a flag goes after it: putnami help projects [options]`},
		{[]string{"help", "--output", "json", "build"}, `--output comes before the command "build"`},
	}
	for _, tc := range refused {
		parsed := ParseArgs(tc.args, nil, nil)
		if !errors.Is(parsed.Err, cmderr.ErrUsage) {
			t.Errorf("ParseArgs(%v) error = %v, want a usage error", tc.args, parsed.Err)
			continue
		}
		if !strings.Contains(parsed.Err.Error(), tc.want) {
			t.Errorf("ParseArgs(%v) error = %q, want it to contain %q", tc.args, parsed.Err, tc.want)
		}
	}

	accepted := [][]string{
		{"help"},
		{"help", "-v"},
		{"help", "--man"},
		{"help", "--man", "projects", "list"},
		{"help", "projects", "-v"},
		{"help", "projects", "list", "-v"},
	}
	for _, args := range accepted {
		if parsed := ParseArgs(args, nil, nil); parsed.Err != nil {
			t.Errorf("ParseArgs(%v) error = %v, want none", args, parsed.Err)
		}
	}

	helped := ParseArgs([]string{"help", "-v", "projects", "--help"}, nil, nil)
	if helped.Err != nil || !helped.Global.Help {
		t.Errorf("help -v projects --help: err = %v, help = %v, want help with no error", helped.Err, helped.Global.Help)
	}
}
