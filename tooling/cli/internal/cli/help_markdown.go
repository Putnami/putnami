package cli

import (
	"io"
	"os"
	"strings"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// PrintHelpMarkdown prints the full help as Markdown, with no extension
// loaded.
func PrintHelpMarkdown() {
	printHelpMarkdown(os.Stdout, nil)
}

// printHelpMarkdown writes the full help of scope to w as Markdown: the job
// commands and aliases the loaded extensions declare, every core command, and
// the global flags help lists (helpScope.flagCategories).
func printHelpMarkdown(w io.Writer, scope *helpScope) {
	iox.Fprintf(w, "# putnami\n\n")
	iox.Fprintf(w, "> polyglot build orchestrator — v%s\n\n", Version)
	iox.Fprintf(w, "## Usage\n\n```\nputnami <command[,command...]> [options]\n```\n\n")

	iox.Fprintf(w, "## Build Commands\n\n")
	if commands := scope.jobCommands(); len(commands) > 0 {
		iox.Fprintf(w, "| Command | Description |\n|---------|-------------|\n")
		printMarkdownGroup(w, commands)
	} else {
		iox.Fprintf(w, "%s\n\n", oneLine(noJobCommandHelp(scope)))
	}

	iox.Fprintf(w, "Multi-command: `putnami <command>,<command> --impacted`\n\n")

	for _, cat := range scope.flagCategories() {
		iox.Fprintf(w, "## %s Options\n\n", cat.Name)
		iox.Fprintf(w, "| Flag | Description |\n|------|-------------|\n")
		for _, f := range cat.Flags {
			flag := "`" + f.Long + "`"
			if f.Short != "" {
				flag += ", `" + f.Short + "`"
			}
			if f.ValueName != "" {
				flag += " " + f.ValueName
			}
			iox.Fprintf(w, "| %s | %s |\n", flag, f.Description)
		}
		iox.Fprintln(w)
	}

	iox.Fprintf(w, "## Default Project Selection\n\n")
	iox.Fprintf(w, "With no project target, feature branches run impacted projects vs trunk. On `main` or `master`, Putnami checks the local same-command/params marker first, then an active remote-cache successful-run marker, and falls back to all projects when no usable marker exists.\n\n")

	iox.Fprintf(w, "## Positional Arguments\n\n")
	iox.Fprintf(w, "| Argument | Description |\n|----------|-------------|\n")
	iox.Fprintf(w, "| `.` | Current directory scope (project or subtree) |\n\n")

	for _, cat := range structuredCommandCategories {
		iox.Fprintf(w, "## %s\n\n", cat.Name)
		iox.Fprintf(w, "| Command | Description |\n|---------|-------------|\n")
		printMarkdownGroup(w, cat.Commands)
	}

	// The reference section is generated from the command catalog, so every
	// documented command appears here. Alternate spellings (`scopes` for
	// `scopes list`, `init` for `workspace init`) share a section rather than
	// printing the same prose twice.
	iox.Fprintf(w, "## Command Reference\n\n")
	for _, command := range commandmeta.ReferenceCommands() {
		printCommandReferenceMarkdown(w, command)
	}

	if aliases := scope.aliases(); len(aliases) > 0 {
		parts := make([]string, 0, len(aliases))
		for _, alias := range aliases {
			parts = append(parts, "`"+alias.Name+"`="+alias.Command)
		}
		iox.Fprintf(w, "## Aliases\n\n%s\n\n", strings.Join(parts, ", "))
	}

	iox.Fprintf(w, "## Exit Codes\n\n")
	iox.Fprintf(w, "| Code | Meaning |\n|------|----------|\n")
	iox.Fprintf(w, "| 0 | Success |\n")
	iox.Fprintf(w, "| 1 | Failure — a job/build/test failed, or an unexpected internal error |\n")
	iox.Fprintf(w, "| 2 | Usage — bad flags/args, unknown command, no jobs matched, or validation error |\n")
	iox.Fprintf(w, "| 3 | Auth — authentication or authorization failed |\n")
	iox.Fprintf(w, "| 4 | API — a remote or upstream API call failed |\n")
	iox.Fprintf(w, "| 130 | Interrupted by signal |\n\n")

	iox.Fprintf(w, "## Environment Variables\n\n")
	iox.Fprintf(w, "| Variable | Description |\n|----------|-------------|\n")
	iox.Fprintf(w, "| `PUTNAMI_OUTPUT` | Default output format |\n")
	iox.Fprintf(w, "| `PUTNAMI_VERBOSE` | Enable verbose output |\n")
	iox.Fprintf(w, "| `PUTNAMI_QUIET` | Suppress non-error output |\n")
	iox.Fprintf(w, "| `PUTNAMI_CACHE_TRUST` | Remote cache trust policy (`ci`, `any`, or `none`); also exported to every job |\n")
	iox.Fprintf(w, "| `PUTNAMI_CACHE_OBJECT_SOCKET` | Set BY the CLI for each job: the cache provider's object-cache socket |\n")
	iox.Fprintf(w, "| `PUTNAMI_NO_COLOR` | Disable color output |\n")
	iox.Fprintf(w, "| `NO_COLOR` | Standard no-color convention |\n")
}

func printMarkdownGroup(w io.Writer, entries []CommandInfo) {
	for _, c := range entries {
		iox.Fprintf(w, "| `putnami %s` | %s |\n", c.Name, c.Description)
	}
	iox.Fprintln(w)
}

// printCommandReferenceMarkdown renders one catalog command: description, usage
// block, flag table, and examples.
func printCommandReferenceMarkdown(w io.Writer, command commandmeta.Command) {
	iox.Fprintf(w, "### `putnami %s`\n\n", command.Path)
	if command.Description != "" {
		iox.Fprintf(w, "%s\n\n", command.Description)
	}
	if command.Usage != "" {
		iox.Fprintf(w, "**Usage**\n\n```\n%s\n```\n\n", command.Usage)
	}
	if len(command.Flags) > 0 {
		iox.Fprintf(w, "**Flags**\n\n| Flag | Description |\n|------|-------------|\n")
		for _, flag := range command.Flags {
			label := "`" + flag.Long + "`"
			if flag.ValueName != "" {
				label += " " + flag.ValueName
			}
			iox.Fprintf(w, "| %s | %s |\n", label, flag.Description)
		}
		iox.Fprintln(w)
	}
	if len(command.Examples) > 0 {
		iox.Fprintf(w, "**Examples**\n\n")
		for _, example := range command.Examples {
			iox.Fprintf(w, "- `%s`\n", example)
		}
		iox.Fprintln(w)
	}
}
