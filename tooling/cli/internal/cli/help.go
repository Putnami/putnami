package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/commands/completion"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// FlagCategory groups flags by purpose for organized help output.
type FlagCategory struct {
	Name  string
	Flags []FlagInfo
}

// FlagInfo describes a single flag for help rendering.
type FlagInfo struct {
	Long        string
	Short       string
	Description string
	ValueName   string // e.g., "<n>", "<format>", "<path>" — empty for booleans
}

// allFlagCategories is the categorized global-flag listing the text, man, and
// markdown help renderers all draw from, in display order. It is DERIVED from
// the command catalog's global-flag table (internal/commandmeta): a flag appears
// here because it declares a Category there. The same table drives shell
// completion, which before slice A1b carried two more wordings of these same
// descriptions (one in the zsh generator, one in the fish generator).
var allFlagCategories = flagCategoriesFromCatalog()

func flagCategoriesFromCatalog() []FlagCategory {
	categories := commandmeta.GlobalFlagCategories()
	out := make([]FlagCategory, 0, len(categories))
	for _, category := range categories {
		infos := make([]FlagInfo, 0, len(category.Flags))
		for _, flag := range category.Flags {
			infos = append(infos, FlagInfo{
				Long:        flag.Long,
				Short:       flag.Short,
				Description: flag.Description,
				ValueName:   flag.ValueName,
			})
		}
		out = append(out, FlagCategory{Name: category.Name, Flags: infos})
	}
	return out
}

// CommandCategory groups commands by purpose for organized help output.
type CommandCategory struct {
	Name     string
	Commands []CommandInfo
}

// CommandInfo describes a single command for help rendering.
type CommandInfo struct {
	Name        string
	Description string
}

// jobCommands are the extension-provided job commands.
var jobCommands = CommandCategory{
	Name:     "Build",
	Commands: publicJobCommandInfos(),
}

func publicJobCommandInfos() []CommandInfo {
	commands := commandmeta.PublicJobCommands()
	out := make([]CommandInfo, 0, len(commands))
	for _, command := range commands {
		out = append(out, CommandInfo{
			Name:        command.Name,
			Description: strings.TrimSuffix(command.Description, "."),
		})
	}
	return out
}

// structuredCommandCategories is the categorized command listing the text, man,
// and markdown help renderers all draw from. It is DERIVED from the command
// catalog (internal/commandmeta): a command appears here because it declares a
// Category there, under its Label when it collapses several paths into one row.
var structuredCommandCategories = structuredCommandCategoriesFromCatalog()

func structuredCommandCategoriesFromCatalog() []CommandCategory {
	categories := commandmeta.Categories()
	out := make([]CommandCategory, 0, len(categories))
	for _, category := range categories {
		infos := make([]CommandInfo, 0, len(category.Commands))
		for _, command := range category.Commands {
			infos = append(infos, CommandInfo{
				Name:        command.DisplayName(),
				Description: command.Summary,
			})
		}
		out = append(out, CommandCategory{Name: category.Name, Commands: infos})
	}
	return out
}

// jobCommandNames returns the job command names joined for prose listings.
func jobCommandNames() string {
	names := make([]string, 0, len(jobCommands.Commands))
	for _, c := range jobCommands.Commands {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

// relatedCommands maps each command to related commands. It is DERIVED from the
// command catalog: the keys are catalog paths, which is what the help renderers
// look up (PrintSubcommandHelp joins command and subcommand the same way).
var relatedCommands = relatedCommandsFromCatalog()

func relatedCommandsFromCatalog() map[string][]string {
	catalog := commandmeta.Commands()
	out := make(map[string][]string, len(catalog))
	for _, command := range catalog {
		if len(command.Related) == 0 {
			continue
		}
		out[command.Path] = append([]string(nil), command.Related...)
	}
	return out
}

// PrintHelp prints the top-level help message with categorized flags.
func PrintHelp() {
	iox.Fprintf(os.Stdout, `putnami %s — polyglot build orchestrator

Usage:
  putnami <command[,command...]> [options]

Build (from extensions):
  %s
  Multi-command: putnami lint,test,build --impacted

`, Version, jobCommandNames())

	for _, cat := range structuredCommandCategories {
		iox.Fprintf(os.Stdout, "%s:\n", cat.Name)
		for _, c := range cat.Commands {
			iox.Fprintf(os.Stdout, "  %-28s %s\n", c.Name, c.Description)
		}
		iox.Fprintln(os.Stdout)
	}

	for _, cat := range allFlagCategories {
		iox.Fprintf(os.Stdout, "%s:\n", cat.Name)
		for _, f := range cat.Flags {
			printFlagLine(f)
		}
		iox.Fprintln(os.Stdout)
	}

	iox.Fprintf(os.Stdout, `Default Selection:
  no target            Feature branch: impacted vs trunk; main/master: local marker, then remote cache marker when active, otherwise all

Positional:
  .                    Current directory scope (project or subtree)

Aliases:
  %s

Help:
  putnami help <command>   Command help
  putnami help --man       Man page format
  putnami help --markdown  Markdown format

`, builtinAliasSummary())
}

// PrintCommandHelp prints help for a specific command with categorized flags
// and related commands suggestions.
func PrintCommandHelp(command string, flags map[string]extension.FlagDefinition) {
	iox.Fprintf(os.Stdout, "putnami %s\n\n", command)

	if len(flags) > 0 {
		iox.Fprintln(os.Stdout, "Command Flags:")
		names := SortedExtensionFlags(flags)
		for _, name := range names {
			flag := flags[name]
			short := ""
			if flag.Short != "" {
				short = flag.Short + ", "
			}
			def := ""
			if flag.Default != nil {
				def = fmt.Sprintf(" (default: %v)", flag.Default)
			}
			desc := flag.Description
			if desc == "" {
				desc = name
			}
			iox.Fprintf(os.Stdout, "  %s--%s  %s%s\n", short, name, desc, def)
		}
		iox.Fprintln(os.Stdout)
	}

	// Show global flag categories (abbreviated)
	iox.Fprintln(os.Stdout, "Global Options:")
	for _, cat := range allFlagCategories {
		iox.Fprintf(os.Stdout, "  %s: ", cat.Name)
		var names []string
		for _, f := range cat.Flags {
			names = append(names, f.Long)
		}
		iox.Fprintln(os.Stdout, strings.Join(names, ", "))
	}
	iox.Fprintln(os.Stdout)

	// Show related commands
	printRelatedCommands(command)
}

// PrintSubcommandHelp prints help for a structured command subcommand.
func PrintSubcommandHelp(command, subcommand string) {
	full := command
	if subcommand != "" {
		full = command + " " + subcommand
	}
	iox.Fprintf(os.Stdout, "putnami %s\n\n", full)

	if info, ok := completion.StructuredCommandHelp(command, subcommand); ok {
		if info.Description != "" {
			iox.Fprintln(os.Stdout, info.Description)
			iox.Fprintln(os.Stdout)
		}
		if info.Usage != "" {
			iox.Fprintln(os.Stdout, "Usage:")
			iox.Fprintf(os.Stdout, "  %s\n\n", info.Usage)
		}
		if len(info.Flags) > 0 {
			iox.Fprintln(os.Stdout, "Command Flags:")
			for _, flag := range info.Flags {
				printStructuredFlagLine(flag)
			}
			iox.Fprintln(os.Stdout)
		}
		if len(info.Examples) > 0 {
			iox.Fprintln(os.Stdout, "Examples:")
			for _, example := range info.Examples {
				iox.Fprintf(os.Stdout, "  %s\n", example)
			}
			iox.Fprintln(os.Stdout)
		}
	}

	iox.Fprintln(os.Stdout, "Global Options:")
	for _, cat := range allFlagCategories {
		iox.Fprintf(os.Stdout, "  %s: ", cat.Name)
		var names []string
		for _, f := range cat.Flags {
			names = append(names, f.Long)
		}
		iox.Fprintln(os.Stdout, strings.Join(names, ", "))
	}
	iox.Fprintln(os.Stdout)

	printRelatedCommands(full)
}

// printRelatedCommands shows suggested related commands.
func printRelatedCommands(command string) {
	related, ok := relatedCommands[command]
	if !ok || len(related) == 0 {
		return
	}
	iox.Fprintln(os.Stdout, "Related Commands:")
	for _, r := range related {
		iox.Fprintf(os.Stdout, "  putnami %s\n", r)
	}
	iox.Fprintln(os.Stdout)
}

// printFlagLine prints a single flag line for the text help output.
func printFlagLine(f FlagInfo) {
	prefix := "  "
	flag := f.Long
	if f.Short != "" {
		flag = f.Short + ", " + f.Long
	}
	if f.ValueName != "" {
		flag += " " + f.ValueName
	}
	iox.Fprintf(os.Stdout, "%s%-28s %s\n", prefix, flag, f.Description)
}

func printStructuredFlagLine(f completion.StructuredFlagInfo) {
	flag := f.Long
	if f.ValueName != "" {
		flag += " " + f.ValueName
	}
	iox.Fprintf(os.Stdout, "  %-28s %s\n", flag, f.Description)
}

// SortedExtensionFlags returns extension flag names sorted alphabetically.
func SortedExtensionFlags(flags map[string]extension.FlagDefinition) []string {
	names := make([]string, 0, len(flags))
	for name := range flags {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func builtinAliasSummary() string {
	aliases := commandmeta.BuiltinAliasList()
	parts := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		parts = append(parts, alias.Name+"="+alias.Command)
	}
	return strings.Join(parts, "  ")
}
