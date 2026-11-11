package moon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/tscscript"
	detect "go.putnami.dev/tooling/observe/detect/moon"
	"go.putnami.dev/tooling/stacks"
)

// languagePrograms are, by moon language, the programs the language
// extension's build, lint and test tasks run. A moon task of an imported
// project moves to the extension only when it runs one of them, alone
// (ADR 0017 of tooling/stacks); TypeScript reads the shared rule instead.
var languagePrograms = map[string]map[string][]string{
	// The Go lint runs golangci-lint with staticcheck and the docs-links
	// check beside it, and the Rust lint cargo fmt --check before clippy -D
	// warnings: a lint task of either moves to no extension, since the
	// extension's lint checks more than the task did. The Rust test runs
	// nextest, which runs no doctest, so a cargo test stays on moon too.
	"go": {
		stacks.TaskBuild: {"go build"},
		stacks.TaskTest:  {"go test"},
	},
	"rust": {
		stacks.TaskBuild: {"cargo build"},
		stacks.TaskTest:  {"cargo nextest", "cargo llvm-cov nextest"},
	},
	"python": {
		stacks.TaskTest: {"pytest"},
		stacks.TaskLint: {"ruff"},
	},
	"ruby": {
		stacks.TaskTest: {"rspec"},
		stacks.TaskLint: {"rubocop"},
	},
	"php": {
		stacks.TaskTest: {"phpunit"},
		stacks.TaskLint: {"phpstan"},
	},
}

// runners are the prefixes that run a program through a package manager or
// an environment without changing the program: the language extension runs
// the program through its own.
var runners = []string{"bundle exec ", "uv run ", "poetry run ", "python -m ", "python3 -m ", "npx ", "bunx ", "pnpm exec ", "yarn "}

// program is the command line without the runner prefixes and without a
// path before its program, such as vendor/bin/.
func program(command string) string {
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
		return false, fmt.Sprintf("%s runs no %s of a moon task: %s", extension, command, keeps)
	case strings.TrimSpace(line) == "":
		return false, fmt.Sprintf("it states no command %s reads: %s", extension, keeps)
	case strings.ContainsAny(line, "&|;"):
		return false, fmt.Sprintf("it chains another command (%q), which %s does not run: %s", line, extension, keeps)
	}
	run := program(line)
	for _, want := range programs {
		if run == want || strings.HasPrefix(run, want+" ") {
			return true, ""
		}
	}
	return false, fmt.Sprintf("%s runs %s, not %q: %s", extension, strings.Join(programs, " or "), line, keeps)
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
	// that moved: the Vitest or Jest runner of its test, an ESLint lint.
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
