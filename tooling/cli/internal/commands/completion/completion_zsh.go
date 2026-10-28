package completion

import (
	"io"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// CompletionZsh generates a zsh completion script.
func CompletionZsh(w io.Writer, wsRoot string, cfg *wsproto.Config, warnings *workspace.WarningReport) {
	ctx := gatherCompletionContext(wsRoot, cfg, warnings)

	iox.Fprintf(w, `#compdef putnami
# zsh completion for putnami
#
# Install:
#   oh-my-zsh:  putnami completion zsh > ${ZSH_CUSTOM:-~/.oh-my-zsh/custom}/completions/_putnami
#   manual:     putnami completion zsh > ~/.zfunc/_putnami
#               echo 'fpath=(~/.zfunc $fpath)' >> ~/.zshrc
#
# Then restart your shell: exec zsh

_putnami_subcommands_for() {
    reply=()
    case "$1" in
`)
	writeZshSubcommandCases(w, ctx)
	iox.Fprintf(w, `    esac
}

_putnami_path_known() {
    case "$1" in
`)
	writeZshKnownPathCases(w, ctx)
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

_putnami_takes_project() {
    case "$1" in
`)
	for _, group := range ctx.projectPositionalGroups() {
		iox.Fprintf(w, "        %s) return 0 ;;\n", bashQuote(group))
	}
	iox.Fprintf(w, `        *) return 1 ;;
    esac
}

_putnami_flags_for() {
    reply=()
    case "$1" in
`)
	writeZshFlagCases(w, ctx)
	iox.Fprintf(w, `    esac
}

_putnami() {
    local -a commands structured_commands global_flags job_global_flags projects project_basenames

    commands=(
`)

	for _, cmd := range ctx.commands {
		desc := ctx.commandDesc[cmd]
		if desc == "" {
			desc = "Run " + cmd + " job"
		}
		iox.Fprintf(w, "        %s\n", zshDescribeEntry(cmd, desc))
	}

	iox.Fprintf(w, "    )\n\n    structured_commands=(\n")

	for _, sc := range ctx.structured {
		desc := ctx.structuredDesc[sc]
		if desc == "" {
			desc = sc
		}
		iox.Fprintf(w, "        %s\n", zshDescribeEntry(sc, desc))
	}

	iox.Fprintf(w, "    )\n\n    global_flags=(\n")
	for _, flag := range ctx.globalFlags {
		if !flag.JobOnly {
			iox.Fprintf(w, "        %s\n", bashQuote(zshFlagSpec(
				flag.Long, flag.Description, flag.ValueName, flag.Values)))
		}
	}
	iox.Fprintf(w, "    )\n\n    job_global_flags=(\n")
	for _, flag := range ctx.globalFlags {
		if flag.JobOnly {
			iox.Fprintf(w, "        %s\n", bashQuote(zshFlagSpec(
				flag.Long, flag.Description, flag.ValueName, flag.Values)))
		}
	}
	iox.Fprintf(w, "    )\n\n")

	if len(ctx.projects) > 0 {
		iox.Fprintf(w, "    projects=(%s)\n\n", zshDescribeNames(ctx.projects))
	}

	// Project path basenames (short-form targets like "http", "application").
	if len(ctx.projectBasenames) > 0 {
		iox.Fprintf(w, "    project_basenames=(%s)\n\n", zshDescribeNames(ctx.projectBasenames))
	}

	// Project IDs for /path completion
	if len(ctx.projectIDs) > 0 {
		iox.Fprintf(w, "    local -a project_ids=(%s)\n\n", zshDescribeNames(ctx.projectIDs))
	}

	// Project aliases for bare-word completion
	if len(ctx.aliases) > 0 {
		iox.Fprintf(w, "    local -a project_aliases=(")
		for _, a := range ctx.aliases {
			iox.Fprintf(w, "%s ", zshDescribeEntry(a.Value, a.Description))
		}
		iox.Fprintf(w, ")\n\n")
	}

	iox.Fprintf(w, `    _arguments -C \
        '1:command:->command' \
        '*::arg:->args'

    case $state in
        command)
            _describe -t commands 'job commands' commands
            _describe -t structured 'structured commands' structured_commands
            ;;
        args)
            local cur="${words[CURRENT]}"
            local prev="${words[CURRENT-1]}"
            local path="${words[1]}"
            # A flag goes after the command: the CLI refuses a command line
            # whose first word is a flag, so nothing completes after it.
            if [[ "${path}" == -* ]]; then
                return 1
            fi
            # A core command takes a job-only flag only as one of its own flags.
            if ! _putnami_core_root "${path}"; then
                global_flags=("${global_flags[@]}" "${job_global_flags[@]}")
            fi
            local i=2
            while (( i < CURRENT )); do
                local word="${words[i]}"
                if [[ "${word}" == -* ]]; then
                    break
                fi
                local candidate="${path} ${word}"
                if _putnami_path_known "${candidate}"; then
                    path="${candidate}"
                    (( i++ ))
                else
                    break
                fi
            done

            local -a nested_subcmds
            _putnami_subcommands_for "${path}"
            nested_subcmds=("${reply[@]}")
            if (( ${#nested_subcmds} > 0 )) && [[ "${cur}" != -* ]] && (( i == CURRENT )); then
                _describe -t subcmds 'subcommand' nested_subcmds
                if _putnami_takes_project "${path}"; then
                    _describe -t project-ids 'project ID' project_ids
                fi
            else
                local -a cmd_values
                case "${path} ${prev}" in
`)
	writeZshValueCases(w, ctx)
	iox.Fprintf(w, `                esac

                if (( ${#cmd_values} > 0 )); then
                    _describe -t values 'value' cmd_values
                elif [[ "${prev}" == "--projects" ]]; then
                        _describe -t project-ids 'project ID' project_ids
                        _describe -t project-names 'project name' projects
                        _describe -t project-names 'project name' project_basenames
`)
	if len(ctx.aliases) > 0 {
		iox.Fprintf(w, "                        _describe -t aliases 'project alias' project_aliases\n")
	}
	if ctx.offersGlobalFlag("--exclude") {
		iox.Fprintf(w, `                    elif [[ "${prev}" == "--exclude" ]]; then
                        _describe -t project-names 'project name' projects
`)
	}
	iox.Fprintf(w, `                    elif [[ "${prev}" == "--output" ]]; then
                        compadd -- %s
                    elif [[ "${cur}" == -* ]]; then
                        local -a cmd_flags
                        _putnami_flags_for "${path}"
                        cmd_flags=("${reply[@]}")
                        _values 'flags' $cmd_flags $global_flags
                    else
                        _describe -t project-ids 'project ID' project_ids
                        _describe -t project-names 'project name' projects
                        _describe -t project-names 'project name' project_basenames
`, bashWords(ctx.globalFlagValues("--output")))
	if len(ctx.aliases) > 0 {
		iox.Fprintf(w, "                        _describe -t aliases 'project alias' project_aliases\n")
	}
	iox.Fprintf(w, `                        _values 'flags' $global_flags
                    fi
            fi
            ;;
    esac
}

_putnami "$@"
`)
}

func writeZshSubcommandCases(w io.Writer, ctx completionContext) {
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
		iox.Fprintf(w, "        %s)\n", bashQuote(key))
		iox.Fprintf(w, "            reply=(\n")
		for _, sub := range subs {
			iox.Fprintf(w, "                %s\n", bashQuote(zshDescribeName(sub)))
		}
		iox.Fprintf(w, "            )\n")
		iox.Fprintf(w, "            ;;\n")
	}
}

func writeZshKnownPathCases(w io.Writer, ctx completionContext) {
	keys := make([]string, 0, len(ctx.knownSubcommandPaths))
	for key := range ctx.knownSubcommandPaths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		iox.Fprintf(w, "        %s) return 0 ;;\n", bashQuote(key))
	}
}

func writeZshFlagCases(w io.Writer, ctx completionContext) {
	keys := make([]string, 0, len(ctx.structuredFlags))
	for key := range ctx.structuredFlags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		flags := ctx.structuredFlags[key]
		if len(flags) == 0 {
			continue
		}
		iox.Fprintf(w, "        %s)\n", bashQuote(key))
		iox.Fprintf(w, "            reply=(\n")
		for _, flag := range flags {
			iox.Fprintf(w, "                %s\n", bashQuote(zshFlagSpec(flag.Long, flag.Description, flag.ValueName, nil)))
		}
		iox.Fprintf(w, "            )\n")
		iox.Fprintf(w, "            ;;\n")
	}
}

// zshDescribeEntry renders one "'name:description'" entry for the zsh
// _describe arrays.
//
// The quoting is the point. These entries are emitted inside single quotes, so
// an apostrophe in the prose ("Run the project's tests") would close the string
// early and corrupt the rest of the array — the same defect this slice fixed on
// the fish side, where it silently truncated the script. bashQuote applies the
// POSIX "'\”" escape that zsh shares, and routing every entry through here
// means a new description cannot reintroduce it.
func zshDescribeEntry(name, description string) string {
	return bashQuote(zshDescribeName(name) + ":" + description)
}

// zshDescribeName escapes a name for a _describe entry. _describe reads the
// name up to the first colon that no backslash escapes, then removes the
// backslashes, so a colon in a name would start its description.
func zshDescribeName(name string) string {
	return strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(name)
}

// zshDescribeNames renders names as the words of a zsh array that _describe
// reads: each name is one word (bashWord) and escaped for _describe.
func zshDescribeNames(names []string) string {
	words := make([]string, len(names))
	for i, name := range names {
		words[i] = bashWord(zshDescribeName(name))
	}
	return strings.Join(words, " ")
}

// zshFlagSpec renders one zsh completion spec: "--name[description]" for a
// boolean, plus ":metavariable:" for a value flag and "(a b c)" when the value
// has a known candidate set. _values reads a backslash as an escape, ends the
// name at "[" or ":" and the description at "]", so the name escapes the
// backslash, "[" and ":", and the description the backslash and "]". bashQuote
// handles the surrounding single quotes, including any apostrophe the prose
// carries.
func zshFlagSpec(long, description, valueName string, values []string) string {
	name := strings.NewReplacer(`\`, `\\`, `[`, `\[`, `:`, `\:`).Replace(long)
	spec := name + "[" + strings.NewReplacer(`\`, `\\`, `]`, `\]`).Replace(description) + "]"
	if valueName == "" {
		return spec
	}
	spec += ":" + strings.Trim(valueName, "<>") + ":"
	if len(values) > 0 {
		spec += "(" + strings.Join(values, " ") + ")"
	}
	return spec
}

func writeZshValueCases(w io.Writer, ctx completionContext) {
	valueKeys := make([]string, 0, len(ctx.structuredFlagValues))
	for key := range ctx.structuredFlagValues {
		valueKeys = append(valueKeys, key)
	}
	sort.Strings(valueKeys)
	for _, key := range valueKeys {
		values := ctx.structuredFlagValues[key]
		if len(values) == 0 {
			continue
		}
		iox.Fprintf(w, "                    %s)\n", bashQuote(key))
		iox.Fprintf(w, "                        cmd_values=(\n")
		for _, value := range values {
			entry := zshDescribeName(value.Value)
			if value.Description != "" {
				entry += ":" + value.Description
			}
			iox.Fprintf(w, "                            %s\n", bashQuote(entry))
		}
		iox.Fprintf(w, "                        )\n")
		iox.Fprintf(w, "                        ;;\n")
	}
}
