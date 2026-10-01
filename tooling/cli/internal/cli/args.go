// Package cli implements the CLI shell: argument parsing, command routing,
// workspace detection, and help printing.
package cli

import (
	"strings"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/engine"
)

// BuiltinAliases maps single-letter shortcuts to command names.
var BuiltinAliases = commandmeta.BuiltinAliases()

// GlobalFlags that the CLI handles before routing to jobs.
//
// The struct itself is declared once, by the engine that consumes it
// (engine.GlobalFlags): the parse layer fills the fields, the run lifecycle
// reads them, and a second copy of the vocabulary here would be exactly the kind
// of duplicate table ADR 0001 exists to prevent.
type GlobalFlags = engine.GlobalFlags

// ParsedArgs is the result of parsing CLI arguments.
type ParsedArgs struct {
	// OriginalArgs retains the invocation arguments so telemetry can record
	// allowlisted flag presence without retaining values.
	OriginalArgs []string
	// Commands is the list of commands to run (e.g., ["lint", "test", "build"]).
	Commands []string
	// Subcommand is for structured commands like "extensions install".
	Subcommand string
	// Global are the parsed global flags.
	Global GlobalFlags
	// JobFlags are flags not consumed by global parsing, passed to job processes.
	JobFlags map[string]any
	// RawJobArgs are the raw string arguments for job flags.
	RawJobArgs []string
	// Err is the usage error the single validation pass produced, or nil. It is
	// carried on the result rather than returned so every existing caller (and
	// A0's parse-acceptance table) keeps the one-value signature; App.Run reports
	// it as ExitUsage after the help and version shortcuts, which deliberately
	// still work on a malformed invocation.
	Err error
	// Warnings are the deprecation notices the parse produced, in the order the
	// flags appeared. They go to stderr so structured stdout stays machine-clean.
	Warnings []string
}

// ParseArgs parses CLI arguments into structured form.
// It handles multi-command syntax (lint,test,build), aliases, global flags,
// and passes remaining flags through for job processes.
//
// extensionGroups lists structured command-group names contributed by
// extensions (e.g. {"cloud": true} for "putnami cloud login"). Pass nil
// when no extension groups are known — only built-in structured commands
// will then be recognized.
func ParseArgs(args []string, userAliases map[string]string, extensionGroups map[string]bool) *ParsedArgs {
	result := &ParsedArgs{
		OriginalArgs: append([]string(nil), args...),
		JobFlags:     make(map[string]any),
	}

	if len(args) == 0 {
		result.Global.Help = true
		return result
	}

	if args[0] == "--version" || args[0] == "-V" {
		globals, remaining, err := parseFlags(args[1:])
		result.Global, result.RawJobArgs, result.Err = globals, remaining, err
		result.Global.Version = true
		return result
	}

	// First positional argument is the command(s) — but only if it doesn't start with "-"
	cmdArg := args[0]
	rest := args[1:]

	if strings.HasPrefix(cmdArg, "-") {
		// No command specified, just flags: this invocation prints help, so a bad
		// flag value is not worth failing on — the user is already being shown the
		// vocabulary.
		result.Global, result.RawJobArgs, _ = parseFlags(args)
		result.Global.Help = true
		return result
	}

	// Check for help flag
	for _, a := range args {
		if a == "--help" || a == "-h" {
			result.Global.Help = true
		}
	}
	helpRequested := result.Global.Help

	// Phase 2 — catalog resolution: comma split, alias chains, and the
	// structured-vs-job decision. A cyclic alias fails HERE, at use, not at
	// config load: a dormant cycle in an alias nobody invokes must not brick
	// every other command in the workspace (epic refinement R9).
	commands, err := resolveCommands(cmdArg, userAliases)
	if err != nil {
		result.Commands = commands
		result.Err = err
		return result
	}

	structured := isStructuredCommand(commands[0]) || extensionGroups[commands[0]]

	// Check if this is a structured command (e.g., "extensions install", "projects list").
	// A structured leaf that takes a positional (qualify <project>) keeps that
	// argument among its own arguments: see commandmeta.PositionalLeaf.
	if structured && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") && !commandmeta.PositionalLeaf(commands[0]) {
		result.Commands = commands
		result.Subcommand = rest[0]
		rest = rest[1:]
	} else {
		result.Commands = commands
	}

	// Phase 3 — one semantic pass. Once a command has been selected, --version is
	// a command/job flag (for example, `putnami upgrade --version 1.2.3`) rather
	// than the top-level version shortcut.
	globals, remaining, err := parseCommandFlags(rest)
	result.Global, result.RawJobArgs = globals, remaining
	result.Global.Help = result.Global.Help || helpRequested
	if err != nil {
		result.Err = err
		return result
	}

	// The first argument not consumed by global flags is a project selection,
	// for job commands only (a structured command's leading positional is its
	// own argument). One implementation, shared with the extension-alias path.
	if !structured {
		result.RawJobArgs = promoteProjectSelector(&result.Global, result.RawJobArgs)
	}

	// A built-in structured command is authoritative about its own flags, so an
	// undeclared one is rejected here. Job commands and extension groups are
	// served by manifests and get a deprecation warning from their dispatch path
	// instead (see parse.go).
	if structured && !extensionGroups[commands[0]] {
		result.Err = validateStructuredFlags(commands[0], result.Subcommand, result.RawJobArgs)
	}

	// --help and --version must keep working on a malformed invocation: a user
	// whose flags are wrong is exactly the user reaching for help.
	if result.Global.Help || result.Global.Version {
		result.Err = nil
	}

	return result
}

// resolveCommands splits comma-separated commands and resolves aliases.
func resolveCommands(input string, userAliases map[string]string) ([]string, error) {
	parts := strings.Split(input, ",")
	commands := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		resolved, err := resolveAlias(p, userAliases)
		if err != nil {
			return commands, err
		}
		commands = append(commands, resolved)
	}
	if len(commands) == 0 {
		return commands, usageErrorf("no command in %q", input)
	}
	return commands, nil
}

// resolveAlias resolves a single command name through alias chains, erroring on
// a cycle instead of silently returning the name unresolved. The error names the
// chain so the offending alias is obvious, and it fires only when the cyclic
// alias is actually invoked.
func resolveAlias(name string, userAliases map[string]string) (string, error) {
	visited := map[string]bool{}
	chain := []string{name}
	current := name
	for {
		if visited[current] {
			return current, usageErrorf("alias %q resolves in a cycle: %s\n  Fix the aliases block in putnami.json.",
				name, strings.Join(chain, " → "))
		}
		visited[current] = true

		target, ok := BuiltinAliases[current]
		if !ok && userAliases != nil {
			target, ok = userAliases[current]
		}
		if !ok {
			return current, nil
		}
		current = target
		chain = append(chain, current)
	}
}

// resolveAliasOrSelf resolves an alias chain for a display-only caller, falling
// back to the last name reached when the chain is cyclic. Help must render
// something for a cyclic alias rather than fail.
func resolveAliasOrSelf(name string, userAliases map[string]string) string {
	resolved, _ := resolveAlias(name, userAliases)
	return resolved
}

// isStructuredCommand returns true for commands that take a subcommand.
//
// The set is the COMMAND CATALOG (ADR 0001 §1), not the handler registry. Slice
// A6a moved the answer here: commandRegistry used to be both the
// vocabulary ("which names are structured commands") and the dispatch table,
// which made it the last hand-maintained table describing commands. It is now
// only the dispatch table, and registerCommand rejects a name the catalog does
// not declare — so the two sets stay identical by construction rather than by
// the consistency test A0 needed while six tables disagreed.
func isStructuredCommand(cmd string) bool {
	return commandmeta.IsStructuredRoot(cmd)
}
