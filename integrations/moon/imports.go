package moon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.putnami.dev/sdk/extension/tscscript"
	detect "go.putnami.dev/tooling/observe/detect/moon"
	"go.putnami.dev/tooling/stacks"
)

// languagePrograms are, by moon language, the programs the language
// extension's build, lint and test tasks run, each with the arguments it
// runs the same without: they choose the whole project or only change the
// output. A moon task of an imported project moves to the extension only
// when it runs one of them with no other argument, since the extension runs
// the program under its own arguments and drops the task's (ADR 0017 of
// tooling/stacks); TypeScript reads the shared rule instead.
var languagePrograms = map[string]map[string][]program{
	// The Go lint runs golangci-lint with staticcheck and the docs-links
	// check beside it, and the Rust lint cargo fmt --check before clippy -D
	// warnings: a lint task of either moves to no extension, since the
	// extension's lint checks more than the task did. The Rust test runs
	// nextest, which runs no doctest, so a cargo test stays on moon too.
	"go": {
		stacks.TaskBuild: {{"go build", []string{"./...", "."}}},
		// The Go test runs go test ./..., so a narrower package pattern such
		// as . stays on moon.
		stacks.TaskTest: {{"go test", []string{"./...", "-v"}}},
	},
	"rust": {
		stacks.TaskBuild: {{"cargo build", []string{"--locked"}}},
		stacks.TaskTest:  {{"cargo nextest", []string{"run"}}, {"cargo llvm-cov nextest", nil}},
	},
	// The Python lint runs ruff format --check beside ruff check: a lint
	// task stays on moon, as Go's and Rust's do.
	"python": {
		stacks.TaskTest: {{"pytest", []string{"-q", "-v"}}},
	},
	"ruby": {
		stacks.TaskTest: {{"rspec", nil}},
		stacks.TaskLint: {{"rubocop", []string{"."}}},
	},
	"php": {
		stacks.TaskTest: {{"phpunit", nil}},
		stacks.TaskLint: {{"phpstan", []string{"analyse", "analyze"}}}, //nolint:misspell // phpstan accepts both spellings of its subcommand
	},
}

// program is a program a language extension's task runs, with the
// arguments it runs the same without.
type program struct {
	name string
	args []string
}

// dropped answers the arguments of run, a command line without its runner,
// that the extension would drop, and whether run runs the program at all.
func (p program) dropped(run string) (string, bool) {
	rest, ok := strings.CutPrefix(run, p.name)
	if !ok || (rest != "" && !strings.HasPrefix(rest, " ")) {
		return "", false
	}
	var out []string
	for _, arg := range strings.Fields(rest) {
		if !slices.Contains(p.args, arg) {
			out = append(out, arg)
		}
	}
	return strings.Join(out, " "), true
}

// runners are the prefixes that run a program through a package manager or
// an environment without changing the program: the language extension runs
// the program through its own.
var runners = []string{"bundle exec ", "uv run ", "poetry run ", "python -m ", "python3 -m ", "npx ", "bunx ", "pnpm exec ", "yarn "}

// bareCommand is the command line without the runner prefixes and without
// a path before its program, such as vendor/bin/.
func bareCommand(command string) string {
	command = strings.TrimSpace(command)
	for trimmed := true; trimmed; {
		trimmed = false
		for _, r := range runners {
			if rest, ok := strings.CutPrefix(command, r); ok {
				command, trimmed = strings.TrimSpace(rest), true
			}
		}
	}
	first, rest, _ := strings.Cut(command, " ")
	if i := strings.LastIndex(first, "/"); i >= 0 {
		first = first[i+1:]
	}
	return strings.TrimSpace(first + " " + rest)
}

// moves reports whether the language extension of an imported project runs
// what the moon task of the command runs, the command line line, and when it
// does not, why. A task that states no command, such as one an export reads
// without them, stays on moon.
func moves(root string, p detect.Project, command, line string) (bool, string) {
	keeps := fmt.Sprintf("it keeps running through moon run %s:%s", p.ID, TaskFor(p, command))
	if p.Language == "typescript" {
		return typeScriptMoves(root, p.Dir, command, line, keeps)
	}
	extension := LanguageExtensions[p.Language]
	programs := languagePrograms[p.Language][command]
	switch {
	case len(programs) == 0:
		return false, fmt.Sprintf("%s has no %s that runs only what a moon task runs: %s", extension, command, keeps)
	case strings.TrimSpace(line) == "":
		return false, fmt.Sprintf("it states no command %s reads: %s", extension, keeps)
	case strings.ContainsAny(line, "&|;"):
		return false, fmt.Sprintf("it chains another command (%q), which %s does not run: %s", line, extension, keeps)
	}
	run := bareCommand(line)
	names := make([]string, 0, len(programs))
	for _, want := range programs {
		names = append(names, want.name)
		dropped, runs := want.dropped(run)
		switch {
		case !runs:
			continue
		case dropped != "":
			return false, fmt.Sprintf("%s runs %s under its own arguments and would drop %q: %s", extension, want.name, dropped, keeps)
		}
		return true, ""
	}
	return false, fmt.Sprintf("%s runs %s, not %q: %s", extension, strings.Join(names, " or "), line, keeps)
}

// typeScriptMoves applies the TypeScript rule to a moon task. The
// TypeScript extension reads the package.json scripts of the project to
// choose its compiler and its linter (ADR 0009 and ADR 0010 of
// typescript/extension), so a task moves only when those scripts lead it to
// the same tool: a tsc build only as the build script, a Bun build only when
// the build script runs no tsc, a Biome lint only when the lint script runs
// no ESLint. A test records its runner, bun, vitest or jest, so it moves
// whatever the test script runs.
func typeScriptMoves(root, dir, command, line, keeps string) (bool, string) {
	if !stacks.TypeScriptRuns(command, line) {
		return false, stacks.TypeScriptUnmappedReason(command, line, keeps)
	}
	script := packageScripts(filepath.Join(root, filepath.FromSlash(dir)))[command]
	switch {
	case command == stacks.TaskBuild && tscscript.Runs(line) && strings.TrimSpace(script) != strings.TrimSpace(line):
		return false, fmt.Sprintf("%s compiles the tsc of the package.json build script, and %q is another command: %s", stacks.TypeScriptExtension, line, keeps)
	case command == stacks.TaskBuild && !tscscript.Runs(line) && tscscript.Runs(script):
		return false, fmt.Sprintf("the package.json build script runs tsc, which %s would compile instead of %q: %s", stacks.TypeScriptExtension, line, keeps)
	case command == stacks.TaskLint && !strings.Contains(line, "eslint") && strings.Contains(firstCommand(script), "eslint"):
		return false, fmt.Sprintf("the package.json lint script runs ESLint, which %s would run instead of %q: %s", stacks.TypeScriptExtension, line, keeps)
	}
	return true, ""
}

// firstCommand is the first command of a script, before any command chained
// to it.
func firstCommand(script string) string {
	if i := strings.IndexAny(script, "&|;"); i >= 0 {
		return script[:i]
	}
	return script
}

// packageScripts reads the scripts of the package.json in dir.
func packageScripts(dir string) map[string]string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json")) //nolint:gosec // a project manifest under the workspace root
	if err != nil {
		return nil
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	_ = json.Unmarshal(data, &manifest)
	return manifest.Scripts
}

// importPlan is what an import writes for one project whose language has an
// extension.
type importPlan struct {
	// extension and also are the extensions the project runs: its
	// language's alone, or the moon extension beside it when a task stays on
	// moon.
	extension string
	also      []string
	// disabled are the commands the project runs none of, bare for a
	// command it has no moon task for, else qualified by the extension that
	// does not run it.
	disabled []string
	// kept are the commands whose moon task stays on moon, by command.
	kept map[string]string
	// unmapped list each kept task with its reason.
	unmapped []Unmapped
	// runner and eslint are what a TypeScript project records for the tasks
	// that moved: the runner of its test, bun, vitest or jest, and an
	// ESLint lint.
	runner string
	eslint bool
	// bunBuild reports a build that moved to the TypeScript extension's Bun
	// build.
	bunBuild bool
}

// planImport maps the moon tasks of a project whose language has an
// extension: each task its extension runs moves, every other one stays on
// moon.
func planImport(root string, p detect.Project, mapped map[string]string) importPlan {
	extension := LanguageExtensions[p.Language]
	plan := importPlan{extension: extension, kept: map[string]string{}}
	commands := map[string]string{}
	for _, t := range p.Tasks {
		commands[t.Name] = t.Command
	}
	var moved []string
	for _, command := range Commands {
		task, ok := mapped[command]
		if !ok {
			plan.disabled = append(plan.disabled, command)
			continue
		}
		line := commands[task]
		if ok, reason := moves(root, p, command, line); !ok {
			plan.kept[command] = task
			plan.unmapped = append(plan.unmapped, Unmapped{Project: p.ID, Task: task, Reason: reason})
			continue
		}
		moved = append(moved, command)
		if p.Language != "typescript" {
			continue
		}
		switch command {
		case stacks.TaskTest:
			// The runner is recorded even for bun test: the extension would
			// otherwise read it from the package.json test script.
			plan.runner = stacks.TestRunner(line)
			if plan.runner == "" {
				plan.runner = "bun"
			}
		case stacks.TaskLint:
			plan.eslint = stacks.LintsWithESLint(line)
		case stacks.TaskBuild:
			plan.bunBuild = stacks.TypeScriptBuildsWithBun(line)
		}
	}
	if len(plan.kept) == 0 {
		return plan
	}
	plan.extension, plan.also = ExtensionName, []string{extension}
	for _, command := range moved {
		plan.disabled = append(plan.disabled, ExtensionName+":"+command)
	}
	for _, command := range Commands {
		if _, ok := plan.kept[command]; ok {
			plan.disabled = append(plan.disabled, extension+":"+command)
		}
	}
	return plan
}
