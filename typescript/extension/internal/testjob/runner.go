package testjob

import (
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Runner is the program that runs a project's test suite.
type Runner string

// The test runners a project may choose.
const (
	RunnerBun    Runner = "bun"
	RunnerVitest Runner = "vitest"
	RunnerJest   Runner = "jest"
)

// RunnerConfigFiles are the project files a Vitest or Jest run reads besides
// its tests and sources: its configuration and setup files. They are inputs of
// the test task's cache key, declared in putnami.extension.json.
var RunnerConfigFiles = []string{
	"vitest.config.*", "vite.config.*", "vitest.setup.*",
	"jest.config.*", "jest.setup.*", "babel.config.*", "tsconfig*.json",
}

// lookPath finds a program on PATH.
var lookPath = osexec.LookPath

// ResolveRunner returns the test runner of the project at projectPath: param,
// the runner parameter of the test command, else the runner its package.json
// test script runs, else Bun. It reads the project's own manifest, so every
// member of a batch resolves its own runner.
func ResolveRunner(projectPath, param string) (Runner, error) {
	switch runner := Runner(strings.TrimSpace(param)); runner {
	case "":
	case RunnerBun, RunnerVitest, RunnerJest:
		return runner, nil
	default:
		return "", fmt.Errorf("the test runner %q is unknown: choose bun, vitest or jest", param)
	}
	scripts := packageScripts(filepath.Join(projectPath, "package.json"))
	if runner := scriptRunner(scripts, scripts["test"], 0); runner != "" {
		return runner, nil
	}
	return RunnerBun, nil
}

// packageScripts reads the scripts of a package.json; nil when absent or
// unreadable.
func packageScripts(path string) map[string]string {
	data, err := os.ReadFile(path) //nolint:gosec // the project's own manifest
	if err != nil {
		return nil
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return nil
	}
	return manifest.Scripts
}

// scriptLaunchers are the words that start another program in a package
// script: the runner is the program after them.
var scriptLaunchers = map[string]bool{"npx": true, "bunx": true, "cross-env": true, "dotenv": true, "--": true, "exec": true}

// nodeValueOptions are the node options whose value is the next word.
var nodeValueOptions = map[string]bool{"-r": true, "--require": true, "--import": true, "--loader": true, "--experimental-loader": true}

// scriptRunDepth bounds how many `run <script>` hops scriptRunner follows.
const scriptRunDepth = 3

// scriptRunner returns the runner a package script runs: the first command of
// the script, among those `&&`, `||`, `;` or `|` separate, that starts Vitest,
// Jest or bun test, or "" when none does. A command that runs another script
// of the package (npm run, bun run, pnpm or yarn with a script name) is
// followed into it.
func scriptRunner(scripts map[string]string, script string, depth int) Runner {
	for _, command := range splitCommands(script) {
		if runner := commandRunner(scripts, strings.Fields(command), depth); runner != "" {
			return runner
		}
	}
	return ""
}

func splitCommands(script string) []string {
	return strings.FieldsFunc(script, func(r rune) bool { return r == '&' || r == '|' || r == ';' })
}

// commandRunner returns the runner one command starts.
func commandRunner(scripts map[string]string, words []string, depth int) Runner {
	for i := 0; i < len(words); i++ {
		word := words[i]
		switch {
		case strings.Contains(word, "=") && !strings.HasPrefix(word, "-"):
			// An environment assignment before the program.
			continue
		case word == "dotenv" && slices.Contains(words[i+1:], "--"):
			// dotenv [options] -- <command>
			i += slices.Index(words[i+1:], "--") + 1
			continue
		case scriptLaunchers[word]:
			continue
		case strings.HasPrefix(word, "-") && i > 0:
			// An option of a launcher, such as dotenv -e .env.
			continue
		}
		rest := words[i+1:]
		switch name := strings.TrimSuffix(filepath.Base(word), filepath.Ext(word)); name {
		case "vitest":
			return RunnerVitest
		case "jest":
			return RunnerJest
		case "bun":
			if len(rest) > 0 && rest[0] == "test" {
				return RunnerBun
			}
			return runScript(scripts, rest, depth)
		case "npm", "pnpm", "yarn":
			return runScript(scripts, rest, depth)
		case "node":
			// node [options] <entry>: the entry names the runner.
			for j := 0; j < len(rest); j++ {
				arg := rest[j]
				if nodeValueOptions[arg] {
					j++
					continue
				}
				if strings.HasPrefix(arg, "-") {
					continue
				}
				switch {
				case strings.Contains(arg, "vitest"):
					return RunnerVitest
				case strings.Contains(arg, "jest"):
					return RunnerJest
				}
				return ""
			}
		}
		return ""
	}
	return ""
}

// runScript follows `run <name>`, or a bare script name, into the package's
// script of that name.
func runScript(scripts map[string]string, args []string, depth int) Runner {
	if depth >= scriptRunDepth || len(args) == 0 {
		return ""
	}
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		// An option of the package manager, such as --silent or --bun.
		args = args[1:]
	}
	if len(args) == 0 {
		return ""
	}
	switch args[0] {
	case "run", "run-script":
		args = args[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "-") {
			args = args[1:]
		}
	case "exec", "dlx", "x":
		return commandRunner(scripts, args[1:], depth)
	}
	if len(args) == 0 {
		return ""
	}
	if script, ok := scripts[args[0]]; ok {
		return scriptRunner(scripts, script, depth+1)
	}
	// Not a script of the package: a binary the package manager runs.
	return commandRunner(scripts, args, depth)
}

// Command is how the test task starts a project's runner.
type Command struct {
	Runner Runner
	// Program is the executable: Bun for RunnerBun, node otherwise.
	Program string
	// Script is the JavaScript entry of the runner's package that node runs;
	// "" for Bun.
	Script string
	// CoverageUnavailable names why the run cannot instrument coverage, or ""
	// when it can.
	CoverageUnavailable string
}

// ResolveCommand finds the programs that start runner for the project at
// projectPath. Vitest and Jest run the bin entry their package declares,
// installed in node_modules of the project or of a directory above it up to
// workspaceRoot, with the node on PATH.
func ResolveCommand(runner Runner, resolveBun func() (string, error), projectPath, workspaceRoot string) (Command, error) {
	if runner == RunnerBun {
		bun, err := resolveBun()
		if err != nil {
			return Command{}, err
		}
		return Command{Runner: runner, Program: bun}, nil
	}
	script, err := packageBin(projectPath, workspaceRoot, string(runner))
	if err != nil {
		return Command{}, err
	}
	node, err := lookPath("node")
	if err != nil {
		return Command{}, fmt.Errorf("the test runner of this project is %s, which runs on node, and no node is on PATH: install Node.js, or choose bun with --runner bun or options[\"@putnami/typescript:test\"].runner", runner)
	}
	command := Command{Runner: runner, Program: node, Script: script}
	if runner == RunnerVitest && !hasVitestCoverageProvider(projectPath, workspaceRoot) {
		command.CoverageUnavailable = "no Vitest coverage provider is installed (@vitest/coverage-v8 or @vitest/coverage-istanbul): the run reports no coverage"
	}
	return command, nil
}

// packageBin returns the JavaScript entry the bin field of the runner's
// package names. node runs that entry directly: a node_modules/.bin shim is a
// shell script or a Windows wrapper.
func packageBin(projectPath, workspaceRoot, runner string) (string, error) {
	manifest := findUp(projectPath, workspaceRoot, filepath.Join("node_modules", runner, "package.json"))
	if manifest == "" {
		return "", fmt.Errorf("the test runner of this project is %s, and no %s package is installed in it or in the workspace: add %s to its devDependencies and run putnami install", runner, runner, runner)
	}
	data, err := os.ReadFile(manifest) //nolint:gosec // an installed package manifest
	if err != nil {
		return "", err
	}
	var pkg struct {
		Bin json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", fmt.Errorf("%s: %w", manifest, err)
	}
	var entry string
	if json.Unmarshal(pkg.Bin, &entry) != nil {
		var bins map[string]string
		_ = json.Unmarshal(pkg.Bin, &bins)
		entry = bins[runner]
	}
	if entry == "" {
		return "", fmt.Errorf("%s declares no %s bin entry", manifest, runner)
	}
	return filepath.Join(filepath.Dir(manifest), filepath.FromSlash(entry)), nil
}

// hasVitestCoverageProvider reports whether a Vitest coverage provider is
// installed for the project.
func hasVitestCoverageProvider(projectPath, workspaceRoot string) bool {
	for _, provider := range []string{"coverage-v8", "coverage-istanbul"} {
		if findUp(projectPath, workspaceRoot, filepath.Join("node_modules", "@vitest", provider, "package.json")) != "" {
			return true
		}
	}
	return false
}

// findUp returns the first existing dir/rel from start up to stop, both
// included; "" when none exists. It never walks above stop, and looks in start
// alone when start is not under stop.
func findUp(start, stop, rel string) string {
	dir := filepath.Clean(start)
	stop = filepath.Clean(stop)
	for {
		candidate := filepath.Join(dir, rel)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		if dir == stop || stop == "" {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(parent+string(filepath.Separator), stop+string(filepath.Separator)) {
			return ""
		}
		dir = parent
	}
}
