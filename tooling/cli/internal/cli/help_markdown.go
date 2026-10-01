package cli

import (
	"os"
	"strings"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// PrintHelpMarkdown prints the full help as markdown documentation.
func PrintHelpMarkdown() {
	iox.Fprintf(os.Stdout, "# putnami\n\n")
	iox.Fprintf(os.Stdout, "> polyglot build orchestrator — v%s\n\n", Version)
	iox.Fprintf(os.Stdout, "## Usage\n\n```\nputnami <command[,command...]> [options]\n```\n\n")

	iox.Fprintf(os.Stdout, "## Build Commands\n\n")
	iox.Fprintf(os.Stdout, "| Command | Description |\n|---------|-------------|\n")
	printMarkdownGroup(jobCommands.Commands)

	iox.Fprintf(os.Stdout, "Multi-command: `putnami lint,test,build --impacted`\n\n")

	for _, cat := range allFlagCategories {
		iox.Fprintf(os.Stdout, "## %s Options\n\n", cat.Name)
		iox.Fprintf(os.Stdout, "| Flag | Description |\n|------|-------------|\n")
		for _, f := range cat.Flags {
			flag := "`" + f.Long + "`"
			if f.Short != "" {
				flag += ", `" + f.Short + "`"
			}
			if f.ValueName != "" {
				flag += " " + f.ValueName
			}
			iox.Fprintf(os.Stdout, "| %s | %s |\n", flag, f.Description)
		}
		iox.Fprintln(os.Stdout)
	}

	iox.Fprintf(os.Stdout, "## Default Project Selection\n\n")
	iox.Fprintf(os.Stdout, "With no project target, feature branches run impacted projects vs trunk. On `main` or `master`, Putnami checks the local same-command/params marker first, then an active remote-cache successful-run marker, and falls back to all projects when no usable marker exists.\n\n")

	iox.Fprintf(os.Stdout, "## Positional Arguments\n\n")
	iox.Fprintf(os.Stdout, "| Argument | Description |\n|----------|-------------|\n")
	iox.Fprintf(os.Stdout, "| `.` | Current directory scope (project or subtree) |\n\n")

	for _, cat := range structuredCommandCategories {
		iox.Fprintf(os.Stdout, "## %s\n\n", cat.Name)
		iox.Fprintf(os.Stdout, "| Command | Description |\n|---------|-------------|\n")
		printMarkdownGroup(cat.Commands)
	}

	// The reference section is generated from the command catalog, so every
	// documented command appears here. Alternate spellings (`scopes` for
	// `scopes list`, `init` for `workspace init`) share a section rather than
	// printing the same prose twice.
	iox.Fprintf(os.Stdout, "## Command Reference\n\n")
	for _, command := range commandmeta.ReferenceCommands() {
		printCommandReferenceMarkdown(command)
	}

	iox.Fprintf(os.Stdout, "## Aliases\n\n")
	iox.Fprintf(os.Stdout, "%s\n\n", builtinAliasMarkdown())

	iox.Fprintf(os.Stdout, "## Exit Codes\n\n")
	iox.Fprintf(os.Stdout, "| Code | Meaning |\n|------|----------|\n")
	iox.Fprintf(os.Stdout, "| 0 | Success |\n")
	iox.Fprintf(os.Stdout, "| 1 | Failure — a job/build/test failed, or an unexpected internal error |\n")
	iox.Fprintf(os.Stdout, "| 2 | Usage — bad flags/args, unknown command, no jobs matched, or validation error |\n")
	iox.Fprintf(os.Stdout, "| 3 | Auth — authentication or authorization failed |\n")
	iox.Fprintf(os.Stdout, "| 4 | API — a remote or upstream API call failed |\n")
	iox.Fprintf(os.Stdout, "| 130 | Interrupted by signal |\n\n")

	iox.Fprintf(os.Stdout, "## Environment Variables\n\n")
	iox.Fprintf(os.Stdout, "| Variable | Description |\n|----------|-------------|\n")
	iox.Fprintf(os.Stdout, "| `PUTNAMI_OUTPUT` | Default output format |\n")
	iox.Fprintf(os.Stdout, "| `PUTNAMI_VERBOSE` | Enable verbose output |\n")
	iox.Fprintf(os.Stdout, "| `PUTNAMI_QUIET` | Suppress non-error output |\n")
	iox.Fprintf(os.Stdout, "| `PUTNAMI_CACHE_TRUST` | Remote cache trust policy (`ci`, `any`, or `none`); also exported to every job |\n")
	iox.Fprintf(os.Stdout, "| `PUTNAMI_CACHE_OBJECT_SOCKET` | Set BY the CLI for each job: the cache provider's object-cache socket |\n")
	iox.Fprintf(os.Stdout, "| `PUTNAMI_NO_COLOR` | Disable color output |\n")
	iox.Fprintf(os.Stdout, "| `NO_COLOR` | Standard no-color convention |\n")
}

func builtinAliasMarkdown() string {
	aliases := commandmeta.BuiltinAliasList()
	parts := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		parts = append(parts, "`"+alias.Name+"`="+alias.Command)
	}
	return strings.Join(parts, ", ")
}

func printMarkdownGroup(entries []CommandInfo) {
	for _, c := range entries {
		iox.Fprintf(os.Stdout, "| `putnami %s` | %s |\n", c.Name, c.Description)
	}
	iox.Fprintln(os.Stdout)
}

// printCommandReferenceMarkdown renders one catalog command: description, usage
// block, flag table, and examples.
func printCommandReferenceMarkdown(command commandmeta.Command) {
	iox.Fprintf(os.Stdout, "### `putnami %s`\n\n", command.Path)
	if command.Description != "" {
		iox.Fprintf(os.Stdout, "%s\n\n", command.Description)
	}
	if command.Usage != "" {
		iox.Fprintf(os.Stdout, "**Usage**\n\n```\n%s\n```\n\n", command.Usage)
	}
	if len(command.Flags) > 0 {
		iox.Fprintf(os.Stdout, "**Flags**\n\n| Flag | Description |\n|------|-------------|\n")
		for _, flag := range command.Flags {
			label := "`" + flag.Long + "`"
			if flag.ValueName != "" {
				label += " " + flag.ValueName
			}
			iox.Fprintf(os.Stdout, "| %s | %s |\n", label, flag.Description)
		}
		iox.Fprintln(os.Stdout)
	}
	if len(command.Examples) > 0 {
		iox.Fprintf(os.Stdout, "**Examples**\n\n")
		for _, example := range command.Examples {
			iox.Fprintf(os.Stdout, "- `%s`\n", example)
		}
		iox.Fprintln(os.Stdout)
	}
}
