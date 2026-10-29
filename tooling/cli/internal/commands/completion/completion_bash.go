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

# _putnami_subcommands_for and _putnami_flags_for set the array _putnami_words,
# which _putnami_completions declares local.
_putnami_subcommands_for() {
    _putnami_words=()
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

# help and its aliases read the command they describe from their next words.
_putnami_is_help() {
    case "$1" in
`)
	for _, name := range ctx.helpNames() {
		iox.Fprintf(w, "        %s) return 0 ;;\n", bashQuote(name))
	}
	iox.Fprintf(w, `        *) return 1 ;;
    esac
}

_putnami_flags_for() {
    _putnami_words=()
    case "$1" in
`)
	writeBashFlagCases(w, ctx)
	iox.Fprintf(w, `    esac
}

# _putnami_global_flag_values sets _putnami_words to the values of the global
# flag $1 and succeeds when $1 takes a value that is neither a project target
# nor a project name. A flag whose value is free sets no word, and neither does
# a job-only flag when $2 is 1, on a core command, which refuses it.
_putnami_global_flag_values() {
    _putnami_words=()
    case "$1" in
`)
	writeBashGlobalFlagValueCases(w, ctx)
	iox.Fprintf(w, `        *) return 1 ;;
    esac
}

# _putnami_reply offers each argument that compgen matches with the current
# word, which compgen reads as the shell does, quotes and backslashes removed.
# compgen reads one argument at a time, single-quoted, and only its exit status
# counts, so each argument stays one word, a newline included. Each reply is
# quoted, so the command line reads it back as the same word. Bash 3.2 leaves a
# leading "~" bare, which the command line would expand to a home directory.
# When COMP_WORDBREAKS holds a colon, the command line replaces only the text
# after the last colon of the current word that no quote or backslash
# protects, so each reply leaves out the name up to that colon.
_putnami_reply() {
    COMPREPLY=()
    local word quoted prefix="" q="'\\''"
    if [[ "${COMP_WORDBREAKS-}" == *:* ]]; then
        _putnami_dequote "${cur}"
        prefix="${_putnami_word:0:${_putnami_colon}}"
    fi
    for word in "$@"; do
        compgen -W "'${word//\'/${q}}'" -- "${cur}" >/dev/null || continue
        printf -v quoted '%%q' "${word#"${prefix}"}"
        if [[ "${quoted}" == \~* ]]; then
            quoted="\\${quoted}"
        fi
        COMPREPLY+=("${quoted}")
    done
}

# _putnami_dequote sets _putnami_word to the word $1 as the shell reads it,
# without its single quotes, double quotes, backslashes and $'...' quoting:
# completion receives each word as the user typed it. It sets _putnami_colon to
# the length of that word up to its last colon that no quote or backslash
# protects, or 0. It expands nothing and uses no eval, because a typed word
# can hold a command substitution. Inside
# $'...', it decodes the escapes that printf %%q writes: a letter such as \n or
# \E, an octal \NNN and a hex \xHH. It keeps the backslash of an escape it
# does not decode, such as \cX.
_putnami_dequote() {
    _putnami_word=""
    _putnami_colon=0
    local rest="$1" c quote=""
    while [[ -n "${rest}" ]]; do
        c="${rest:0:1}"
        rest="${rest:1}"
        case "${quote}" in
        single)
            if [[ "${c}" == "'" ]]; then quote=""; else _putnami_word+="${c}"; fi
            ;;
        ansi)
            if [[ "${c}" == "'" ]]; then
                quote=""
            elif [[ "${c}" == "\\" && -n "${rest}" ]]; then
                c="${rest:0:1}"
                rest="${rest:1}"
                # printf reads a format of a backslash and checked escape
                # characters only, so it decodes and runs nothing else.
                case "${c}" in
                    [abfnrtv]) printf -v c "\\${c}" ;;
                    [eE]) c=$'\E' ;;
                    [0-7])
                        while [[ ${#c} -lt 3 && "${rest:0:1}" == [0-7] ]]; do
                            c+="${rest:0:1}"
                            rest="${rest:1}"
                        done
                        printf -v c "\\${c}"
                        ;;
                    x)
                        if [[ "${rest:0:1}" == [0-9A-Fa-f] ]]; then
                            c="${rest:0:1}"
                            rest="${rest:1}"
                            if [[ "${rest:0:1}" == [0-9A-Fa-f] ]]; then
                                c+="${rest:0:1}"
                                rest="${rest:1}"
                            fi
                            printf -v c "\\x${c}"
                        else
                            c="\\x"
                        fi
                        ;;
                    "\\"|"'"|'"'|'?') ;;
                    *) c="\\${c}" ;;
                esac
                _putnami_word+="${c}"
            else
                _putnami_word+="${c}"
            fi
            ;;
        double)
            if [[ "${c}" == '"' ]]; then
                quote=""
            elif [[ "${c}" == "\\" ]]; then
                case "${rest:0:1}" in
                    '$'|'"'|"\\"|'`+"`"+`') _putnami_word+="${rest:0:1}"; rest="${rest:1}" ;;
                    $'\n') rest="${rest:1}" ;;
                    *) _putnami_word+="${c}" ;;
                esac
            else
                _putnami_word+="${c}"
            fi
            ;;
        *)
            case "${c}" in
                "'") quote=single ;;
                '"') quote=double ;;
                "\\")
                    if [[ "${rest:0:1}" != $'\n' ]]; then _putnami_word+="${rest:0:1}"; fi
                    rest="${rest:1}"
                    ;;
                '$')
                    case "${rest:0:1}" in
                        "'") quote=ansi; rest="${rest:1}" ;;
                        '"') quote=double; rest="${rest:1}" ;;
                        *) _putnami_word+="${c}" ;;
                    esac
                    ;;
                ':')
                    _putnami_word+="${c}"
                    _putnami_colon=${#_putnami_word}
                    ;;
                *) _putnami_word+="${c}" ;;
            esac
            ;;
        esac
    done
}

_putnami_completions() {
    local cur prev words cword
    # Bash 4 and later split a word at each character of COMP_WORDBREAKS, a
    # colon included; -n : joins the pieces of a word back.
    _init_completion -n : || return

    # An array expands as ${name[@]+"${name[@]}"}: bash before 4.4 refuses
    # "${name[@]}" for an empty array under set -u.
    local -a _putnami_words
    local _putnami_word _putnami_colon
    local -a commands=(%s)
    local -a structured=(%s)
    local -a global_flags=(%s)
    local -a job_global_flags=(%s)
    local -a projects=(%s)
    local -a project_ids=(%s)
    local -a project_basenames=(%s)
    local -a project_aliases=(%s)
    # Every form ResolveTarget accepts for a positional/--projects target.
    local -a project_targets=(${project_ids[@]+"${project_ids[@]}"} ${projects[@]+"${projects[@]}"} ${project_basenames[@]+"${project_basenames[@]}"} ${project_aliases[@]+"${project_aliases[@]}"})

    # First argument: command name
    if [[ ${cword} -eq 1 ]]; then
        _putnami_reply ${commands[@]+"${commands[@]}"}
        return
    fi

    _putnami_dequote "${words[1]}"
    local cmd="${_putnami_word}"
    _putnami_dequote "${prev}"
    prev="${_putnami_word}"
    # bash splits a word at "=", so the value of --flag=value follows a word
    # "=" and the flag is the word before it: the slot is that flag's.
    if [[ "${prev}" == "=" && ${cword} -ge 3 ]]; then
        _putnami_dequote "${words[cword-2]}"
        prev="${_putnami_word}"
    fi
    local on_core_root=0
    if _putnami_core_root "${cmd}"; then
        on_core_root=1
    fi
    # A flag goes after the command: the CLI refuses a command line whose
    # first word is a flag, so nothing completes after it.
    if [[ "${cmd}" == -* ]]; then
        COMPREPLY=()
        return
    fi
    # help, or an alias of it, reads the command it describes from its next two
    # words, and the CLI refuses a flag before either of them: the first word
    # completes the command names, the second the subcommands of the first, and
    # nothing else, since help takes no project target. Its flags and the value
    # of a global flag other than --projects and --exclude, such as --output,
    # complete below.
    if _putnami_is_help "${cmd}" && [[ "${cur}" != -* ]] && ! _putnami_global_flag_values "${prev}" "${on_core_root}"; then
        if [[ ${cword} -eq 2 ]]; then
            _putnami_reply ${commands[@]+"${commands[@]}"}
        elif [[ ${cword} -eq 3 && "${words[2]}" != -* ]]; then
            _putnami_subcommands_for "${words[2]}"
            _putnami_reply ${_putnami_words[@]+"${_putnami_words[@]}"}
        else
            COMPREPLY=()
        fi
        return
    fi
    # A core command takes a job-only flag only as one of its own flags.
    if [[ ${on_core_root} -eq 0 ]]; then
        global_flags+=(${job_global_flags[@]+"${job_global_flags[@]}"})
    fi

    local path="${cmd}"
    local i=2
    while [[ ${i} -lt ${cword} ]]; do
        _putnami_dequote "${words[${i}]}"
        local word="${_putnami_word}"
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

    _putnami_subcommands_for "${path}"
    local -a nested_subcommands=(${_putnami_words[@]+"${_putnami_words[@]}"})
    if [[ ${#nested_subcommands[@]} -gt 0 && "${cur}" != -* && ${i} -eq ${cword} ]]; then
        if _putnami_takes_project "${path}"; then
            nested_subcommands+=(${project_ids[@]+"${project_ids[@]}"})
        fi
        _putnami_reply ${nested_subcommands[@]+"${nested_subcommands[@]}"}
        return
    fi
`, bashWords(allCommands),
		bashWords(ctx.structured),
		bashWords(ctx.sharedGlobalFlagNames()),
		bashWords(ctx.jobGlobalFlagNames()),
		bashWords(ctx.projects),
		bashWords(ctx.projectIDs),
		bashWords(ctx.projectBasenames),
		bashWords(aliasNames(ctx.aliases)))

	iox.Fprintf(w, `
    # Flags and project names. The word after a flag of the command that takes
    # a value is a value slot: it offers the values of that flag, which may be none.
    local -a structured_values=()
    local value_slot=0
    case "${path} ${prev}" in
`)

	for _, slot := range ctx.commandValueSlots() {
		var names []string
		for _, value := range slot.Values {
			names = append(names, value.Value)
		}
		iox.Fprintf(w, "        %s)\n", bashQuote(slot.Key))
		iox.Fprintf(w, "            structured_values=(%s)\n", bashWords(names))
		iox.Fprintf(w, "            value_slot=1\n")
		iox.Fprintf(w, "            ;;\n")
	}

	// A value slot comes before a flag: the word after a flag that takes a
	// value is its value even when it starts with a dash.
	iox.Fprintf(w, `    esac

    if [[ ${value_slot} -eq 1 ]]; then
        _putnami_reply ${structured_values[@]+"${structured_values[@]}"}
    elif [[ "${prev}" == "--projects" ]]; then
        _putnami_reply ${project_targets[@]+"${project_targets[@]}"}
`)
	if ctx.offersGlobalFlag("--exclude") {
		iox.Fprintf(w, `    elif [[ "${prev}" == "--exclude" ]]; then
        # --exclude matches project names, and a core command refuses it.
        if [[ ${on_core_root} -eq 1 ]]; then
            COMPREPLY=()
        else
            _putnami_reply ${projects[@]+"${projects[@]}"}
        fi
`)
	}
	iox.Fprintf(w, `    elif _putnami_global_flag_values "${prev}" "${on_core_root}"; then
        # The values the catalog lists for the flag, or none when they are free.
        _putnami_reply ${_putnami_words[@]+"${_putnami_words[@]}"}
    elif [[ "${cur}" == -* ]]; then
        _putnami_flags_for "${path}"
        _putnami_reply ${_putnami_words[@]+"${_putnami_words[@]}"} ${global_flags[@]+"${global_flags[@]}"}
    else
        # Positional project target: IDs, names, basenames, and aliases
        _putnami_reply ${project_targets[@]+"${project_targets[@]}"} ${global_flags[@]+"${global_flags[@]}"}
    fi
}

complete -F _putnami_completions putnami
`)
}

func writeBashGlobalFlagValueCases(w io.Writer, ctx completionContext) {
	for _, flag := range ctx.valueGlobalFlags() {
		if flag.JobOnly {
			iox.Fprintf(w, "        %s) _putnami_words=(%s); if [[ \"$2\" == 1 ]]; then _putnami_words=(); fi; return 0 ;;\n", bashQuote(flag.Long), bashWords(flag.Values))
			continue
		}
		iox.Fprintf(w, "        %s) _putnami_words=(%s); return 0 ;;\n", bashQuote(flag.Long), bashWords(flag.Values))
	}
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
		iox.Fprintf(w, "        %s) _putnami_words=(%s) ;;\n", bashQuote(key), bashWords(subs))
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
		iox.Fprintf(w, "        %s) _putnami_words=(%s) ;;\n", bashQuote(key), bashWords(names))
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

// bashWords renders values as bash or zsh words separated by spaces
// (bashWord).
func bashWords(values []string) string {
	words := make([]string, len(values))
	for i, value := range values {
		words[i] = bashWord(value)
	}
	return strings.Join(words, " ")
}
