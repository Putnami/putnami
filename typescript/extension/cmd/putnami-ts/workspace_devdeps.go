package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/typescript/extension/internal/lint"
)

// The workspace devDependencies of the extension provide the tools its jobs
// run: Biome for lint, and the TypeScript compiler and Bun's types for the
// build, the package, the API surface validate reads and the type check of
// the tests. A repository adopted in import
// mode keeps on its own tool every task the extension does not run with the
// same tool, so a devDependency is added only when a member runs a job that
// needs it: adding it otherwise changes the lockfile for nothing.

// typeScriptExtensionName is the name a putnami.json uses for this extension.
const typeScriptExtensionName = "@putnami/typescript"

// typeCheckJobs are the jobs that run the TypeScript compiler, which every run
// of the extension points at Bun's types (--types bun).
var typeCheckJobs = []string{"build", "package", "test", "validate"}

// workspaceDevDependencyJobs names the jobs that need each workspace
// devDependency. A devDependency it does not list is needed by every job.
var workspaceDevDependencyJobs = map[string][]string{
	"@biomejs/biome": {"lint"},
	"@types/bun":     typeCheckJobs,
	"typescript":     typeCheckJobs,
}

// devDependencyNeed reports which workspace devDependencies the members of
// the job context need. A context without the membership needs them all: an
// unknown membership never decides what a workspace lacks.
type devDependencyNeed struct {
	known bool
	// jobs are the jobs some member runs on this extension, lint only when
	// that member lints with Biome.
	jobs map[string]bool
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
			if job == "lint" {
				if linter, err := config.linter(dir); err != nil || linter != lint.LinterBiome {
					continue
				}
			}
			need.jobs[job] = true
		}
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

// linter returns the linter the member at dir lints with: the lint option of
// its own putnami.json, else what its lint script runs (lint.ChooseLinter).
func (c memberConfig) linter(dir string) (string, error) {
	option, err := lint.ReadLinterOption(c.Options[typeScriptExtensionName]["lint"])
	if err != nil {
		return "", err
	}
	return lint.ChooseLinter(option, dir)
}
