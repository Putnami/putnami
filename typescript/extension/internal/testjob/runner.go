package testjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
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

// extensionName is the key of this extension's layer in a project's options.
const extensionName = "@putnami/typescript"

// RunnerConfigFiles are the project files a Vitest or Jest run reads its
// configuration from. They are inputs of the test task's cache key, declared in
// putnami.extension.json.
var RunnerConfigFiles = []string{"vitest.config.*", "vite.config.*", "jest.config.*"}

// lookPath finds a program on PATH.
var lookPath = osexec.LookPath

// ResolveRunner returns the test runner of the project at projectPath:
// options["@putnami/typescript"].test.runner of its putnami.json, else the
// runner its package.json test script runs, else Bun. It reads the project's
// own files, so every member of a batch resolves its own runner.
func ResolveRunner(projectPath string) (Runner, error) {
	configured, err := configuredRunner(filepath.Join(projectPath, "putnami.json"))
	if err != nil || configured != "" {
		return configured, err
	}
	if runner := scriptRunner(packageTestScript(filepath.Join(projectPath, "package.json"))); runner != "" {
		return runner, nil
	}
	return RunnerBun, nil
}

// configuredRunner reads options["@putnami/typescript"].test.runner of a
// putnami.json; "" when the file or the member is absent.
func configuredRunner(path string) (Runner, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the project's own config file
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var doc struct {
		Options map[string]struct {
			Test struct {
				Runner *string `json:"runner"`
			} `json:"test"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	value := doc.Options[extensionName].Test.Runner
	if value == nil {
		return "", nil
	}
	switch runner := Runner(*value); runner {
	case RunnerBun, RunnerVitest, RunnerJest:
		return runner, nil
	}
	return "", fmt.Errorf("%s: options[%q].test.runner is %q: choose bun, vitest or jest", path, extensionName, *value)
}

// packageTestScript reads scripts.test of a package.json; "" when absent or
// unreadable.
func packageTestScript(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // the project's own manifest
	if err != nil {
		return ""
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return ""
	}
	return manifest.Scripts["test"]
}

// scriptLaunchers are the words that start another program in a package
// script: the runner is the program after them.
var scriptLaunchers = map[string]bool{"npx": true, "bunx": true, "cross-env": true, "dotenv": true, "--": true}

// scriptRunner returns the runner the first command of a package script runs,
// or "" when it runs none of them.
func scriptRunner(script string) Runner {
	command := script
	for _, separator := range []string{"&&", "||", ";", "|"} {
		command, _, _ = strings.Cut(command, separator)
	}
	words := strings.Fields(command)
	for i := 0; i < len(words); i++ {
		word := words[i]
		switch {
		case strings.Contains(word, "=") && !strings.HasPrefix(word, "-"):
			// An environment assignment before the program.
			continue
		case scriptLaunchers[word]:
			continue
		case (word == "pnpm" || word == "yarn" || word == "npm") && i+1 < len(words) && (words[i+1] == "exec" || words[i+1] == "dlx"):
			i++
			continue
		case word == "yarn" || word == "pnpm":
			continue
		}
		switch filepath.Base(word) {
		case "vitest":
			return RunnerVitest
		case "jest":
			return RunnerJest
		case "bun":
			if i+1 < len(words) && words[i+1] == "test" {
				return RunnerBun
			}
		}
		return ""
	}
	return ""
}

// Command is how the test task starts a project's runner.
type Command struct {
	Runner Runner
	// Program is the executable: Bun for RunnerBun, node otherwise.
	Program string
	// Script is the runner's entry from node_modules/.bin that node runs; ""
	// for Bun.
	Script string
	// CoverageUnavailable names why the run cannot instrument coverage, or ""
	// when it can.
	CoverageUnavailable string
}

// ResolveCommand finds the programs that start runner for the project at
// projectPath. Vitest and Jest run from node_modules/.bin of the project or
// of a directory above it up to workspaceRoot, with the node on PATH.
func ResolveCommand(runner Runner, resolveBun func() (string, error), projectPath, workspaceRoot string) (Command, error) {
	if runner == RunnerBun {
		bun, err := resolveBun()
		if err != nil {
			return Command{}, err
		}
		return Command{Runner: runner, Program: bun}, nil
	}
	script := findUp(projectPath, workspaceRoot, filepath.Join("node_modules", ".bin", string(runner)))
	if script == "" {
		return Command{}, fmt.Errorf("the test runner of this project is %s, and no node_modules/.bin/%s is installed in it or in the workspace: add %s to its devDependencies and run putnami install", runner, runner, runner)
	}
	node, err := lookPath("node")
	if err != nil {
		return Command{}, fmt.Errorf("the test runner of this project is %s, which runs on node, and no node is on PATH: install Node.js, or set options[%q].test.runner to bun in its putnami.json", runner, extensionName)
	}
	command := Command{Runner: runner, Program: node, Script: script}
	if runner == RunnerVitest && !hasVitestCoverageProvider(projectPath, workspaceRoot) {
		command.CoverageUnavailable = "no Vitest coverage provider is installed (@vitest/coverage-v8 or @vitest/coverage-istanbul): the run reports no coverage"
	}
	return command, nil
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
