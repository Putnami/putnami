package lint

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"go.putnami.dev/go/extension/internal/parse"
	"go.putnami.dev/go/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
)

// Batch staticcheck reuses ONE staticcheck process across several compatible
// projects: `staticcheck ./m1/... ./m2/... …` run from the governing `go.work`
// root instead of `cd $m && staticcheck ./...` once per module.
//
// What the batch may share, and why it needs no config partition. golangci-lint
// applies a single `--config` per invocation, so its batch has to group projects
// by their effective config file (lint_batch.go). staticcheck does not: it
// filters each result through `res.Config.Checks`, resolved PER PACKAGE from
// that package's own `staticcheck.conf` hierarchy, so a `staticcheck.conf` next
// to one member narrows that member's subtree and only that subtree, batched or
// not. The only grouping key a shared invocation actually needs is the governing
// `go.work` root — every member's package pattern must be expressible relative
// to the directory the one process runs from, and every member must resolve
// through the same workspace file.
//
// The economics: one process over a 12-module group costs 3.16 s cold / 0.58 s
// warm against 8.56 s / 3.45 s for twelve processes (2.7x cold, 5.9x warm), and
// ~0.29 s of the per-invocation floor is pure process and package-listing
// overhead that buys no analysis at all.
//
// NOT narrowing. The same audit measured `-checks=` and found it is a
// POST-ANALYSIS DISPLAY FILTER — running with every check disabled costs the
// same wall time and writes a byte-identical analysis cache — so this file adds
// no check selection. The invocation stays `staticcheck <patterns>`, exactly the
// solo path's check set.

type staticcheckBatchProject struct {
	ref      pctx.ProjectRef
	fullPath string
	// pattern is the package pattern this member contributes to the shared
	// invocation, relative to its group root ("./go/framework/errors/..."). It is
	// retained so a pattern-scoped tool error can be attributed back to the one
	// member that asked for it.
	pattern string
}

type staticcheckBatchGroup struct {
	groupRoot string
	projects  []*staticcheckBatchProject
}

// runStaticcheckBatch runs one staticcheck invocation per governing go.work root
// and returns ordinary per-project results for the scheduler to split. A
// one-project group still uses this protocol: a mixed cache hit/miss batch can
// leave a single project to execute after the scheduler restores the hits.
func runStaticcheckBatch(ctx *pctx.Context, _ lintOptions) (string, map[string]any, error) {
	if _, err := toolchain.ResolveGo(); err != nil {
		return "OK", map[string]any{
			"batchResults": failAllProjects(ctx.SelectedProjects, err.Error()),
		}, nil
	}
	binary, err := resolveStaticcheckBinary(ctx.WorkspaceRoot)
	if err != nil {
		return "OK", map[string]any{
			"batchResults": failAllProjects(
				ctx.SelectedProjects,
				err.Error()+"; install the pinned tools with `putnami deps install --tag go` before running lint",
			),
		}, nil
	}

	results := make([]batchProjectResult, len(ctx.SelectedProjects))
	index := make(map[string]int, len(ctx.SelectedProjects))
	groups := make(map[string]*staticcheckBatchGroup)
	var order []string

	for i, ref := range ctx.SelectedProjects {
		results[i] = batchProjectResult{ProjectID: ref.ID, Status: "OK"}
		index[ref.ID] = i

		fullPath := ref.FullPath
		if fullPath == "" {
			fullPath = filepath.Join(ctx.WorkspaceRoot, ref.Path)
		}
		groupRoot := fullPath
		if goWork := toolchain.FindGoWork(fullPath); goWork != "" {
			groupRoot = filepath.Dir(goWork)
		}
		rel, relErr := batchRel(groupRoot, fullPath)
		if relErr != nil {
			results[i] = failedProjectResult(ref.ID, relErr.Error())
			continue
		}

		group := groups[groupRoot]
		if group == nil {
			group = &staticcheckBatchGroup{groupRoot: groupRoot}
			groups[groupRoot] = group
			order = append(order, groupRoot)
		}
		group.projects = append(group.projects, &staticcheckBatchProject{
			ref:      ref,
			fullPath: fullPath,
			pattern:  patternForRel(rel),
		})
	}

	// Groups run SEQUENTIALLY, unlike the golangci batch. There a coarse
	// scheduler group routinely splits into several config groups that would
	// otherwise serialize under one shared deadline, which is why it pays for
	// concurrency. Here a group is exactly one governing go.work root: a batch
	// spanning two roots is the rare case, and running the groups one at a time
	// keeps the peak memory of a shared analysis process bounded to one process
	// — the property `maxProjects` exists to protect.
	for _, key := range order {
		for _, result := range executeStaticcheckGroup(binary, groups[key], ctx.WorkspaceRoot) {
			if i, ok := index[result.ProjectID]; ok {
				results[i] = result
			}
		}
	}

	// A successful batch protocol may still contain failed projects. Returning
	// OK lets the scheduler retain independent DAG and cache outcomes.
	return "OK", map[string]any{"batchResults": results}, nil
}

// staticcheckPatternErrorRe matches the package-pattern errors staticcheck
// reports without a source position, e.g.
//
//	-: pattern ./go/framework/api/...: directory prefix … does not contain modules listed in go.work…
//
// The pattern names exactly one batch member, so the error is attributable even
// though nothing in it carries a file. Without this the shared process would
// report a member's load failure as unattributed group noise while a solo run
// fails that project outright.
var staticcheckPatternErrorRe = regexp.MustCompile(`\bpattern\s+(\S+?):`)

func executeStaticcheckGroup(
	binary string,
	group *staticcheckBatchGroup,
	workspaceRoot string,
) []batchProjectResult {
	if group == nil || len(group.projects) == 0 {
		return nil
	}

	patterns := make([]string, 0, len(group.projects))
	for _, project := range group.projects {
		patterns = append(patterns, project.pattern)
	}

	output, runErr := runStaticcheckCommand(binary, patterns, group.groupRoot)
	outStr := string(output)

	diagnostics := make(map[string][]parse.ToolDiagnostic, len(group.projects))
	failed := make(map[string]bool, len(group.projects))
	attributed := false
	appendDiag := func(id string, diag parse.ToolDiagnostic) {
		diagnostics[id] = append(diagnostics[id], diag)
		failed[id] = true
		attributed = true
	}

	findings, leftover := parse.LintFindings(outStr)
	owners := staticcheckOwners(group.projects)
	skipped, sawCompat := staticcheckCompatSkips(findings, leftover, group, owners, outStr)

	var unattributed []string
	for _, finding := range findings {
		id := batchProjectForFile(finding.File, group.groupRoot, owners)
		if id == "" {
			// A finding whose file matches no selected project: keep it as
			// unattributed evidence rather than silently dropping it.
			unattributed = append(unattributed, finding.File+": "+finding.Message)
			continue
		}
		if skipped[id] {
			continue
		}
		appendDiag(id, parse.ToolDiagnostic{
			Severity:    "error",
			Description: finding.Message,
			File:        parse.DiagPath(workspaceRoot, group.groupRoot, finding.File),
			Line:        finding.Line,
			Column:      finding.Column,
		})
	}
	for _, line := range leftover {
		id := staticcheckPatternOwner(line, group.projects)
		if id == "" {
			unattributed = append(unattributed, line)
			continue
		}
		if skipped[id] {
			continue
		}
		appendDiag(id, parse.ToolDiagnostic{
			Severity:    "error",
			Description: strings.TrimSpace(line),
		})
	}

	// The run failed without anything a member can be charged for (a crash, a bad
	// flag): surface the raw tail as a group-level failure rather than reporting
	// every member bare green.
	//
	// A toolchain-mismatch abort is deliberately NOT that case. It is the solo
	// path's SKIP, and when it is the only thing in the output every member ends
	// OK with no diagnostics — which is what falling through to
	// buildStaticcheckResults already produces.
	if runErr != nil && !attributed && !sawCompat {
		tail := strings.TrimSpace(strings.Join(unattributed, "\n"))
		if tail == "" {
			tail = "staticcheck failed without attributable findings"
		}
		for _, project := range group.projects {
			appendDiag(project.ref.ID, parse.ToolDiagnostic{Severity: "error", Description: tail})
		}
	}

	return buildStaticcheckResults(group.projects, diagnostics, failed)
}

// staticcheckCompatSkips locates the toolchain-mismatch abort in a shared run
// and returns the members it silences, plus whether any compat evidence was seen
// at all.
//
// The solo path SKIPS a project it cannot type-check — the pinned binary is
// older than that project's sources require, so it analyzes nothing and reports
// nothing — rather than failing it. A batch must reproduce that at MEMBER
// granularity, not group granularity. Testing the combined output and skipping
// everything was a silent-green bug: one member carrying a `//go:build go1.NN`
// file newer than the pinned binary would suppress every OTHER member's real
// findings, and each of those members would then cache its own bare OK under its
// own key and keep serving it warm until its sources changed. That is exactly
// the mid-toolchain-upgrade state, so "one binary against one go.work makes the
// abort all-or-nothing" is false — Go version requirements are per file and per
// module.
//
// The abort line carries a position (`x.go:1:1: file requires newer Go version
// …`), so it attributes like any other finding. A member named by one is
// silenced WHOLESALE — its other findings dropped too — because that is what its
// solo run does with the same output.
//
// combined is the raw output, consulted only as a fallback: the signature is a
// pair of substrings and a future staticcheck could split them across lines,
// where no single member can be named. Recording it as group-wide evidence keeps
// such a shape a skip when nothing else is attributable, without letting it
// silence findings that WERE attributed.
func staticcheckCompatSkips(
	findings []parse.LintFinding,
	leftover []string,
	group *staticcheckBatchGroup,
	owners []batchFileOwner,
	combined string,
) (map[string]bool, bool) {
	skipped := make(map[string]bool, len(group.projects))
	sawCompat := false
	for _, finding := range findings {
		if !isStaticcheckToolchainCompatibilityError(finding.Message) {
			continue
		}
		sawCompat = true
		if id := batchProjectForFile(finding.File, group.groupRoot, owners); id != "" {
			skipped[id] = true
		}
	}
	for _, line := range leftover {
		if !isStaticcheckToolchainCompatibilityError(line) {
			continue
		}
		sawCompat = true
		if id := staticcheckPatternOwner(line, group.projects); id != "" {
			skipped[id] = true
		}
	}
	if !sawCompat && isStaticcheckToolchainCompatibilityError(combined) {
		sawCompat = true
	}
	return skipped, sawCompat
}

// staticcheckPatternOwner returns the member whose package pattern a
// position-less tool error names, or "".
func staticcheckPatternOwner(line string, projects []*staticcheckBatchProject) string {
	match := staticcheckPatternErrorRe.FindStringSubmatch(line)
	if match == nil {
		return ""
	}
	for _, project := range projects {
		if project.pattern == match[1] {
			return project.ref.ID
		}
	}
	return ""
}

// staticcheckOwners is the group's attribution table, built once per invocation.
// staticcheck reports paths relative to its run directory (the group root), so
// batchProjectForFile resolves them there before charging the longest matching
// project root.
//
// Nested modules keep their own findings without help here: `go`'s pattern
// expansion stops at a module boundary, so `./go/framework/migration/...` never
// yields packages from the nested `migration/migratecli` module, exactly as the
// solo `./...` in that directory does not.
func staticcheckOwners(projects []*staticcheckBatchProject) []batchFileOwner {
	owners := make([]batchFileOwner, 0, len(projects))
	for _, project := range projects {
		owners = append(owners, batchFileOwner{id: project.ref.ID, root: project.fullPath})
	}
	return owners
}

func buildStaticcheckResults(
	projects []*staticcheckBatchProject,
	diagnostics map[string][]parse.ToolDiagnostic,
	failed map[string]bool,
) []batchProjectResult {
	results := make([]batchProjectResult, 0, len(projects))
	for _, project := range projects {
		diags := diagnostics[project.ref.ID]
		summary := summarizeLintDiagnostics(diags)
		status := "OK"
		if failed[project.ref.ID] {
			status = "FAILED"
		}
		results = append(results, batchProjectResult{
			ProjectID:   project.ref.ID,
			Status:      status,
			Data:        map[string]any{"lintSummary": map[string]any{"errors": summary.Errors}},
			Diagnostics: diags,
			Summary:     summary,
		})
	}
	return results
}

// runStaticcheckCommand is a package variable so tests can substitute the tool.
// The environment is built exactly as the solo path builds it, from the
// directory the process runs in: for a batch that is the go.work root, so GOWORK
// resolves to the same workspace file every member's solo run would have used.
var runStaticcheckCommand = func(binary string, patterns []string, dir string) ([]byte, error) {
	cmd := exec.Command(binary, patterns...)
	cmd.Dir = dir
	cmd.Env = toolchain.WorkspaceBuildEnv(os.Environ(), dir, toolchain.CurrentGoBinary())
	return cmd.CombinedOutput()
}

// resolveStaticcheckBinary is a package variable so tests can bypass the real
// toolchain resolution and install fallback.
var resolveStaticcheckBinary = func(workspaceRoot string) (string, error) {
	binary, err := toolchain.ResolvePinnedToolBinary("staticcheck", workspaceRoot)
	if err == nil {
		return binary, nil
	}
	binary, installErr := toolchain.InstallTool("staticcheck", toolchain.CurrentGoBinary(), workspaceRoot)
	if installErr != nil {
		return "", fmt.Errorf("%w; unable to install pinned staticcheck: %w", err, installErr)
	}
	return binary, nil
}
