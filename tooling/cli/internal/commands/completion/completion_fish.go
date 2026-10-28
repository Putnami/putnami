package completion

import (
	"io"
	"slices"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// CompletionFish generates a fish completion script.
func CompletionFish(w io.Writer, wsRoot string, cfg *wsproto.Config, warnings *workspace.WarningReport) {
	ctx := gatherCompletionContext(wsRoot, cfg, warnings)

	iox.Fprintf(w, `# fish completion for putnami
# Install: putnami completion fish > ~/.config/fish/completions/putnami.fish

# Disable file completions by default
complete -c putnami -f

# commandline -opc lists the words before the one under the cursor.
function __putnami_complete_children
    set -l path $argv
    set -l tokens (commandline -opc)
    set -l expected (math (count $path) + 1)
    if test (count $tokens) -ne $expected
        return 1
    end
    for i in (seq (count $path))
        set -l token_index (math $i + 1)
        if test "$tokens[$token_index]" != "$path[$i]"
            return 1
        end
    end
    return 0
end

function __putnami_seen_path
    set -l path $argv
    set -l tokens (commandline -opc)
    if test (count $tokens) -lt (math (count $path) + 1)
        return 1
    end
    for i in (seq (count $path))
        set -l token_index (math $i + 1)
        if test "$tokens[$token_index]" != "$path[$i]"
            return 1
        end
    end
    return 0
end

# A flag goes after the command: the CLI refuses a command line whose first
# word is a flag, so no completion below offers anything after one. The path
# checks above fail there already, since no path starts with a flag.
function __putnami_leading_flag
    set -l tokens (commandline -opc)
    test (count $tokens) -ge 2; and string match -q -- '-*' $tokens[2]
end

function __putnami_use_command
    not __putnami_leading_flag; and __fish_use_subcommand
end

function __putnami_no_leading_flag
    not __putnami_leading_flag
end

function __putnami_seen_from
    not __putnami_leading_flag; and __fish_seen_subcommand_from $argv
end

# A core command takes a job-only flag only as one of its own flags.
function __putnami_core_root
    set -l tokens (commandline -opc)
    if test (count $tokens) -lt 2
        return 1
    end
    contains -- $tokens[2] %s
end

function __putnami_job_flag
    not __putnami_leading_flag; and not __putnami_core_root
end

# help and its aliases read the command they describe from their next words,
# and take no project target.
function __putnami_is_help
    set -l tokens (commandline -opc)
    test (count $tokens) -ge 2; and contains -- $tokens[2] %s
end

# The first word after help completes the command names; with a command as
# argument, the second word completes its subcommands.
function __putnami_help_word
    set -l tokens (commandline -opc)
    __putnami_is_help; and test (count $tokens) -eq (math (count $argv) + 2)
    and begin; test (count $argv) -eq 0; or test "$tokens[3]" = "$argv[1]"; end
end

function __putnami_job_target
    not __putnami_is_help; and __putnami_seen_from $argv
end

# --projects takes a project target on every command but help.
function __putnami_target_flag
    not __putnami_leading_flag; and not __putnami_is_help
end

# Job commands
`, strings.Join(fishQuoteAll(ctx.coreRoots()), " "), strings.Join(fishQuoteAll(ctx.helpNames()), " "))

	for _, cmd := range ctx.commands {
		desc := ctx.commandDesc[cmd]
		if desc == "" {
			desc = "Run " + cmd + " job"
		}
		iox.Fprintf(w, "complete -c putnami -n %s -a %s -d %s\n", fishCondition("__putnami_use_command"), fishArguments(cmd), fishQuote(desc))
	}

	iox.Fprintf(w, "\n# Structured commands\n")

	for _, sc := range ctx.structured {
		desc := ctx.structuredDesc[sc]
		if desc == "" {
			desc = sc
		}
		iox.Fprintf(w, "complete -c putnami -n %s -a %s -d %s\n", fishCondition("__putnami_use_command"), fishArguments(sc), fishQuote(desc))
	}

	iox.Fprintf(w, "\n# Subcommands\n")
	subcommandPaths := make([]string, 0, len(ctx.subcommands))
	for path := range ctx.subcommands {
		subcommandPaths = append(subcommandPaths, path)
	}
	sort.Strings(subcommandPaths)
	for _, path := range subcommandPaths {
		for _, sub := range ctx.subcommands[path] {
			iox.Fprintf(w, "complete -c putnami -n %s -a %s\n",
				fishCondition("__putnami_complete_children", strings.Fields(path)...), fishArguments(sub))
		}
	}

	writeFishHelpCompletions(w, ctx, subcommandPaths)

	// The word after a group whose default takes a project positional.
	for _, group := range ctx.projectPositionalGroups() {
		for _, id := range ctx.projectIDs {
			iox.Fprintf(w, "complete -c putnami -n %s -a %s -d 'project'\n",
				fishCondition("__putnami_complete_children", strings.Fields(group)...), fishArguments(id))
		}
	}

	iox.Fprintf(w, "\n# Global flags\n")

	writeFishGlobalFlags(w, ctx)

	iox.Fprintf(w, "\n# Structured command flags\n")
	keys := make([]string, 0, len(ctx.structuredFlags))
	for key := range ctx.structuredFlags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		flags := ctx.structuredFlags[key]
		// A key is a command path, its words joined by spaces, or a top-level
		// command, an alias included, which is one word even when it holds a
		// space.
		path := strings.Fields(key)
		if slices.Contains(ctx.commands, key) {
			path = []string{key}
		}
		condition := fishCondition("__putnami_seen_path", path...)
		valueKeyPrefix := key + " "
		for _, flag := range flags {
			shortFlag := strings.TrimPrefix(flag.Long, "--")
			option := "complete -c putnami -n " + condition + " -l " + fishQuote(shortFlag) + " -d " + fishQuote(flag.Description)
			if values, ok := ctx.structuredFlagValues[valueKeyPrefix+flag.Long]; ok && len(values) > 0 {
				var names []string
				for _, value := range values {
					names = append(names, value.Value)
				}
				option += " -a " + fishArguments(names...)
			}
			if flag.ValueName != "" {
				option += " -x"
			}
			iox.Fprintln(w, option)
		}
	}

	// Positional project targets after job commands: IDs, names, basenames, aliases.
	if len(ctx.projectIDs) > 0 || len(ctx.projects) > 0 || len(ctx.projectBasenames) > 0 || len(ctx.aliases) > 0 {
		afterJob := fishCondition("__putnami_job_target", ctx.commands...)
		iox.Fprintf(w, "\n# Project targets (positional argument after job commands)\n")
		for _, id := range ctx.projectIDs {
			iox.Fprintf(w, "complete -c putnami -n %s -a %s -d 'project'\n", afterJob, fishArguments(id))
		}
		for _, name := range ctx.projects {
			iox.Fprintf(w, "complete -c putnami -n %s -a %s -d 'project'\n", afterJob, fishArguments(name))
		}
		for _, base := range ctx.projectBasenames {
			iox.Fprintf(w, "complete -c putnami -n %s -a %s -d 'project'\n", afterJob, fishArguments(base))
		}
		for _, a := range ctx.aliases {
			desc := a.Description
			if desc == "" {
				desc = "alias"
			}
			iox.Fprintf(w, "complete -c putnami -n %s -a %s -d %s\n", afterJob, fishArguments(a.Value), fishQuote(desc))
		}
	}
}

// writeFishGlobalFlags declares the global flags.
func writeFishGlobalFlags(w io.Writer, ctx completionContext) {
	// A flag that takes a value is declared exclusive (-x: it requires a value
	// and offers no file), so the word after it offers that flag's arguments
	// and nothing else: the values the catalog lists, the project targets for
	// --projects, the project names for --exclude, and none for a flag whose
	// value is free. Several entries for one flag merge their arguments where
	// their conditions hold.
	projectTargets := make([]string, 0, len(ctx.projectIDs)+len(ctx.projects)+len(ctx.projectBasenames)+len(ctx.aliases))
	projectTargets = append(projectTargets, ctx.projectIDs...)
	projectTargets = append(projectTargets, ctx.projects...)
	projectTargets = append(projectTargets, ctx.projectBasenames...)
	projectTargets = append(projectTargets, aliasNames(ctx.aliases)...)
	for _, flag := range ctx.globalFlags {
		condition := fishCondition("__putnami_no_leading_flag")
		if flag.JobOnly {
			condition = fishCondition("__putnami_job_flag")
		}
		name := fishQuote(strings.TrimPrefix(flag.Long, "--"))
		option := "complete -c putnami -n " + condition + " -l " + name + " -d " + fishQuote(flag.Description)
		if flag.Type == commandmeta.FlagValue {
			option += " -x"
		}
		switch {
		case flag.Long == "--exclude" && len(ctx.projects) > 0:
			// --exclude matches project names. It is job-only, which excludes
			// help, a core root.
			option += " -a " + fishArguments(ctx.projects...)
		case flag.Type == commandmeta.FlagValue && flag.Long != "--projects" && len(flag.Values) > 0:
			option += " -a " + fishArguments(flag.Values...)
		}
		iox.Fprintln(w, option)
		if flag.Long == "--projects" && len(projectTargets) > 0 {
			// --projects accepts any target form ResolveTarget understands:
			// IDs, names, basenames, and aliases. Help takes no project target.
			iox.Fprintf(w, "complete -c putnami -n %s -l %s -d %s -x -a %s\n",
				fishCondition("__putnami_target_flag"), name, fishQuote(flag.Description), fishArguments(projectTargets...))
		}
	}
}

// writeFishHelpCompletions completes the command help describes, which help
// reads from its next two words: the command names, then the subcommands of
// the first word. paths are the sorted keys of ctx.subcommands. A flag before
// either word fails the path check, as the CLI refuses it.
func writeFishHelpCompletions(w io.Writer, ctx completionContext, paths []string) {
	iox.Fprintf(w, "\n# The command help describes\n")
	for _, name := range append(slices.Clone(ctx.commands), ctx.structured...) {
		iox.Fprintf(w, "complete -c putnami -n %s -a %s\n", fishCondition("__putnami_help_word"), fishArguments(name))
	}
	for _, root := range paths {
		if !slices.Contains(ctx.commands, root) && !slices.Contains(ctx.structured, root) {
			continue
		}
		for _, sub := range ctx.subcommands[root] {
			iox.Fprintf(w, "complete -c putnami -n %s -a %s\n", fishCondition("__putnami_help_word", root), fishArguments(sub))
		}
	}
}

// fishQuote single-quotes a value as one fish word, read once: a description
// for `complete -d`, a flag name for `complete -l`, or a word of the script
// itself. Inside fish single quotes only \' and \\ are escapes, and an
// unescaped apostrophe does not end the string quietly: it swallows the REST OF
// THE FILE until the next quote, silently dropping every completion after it.
// The descriptions are catalog prose ("Don't abort on first failure"), so this
// is not hypothetical.
func fishQuote(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + replacer.Replace(value) + "'"
}

// fishQuoteAll quotes each value as one fish word (fishQuote).
func fishQuoteAll(values []string) []string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = fishQuote(value)
	}
	return quoted
}

// fishWord quotes a value as one fish word, unless fish reads it as itself.
func fishWord(value string) string {
	if plainWord(value) {
		return value
	}
	return fishQuote(value)
}

// fishWords renders values as fish words separated by spaces (fishWord).
func fishWords(values []string) string {
	words := make([]string, len(values))
	for i, value := range values {
		words[i] = fishWord(value)
	}
	return strings.Join(words, " ")
}

// fishArguments renders values as the argument of `complete -a`. Fish reads
// that string twice: once with the script, and again when it completes, where
// it splits the string into words and expands variables and command
// substitutions. Each value is quoted for the second read and the whole list
// for the first, so a name such as "p $l" or "x(cmd)" is one literal candidate.
func fishArguments(values ...string) string {
	return fishQuote(fishWords(values))
}

// fishCondition renders the argument of `complete -n`: the command function,
// which the generator writes, followed by args. Fish runs that string as a
// command when it completes, so each arg is quoted for that read and the whole
// string for the first: a ";" or an apostrophe in a name stays in the name.
func fishCondition(function string, args ...string) string {
	if len(args) == 0 {
		return fishQuote(function)
	}
	return fishQuote(function + " " + fishWords(args))
}
