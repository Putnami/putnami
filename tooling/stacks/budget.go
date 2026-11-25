package stacks

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.putnami.dev/sdk/extension/cpubudget"
)

// CPUConfig is a file in which a component's own configuration sets a width
// of the stack's tools, such as Gradle's org.gradle.workers.max in
// gradle.properties.
type CPUConfig struct {
	// File is relative to the project directory, or to the stack root when
	// it starts with "{root}/".
	File string `json:"file"`
	// Pattern is a regular expression that matches the setting in the file.
	Pattern string `json:"pattern"`
}

// CPUBudget binds the job's CPU budget to the steps of a task: env with each
// variable of the profile's cpuEnv set to the budget unless env sets it, and
// the value {cpus} expands to. When a component's own configuration sets a
// width (cpuConfig), the budget binds nothing and cpus is empty: each step
// argument that names {cpus} is then left out, so the project's setting
// stands. projects are the project directories the steps run for.
func (p Profile) CPUBudget(env []string, rootAbs string, projects ...string) (bound []string, cpus string) {
	for _, project := range projects {
		if p.configuresWidth(project, rootAbs) {
			return env, ""
		}
	}
	return cpubudget.Bind(env, p.CPUEnv...), strconv.Itoa(cpubudget.ThreadsIn(env))
}

// configuresWidth reports whether one of the profile's cpuConfig files holds
// its setting for the component at projectAbs.
func (p Profile) configuresWidth(projectAbs, rootAbs string) bool {
	for _, c := range p.CPUConfig {
		path := filepath.Join(projectAbs, filepath.FromSlash(c.File))
		if rel, ok := strings.CutPrefix(c.File, "{root}/"); ok {
			path = filepath.Join(rootAbs, filepath.FromSlash(rel))
		}
		data, err := os.ReadFile(path) //nolint:gosec // a file of the component the profile names
		if err != nil {
			continue
		}
		if re, err := regexp.Compile(c.Pattern); err == nil && re.Match(data) {
			return true
		}
	}
	return false
}

// withoutCPUArgs answers steps with each argument that names {cpus} left out.
func withoutCPUArgs(steps [][]string) [][]string {
	out := make([][]string, len(steps))
	for i, step := range steps {
		for _, arg := range step {
			if !strings.Contains(arg, CPUsPlaceholder) {
				out[i] = append(out[i], arg)
			}
		}
	}
	return out
}

// validateCPUBudget checks cpuEnv and cpuConfig, and that a step names {cpus}
// within an argument that holds its flag, such as --jobs={cpus}: the argument
// is left out whole when the component configures its width.
func (p Profile) validateCPUBudget() error {
	for i, name := range p.CPUEnv {
		if name == "" || strings.ContainsAny(name, "= ") {
			return fmt.Errorf("cpuEnv[%d]: %q is not a variable name", i, name)
		}
	}
	for i, c := range p.CPUConfig {
		if c.File == "" || filepath.IsAbs(c.File) || strings.Contains(c.File, "..") {
			return fmt.Errorf("cpuConfig[%d].file: %q is not a relative path", i, c.File)
		}
		if _, err := regexp.Compile(c.Pattern); err != nil || c.Pattern == "" {
			return fmt.Errorf("cpuConfig[%d].pattern: %q is not a regular expression", i, c.Pattern)
		}
	}
	return nil
}

// checkCPUArg refuses a step argument that is {cpus} alone.
func checkCPUArg(arg string) error {
	if arg == CPUsPlaceholder {
		return fmt.Errorf("%s stands alone; write it with its flag, such as --jobs=%s", CPUsPlaceholder, CPUsPlaceholder)
	}
	return nil
}
