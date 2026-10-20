package cli

import (
	"fmt"
	"io"
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
	// Providers are the reserved commands that give the flag meaning
	// (commandmeta.GlobalFlag.Providers); helpScope.listsFlag reads them.
	Providers []string
	// JobOnly marks a flag that only extension commands read
	// (commandmeta.GlobalFlag.JobOnly); helpScope.listsFlag reads it.
	JobOnly bool
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
				Providers:   flag.Providers,
				JobOnly:     flag.JobOnly,
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

// The help of a scope that lists no job command says why and how to add an
// extension that declares one, wrapped for the text help.
const (
	noExtensionLoaded = `No extension is loaded. Name a language extension in
  putnami.workspace.json, then run: putnami extensions install`
	noJobCommandDeclared = `No extension of this workspace declares a job command. Name a language
  extension in putnami.workspace.json, then run: putnami extensions install`
)

// noJobCommandHelp returns what help says when scope lists no job command:
// outside a workspace no extension is loaded, and inside one none declares a
// job command.
func noJobCommandHelp(scope *helpScope) string {
	if scope == nil {
		return noExtensionLoaded
	}
	return noJobCommandDeclared
}

// oneLine joins wrapped help text into one line, for the man page and the
// Markdown reference, which wrap on their own.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// commandGroupsTitle heads the help section that lists the command groups of
// the loaded extensions.
const commandGroupsTitle = "Commands (from extensions):"

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

// jobCommandNames returns the job command names scope lists, joined for
// prose listings.
func jobCommandNames(scope *helpScope) string {
	commands := scope.jobCommands()
	names := make([]string, 0, len(commands))
	for _, c := range commands {
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

// PrintHelp prints the top-level help with no extension loaded: the core's
// commands and flags, and how to add an extension.
func PrintHelp() {
	printHelp(nil)
}

// printHelp prints the top-level help of scope to the process's standard
// output.
func printHelp(scope *helpScope) {
	printHelpTo(os.Stdout, scope)
}

// printHelpTo writes the top-level help of scope to w: the job commands, the
// command groups, the aliases and the global flags the loaded extensions give
// meaning. A nil scope loads no extension, so it lists none of them.
func printHelpTo(w io.Writer, scope *helpScope) {
	iox.Fprintf(w, `putnami %s — polyglot build orchestrator

Usage:
  putnami <command[,command...]> [options]

`, Version)
	if names := jobCommandNames(scope); names != "" {
		iox.Fprintf(w, `Build (from extensions):
  %s
  Multi-command: putnami <command>,<command> --impacted

`, names)
	} else {
		iox.Fprintf(w, `Build (from extensions):
  %s

`, noJobCommandHelp(scope))
	}

	printCommandGroups(w, scope)

	for _, cat := range structuredCommandCategories {
		iox.Fprintf(w, "%s:\n", cat.Name)
		for _, c := range cat.Commands {
			iox.Fprintf(w, "  %-28s %s\n", c.Name, c.Description)
		}
		iox.Fprintln(w)
	}

	for _, cat := range scope.flagCategories() {
		iox.Fprintf(w, "%s:\n", cat.Name)
		for _, f := range cat.Flags {
			printFlagLine(w, f)
		}
		iox.Fprintln(w)
	}

	iox.Fprintf(w, `Default Selection:
  no target            Feature branch: impacted vs trunk; main/master: local marker, then remote cache marker when active, otherwise all

Positional:
  .                    Current directory scope (project or subtree)

`)
	if aliases := aliasSummary(scope); aliases != "" {
		iox.Fprintf(w, `Aliases:
  %s

`, aliases)
	}
	iox.Fprint(w, `Help:
  putnami help <command>   Command help
  putnami help --man       Man page format
  putnami help --markdown  Markdown format

`)
}

// printCommandGroups writes the command groups the loaded extensions declare,
// one row each in the column format of the structured categories. A scope
// without a group prints no section.
func printCommandGroups(w io.Writer, scope *helpScope) {
	groups := scope.commandGroups()
	if len(groups) == 0 {
		return
	}
	iox.Fprintf(w, "%s\n", commandGroupsTitle)
	for _, group := range groups {
		if group.Description == "" {
			iox.Fprintf(w, "  %s\n", group.Name)
			continue
		}
		iox.Fprintf(w, "  %-28s %s\n", group.Name, group.Description)
	}
	iox.Fprintln(w)
}

// extensionFlagHelpLine is one manifest flag's help line. A boolean flag on by
// default also lists the --no- spelling that turns it off: serve's
// `-w, --watch (default: true)` alone offered no way to stop its restarts.
func extensionFlagHelpLine(name string, flag extension.FlagDefinition) string {
	spelling := "--" + name
	if flag.Short != "" {
		spelling = flag.Short + ", " + spelling
	}
	if on, boolean := flag.Default.(bool); on && boolean && (flag.Type == "" || flag.Type == "boolean") {
		spelling += ", --no-" + name
	}
	def := ""
	if flag.Default != nil {
		def = fmt.Sprintf(" (default: %v)", flag.Default)
	}
	desc := flag.Description
	if desc == "" {
		desc = name
	}
	return fmt.Sprintf("  %s  %s%s", spelling, desc, def)
}

// printCommandHelp prints the help of a job command, listing the global flags
// and related commands of scope.
func printCommandHelp(command string, flags map[string]extension.FlagDefinition, scope *helpScope) {
	iox.Fprintf(os.Stdout, "putnami %s\n\n", command)

	if len(flags) > 0 {
		iox.Fprintln(os.Stdout, "Command Flags:")
		names := SortedExtensionFlags(flags)
		for _, name := range names {
			iox.Fprintln(os.Stdout, extensionFlagHelpLine(name, flags[name]))
		}
		iox.Fprintln(os.Stdout)
	}

	printGlobalOptions(scope)
	printRelatedCommands(command, scope)
}

// PrintSubcommandHelp prints help for a structured command subcommand.
func PrintSubcommandHelp(command, subcommand string) {
	printSubcommandHelp(command, subcommand, nil)
}

// printSubcommandHelp prints the help of a built-in command, listing the
// global flags and related commands of scope.
func printSubcommandHelp(command, subcommand string, scope *helpScope) {
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

	printGlobalOptions(scope)
	printRelatedCommands(full, scope)
}

// printGlobalOptions prints the abbreviated global-flag listing of scope.
func printGlobalOptions(scope *helpScope) {
	iox.Fprintln(os.Stdout, "Global Options:")
	for _, cat := range scope.flagCategories() {
		iox.Fprintf(os.Stdout, "  %s: ", cat.Name)
		var names []string
		for _, f := range cat.Flags {
			names = append(names, f.Long)
		}
		iox.Fprintln(os.Stdout, strings.Join(names, ", "))
	}
	iox.Fprintln(os.Stdout)
}

// printRelatedCommands shows the suggested related commands scope lists.
func printRelatedCommands(command string, scope *helpScope) {
	var related []string
	for _, r := range scope.related(command) {
		if scope.listsJobCommand(r) {
			related = append(related, r)
		}
	}
	if len(related) == 0 {
		return
	}
	iox.Fprintln(os.Stdout, "Related Commands:")
	for _, r := range related {
		iox.Fprintf(os.Stdout, "  putnami %s\n", r)
	}
	iox.Fprintln(os.Stdout)
}

// printFlagLine prints a single flag line for the text help output.
func printFlagLine(w io.Writer, f FlagInfo) {
	prefix := "  "
	flag := f.Long
	if f.Short != "" {
		flag = f.Short + ", " + f.Long
	}
	if f.ValueName != "" {
		flag += " " + f.ValueName
	}
	iox.Fprintf(w, "%s%-28s %s\n", prefix, flag, f.Description)
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

// aliasSummary returns the aliases scope lists, as name=command pairs.
func aliasSummary(scope *helpScope) string {
	aliases := scope.aliases()
	parts := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		parts = append(parts, alias.Name+"="+alias.Command)
	}
	return strings.Join(parts, "  ")
}
