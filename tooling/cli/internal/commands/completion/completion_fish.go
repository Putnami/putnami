package completion

import (
	"io"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// CompletionFish generates a fish completion script.
func CompletionFish(w io.Writer, wsRoot string, cfg *wsproto.Config) {
	ctx := gatherCompletionContext(wsRoot, cfg)

	iox.Fprintf(w, `# fish completion for putnami
# Install: putnami completion fish > ~/.config/fish/completions/putnami.fish

# Disable file completions by default
complete -c putnami -f

function __putnami_complete_children
    set -l path $argv
    set -l tokens (commandline -opc)
    set -l current (commandline -ct)
    set -l completed_count (count $tokens)
    if test -n "$current"
        set completed_count (math $completed_count - 1)
    end
    set -l expected (math (count $path) + 1)
    if test $completed_count -ne $expected
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

# Job commands
`)

	for _, cmd := range ctx.commands {
		desc := ctx.commandDesc[cmd]
		if desc == "" {
			desc = "Run " + cmd + " job"
		}
		iox.Fprintf(w, "complete -c putnami -n '__fish_use_subcommand' -a '%s' -d %s\n", cmd, fishQuote(desc))
	}

	iox.Fprintf(w, "\n# Structured commands\n")

	for _, sc := range ctx.structured {
		desc := ctx.structuredDesc[sc]
		if desc == "" {
			desc = sc
		}
		iox.Fprintf(w, "complete -c putnami -n '__fish_use_subcommand' -a '%s' -d %s\n", sc, fishQuote(desc))
	}

	iox.Fprintf(w, "\n# Subcommands\n")
	subcommandPaths := make([]string, 0, len(ctx.subcommands))
	for path := range ctx.subcommands {
		subcommandPaths = append(subcommandPaths, path)
	}
	sort.Strings(subcommandPaths)
	for _, path := range subcommandPaths {
		for _, sub := range ctx.subcommands[path] {
			iox.Fprintf(w, "complete -c putnami -n '__putnami_complete_children %s' -a '%s'\n", fishPathArgs(path), sub)
		}
	}

	iox.Fprintf(w, "\n# Global flags\n")

	for _, flag := range ctx.globalFlags {
		iox.Fprintf(w, "complete -c putnami -l '%s' -d %s\n",
			strings.TrimPrefix(flag.Long, "--"), fishQuote(flag.Description))
	}

	iox.Fprintf(w, "\n# Structured command flags\n")
	keys := make([]string, 0, len(ctx.structuredFlags))
	for key := range ctx.structuredFlags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		flags := ctx.structuredFlags[key]
		condition := "__putnami_seen_path " + fishPathArgs(key)
		valueKeyPrefix := key + " "
		for _, flag := range flags {
			shortFlag := strings.TrimPrefix(flag.Long, "--")
			option := "complete -c putnami -n '" + condition + "' -l '" + shortFlag + "' -d " + fishQuote(flag.Description)
			if values, ok := ctx.structuredFlagValues[valueKeyPrefix+flag.Long]; ok && len(values) > 0 {
				var names []string
				for _, value := range values {
					names = append(names, value.Value)
				}
				option += " -a '" + strings.Join(names, " ") + "'"
			}
			if flag.ValueName != "" {
				option += " -r"
			}
			iox.Fprintln(w, option)
		}
	}

	// Output format completion
	iox.Fprintf(w, "\n# Output format values\n")
	iox.Fprintf(w, "complete -c putnami -n '__fish_seen_subcommand_from --output' -a '%s'\n",
		strings.Join(ctx.globalFlagValues("--output"), " "))

	// --projects accepts any target form ResolveTarget understands:
	// IDs, names, basenames, and aliases.
	projectTargets := make([]string, 0, len(ctx.projectIDs)+len(ctx.projects)+len(ctx.projectBasenames)+len(ctx.aliases))
	projectTargets = append(projectTargets, ctx.projectIDs...)
	projectTargets = append(projectTargets, ctx.projects...)
	projectTargets = append(projectTargets, ctx.projectBasenames...)
	projectTargets = append(projectTargets, aliasNames(ctx.aliases)...)
	if len(projectTargets) > 0 {
		iox.Fprintf(w, "\n# Project targets (for --projects flag value)\n")
		iox.Fprintf(w, "complete -c putnami -n '__fish_seen_subcommand_from --projects' -a '%s'\n",
			strings.Join(projectTargets, " "))
	}

	// --exclude matches project names.
	if len(ctx.projects) > 0 {
		iox.Fprintf(w, "\n# Project names (for --exclude flag value)\n")
		iox.Fprintf(w, "complete -c putnami -n '__fish_seen_subcommand_from --exclude' -a '%s'\n",
			strings.Join(ctx.projects, " "))
	}

	// Positional project targets after job commands: IDs, names, basenames, aliases.
	if len(ctx.projectIDs) > 0 || len(ctx.projects) > 0 || len(ctx.projectBasenames) > 0 || len(ctx.aliases) > 0 {
		jobList := strings.Join(ctx.commands, " ")
		iox.Fprintf(w, "\n# Project targets (positional argument after job commands)\n")
		for _, id := range ctx.projectIDs {
			iox.Fprintf(w, "complete -c putnami -n '__fish_seen_subcommand_from %s' -a '%s' -d 'project'\n", jobList, id)
		}
		for _, name := range ctx.projects {
			iox.Fprintf(w, "complete -c putnami -n '__fish_seen_subcommand_from %s' -a '%s' -d 'project'\n", jobList, name)
		}
		for _, base := range ctx.projectBasenames {
			iox.Fprintf(w, "complete -c putnami -n '__fish_seen_subcommand_from %s' -a '%s' -d 'project'\n", jobList, base)
		}
		for _, a := range ctx.aliases {
			desc := a.Description
			if desc == "" {
				desc = "alias"
			}
			iox.Fprintf(w, "complete -c putnami -n '__fish_seen_subcommand_from %s' -a '%s' -d %s\n", jobList, a.Value, fishQuote(desc))
		}
	}
}

func fishPathArgs(path string) string {
	return strings.Join(strings.Fields(path), " ")
}

// fishQuote single-quotes a description for a fish `complete -d` argument.
// Inside fish single quotes only \' and \\ are escapes, and an unescaped
// apostrophe does not end the string quietly: it swallows the REST OF THE FILE
// until the next quote, silently dropping every completion after it. The
// descriptions are catalog prose ("Don't abort on first failure"), so this is
// not hypothetical.
func fishQuote(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + replacer.Replace(value) + "'"
}
