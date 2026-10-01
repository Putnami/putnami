package completion

import (
	"io"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// CompletionBash generates a bash completion script.
func CompletionBash(w io.Writer, wsRoot string, cfg *wsproto.Config) {
	ctx := gatherCompletionContext(wsRoot, cfg)

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

_putnami_flags_for() {
    case "$1" in
`)
	writeBashFlagCases(w, ctx)
	iox.Fprintf(w, `    esac
}

_putnami_completions() {
    local cur prev words cword
    _init_completion || return

    local commands="%s"
    local structured="%s"
    local global_flags="%s"
    local projects="%s"
    local project_ids="%s"
    local project_basenames="%s"
    local project_aliases="%s"
    # Every form ResolveTarget accepts for a positional/--projects target.
    local project_targets="${project_ids} ${projects} ${project_basenames} ${project_aliases}"

    # First argument: command name
    if [[ ${cword} -eq 1 ]]; then
        COMPREPLY=( $(compgen -W "${commands}" -- "${cur}") )
        return
    fi

    local cmd="${words[1]}"

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
        COMPREPLY=( $(compgen -W "${nested_subcommands}" -- "${cur}") )
        return
    fi
`, strings.Join(allCommands, " "),
		strings.Join(ctx.structured, " "),
		strings.Join(ctx.globalFlagNames(), " "),
		strings.Join(ctx.projects, " "),
		strings.Join(ctx.projectIDs, " "),
		strings.Join(ctx.projectBasenames, " "),
		strings.Join(aliasNames(ctx.aliases), " "))

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
		iox.Fprintf(w, "        '%s')\n", key)
		iox.Fprintf(w, "            structured_values='%s'\n", strings.Join(names, " "))
		iox.Fprintf(w, "            ;;\n")
	}

	iox.Fprintf(w, `    esac

    if [[ -n "${structured_values}" ]]; then
        COMPREPLY=( $(compgen -W "${structured_values}" -- "${cur}") )
    elif [[ "${cur}" == -* ]]; then
        local structured_flags="$(_putnami_flags_for "${path}")"
        COMPREPLY=( $(compgen -W "${structured_flags} ${global_flags}" -- "${cur}") )
    elif [[ "${prev}" == "--projects" ]]; then
        COMPREPLY=( $(compgen -W "${project_targets}" -- "${cur}") )
    elif [[ "${prev}" == "--exclude" ]]; then
        # --exclude matches project names.
        COMPREPLY=( $(compgen -W "${projects}" -- "${cur}") )
    elif [[ "${prev}" == "--output" ]]; then
        COMPREPLY=( $(compgen -W "%s" -- "${cur}") )
    elif [[ "${prev}" == "--tag" || "${prev}" == "--exclude-tag" ]]; then
        # Tags are dynamic — no static completion
        :
    else
        # Positional project target: IDs, names, basenames, and aliases
        COMPREPLY=( $(compgen -W "${project_targets} ${global_flags}" -- "${cur}") )
    fi
}

complete -F _putnami_completions putnami
`, strings.Join(ctx.globalFlagValues("--output"), " "))
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
		iox.Fprintf(w, "        %s) printf '%%s\\n' %s ;;\n", bashQuote(key), bashQuote(strings.Join(subs, " ")))
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
		iox.Fprintf(w, "        %s) printf '%%s\\n' %s ;;\n", bashQuote(key), bashQuote(strings.Join(names, " ")))
	}
}

func bashQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
