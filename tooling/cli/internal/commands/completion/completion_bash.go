package completion

import (
	"io"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// CompletionBash generates a bash completion script.
func CompletionBash(w io.Writer, wsRoot string, cfg *wsproto.Config, warnings *workspace.WarningReport) {
	ctx := gatherCompletionContext(wsRoot, cfg, warnings)

	allCommands := make([]string, 0, len(ctx.commands)+len(ctx.structured))
	allCommands = append(allCommands, ctx.commands...)
	allCommands = append(allCommands, ctx.structured...)
	sort.Strings(allCommands)

	iox.Fprintf(w, `# bash completion for putnami
# Install: putnami completion bash > /etc/bash_completion.d/putnami
#      or: putnami completion bash >> ~/.bashrc

_putnami_subcommands_for() {
    case "$1" in
`)
	writeBashSubcommandCases(w, ctx)
	iox.Fprintf(w, `    esac
}

_putnami_path_known() {
    case "$1" in
`)
	writeBashKnownPathCases(w, ctx)
	iox.Fprintf(w, `        *) return 1 ;;
    esac
}

_putnami_takes_project() {
    case "$1" in
`)
	for _, group := range ctx.projectPositionalGroups() {
		iox.Fprintf(w, "        %s) return 0 ;;\n", bashQuote(group))
	}
	iox.Fprintf(w, `        *) return 1 ;;
    esac
}

_putnami_core_root() {
    case "$1" in
`)
	for _, root := range ctx.coreRoots() {
		iox.Fprintf(w, "        %s) return 0 ;;\n", bashQuote(root))
	}
	iox.Fprintf(w, `        *) return 1 ;;
    esac
}

_putnami_flags_for() {
    case "$1" in
`)
	writeBashFlagCases(w, ctx)
	iox.Fprintf(w, `    esac
}

# _putnami_reply offers the words of the compgen word list $1 that start with
# the current word. compgen expands each word of the list once, so the list
# holds quoted words; each reply is quoted again, so the command line reads it
# back as the same word. Bash 3.2 leaves a leading "~" bare, which the command
# line would expand to a home directory.
_putnami_reply() {
    COMPREPLY=()
    local match quoted
    while IFS= read -r match; do
        printf -v quoted '%%q' "${match}"
        if [[ "${quoted}" == \~* ]]; then
            quoted="\\${quoted}"
        fi
        COMPREPLY+=("${quoted}")
    done < <(compgen -W "$1" -- "${cur}")
}

_putnami_completions() {
    local cur prev words cword
    _init_completion || return

    local commands=%s
    local structured=%s
    local global_flags=%s
    local job_global_flags=%s
    local projects=%s
    local project_ids=%s
    local project_basenames=%s
    local project_aliases=%s
    # Every form ResolveTarget accepts for a positional/--projects target.
    local project_targets="${project_ids} ${projects} ${project_basenames} ${project_aliases}"

    # First argument: command name
    if [[ ${cword} -eq 1 ]]; then
        _putnami_reply "${commands}"
        return
    fi

    local cmd="${words[1]}"
    # A core command takes a job-only flag only as one of its own flags.
    if ! _putnami_core_root "${cmd}"; then
        global_flags="${global_flags} ${job_global_flags}"
    fi

    local path="${cmd}"
    local i=2
    while [[ ${i} -lt ${cword} ]]; do
        local word="${words[${i}]}"
        if [[ "${word}" == -* ]]; then
            break
        fi
        local candidate="${path} ${word}"
        if _putnami_path_known "${candidate}"; then
            path="${candidate}"
            ((i++))
        else
            break
        fi
    done

    local nested_subcommands="$(_putnami_subcommands_for "${path}")"
    if [[ -n "${nested_subcommands}" && "${cur}" != -* && ${i} -eq ${cword} ]]; then
        if _putnami_takes_project "${path}"; then
            nested_subcommands="${nested_subcommands} ${project_ids}"
        fi
        _putnami_reply "${nested_subcommands}"
        return
    fi
`, bashWordList(allCommands),
		bashWordList(ctx.structured),
		bashWordList(ctx.sharedGlobalFlagNames()),
		bashWordList(ctx.jobGlobalFlagNames()),
		bashWordList(ctx.projects),
		bashWordList(ctx.projectIDs),
		bashWordList(ctx.projectBasenames),
		bashWordList(aliasNames(ctx.aliases)))

	iox.Fprintf(w, `
    # Flags and project names
    local structured_values=""
    case "${path} ${prev}" in
`)

	valueKeys := make([]string, 0, len(ctx.structuredFlagValues))
	for key := range ctx.structuredFlagValues {
		valueKeys = append(valueKeys, key)
	}
	sort.Strings(valueKeys)

	for _, key := range valueKeys {
		values := ctx.structuredFlagValues[key]
		var names []string
		for _, value := range values {
			names = append(names, value.Value)
		}
		iox.Fprintf(w, "        %s)\n", bashQuote(key))
		iox.Fprintf(w, "            structured_values=%s\n", bashWordList(names))
		iox.Fprintf(w, "            ;;\n")
	}

	iox.Fprintf(w, `    esac

    if [[ -n "${structured_values}" ]]; then
        _putnami_reply "${structured_values}"
    elif [[ "${cur}" == -* ]]; then
        local structured_flags="$(_putnami_flags_for "${path}")"
        _putnami_reply "${structured_flags} ${global_flags}"
    elif [[ "${prev}" == "--projects" ]]; then
        _putnami_reply "${project_targets}"
`)
	if ctx.offersGlobalFlag("--exclude") {
		iox.Fprintf(w, `    elif [[ "${prev}" == "--exclude" ]]; then
        # --exclude matches project names.
        _putnami_reply "${projects}"
`)
	}
	iox.Fprintf(w, `    elif [[ "${prev}" == "--output" ]]; then
        _putnami_reply %s
    elif [[ "${prev}" == "--tag" || "${prev}" == "--exclude-tag" ]]; then
        # Tags are dynamic — no static completion
        :
    else
        # Positional project target: IDs, names, basenames, and aliases
        _putnami_reply "${project_targets} ${global_flags}"
    fi
}

complete -F _putnami_completions putnami
`, bashWordList(ctx.globalFlagValues("--output")))
}

func writeBashSubcommandCases(w io.Writer, ctx completionContext) {
	keys := make([]string, 0, len(ctx.subcommands))
	for key := range ctx.subcommands {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		subs := ctx.subcommands[key]
		if len(subs) == 0 {
			continue
		}
		iox.Fprintf(w, "        %s) printf '%%s\\n' %s ;;\n", bashQuote(key), bashWordList(subs))
	}
}

func writeBashKnownPathCases(w io.Writer, ctx completionContext) {
	keys := make([]string, 0, len(ctx.knownSubcommandPaths))
	for key := range ctx.knownSubcommandPaths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		iox.Fprintf(w, "        %s) return 0 ;;\n", bashQuote(key))
	}
}

func writeBashFlagCases(w io.Writer, ctx completionContext) {
	keys := make([]string, 0, len(ctx.structuredFlags))
	for key := range ctx.structuredFlags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		flags := ctx.structuredFlags[key]
		var names []string
		for _, flag := range flags {
			names = append(names, flag.Long)
		}
		if len(names) == 0 {
			continue
		}
		iox.Fprintf(w, "        %s) printf '%%s\\n' %s ;;\n", bashQuote(key), bashWordList(names))
	}
}

// bashQuote single-quotes a value as one bash or zsh word, read once.
func bashQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// bashWord quotes a value as one bash or zsh word, unless the shell reads it as
// itself.
func bashWord(value string) string {
	if plainWord(value) {
		return value
	}
	return bashQuote(value)
}

// bashWordList renders values as a compgen word list, quoted as one word of
// the script. Bash reads the list twice: once with the script, and again when
// compgen -W expands each of its words. Each value is quoted for the second
// read and the whole list for the first, so a name such as "p $l" or
// "x$(cmd)" is one literal candidate.
func bashWordList(values []string) string {
	return bashQuote(bashWords(values))
}

// bashWords renders values as bash or zsh words separated by spaces
// (bashWord).
func bashWords(values []string) string {
	words := make([]string, len(values))
	for i, value := range values {
		words[i] = bashWord(value)
	}
	return strings.Join(words, " ")
}
