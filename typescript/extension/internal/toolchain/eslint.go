package toolchain

import (
	"errors"
	"os/exec"
)

// ErrESLintNotFound is the error ResolveESLint returns when no
// node_modules/.bin from the project up to the workspace holds eslint.
var ErrESLintNotFound = errors.New("eslint not found in node_modules/.bin from the project up to the workspace: install eslint as a devDependency")

// ErrNodeNotFound is the error ResolveNode returns when no node is on PATH.
var ErrNodeNotFound = errors.New("ESLint runs on Node, and no node is on PATH: install Node, or lint the project with Biome (options[\"@putnami/typescript\"].lint.linter: biome)")

// lookPath finds a program on PATH. A package var so tests choose the answer.
var lookPath = exec.LookPath

// ResolveESLint finds the file that starts eslint for a project: the nearest
// node_modules/.bin/eslint from the project up to the workspace, eslint.exe
// then eslint.cmd on Windows. Unlike Biome, an eslint on PATH is never used:
// the project's own release reads the project's configuration and plugins.
func ResolveESLint(projectRoot, workspaceRoot string) (string, error) {
	return resolveESLintOn(hostPlatform(), projectRoot, workspaceRoot)
}

// resolveESLintOn is ResolveESLint for a process that runs on p.
func resolveESLintOn(p platform, projectRoot, workspaceRoot string) (string, error) {
	path := resolveInNodeModulesFor("eslint", projectRoot, workspaceRoot, p)
	if path == "" {
		return "", ErrESLintNotFound
	}
	return path, nil
}

// ResolveNode finds the node program on PATH that ESLint runs on.
func ResolveNode() (string, error) {
	path, err := lookPath("node")
	if err != nil || path == "" {
		return "", ErrNodeNotFound
	}
	return path, nil
}

// ESLintCommand returns the program and the arguments that start eslintBin
// with args. A script whose shebang names node, the symbolic link npm, Bun and
// Yarn write in node_modules/.bin, starts with node, the Node on PATH that
// ResolveNode found; Bun never starts ESLint, whose plugins expect Node. Any
// other file, such as the shell script pnpm writes or a Windows shim, starts as
// itself and finds node on PATH.
func ESLintCommand(eslintBin, node string, args []string) (string, []string, error) {
	if node == "" {
		return "", nil, ErrNodeNotFound
	}
	if !isNodeScript(eslintBin) {
		return eslintBin, args, nil
	}
	return node, append([]string{eslintBin}, args...), nil
}
