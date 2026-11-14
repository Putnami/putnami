package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/typescript/extension/internal/lint"
)

// The workspace devDependencies of the extension provide the tools its jobs
// run: Biome for lint, the TypeScript compiler and Bun's types for the build,
// the package and the type check of the tests, and the TypeScript parser for
// the API surface validate reads. A repository adopted in import mode keeps on
// its own tool every task the extension does not run with the same tool, so a
// devDependency, and a root configuration file, is written only when a member
// runs a job that reads it: writing it otherwise changes the lockfile, or the
// tool the repository checks its code with, for nothing.

// typeScriptExtensionName is the name a putnami.json uses for this extension.
const typeScriptExtensionName = "@putnami/typescript"

// typeCheckJobs are the jobs that run the TypeScript compiler with Bun's types
// (--types bun) and resolve the project's tsconfig, else the workspace root's.
// package counts only for a member whose build the extension runs, since it
// builds with the build's own steps, and test only for a member whose test
// type-checks.
var typeCheckJobs = []string{"build", "package", "test"}

// workspaceDevDependencyJobs names the jobs that need each workspace
// devDependency. A devDependency it does not list is needed by every job.
var workspaceDevDependencyJobs = map[string][]string{
	"@biomejs/biome": {"lint"},
	"@types/bun":     typeCheckJobs,
	"typescript":     append(slices.Clone(typeCheckJobs), "validate"),
}

// devDependencyNeed reports which workspace devDependencies and root
// configuration files the members of the job context need. A context without
// the membership needs them all: an unknown membership never decides what a
// workspace lacks.
type devDependencyNeed struct {
	known bool
	// jobs are the jobs some member runs on this extension: lint only when
	// that member lints with Biome, package only when it runs the build too,
	// and test only when its test type-checks.
	jobs map[string]bool
	// rootTSConfig reports a member whose own tsconfig.json extends the
	// workspace root's, as the one an import writes (D-066).
	rootTSConfig bool
}

func devDependencyNeeds(ctx *pctx.Context) devDependencyNeed {
	if len(ctx.WorkspaceProjects) == 0 {
		return devDependencyNeed{}
	}
	need := devDependencyNeed{known: true, jobs: map[string]bool{}}
	for _, member := range typeScriptMembers(ctx.WorkspaceRoot, ctx.WorkspaceProjects) {
		dir := filepath.Join(ctx.WorkspaceRoot, filepath.FromSlash(member.Path))
		config := readMemberConfig(dir)
		if !config.runsOn(typeScriptExtensionName) {
			continue
		}
		for _, job := range []string{"build", "lint", "package", "test", "validate"} {
			if config.disables(typeScriptExtensionName, job) {
				continue
			}
			switch job {
			case "lint":
				if linter, err := config.linter(dir); err != nil || linter != lint.LinterBiome {
					continue
				}
			case "package":
				if config.disables(typeScriptExtensionName, "build") {
					continue
				}
			case "test":
				if !config.testTypeChecks() {
					continue
				}
			}
			need.jobs[job] = true
		}
		need.rootTSConfig = need.rootTSConfig || extendsRootTSConfig(ctx.WorkspaceRoot, dir)
	}
	return need
}

// needs reports whether the workspace needs the devDependency name.
func (n devDependencyNeed) needs(name string) bool {
	if !n.known {
		return true
	}
	jobs, listed := workspaceDevDependencyJobs[name]
	if !listed {
		return len(n.jobs) > 0
	}
	return slices.ContainsFunc(jobs, func(job string) bool { return n.jobs[job] })
}

// needsBiomeConfig reports whether the workspace needs a root biome.json: a
// member lints with Biome.
func (n devDependencyNeed) needsBiomeConfig() bool {
	return !n.known || n.jobs["lint"]
}

// needsRootTSConfig reports whether the workspace needs a root tsconfig.json:
// a member runs a job that resolves its tsconfig, which falls back to the
// root's, or holds a tsconfig.json that extends the root's. A lint and a
// validate read none.
func (n devDependencyNeed) needsRootTSConfig() bool {
	return !n.known || n.rootTSConfig || slices.ContainsFunc(typeCheckJobs, func(job string) bool { return n.jobs[job] })
}

// memberConfig is what a member's putnami.json says about the extensions and
// jobs it runs.
type memberConfig struct {
	Extensions *[]string                             `json:"extensions"`
	Options    map[string]map[string]json.RawMessage `json:"options"`
	Disable    struct {
		Extensions []string `json:"extensions"`
		Jobs       []string `json:"jobs"`
	} `json:"disable"`
}

// readMemberConfig reads the putnami.json in dir; a member without one, or
// with one it cannot read, runs every job of its extensions.
func readMemberConfig(dir string) memberConfig {
	var config memberConfig
	data, err := os.ReadFile(filepath.Join(dir, "putnami.json")) //nolint:gosec // a member config under the workspace root
	if err != nil {
		return config
	}
	_ = json.Unmarshal(data, &config)
	return config
}

// runsOn reports whether the member runs extension: it names no extensions,
// so its manifests choose them, or it names extension, and does not disable it.
func (c memberConfig) runsOn(extension string) bool {
	if c.Extensions != nil && !slices.Contains(*c.Extensions, extension) {
		return false
	}
	return !slices.Contains(c.Disable.Extensions, extension)
}

// disables reports whether the member disables job, by its name or as
// extension:job.
func (c memberConfig) disables(extension, job string) bool {
	return slices.Contains(c.Disable.Jobs, job) || slices.Contains(c.Disable.Jobs, extension+":"+job)
}

// testTypeChecks reports whether the member's test type-checks: the types
// option of options["@putnami/typescript:test"] is not false. An import sets
// it false for a test it moves, which type-checked nothing.
func (c memberConfig) testTypeChecks() bool {
	var types bool
	if raw, ok := c.Options[typeScriptExtensionName+":test"]["types"]; ok && json.Unmarshal(raw, &types) == nil {
		return types
	}
	return true
}

// extendsRootTSConfig reports whether the tsconfig.json of the member at dir
// extends the tsconfig.json of the workspace root.
func extendsRootTSConfig(root, dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "tsconfig.json")) //nolint:gosec // a member config under the workspace root
	if err != nil {
		return false
	}
	want := filepath.Join(root, "tsconfig.json")
	for _, specifier := range fileSpecifiers(data, true) {
		if !strings.HasPrefix(specifier, ".") {
			continue
		}
		// TypeScript reads a relative specifier without an extension as the
		// .json file of that name.
		if resolved := filepath.Join(dir, filepath.FromSlash(specifier)); resolved == want || resolved+".json" == want {
			return true
		}
	}
	return false
}

// linter returns the linter the member at dir lints with: the lint option of
// its own putnami.json, else what its lint script runs (lint.ChooseLinter).
func (c memberConfig) linter(dir string) (string, error) {
	option, err := lint.ReadLinterOption(c.Options[typeScriptExtensionName]["lint"])
	if err != nil {
		return "", err
	}
	return lint.ChooseLinter(option, dir)
}
