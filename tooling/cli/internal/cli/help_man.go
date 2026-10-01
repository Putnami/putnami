package cli

import (
	"os"
	"strings"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// PrintHelpMan prints the full help as a man page.
func PrintHelpMan() {
	iox.Fprintf(os.Stdout, `.TH PUTNAMI 1 "" "putnami %s" "User Commands"
.SH NAME
putnami \- polyglot build orchestrator
.SH SYNOPSIS
.B putnami
.RI < command[,command...] >
.RI [ options ]
.br
.B putnami
.RI < structured-command >
.RI < subcommand >
.RI [ options ]
.SH DESCRIPTION
Putnami is a polyglot build orchestrator for monorepo workspaces.
It reads extension manifests, resolves a dependency DAG, and executes
tasks in parallel via subprocesses.
.PP
Multiple commands can be chained with commas:
.B putnami lint,test,build --impacted
.SH JOB COMMANDS
`, Version)

	for _, c := range jobCommands.Commands {
		iox.Fprintf(os.Stdout, ".TP\n.B %s\n%s.\n", c.Name, c.Description)
	}

	for _, cat := range allFlagCategories {
		iox.Fprintf(os.Stdout, ".SH %s OPTIONS\n", strings.ToUpper(cat.Name))
		for _, f := range cat.Flags {
			if f.Short != "" {
				iox.Fprintf(os.Stdout, ".TP\n.BR %s \" , \" %s", f.Short, f.Long)
			} else {
				iox.Fprintf(os.Stdout, ".TP\n.B %s", f.Long)
			}
			if f.ValueName != "" {
				iox.Fprintf(os.Stdout, " \" %s\"", f.ValueName)
			}
			iox.Fprintf(os.Stdout, "\n%s\n", f.Description)
		}
	}

	iox.Fprint(os.Stdout, `.SH DEFAULT PROJECT SELECTION
With no project target, feature branches run impacted projects vs trunk.
On main or master, Putnami checks the local same-command/params marker first,
then an active remote-cache successful-run marker, and falls back to all projects
when no usable marker exists.
.TP
.B .
Current directory scope (project or subtree).
`)

	iox.Fprint(os.Stdout, ".SH STRUCTURED COMMANDS\n")
	for _, cat := range structuredCommandCategories {
		iox.Fprintf(os.Stdout, ".SS %s\n", cat.Name)
		for _, c := range cat.Commands {
			iox.Fprintf(os.Stdout, ".TP\n.B %s\n%s.\n", c.Name, c.Description)
		}
	}

	// The reference section is generated from the command catalog, so every
	// documented command appears here. Alternate spellings (`scopes` for
	// `scopes list`, `init` for `workspace init`) share a section rather than
	// printing the same prose twice.
	iox.Fprint(os.Stdout, ".SH COMMAND REFERENCE\n")
	for _, command := range commandmeta.ReferenceCommands() {
		printCommandReferenceMan(command)
	}

	iox.Fprint(os.Stdout, ".SH ALIASES\n")
	iox.Fprint(os.Stdout, builtinAliasMan())
	iox.Fprint(os.Stdout, `
.SH EXIT CODES
.TP
.B 0
Success.
.TP
.B 1
Failure: a job/build/test failed, or an unexpected internal error.
.TP
.B 2
Usage: bad flags or arguments, an unknown command, no projects/jobs matched, or a contract/config validation error.
.TP
.B 3
Auth: authentication or authorization failed.
.TP
.B 4
API: a remote or upstream API call failed.
.TP
.B 130
Interrupted by signal (Ctrl+C).
.SH ENVIRONMENT
.TP
.B PUTNAMI_OUTPUT
Default output format (text, json, jsonl, cloud-logging).
.TP
.B PUTNAMI_VERBOSE
Enable verbose output.
.TP
.B PUTNAMI_QUIET
Suppress non-error output.
.TP
.B PUTNAMI_CACHE_TRUST
Remote cache trust policy (ci, any, or none). The resolved value is also exported to every job.
.TP
.B PUTNAMI_CACHE_OBJECT_SOCKET
Set BY the CLI for each job: the cache provider's object-cache socket. Absent under --no-cache, under trust none, or without a provider.
.TP
.B PUTNAMI_NO_COLOR
Disable color output.
.TP
.B NO_COLOR
Standard no-color convention.
`)
}

// printCommandReferenceMan renders one catalog command as a man subsection:
// description, usage line, and command flags, matching the layout the two
// hand-inlined sections used before the catalog generated all of them.
func printCommandReferenceMan(command commandmeta.Command) {
	iox.Fprintf(os.Stdout, ".SS %s\n%s\n", command.Path, manSentence(command.Description))
	if command.Usage != "" {
		iox.Fprintf(os.Stdout, ".PP\n.B Usage:\n.br\n%s\n", command.Usage)
	}
	for _, flag := range command.Flags {
		iox.Fprintf(os.Stdout, ".TP\n.B %s\n%s\n", flag.Long, manSentence(flag.Description))
	}
}

// manSentence terminates a catalog description with a period, the man page's
// convention, without doubling one that is already there.
func manSentence(text string) string {
	if text == "" {
		return text
	}
	switch text[len(text)-1] {
	case '.', '!', '?', ':':
		return text
	}
	return text + "."
}

func builtinAliasMan() string {
	aliases := commandmeta.BuiltinAliasList()
	parts := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		parts = append(parts, alias.Name+"="+alias.Command)
	}
	return strings.Join(parts, ", ")
}
