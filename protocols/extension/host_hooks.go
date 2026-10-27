// Host hooks: the optional top-level `hostHooks` member of an extension
// manifest.
//
// An agent host (Claude Code, Codex) runs commands at points of a session: when
// it starts, after a tool call, when a turn stops. An extension declares the
// hooks it wants there, and the CLI writes them into the host's own hook file
// on an explicit install, init or upgrade. Each hook runs one interactive
// subcommand of the extension, through the workspace's launcher, so the hook and
// the command it runs always come from the same release.
//
// The member is gated twice. The workspace must opt into the extension's agent
// content (`extension:<name>` in agentArtifacts), and it must bind the
// collaboration contract the hooks require (`options.collaboration.<name>`).
// Declaring the member is additive vocabulary: the manifest must carry
// cliContract HostHooksContract, which a reader that predates the member
// refuses instead of installing the extension without its hooks.

package extension

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// The agent hosts a manifest may declare hooks for. The list is closed: the CLI
// knows how to write each one's hook file, and nothing else.
const (
	// HostClaudeCode is Claude Code. Its hooks live in .claude/settings.json.
	HostClaudeCode = "claude-code"
	// HostCodex is Codex. Its hooks live in .codex/hooks.json.
	HostCodex = "codex"
)

// HostHookEventPostToolUse is the one event that takes a tool matcher.
const HostHookEventPostToolUse = "PostToolUse"

// hostHookEvents is the closed list of events each host runs a hook at, in the
// order the CLI writes new event keys.
var hostHookEvents = map[string][]string{
	HostClaudeCode: {"SessionStart", HostHookEventPostToolUse, "Stop", "StopFailure"},
	HostCodex:      {"SessionStart", HostHookEventPostToolUse, "Stop"},
}

// HostHookHosts returns the hosts a manifest may declare hooks for, sorted.
func HostHookHosts() []string {
	hosts := make([]string, 0, len(hostHookEvents))
	for host := range hostHookEvents {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

// HostHookEvents returns the events host runs a hook at, in the order the CLI
// writes new event keys. It returns nil for a host outside the closed list.
func HostHookEvents(host string) []string {
	return slices.Clone(hostHookEvents[host])
}

// Bounds of a host hook declaration.
const (
	// MaxHostHookTimeoutSeconds is the longest timeout a hook may declare.
	MaxHostHookTimeoutSeconds = 3600
	// maxHostHookMatcherNames bounds the tools and MCP tools of one entry.
	maxHostHookMatcherNames = 32
	// maxHostHookArgs bounds the arguments of one entry.
	maxHostHookArgs = 16
	// maxHostHookTokenLength bounds every name and argument.
	maxHostHookTokenLength = 128
)

// HostHooksDeclaration is the manifest's `hostHooks` member.
type HostHooksDeclaration struct {
	// Requires names what a workspace must provide before the CLI writes the
	// hooks.
	Requires HostHooksRequirement `json:"requires"`
	// Command is the path of the extension subcommand every hook runs, from
	// its command group down: ["agent", "hook"] runs `putnami agent hook`. The
	// path must name an interactive subcommand of this manifest, so the
	// command's exit code reaches the host unchanged.
	Command []string `json:"command"`
	// Hosts maps each agent host to the hooks the extension declares there.
	Hosts map[string][]HostHookEntry `json:"hosts"`
}

// HostHooksRequirement names what a workspace must provide before the CLI
// writes an extension's hooks.
type HostHooksRequirement struct {
	// Contract is the collaboration contract the hooks call. The CLI writes
	// the hooks only when the workspace binds it under
	// options.collaboration.<contract>.
	Contract string `json:"contract"`
}

// HostHookEntry is one hook: the event it runs at and how the command is
// called there.
type HostHookEntry struct {
	// Event is the host event the hook runs at, from the host's closed list
	// (HostHookEvents).
	Event string `json:"event"`
	// Tools names the host's own tools a PostToolUse hook runs after.
	Tools []string `json:"tools,omitempty"`
	// MCPTools names the Putnami MCP tools a PostToolUse hook runs after,
	// as the extension declares them (`missions.ask`). The CLI renders each
	// one as the host names it.
	MCPTools []string `json:"mcpTools,omitempty"`
	// Args are the arguments the command receives after its path.
	Args []string `json:"args,omitempty"`
	// Wake asks the host to start a turn when the command exits 75. Only
	// Claude Code supports it. Every other exit code, and every exit code of an
	// entry without Wake, leaves the session as it is.
	Wake bool `json:"wake,omitempty"`
	// TimeoutSeconds is how long the host lets the hook run, 1 to
	// MaxHostHookTimeoutSeconds.
	TimeoutSeconds int `json:"timeoutSeconds"`
}

// DeclaresHostHooks reports whether the manifest carries a hostHooks member at
// all. An empty member counts: validation rejects it rather than treating it as
// absent.
func DeclaresHostHooks(m *Manifest) bool {
	return m != nil && m.HostHooks != nil
}

var (
	// hostHooksContractFormat is the grammar of a collaboration contract name.
	hostHooksContractFormat = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	// hostHooksCommandTokenFormat is the grammar of a command group and
	// subcommand word.
	hostHooksCommandTokenFormat = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// hostHookToolFormat is the grammar of a host tool name. It holds no
	// regular expression character, so a matcher built from it matches the
	// names and nothing else.
	hostHookToolFormat = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	// hostHookMCPToolFormat is the grammar of a Putnami MCP tool name in a
	// hook: namespaced, without a hyphen. A host turns the dot into an
	// underscore; a hyphen has no rendering both hosts agree on, so it is
	// refused.
	hostHookMCPToolFormat = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	// hostHookArgFormat is the grammar of an argument: words a POSIX shell
	// reads as written, with no quoting, expansion or operator.
	hostHookArgFormat = regexp.MustCompile(`^[A-Za-z0-9_./:=-]+$`)
)

// ValidateHostHooks checks the hostHooks member. It is inert for a manifest
// that declares none, so every manifest written before the member existed
// keeps its exact verdict.
func ValidateHostHooks(m *Manifest) []diag.Diagnostic {
	if !DeclaresHostHooks(m) {
		return nil
	}
	hooks := m.HostHooks
	const field = "hostHooks"
	var diags []diag.Diagnostic

	// The opt-in that gates the hooks is the opt-in into the agent content, so
	// an extension without content offers hooks no workspace can enable.
	if !m.DeclaresAgentContent() {
		diags = append(diags, diag.Errorf("host-hooks-without-agent-content", field,
			"host hooks are enabled by the extension:<name> opt-in into agent content, so an extension that declares host hooks must declare agentContent"))
	}

	switch contract := hooks.Requires.Contract; {
	case contract == "":
		diags = append(diags, diag.Errorf("required-field", field+".requires.contract",
			"host hooks name the collaboration contract they require"))
	case !hostHooksContractFormat.MatchString(contract):
		diags = append(diags, diag.Errorf("invalid-host-hooks-contract", field+".requires.contract",
			"collaboration contract %q must be lowercase letters, digits and hyphens, starting with a letter", contract))
	}

	diags = append(diags, validateHostHooksCommand(m, hooks.Command)...)

	if len(hooks.Hosts) == 0 {
		diags = append(diags, diag.Errorf("required-field", field+".hosts",
			"host hooks declare at least one host (%s)", strings.Join(HostHookHosts(), ", ")))
	}
	hosts := make([]string, 0, len(hooks.Hosts))
	for host := range hooks.Hosts {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		hostField := field + ".hosts." + host
		events, known := hostHookEvents[host]
		if !known {
			diags = append(diags, diag.Errorf("unknown-host-hooks-host", hostField,
				"unknown agent host %q; the hosts are %s", host, strings.Join(HostHookHosts(), ", ")))
			continue
		}
		entries := hooks.Hosts[host]
		if len(entries) == 0 {
			diags = append(diags, diag.Errorf("empty-host-hooks", hostField,
				"host %s lists no hook; remove it or declare one", host))
			continue
		}
		seen := make(map[string]int, len(entries))
		for index, entry := range entries {
			entryField := hostField + "." + strconv.Itoa(index)
			diags = append(diags, validateHostHookEntry(host, events, entryField, entry)...)
			key := hostHookEntryKey(entry)
			if first, dup := seen[key]; dup {
				diags = append(diags, diag.Errorf("duplicate-host-hook", entryField,
					"hook repeats %s.%d: the host would run the command twice", hostField, first))
				continue
			}
			seen[key] = index
		}
	}
	return diags
}

// validateHostHooksCommand checks that the command path names an interactive
// subcommand of the manifest. The CLI hands an interactive subcommand's exit
// code to its caller unchanged, and the hooks rely on that: a wake asks with an
// exit code no failure of the CLI itself produces.
func validateHostHooksCommand(m *Manifest, path []string) []diag.Diagnostic {
	const field = "hostHooks.command"
	if len(path) == 0 {
		return []diag.Diagnostic{diag.Errorf("required-field", field,
			"host hooks name the command they run, from its command group down")}
	}
	for index, token := range path {
		if !hostHooksCommandTokenFormat.MatchString(token) {
			return []diag.Diagnostic{diag.Errorf("invalid-host-hooks-command", field+"."+strconv.Itoa(index),
				"command word %q must be lowercase letters, digits and hyphens", token)}
		}
	}
	display := strings.Join(path, " ")
	if len(path) < 2 {
		return []diag.Diagnostic{diag.Errorf("invalid-host-hooks-command", field,
			"command %q names a group but no subcommand; a hook runs one subcommand", display)}
	}
	group, ok := m.CommandGroups[path[0]]
	if !ok {
		return []diag.Diagnostic{diag.Errorf("invalid-host-hooks-command", field,
			"command %q names no command group of this extension", display)}
	}
	sub, ok := group.Subcommands[path[1]]
	if !ok {
		return []diag.Diagnostic{diag.Errorf("invalid-host-hooks-command", field,
			"command %q names no subcommand of group %s", display, path[0])}
	}
	if !sub.Interactive {
		return []diag.Diagnostic{diag.Errorf("invalid-host-hooks-command", field,
			"command %q must be interactive: only an interactive subcommand hands its exit code to the host", display)}
	}
	nested := sub
	for _, token := range path[2:] {
		next, ok := nested.Subcommands[token]
		if !ok {
			return []diag.Diagnostic{diag.Errorf("invalid-host-hooks-command", field,
				"command %q names no subcommand %s", display, token)}
		}
		nested = next
	}
	return nil
}

func validateHostHookEntry(host string, events []string, field string, entry HostHookEntry) []diag.Diagnostic {
	var diags []diag.Diagnostic
	switch {
	case entry.Event == "":
		diags = append(diags, diag.Errorf("required-field", field+".event", "a hook names the event it runs at"))
	case !slices.Contains(events, entry.Event):
		diags = append(diags, diag.Errorf("unknown-host-hook-event", field+".event",
			"host %s runs no hook at event %q; its events are %s", host, entry.Event, strings.Join(events, ", ")))
	}

	matches := len(entry.Tools) + len(entry.MCPTools)
	switch {
	case matches > 0 && entry.Event != HostHookEventPostToolUse:
		diags = append(diags, diag.Errorf("invalid-host-hook-matcher", field,
			"only a %s hook names the tools it runs after", HostHookEventPostToolUse))
	case matches == 0 && entry.Event == HostHookEventPostToolUse:
		diags = append(diags, diag.Errorf("invalid-host-hook-matcher", field,
			"a %s hook names the tools it runs after in tools or mcpTools; without them it runs after every tool", HostHookEventPostToolUse))
	}
	if matches > maxHostHookMatcherNames {
		diags = append(diags, diag.Errorf("invalid-host-hook-matcher", field,
			"a hook names at most %d tools and MCP tools, this one names %d", maxHostHookMatcherNames, matches))
	}
	diags = append(diags, validateHostHookNames(field+".tools", entry.Tools, hostHookToolFormat,
		"host tool %q must be letters, digits and underscores, starting with a letter")...)
	diags = append(diags, validateHostHookNames(field+".mcpTools", entry.MCPTools, hostHookMCPToolFormat,
		"MCP tool %q must be a namespaced name (for example, missions.ask) of lowercase letters, digits and underscores")...)

	if len(entry.Args) > maxHostHookArgs {
		diags = append(diags, diag.Errorf("invalid-host-hook-args", field+".args",
			"a hook passes at most %d arguments, this one passes %d", maxHostHookArgs, len(entry.Args)))
	}
	for index, arg := range entry.Args {
		if len(arg) > maxHostHookTokenLength || !hostHookArgFormat.MatchString(arg) {
			diags = append(diags, diag.Errorf("invalid-host-hook-args", field+".args."+strconv.Itoa(index),
				"argument %q must be at most %d letters, digits and the characters _ . / : = -, so the shell reads it as written",
				arg, maxHostHookTokenLength))
		}
	}

	if entry.Wake && host != HostClaudeCode {
		diags = append(diags, diag.Errorf("invalid-host-hook-wake", field+".wake",
			"only %s starts a turn from a hook; host %s cannot wake", HostClaudeCode, host))
	}
	if entry.TimeoutSeconds < 1 || entry.TimeoutSeconds > MaxHostHookTimeoutSeconds {
		diags = append(diags, diag.Errorf("invalid-host-hook-timeout", field+".timeoutSeconds",
			"timeoutSeconds must be from 1 to %d, got %d", MaxHostHookTimeoutSeconds, entry.TimeoutSeconds))
	}
	return diags
}

func validateHostHookNames(field string, names []string, format *regexp.Regexp, message string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	seen := make(map[string]int, len(names))
	for index, name := range names {
		nameField := field + "." + strconv.Itoa(index)
		if len(name) > maxHostHookTokenLength || !format.MatchString(name) {
			diags = append(diags, diag.Errorf("invalid-host-hook-matcher", nameField, message, name))
		}
		if first, dup := seen[name]; dup {
			diags = append(diags, diag.Errorf("invalid-host-hook-matcher", nameField,
				"%q repeats %s.%d", name, field, first))
			continue
		}
		seen[name] = index
	}
	return diags
}

// hostHookEntryKey identifies what a hook does, without its timeout: two
// entries with the same key would run the same command at the same point.
func hostHookEntryKey(entry HostHookEntry) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%t", entry.Event,
		strings.Join(entry.Tools, "\x01"), strings.Join(entry.MCPTools, "\x01"), strings.Join(entry.Args, "\x01"), entry.Wake)
}
