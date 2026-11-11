package lint

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// The linters a project chooses between.
const (
	LinterBiome  = "biome"
	LinterESLint = "eslint"
)

// CodeLinterUnknown is the diagnostic code of a lint.linter option that names
// neither biome nor eslint.
const CodeLinterUnknown = "typescript.lint_linter_unknown"

// LinterOption is the lint member of options["@putnami/typescript"], which
// reaches the task as its lint parameter.
type LinterOption struct {
	Linter string `json:"linter"`
}

// ReadLinterOption decodes the lint parameter: an object whose linter member
// names the project's linter. An absent parameter answers "". A parameter
// that is no such object is an error.
func ReadLinterOption(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var option LinterOption
	if err := json.Unmarshal(raw, &option); err != nil {
		return "", fmt.Errorf("%s: options[\"@putnami/typescript\"].lint must be an object such as {\"linter\": \"eslint\"}: %w", CodeLinterUnknown, err)
	}
	return option.Linter, nil
}

// ChooseLinter returns the linter of the project at projectPath. The option,
// options["@putnami/typescript"].lint.linter, decides when it is set; another
// value than biome or eslint is an error. Without it, the linter is the one
// the lint script of the project's package.json runs, and Biome otherwise. A
// configuration file present never chooses: a workspace install writes a
// root biome.json whatever the projects lint with.
func ChooseLinter(option, projectPath string) (string, error) {
	switch strings.TrimSpace(option) {
	case LinterBiome:
		return LinterBiome, nil
	case LinterESLint:
		return LinterESLint, nil
	case "":
	default:
		return "", fmt.Errorf("%s: options[\"@putnami/typescript\"].lint.linter is %q; it names biome or eslint", CodeLinterUnknown, option)
	}
	if ScriptLinter(lintScript(projectPath)) == LinterESLint {
		return LinterESLint, nil
	}
	return LinterBiome, nil
}

// lintScript returns the lint script of the package.json in projectPath, or
// "" when it has none or cannot be read.
func lintScript(projectPath string) string {
	data, err := os.ReadFile(filepath.Join(projectPath, "package.json"))
	if err != nil {
		return ""
	}
	var manifest struct {
		Scripts map[string]any `json:"scripts"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return ""
	}
	script, _ := manifest.Scripts["lint"].(string)
	return script
}

// CodeScriptArgumentsIgnored is the diagnostic code of the warning that an
// ESLint project's lint script carries arguments or commands the lint does
// not run.
const CodeScriptArgumentsIgnored = "typescript.lint_script_arguments_ignored"

// IgnoredScriptPart returns what an ESLint lint of the project at projectPath
// does not run of its lint script: the arguments ESLint receives besides ".",
// and the commands the script chains after it. It returns "" when the script
// runs no eslint first, or runs it bare.
func IgnoredScriptPart(projectPath string) string {
	script := strings.TrimSpace(lintScript(projectPath))
	if ScriptLinter(script) != LinterESLint {
		return ""
	}
	first := firstCommand(script)
	fields := strings.Fields(first)
	for _, prefix := range runnerPrefixes {
		if len(fields) > len(prefix) && equalFields(fields[:len(prefix)], prefix) {
			fields = fields[len(prefix):]
			break
		}
	}
	var ignored []string
	for _, field := range fields[1:] {
		if field != "." {
			ignored = append(ignored, field)
		}
	}
	if rest := strings.TrimSpace(script[len(first):]); rest != "" {
		ignored = append(ignored, rest)
	}
	return strings.Join(ignored, " ")
}

// runnerPrefixes are the commands that run a package binary in a script.
var runnerPrefixes = [][]string{
	{"npx"}, {"bunx"}, {"pnpm", "exec"}, {"pnpm"}, {"yarn", "exec"}, {"yarn"}, {"npm", "exec"},
}

// ScriptLinter returns the linter the first command of a package script
// runs: eslint or biome, run plainly, by a node_modules/.bin path, or through
// npx, bunx, pnpm exec, yarn or npm exec. It returns "" for any other
// program.
func ScriptLinter(script string) string {
	fields := strings.Fields(firstCommand(script))
	for _, prefix := range runnerPrefixes {
		if len(fields) > len(prefix) && equalFields(fields[:len(prefix)], prefix) {
			fields = fields[len(prefix):]
			break
		}
	}
	if len(fields) == 0 {
		return ""
	}
	program := path.Base(filepath.ToSlash(fields[0]))
	switch program {
	case "eslint":
		return LinterESLint
	case "biome":
		return LinterBiome
	}
	return ""
}

// firstCommand returns script up to its first shell operator that joins or
// pipes commands: &&, ||, ;, | or &.
func firstCommand(script string) string {
	if i := strings.IndexAny(script, "&|;"); i >= 0 {
		return script[:i]
	}
	return script
}

func equalFields(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
