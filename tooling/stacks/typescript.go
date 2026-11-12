package stacks

import (
	"fmt"
	"slices"
	"strings"

	"go.putnami.dev/sdk/extension/tscscript"
)

// TypeScriptExtension is the extension an import hands a TypeScript
// project's targets to (ADR 0017).
const TypeScriptExtension = "@putnami/typescript"

// TypeScriptPrograms are the programs the TypeScript extension's task of the
// same name runs. A script that runs one, and nothing chained to it, keeps
// its tool when the extension takes the task over: the test task runs the
// Vitest or Jest the script runs, and the lint task the ESLint the script
// runs, from the project's configuration (ADR 0008 and ADR 0009 of
// typescript/extension). A build that runs tsc alone is the tscscript
// package's rule (ADR 0010 of typescript/extension).
var TypeScriptPrograms = map[string][]string{
	TaskBuild: {"bun build"},
	TaskTest:  {"bun test", "vitest", "jest", "npx vitest", "npx jest"},
	TaskLint:  {"biome", "bunx biome", "bunx @biomejs/biome", "eslint"},
}

// ESLintScripts are the lint scripts that run ESLint the way the TypeScript
// lint does: over the project, with no argument the TypeScript lint would
// drop, plainly or through a package runner.
var ESLintScripts = []string{
	"eslint", "eslint .",
	"npx eslint", "npx eslint .",
	"bunx eslint", "bunx eslint .",
	"pnpm exec eslint", "pnpm exec eslint .",
	"yarn eslint", "yarn eslint .",
}

// TypeScriptRuns reports whether the TypeScript extension's task runs what a
// script of the task runs, and nothing else: a script that chains or pipes
// another command, such as eslint . && prettier --check ., runs a tool the
// TypeScript extension does not.
func TypeScriptRuns(task, script string) bool {
	if task == TaskBuild && tscscript.Runs(script) {
		return true
	}
	script = strings.TrimSpace(script)
	if script == "" || strings.ContainsAny(script, "&|;") {
		return false
	}
	if task == TaskLint && strings.Contains(script, "eslint") {
		return slices.Contains(ESLintScripts, strings.Join(strings.Fields(script), " "))
	}
	if task == TaskTest && TestArgumentsDropped(script) != "" {
		return false
	}
	return runsProgram(script, TypeScriptPrograms[task])
}

// testFlags are the arguments of a test script the TypeScript extension's
// test runs the same without: they choose no configuration and no subset of
// the suite. Any other argument, such as --config, --project or a path,
// changes what the runner checks, and the extension passes none of the
// script's arguments (ADR 0008 of typescript/extension).
var testFlags = map[string]bool{
	"--run": true, "--coverage": true, "--passWithNoTests": true, "--silent": true,
	"--ci": true, "--runInBand": true, "-i": true, "--detectOpenHandles": true, "--forceExit": true,
}

// TestArgumentsDropped answers the arguments of a Vitest, Jest or bun test
// script that the TypeScript extension's test would drop, or "" when it
// takes none of them or the script runs another program.
func TestArgumentsDropped(script string) string {
	fields := strings.Fields(script)
	if len(fields) > 0 && fields[0] == "npx" {
		fields = fields[1:]
	}
	switch {
	case len(fields) > 1 && fields[0] == "bun" && fields[1] == "test":
		fields = fields[2:]
	case len(fields) > 0 && (fields[0] == "vitest" || fields[0] == "jest"):
		if len(fields) > 1 && fields[0] == "vitest" && fields[1] == "run" {
			// vitest run is Vitest's single run, which the test task runs; for
			// bun test, run is a file filter.
			fields = fields[1:]
		}
		fields = fields[1:]
	default:
		return ""
	}
	var dropped []string
	for _, f := range fields {
		if !testFlags[f] {
			dropped = append(dropped, f)
		}
	}
	return strings.Join(dropped, " ")
}

// TypeScriptBuildsWithBun reports whether the TypeScript extension builds a
// build script with Bun: the script runs bun build alone. Such a build writes
// its bundle under Putnami's output, nothing in the project.
func TypeScriptBuildsWithBun(script string) bool {
	script = strings.TrimSpace(script)
	return !strings.ContainsAny(script, "&|;") && runsProgram(script, TypeScriptPrograms[TaskBuild])
}

// TestRunner is the runner of a test script the TypeScript extension runs:
// vitest or jest, or "" for bun test and any other script.
func TestRunner(script string) string {
	command := strings.TrimPrefix(strings.TrimSpace(script), "npx ")
	for _, runner := range []string{"vitest", "jest"} {
		if command == runner || strings.HasPrefix(command, runner+" ") {
			return runner
		}
	}
	return ""
}

// LintsWithESLint reports whether a lint script the TypeScript extension
// runs lints with ESLint.
func LintsWithESLint(script string) bool {
	return TypeScriptRuns(TaskLint, script) && strings.Contains(script, "eslint")
}

// TypeScriptUnmappedReason says why a script of a TypeScript project keeps
// running through the tool, ending with keeps, which says how it runs.
func TypeScriptUnmappedReason(task, script, keeps string) string {
	switch {
	case strings.TrimSpace(script) == "":
		return fmt.Sprintf("it runs no command %s reads: %s", TypeScriptExtension, keeps)
	case strings.ContainsAny(script, "&|;"):
		return fmt.Sprintf("its script chains another command (%q), which %s does not run: %s", script, TypeScriptExtension, keeps)
	case task == TaskLint && strings.Contains(script, "eslint"):
		return ESLintArgumentsReason(script, keeps)
	case task == TaskTest && TestArgumentsDropped(script) != "":
		return fmt.Sprintf("its test runner takes arguments (%q) that %s would drop: move them to the runner's configuration; %s", TestArgumentsDropped(script), TypeScriptExtension, keeps)
	}
	runs := TypeScriptPrograms[task]
	if task == TaskBuild {
		runs = append(slices.Clone(runs), "tsc alone")
	}
	return fmt.Sprintf("%s runs %s, not %q: %s", TypeScriptExtension, strings.Join(runs, " or "), script, keeps)
}

// eslintCommandLineOnly are the ESLint options no configuration file holds:
// --flag enables an ESLint feature flag, which only the command line and the
// ESLINT_FLAGS variable set. Advice to move one to the ESLint configuration
// could not be followed.
var eslintCommandLineOnly = []string{"--flag"}

// ESLintDroppedArguments splits the arguments a lint script passes ESLint, and
// that the TypeScript lint drops, into the ones an ESLint configuration can
// hold and the ones it cannot (eslintCommandLineOnly), each with its value.
// The "." the TypeScript lint lints anyway is no dropped argument. A script
// that does not start ESLint as a word answers its whole text as holdable.
func ESLintDroppedArguments(script string) (holdable, commandLineOnly []string) {
	fields := strings.Fields(script)
	start := slices.Index(fields, "eslint")
	if start < 0 {
		return []string{strings.TrimSpace(script)}, nil
	}
	fields = fields[start+1:]
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		name, _, inline := strings.Cut(field, "=")
		switch {
		case field == ".":
		case slices.Contains(eslintCommandLineOnly, name):
			if !inline && i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
				field += " " + fields[i+1]
				i++
			}
			commandLineOnly = append(commandLineOnly, field)
		default:
			holdable = append(holdable, field)
		}
	}
	return holdable, commandLineOnly
}

// ESLintArgumentsReason says why an ESLint lint script keeps running through
// the tool, ending with keeps: it names the arguments the TypeScript lint
// would drop, and advises moving them to the ESLint configuration only when
// that configuration can hold every one of them.
func ESLintArgumentsReason(script, keeps string) string {
	holdable, commandLineOnly := ESLintDroppedArguments(script)
	if len(commandLineOnly) > 0 {
		return fmt.Sprintf("its ESLint takes %q, which %s would drop and no ESLint configuration can hold: %s",
			strings.Join(commandLineOnly, " "), TypeScriptExtension, keeps)
	}
	return fmt.Sprintf("its ESLint takes arguments (%q) that %s would drop: move them to the ESLint configuration; %s",
		strings.Join(holdable, " "), TypeScriptExtension, keeps)
}

// runsProgram reports whether command is one of programs, or one of them
// followed by its arguments.
func runsProgram(command string, programs []string) bool {
	for _, program := range programs {
		if command == program || strings.HasPrefix(command, program+" ") {
			return true
		}
	}
	return false
}

// TypeScriptRunnerLayer is the option layer of the TypeScript extension's
// test command, whose runner member an import writes.
const TypeScriptRunnerLayer = TypeScriptExtension + ":test"

// SetTypeScriptRunner sets options["@putnami/typescript:test"].runner of a
// putnami.json document to runner, or removes it, and every object it
// empties, when runner is "".
func SetTypeScriptRunner(doc map[string]any, runner string) {
	options, _ := doc["options"].(map[string]any)
	layer, _ := options[TypeScriptRunnerLayer].(map[string]any)
	if runner != "" {
		layer = ensureObject(ensureObject(doc, "options"), TypeScriptRunnerLayer)
		layer["runner"] = runner
		return
	}
	if layer == nil {
		return
	}
	delete(layer, "runner")
	pruneEmpty(options, TypeScriptRunnerLayer)
	pruneEmpty(doc, "options")
}

// ESLintLinter is the value of options["@putnami/typescript"].lint.linter
// that lints a project with ESLint.
const ESLintLinter = "eslint"

// SetTypeScriptLinter writes options["@putnami/typescript"].lint.linter
// eslint into a putnami.json document when eslint is set, unless the project
// chose another linter, and otherwise removes an eslint the sync wrote, with
// every object it empties.
func SetTypeScriptLinter(doc map[string]any, eslint bool) {
	options, _ := doc["options"].(map[string]any)
	layer, _ := options[TypeScriptExtension].(map[string]any)
	lint, _ := layer["lint"].(map[string]any)
	if eslint {
		if current, set := lint["linter"].(string); set && current != "" && current != ESLintLinter {
			// The project chose its linter: the sync keeps it.
			return
		}
		lint = ensureObject(ensureObject(ensureObject(doc, "options"), TypeScriptExtension), "lint")
		lint["linter"] = ESLintLinter
		return
	}
	if lint == nil || lint["linter"] != ESLintLinter {
		return
	}
	delete(lint, "linter")
	pruneEmpty(layer, "lint")
	pruneEmpty(options, TypeScriptExtension)
	pruneEmpty(doc, "options")
}

// ensureObject answers the object at key of doc, which it creates when
// absent.
func ensureObject(doc map[string]any, key string) map[string]any {
	child, _ := doc[key].(map[string]any)
	if child == nil {
		child = map[string]any{}
		doc[key] = child
	}
	return child
}

// pruneEmpty removes the object at key of doc when it is empty.
func pruneEmpty(doc map[string]any, key string) {
	if doc == nil {
		return
	}
	if child, ok := doc[key].(map[string]any); ok && len(child) == 0 {
		delete(doc, key)
	}
}
